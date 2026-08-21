package upgrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/stages"
)

// Rollback means two different things depending on which stage failed, and
// conflating them is how people end up surprised.
//
//	Platform components — GENUINE ROLLBACK. Each is a Helm release, so
//	reverting is rewriting its resource at the previous pinned version and
//	letting the cluster's own helm-controller take it back. Seconds, nothing
//	lost.
//
//	Kubernetes — NO SUCH THING AS A DOWNGRADE. Neither Kubernetes nor k3s
//	supports one. Going back means restoring the datastore snapshot taken at
//	stage 2, which is a genuine restore with a service interruption, not a
//	rollback.
//
// AND IT DOES NOT RESTORE APPLICATION DATA, deliberately. A datastore restore
// rolls back Kubernetes objects; persistent volumes are untouched, so
// database contents and uploaded files survive. Rolling application data back
// to the start of the maintenance window would discard every transaction
// since, which is nearly always worse than the failed upgrade.

// Mechanism is how a rollback will be performed.
type Mechanism string

const (
	// MechanismComponents reverts Helm releases. Fast, no data implications.
	MechanismComponents Mechanism = "components"
	// MechanismRestore restores the datastore snapshot. A service
	// interruption, and on single-server a control-plane outage.
	MechanismRestore Mechanism = "restore"
	// MechanismNothing means nothing was changed and there is nothing to
	// undo.
	MechanismNothing Mechanism = "nothing"
)

// Plan describes what a rollback would do, so it can be reported BEFORE
// anything happens and confirmed when it is the expensive kind.
type RollbackPlan struct {
	Mechanism Mechanism
	// From and To are the bundle versions: rolling back moves the cluster
	// from the partially-applied To back to From.
	From, To string
	// Components are the releases that would be reverted.
	Components []string
	// Snapshot is the datastore snapshot that would be restored.
	Snapshot   string
	SnapshotAt time.Time
	// Reason explains which mechanism applies and why.
	Reason string
}

func (p RollbackPlan) String() string {
	var b strings.Builder
	switch p.Mechanism {
	case MechanismNothing:
		fmt.Fprintf(&b, "Nothing to roll back: %s\n", p.Reason)
	case MechanismComponents:
		fmt.Fprintf(&b, "Rolling back %s → %s by reverting platform components.\n", p.To, p.From)
		fmt.Fprintf(&b, "  %s\n", p.Reason)
		for _, c := range p.Components {
			fmt.Fprintf(&b, "  revert %s\n", c)
		}
		b.WriteString("\nThis takes seconds and has no data implications: each component is a Helm\nrelease and reverts to its previous revision.\n")
	case MechanismRestore:
		fmt.Fprintf(&b, "Rolling back %s → %s by RESTORING THE DATASTORE SNAPSHOT.\n", p.To, p.From)
		fmt.Fprintf(&b, "  %s\n", p.Reason)
		fmt.Fprintf(&b, "  snapshot %s, taken %s\n", p.Snapshot, p.SnapshotAt.Format(time.RFC3339))
		b.WriteString("\nThis is a genuine restore, not a rollback:\n")
		b.WriteString("  - the cluster returns to its state at the moment the snapshot was taken\n")
		b.WriteString("  - there is a service interruption while it happens\n")
		b.WriteString("  - your PERSISTENT VOLUMES ARE NOT TOUCHED, so database contents and uploaded\n")
		b.WriteString("    files survive. Cluster objects go back; your data does not.\n")
		b.WriteString("  - a PersistentVolumeClaim created during the upgrade window will not exist in\n")
		b.WriteString("    the restored cluster while its volume still exists on disk. Any found are\n")
		b.WriteString("    named in the report afterwards.\n")
	}
	return b.String()
}

// PlanRollback decides which mechanism applies, from the journal alone.
//
// The decision is the ordering made concrete: if the kubernetes stage never
// started, everything that changed is a Helm release and reverting is cheap.
// Once it has started, the datastore may already hold data in the new schema
// and only a restore goes back.
func PlanRollback(j *stages.Journal, from, to *manifest.Manifest, rec record) RollbackPlan {
	plan := RollbackPlan{
		From: rec.FromBundle, To: rec.ToBundle,
		Snapshot: rec.Snapshot, SnapshotAt: rec.SnapshotAt,
	}
	if plan.From == "" && from != nil {
		plan.From = from.Bundle
	}
	if plan.To == "" && to != nil {
		plan.To = to.Bundle
	}

	started := map[string]bool{}
	for _, e := range j.Entries {
		started[e.Stage] = true
	}
	if !started[StageComponents] {
		plan.Mechanism = MechanismNothing
		plan.Reason = "the upgrade stopped before any component was moved"
		return plan
	}
	if started[StageKubernetes] {
		plan.Mechanism = MechanismRestore
		plan.Reason = "the kubernetes stage started, and Kubernetes does not downgrade"
		return plan
	}

	plan.Mechanism = MechanismComponents
	plan.Reason = "the kubernetes stage never started, so everything that changed is a Helm release"
	if from != nil && to != nil {
		for _, c := range coreComponents() {
			was, _ := from.Core.Version(c.key)
			now, _ := to.Core.Version(c.key)
			if was != "" && was != now {
				plan.Components = append(plan.Components, fmt.Sprintf("%s %s → %s", c.key, now, was))
			}
		}
	}
	return plan
}

// RevertComponents takes every core component back to the bundle it came
// from. This is the cheap path, and it is cheap because of where the
// irreversible stage sits rather than because of anything here.
func RevertComponents(ctx context.Context, r k3s.Runner, from *manifest.Manifest, rep converge.Reporter, log func(string, ...any)) error {
	for _, c := range coreComponents() {
		version, err := from.Core.Version(c.key)
		if err != nil {
			continue
		}
		log("  reverting %s to %s", c.key, version)
		if err := stages.NewComponentError(c.key, c.install(ctx, r, from, rep)); err != nil {
			return err
		}
	}
	return nil
}

// RestoreSnapshot puts the datastore back to the stage-2 snapshot.
//
// This is k3s's own cluster-reset-restore path: the server stops, restores,
// and comes back with the cluster's objects as they were. It is deliberately
// not automatic anywhere — an operator asks for it, having been told what it
// costs.
func RestoreSnapshot(ctx context.Context, r k3s.Runner, snapshot string) error {
	if snapshot == "" {
		return fmt.Errorf("no datastore snapshot was recorded for this upgrade, so there is nothing to restore to. The stage-2 snapshot is taken before any component moves; an upgrade that failed before it has nothing to roll back")
	}
	steps := []struct{ what, command string }{
		{"stopping k3s", "sudo -n systemctl stop k3s"},
		{"restoring the datastore", fmt.Sprintf(
			"sudo -n k3s server --cluster-reset --cluster-reset-restore-path=/var/lib/rancher/k3s/server/db/snapshots/%s", snapshot)},
		{"starting k3s", "sudo -n systemctl start k3s"},
	}
	for _, step := range steps {
		res, err := r.Run(ctx, step.command)
		if err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
		// cluster-reset exits non-zero in some k3s versions after a
		// successful restore because it terminates the server it just
		// reset; the message is what distinguishes the two.
		if res.ExitCode != 0 && !strings.Contains(res.Stdout+res.Stderr, "has been reset") {
			return fmt.Errorf("%s: exit %d: %s", step.what, res.ExitCode, firstLine(res.Stderr))
		}
	}
	return nil
}

// OrphanedVolumes finds PersistentVolumes whose claim no longer exists, which
// is the one residual a datastore restore leaves: a PVC created DURING the
// upgrade window will not exist in the restored cluster while its volume
// still exists on disk.
//
// Naming them is the whole point. They are harmless until someone needs the
// disk space and cannot work out what is using it.
func OrphanedVolumes(ctx context.Context, r k3s.Runner) ([]string, error) {
	out, err := k3s.Kubectl(ctx, r,
		`get pv -o jsonpath='{range .items[?(@.status.phase=="Released")]}{.metadata.name}{" "}{.spec.claimRef.namespace}{"/"}{.spec.claimRef.name}{"\n"}{end}'`)
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, line := range strings.Split(strings.Trim(out, "'"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			orphans = append(orphans, line)
		}
	}
	return orphans, nil
}
