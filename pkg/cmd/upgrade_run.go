package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/upgrade"
	"kubenest.io/cli/pkg/window"
)

// UpgradeFlags is the flag surface of `kubenest platform upgrade`.
type UpgradeFlags struct {
	Cluster string
	To      string
	SSHUser string
	SSHKey  string
	// Acknowledge accepts individual deprecation findings by
	// namespace/Kind/name. There is deliberately no blanket --force.
	Acknowledge []string
	// Servers and Agents override the node list when there is no local
	// install journal — an upgrade run from a different machine than the
	// install.
	Servers []string
	Agents  []string
	// Wait holds the run until the cluster's maintenance window opens, holding
	// nothing while it waits, then takes the operation lock and lets every gate
	// re-run against the cluster as it is at that moment.
	Wait bool
	// Now acts immediately, consulting no window at all. It bypasses the
	// maintenance window and nothing else: the other six gates still run,
	// because asking to act now is not asking to act on a degraded cluster.
	Now bool
}

// buildUpgradeSession assembles everything an upgrade needs: the cluster's
// own record, both bundle manifests, the node connections, the maintenance
// window, and the journal.
//
// THE RECORD COMES FROM THE CONTROL PLANE. Every cluster registers with one
// (decision D17, 2026-09-24) — the management cluster included — so a cluster
// that has no record there has not been installed by this platform and is
// refused rather than guessed at.
func buildUpgradeSession(ctx context.Context, out io.Writer, f UpgradeFlags) (*upgrade.Session, error) {
	client, err := controlPlaneClient()
	if err != nil {
		return nil, err
	}

	clusterID, err := resolveCluster(ctx, client, f.Cluster)
	if err != nil {
		return nil, err
	}
	records := upgrade.ControlPlaneRecords{Client: client, ClusterID: clusterID}
	// The cluster's own record is the authority on what it IS. An upgrade
	// that took its starting point from an argument could move a cluster it
	// had misidentified.
	recorded, err := records.Load(ctx)
	if err != nil {
		return nil, err
	}

	from, err := fetchManifest(ctx, client, recorded.BundleVersion)
	if err != nil {
		return nil, err
	}
	to, err := fetchManifest(ctx, client, f.To)
	if err != nil {
		return nil, err
	}

	servers, agents, err := upgradeNodes(f)
	if err != nil {
		return nil, err
	}

	opts := upgrade.Options{
		Cluster: f.Cluster, To: f.To,
		Servers: servers, Agents: agents,
		SSHUser: f.SSHUser, SSHKey: f.SSHKey,
		Acknowledge: f.Acknowledge,
		// --now bypasses the window and nothing else: the other six gates still
		// run, and they still run against the cluster as it is.
		BypassWindow: f.Now,
	}

	journalPath, err := upgrade.JournalPath(f.Cluster)
	if err != nil {
		return nil, err
	}
	journal, err := stages.OpenJournal(journalPath, opts.Identity(recorded.BundleVersion))
	if err != nil {
		return nil, err
	}
	journal.ClusterID = clusterID

	session := &upgrade.Session{
		ID:       stages.NewRunID(),
		Opts:     opts,
		From:     from,
		To:       to,
		Jnl:      journal,
		Reporter: converge.NewTextReporter(out),
		Out:      out,
		API:      client,
		Cluster:  recorded,
		Records:  records,
	}
	// THE WINDOW COMES FROM THE CONTROL PLANE, like the record: it is the one
	// definition shared by this upgrade and by OS reboots, and a window taken
	// from a flag could be a window nobody stored. A window that cannot be read
	// or that this CLI cannot represent is carried on the session for the
	// window gate to refuse by name — never dropped, because a window silently
	// dropped reads as "any time is inside it" (kn-nqj).
	session.Window, session.WindowErr = records.Window(ctx)
	session.Emit = stages.Emitters{
		stages.TextEmitter{W: out},
		stages.NewControlPlaneEmitter(client, func() string { return journal.ClusterID }),
	}
	return session, nil
}

// upgradeNodes resolves which hosts this upgrade acts on.
func upgradeNodes(f UpgradeFlags) ([]string, []string, error) {
	servers, agents := f.Servers, f.Agents
	if len(servers) == 0 {
		// The install journal on this machine knows the nodes. Without it,
		// the operator names them.
		if path, pathErr := install.JournalPath(f.Cluster); pathErr == nil {
			if journal, readErr := install.ReadJournal(path); readErr == nil {
				servers, agents = install.NodesFromJournal(journal)
			}
		}
	}
	if len(servers) == 0 {
		return nil, nil, fmt.Errorf("no nodes for cluster %q: this machine has no install journal for it, so pass --server (and --agent) for the hosts to upgrade", f.Cluster)
	}
	return servers, agents, nil
}

// resolveCluster turns a cluster name into its control-plane id.
func resolveCluster(ctx context.Context, client *api.Client, name string) (string, error) {
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, org := range orgs {
		clusters, err := client.ListOrgClusters(ctx, org.ID)
		if err != nil {
			return "", err
		}
		for _, c := range clusters {
			if c.Name == name {
				matches = append(matches, c.ID)
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no cluster named %q is registered with this control plane", name)
	default:
		return "", fmt.Errorf("more than one cluster is named %q, which should be impossible: names are unique across KubeNest", name)
	}
}

func fetchManifest(ctx context.Context, client *api.Client, version string) (*manifest.Manifest, error) {
	raw, err := client.BundleManifest(ctx, version)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bundle %s from the control plane is not a valid manifest: %w", version, err)
	}
	return m, nil
}

// runUpgrade is `kubenest platform upgrade`.
//
// The order of the three things it does before a stage runs is the point:
//
//	--wait        holds until the maintenance window opens, HOLDING NOTHING
//	              (no node connection, no operation record, no cluster change)
//	the lock      taken once the window is open, so a wait cannot hold the
//	              cluster for hours (PLAN 7.2)
//	the gates     every pre-flight gate runs inside stages.Execute, against the
//	              cluster as it is at that moment rather than as it was when the
//	              command was typed
func runUpgrade(ctx context.Context, out io.Writer, f UpgradeFlags) error {
	session, err := buildUpgradeSession(ctx, out, f)
	if err != nil {
		return err
	}
	defer session.Close()

	if f.Wait {
		if err := session.WaitForWindow(ctx, out); err != nil {
			return err
		}
	}
	if err := session.Connect(ctx); err != nil {
		return err
	}

	// The record-as-lock, taken AFTER the window opened. `defer` is not enough
	// here: the record has three endings, and a pause is not one of them.
	var (
		lockStore  *operation.Store
		lockHandle *operation.Handle
	)
	if f.Wait {
		lockStore, lockHandle, err = session.LockOperation(ctx)
		if err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "Upgrading %s from bundle %s to %s.\n", f.Cluster, session.From.Bundle, session.To.Bundle)
	fmt.Fprintf(out, "Components first, Kubernetes last: everything before the kubernetes stage reverts\nin seconds. That stage is the point of no return.\n\n")

	result, err := stages.Execute(ctx, session, upgrade.Plan(session))
	endOperation(ctx, out, lockStore, lockHandle, err)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nUpgraded to %s in %s.\n", session.To.Bundle, result.Elapsed.Round(time.Second))
	if len(result.Skipped) > 0 {
		fmt.Fprintf(out, "Skipped %d stage(s) completed by an earlier run: %s\n",
			len(result.Skipped), strings.Join(result.Skipped, ", "))
	}
	return nil
}

// endOperation closes the operation record the way the run ended.
//
// A PAUSE IS NOT AN ENDING. The cluster is mid-upgrade and will be resumed, so
// the record is left where it is with its executor stopped; marking it terminal
// would let the next operation take the record over and find a half-upgraded
// cluster that the record says is finished. The record staying live is also
// what refuses a second laptop while this upgrade is unfinished.
//
// A failure to write the record is reported and never replaces the run's own
// error: the record is the lock and the resume path, and neither is worth
// hiding the reason the upgrade failed.
func endOperation(ctx context.Context, out io.Writer, store *operation.Store, handle *operation.Handle, runErr error) {
	if store == nil || handle == nil {
		return
	}
	var err error
	switch {
	case runErr == nil:
		err = store.Complete(ctx, handle, operation.ResultSucceeded)
	case errors.Is(runErr, stages.ErrPaused):
		err = store.Stop(ctx, handle)
	default:
		err = store.Complete(ctx, handle, operation.ResultFailed)
	}
	if err != nil {
		fmt.Fprintf(out, "warning: the operation record %s could not be closed: %v\n", handle.OperationID(), err)
	}
}

// runRollback is `kubenest platform rollback`.
//
// It reports which mechanism it will use BEFORE doing anything, and asks for
// confirmation when that mechanism is a datastore restore — which is a
// service interruption, not a revert.
func runRollback(ctx context.Context, out io.Writer, in io.Reader, f UpgradeFlags, confirmed bool) error {
	session, err := buildUpgradeSession(ctx, out, f)
	if err != nil {
		return err
	}
	defer session.Close()
	if err := session.Connect(ctx); err != nil {
		return err
	}

	plan := session.RollbackPlan()
	fmt.Fprint(out, plan)

	switch plan.Mechanism {
	case upgrade.MechanismNothing:
		return nil
	case upgrade.MechanismRestore:
		if !confirmed {
			return fmt.Errorf("this rollback restores the datastore snapshot, which interrupts service: pass --confirm to proceed")
		}
	}
	return session.Rollback(ctx, plan)
}

// runWindow is `kubenest cluster set-window`.
//
// It READS the window before it writes one, because the write is a
// compare-and-swap on the revision the caller read: two operators setting one
// cluster's window from two laptops is the ordinary case, and the loser is
// refused rather than silently overwriting the winner.
//
// It then prints the three states SEPARATELY. A window that is stored but not
// yet active is not in force, and the command says so in as many words: the
// kn-nqj defect was an operator who had set a window and was told every
// upgrade was inside it.
func runWindow(ctx context.Context, out io.Writer, client *api.Client, cluster string, spec window.Spec) error {
	// Refuse first what this CLI cannot represent — an offset timezone, a
	// zero-length window, a day that is not a day. The control plane validates
	// the same rules; parsing here is what stops the CLI from accepting a
	// window, sending it, and storing something other than what was asked for.
	parsed, err := window.Parse(spec)
	if err != nil {
		return err
	}
	clusterID, err := resolveCluster(ctx, client, cluster)
	if err != nil {
		return err
	}
	current, err := client.MaintenanceWindow(ctx, clusterID)
	if err != nil {
		return err
	}
	canonical := parsed.Spec()
	record, err := client.PutMaintenanceWindow(ctx, clusterID, api.MaintenanceWindow{
		Days:     canonical.Days,
		Start:    canonical.Start,
		End:      canonical.End,
		Timezone: canonical.Timezone,
	}, current.CurrentRevision())
	if err != nil {
		return err
	}
	printWindowStates(out, cluster, record)
	return nil
}

// printWindowStates prints the stored window, its revision, the three states as
// three distinct lines, and the backup-overlap warning.
func printWindowStates(out io.Writer, cluster string, record api.MaintenanceWindowRecord) {
	revision := "none"
	if record.Revision != nil {
		revision = strconv.Itoa(*record.Revision)
	}
	applied := "none"
	if record.AppliedRevision != nil {
		applied = strconv.Itoa(*record.AppliedRevision)
	}
	window := "none"
	if record.Window != nil {
		window = fmt.Sprintf("%s %s-%s %s",
			strings.Join(record.Window.Days, ","), record.Window.Start, record.Window.End, record.Window.Timezone)
	}
	fmt.Fprintf(out, "Maintenance window for %s: %s, revision %s.\n", cluster, window, revision)

	switch {
	case record.Window == nil:
		fmt.Fprintf(out, "  stored:   no window is stored for this cluster\n")
	case record.Revision != nil:
		fmt.Fprintf(out, "  stored:   %s written to the control plane at revision %s\n", window, revision)
	default:
		fmt.Fprintf(out, "  stored:   %s written to the control plane\n", window)
	}

	// `none` is its own state (kn-nqj.1), printed rather than inferred: the
	// control plane used to report a cluster with no window as `stored`, which
	// is the same word it uses for a window that IS written, so the only way to
	// tell them apart was the null field.
	if record.State == api.WindowStateNone {
		fmt.Fprintf(out, "  none:     no window has ever been written for this cluster; set one with `kubenest cluster set-window`\n")
	}

	if record.State == api.WindowStateApplying {
		fmt.Fprintf(out, "  applying: revision %s has been handed to the cluster and not acknowledged yet\n", revision)
	} else {
		fmt.Fprintf(out, "  applying: revision %s has not been handed to the cluster, or was acknowledged already\n", revision)
	}

	switch record.State {
	case api.WindowStateActive:
		fmt.Fprintf(out, "  active:   revision %s — the operator acknowledged this revision, so this window is in force\n", revision)
	case api.WindowStateApplying:
		fmt.Fprintf(out, "  active:   revision %s — revision %s has not been acknowledged, so the cluster may be disconnected: it is still running its PREVIOUS window (or none) and this window is NOT in force\n", applied, revision)
	default:
		if record.Window == nil {
			fmt.Fprintf(out, "  active:   none — there is no window to be in force\n")
			break
		}
		fmt.Fprintf(out, "  active:   revision %s — the cluster has not acknowledged revision %s, so its previous window (or none) is still in force and this window is NOT\n", applied, revision)
	}

	if record.RejectReason != nil && *record.RejectReason != "" {
		fmt.Fprintf(out, "The operator rejected the last window it was sent: %s\n", *record.RejectReason)
	}
	switch {
	case record.Window == nil:
		// Nothing to overlap a backup.
	case record.Warning == nil:
		fmt.Fprintf(out, "The window does not overlap the nightly backup.\n")
	case *record.Warning == api.WindowBackupUnknown:
		fmt.Fprintf(out, "Warning: unknown — the operator has never measured how long a backup takes, so an overlap with the nightly backup cannot be ruled out. An unmeasured backup is not a passing check.\n")
	default:
		fmt.Fprintf(out, "Warning: %s\n", *record.Warning)
	}
}
