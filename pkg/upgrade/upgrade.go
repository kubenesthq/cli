// Package upgrade moves a cluster from one platform bundle to the next.
//
// THIS IS THE PRODUCT. Upgrades are the pain named independently by Talos
// users, RKE2 users, Rancher churners, and by Omni choosing it as their
// headline feature. The node upgrade mechanism itself is not our increment —
// that is Rancher's system-upgrade-controller, adopted not rebuilt, and
// already placed at its pinned version by the installer's day-2 stage. Our
// increment is three things: orchestrating a whole BUNDLE transition rather
// than one component, the pre-flight checks that make it safe, and rollback
// when it is not.
//
// COMPONENTS FIRST, KUBERNETES LAST. This is the most important design
// decision here and it is an ordering, not a mechanism: platform components
// are Helm releases and revert in seconds, while Kubernetes does not roll
// back at all — reverting it means restoring the stage-2 datastore snapshot,
// with a service interruption. Putting the irreversible step last means the
// great majority of failures happen while retreating is still cheap. Five of
// the seven ways an upgrade can fail cost seconds, and that is a consequence
// of sequencing rather than of any recovery machinery.
//
// The staging engine, the journal and resume are pkg/stages, shared with the
// installer. There is one of each in this CLI on purpose.
package upgrade

import (
	"context"
	"fmt"
	"io"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/window"
)

// Kind names this operation in the journal, so an upgrade journal and an
// install journal can never be opened as each other.
const Kind = "upgrade"

// The eight stage names, exactly as upgrades.mdx names them. They are the
// journal's vocabulary and the wire's payload.stage — the same contract the
// installer emits on (kn-w051), with the four names an upgrade adds.
//
// `backup` is not install's `platform-backup`: that one INSTALLS Velero, this
// one TAKES a backup. Same for `agent` against `kubenest-agent`, and
// `kubernetes` against `k3s-server`/`k3s-agents` — an upgrade moves every
// node through one stage rather than splitting servers from agents.
const (
	StagePreflight  = "preflight"
	StageBackup     = "backup"
	StageComponents = "platform-components"
	StageProfiles   = "profiles"
	StageAgent      = "agent"
	StageKubernetes = "kubernetes"
	StageVerify     = "verify"
	StageRecord     = "record"
)

// StageNames is the order. Everything before StageKubernetes is cheaply
// reversible; StageKubernetes is the point of no return.
var StageNames = []string{
	StagePreflight,
	StageBackup,
	StageComponents,
	StageProfiles,
	StageAgent,
	StageKubernetes,
	StageVerify,
	StageRecord,
}

// PointOfNoReturn is the stage after which a failure costs a datastore
// restore rather than a Helm revert. Named so the failure message can say so
// and so the rollback path can decide which mechanism it needs.
const PointOfNoReturn = StageKubernetes

// exits are the supported ways on from a failed upgrade. They differ from the
// installer's: there is a cluster here already, and destroying it is not one
// of the options.
var exits = []string{
	"resume     fix what the error names, then run the identical command again\n             (completed stages are skipped)",
	"roll back  kubenest platform rollback --cluster <name>",
}

// Options is the upgrade request.
type Options struct {
	// Cluster is the cluster name, as recorded at install.
	Cluster string
	// To is the bundle version being moved to.
	To string
	// Servers and Agents are the node addresses, from the install record.
	Servers []string
	Agents  []string
	SSHUser string
	SSHKey  string
	// Acknowledge accepts individual deprecation findings by
	// namespace/Kind/name. There is deliberately no blanket override.
	Acknowledge []string
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Identity is the part of the request a resume must match exactly. The
// version transition is part of it: resuming a 1.0→1.1 upgrade with a journal
// from 1.0→1.2 would be a different operation wearing the same name.
func (o Options) Identity(from string) stages.Identity {
	return stages.Identity{
		Kind:    Kind,
		Cluster: o.Cluster,
		Fields: map[string]string{
			"from bundle": from,
			"to bundle":   o.To,
			"servers":     stages.List(o.Servers),
			"agents":      stages.List(o.Agents),
		},
	}
}

// Node is one cluster node with an open connection.
type Node struct {
	Address string
	Server  bool
	Runner  k3s.Runner
}

// record is what an upgrade must remember across a resume beyond its entries:
// where it came from, and the snapshot it can go back to.
type record struct {
	FromBundle string `json:"from_bundle"`
	ToBundle   string `json:"to_bundle"`
	// Snapshot is the datastore snapshot taken at stage 2. It is what a
	// rollback after the point of no return restores, so it is written to
	// the journal before the first component moves.
	Snapshot string `json:"snapshot,omitempty"`
	// SnapshotAt is when it was taken, for the rollback report.
	SnapshotAt time.Time `json:"snapshot_at,omitempty"`
	// BackupName is the workload backup taken alongside it.
	BackupName string `json:"backup_name,omitempty"`
}

// Session is one upgrade run.
type Session struct {
	ID   string
	Opts Options
	// From and To are the bundle manifests. Both are needed: From says what
	// is installed now and therefore what a component rollback reverts to,
	// To says what everything is moving to.
	From *manifest.Manifest
	To   *manifest.Manifest

	Jnl      *stages.Journal
	Emit     stages.Emitter
	Reporter converge.Reporter
	Out      io.Writer
	API      *api.Client

	// Window is the cluster's maintenance window, nil if none is set.
	Window *window.Window
	// Cluster is what the control plane records this cluster as: the bundle
	// it is on, its profile set, its tier. Read once, at the start.
	Cluster Recorded
	// Drills reports the last verified restore drill. Nil means no evidence
	// is available, which the gate refuses rather than passes.
	Drills DrillSource

	Nodes  []Node
	Record record

	closers []io.Closer
}

// The engine's Controller.
func (s *Session) RunID() string            { return s.ID }
func (s *Session) Journal() *stages.Journal { return s.Jnl }
func (s *Session) Emitter() stages.Emitter  { return s.Emit }
func (s *Session) BundleVersion() string    { return s.Opts.To }
func (s *Session) Exits() []string          { return exits }

// TotalDeadline bounds the whole upgrade. An upgrade's cost is fixed overhead
// plus a per-node cost, so the deadline scales with the cluster rather than
// being one number for every shape of it.
func (s *Session) TotalDeadline() (time.Duration, error) {
	perNode, err := s.To.Limits.Timeouts.For("upgrade-per-node")
	if err != nil {
		return 0, err
	}
	overhead, err := s.To.Limits.Timeouts.For("install-total")
	if err != nil {
		return 0, err
	}
	return overhead + perNode*time.Duration(len(s.Nodes)), nil
}

// Logf writes narrative to the session's output.
func (s *Session) Logf(format string, args ...any) {
	if s.Out == nil {
		return
	}
	fmt.Fprintf(s.Out, format+"\n", args...)
}

// Close releases every connection the session opened.
func (s *Session) Close() {
	for _, c := range s.closers {
		_ = c.Close()
	}
	s.closers = nil
}

func (s *Session) saveRecord() error { return s.Jnl.SetState(&s.Record) }

// Server returns the primary control-plane node.
func (s *Session) Server() (k3s.Runner, error) {
	for _, n := range s.Nodes {
		if n.Server {
			return n.Runner, nil
		}
	}
	return nil, fmt.Errorf("no server node connection")
}

// now is the session's clock.
func (s *Session) now() time.Time {
	if s.Opts.Now != nil {
		return s.Opts.Now()
	}
	return time.Now()
}

// JournalPath is where this cluster's UPGRADE journal lives.
func JournalPath(cluster string) (string, error) {
	return stages.JournalPath(Kind, cluster)
}

// windowExempt names the stages the window never pauses before, and why each
// one is exempt:
//
//	preflight  it is where the window is checked as a gate in the first
//	           place, so pausing before it would be circular.
//	backup     a rollback depends on the snapshot it takes; that is not
//	           skipped for a clock.
//	verify     pausing between the kubernetes stage and the verification of
//	           it would leave a cluster upgraded and unverified until the
//	           next window.
//	record     the same, for the recording that follows verification.
//
// Everything expensive — components, profiles, the agent, and Kubernetes
// itself — sits between backup and verify, and is not exempt.
func windowExempt(stage string) bool {
	switch stage {
	case StagePreflight, StageBackup, StageVerify, StageRecord:
		return true
	}
	return false
}

// windowStillOpen implements the maintenance-window rule for a stage that has
// not started yet.
func (s *Session) windowStillOpen(stage string) error {
	if s.Window == nil || windowExempt(stage) {
		return nil
	}
	if s.Window.Contains(s.now()) {
		return nil
	}
	next := "the next window"
	if at, ok := s.Window.NextOpen(s.now()); ok {
		next = at.Format("Mon 2 Jan 15:04 MST")
	}
	return stages.Paused(
		"the maintenance window %s has closed, so no new stage is starting. The stage that was running finished; the cluster is mid-upgrade and reports itself as such. The upgrade resumes at %s",
		s.Window, next)
}

// ResumeAdvice is what to do after a clean pause.
func (s *Session) ResumeAdvice() string {
	return "Re-run the identical command inside the window and it will continue from here;\ncompleted stages are skipped."
}

// Connect opens a connection to every node. It is separate from the session
// so a rollback can use the same session shape without running an upgrade.
func (s *Session) Connect(ctx context.Context) error {
	opts := sshx.Options{User: s.Opts.SSHUser, KeyPath: s.Opts.SSHKey, DialTimeout: 15 * time.Second}
	dial := func(address string, server bool) error {
		endpoint, err := sshx.Resolve(address, opts)
		if err != nil {
			return fmt.Errorf("%s: %w", address, err)
		}
		client, err := sshx.Dial(ctx, endpoint, opts)
		if err != nil {
			return fmt.Errorf("%s: %w", address, err)
		}
		s.closers = append(s.closers, client)
		s.Nodes = append(s.Nodes, Node{Address: address, Server: server, Runner: client})
		return nil
	}
	for _, address := range s.Opts.Servers {
		if err := dial(address, true); err != nil {
			return err
		}
	}
	for _, address := range s.Opts.Agents {
		if err := dial(address, false); err != nil {
			return err
		}
	}
	// The drill evidence comes from the cluster itself — the same object the
	// agent reads to build its heartbeat, so the CLI and the control plane
	// look at one source of truth rather than two representations of it.
	if server, err := s.Server(); err == nil && s.Drills == nil {
		s.Drills = InClusterDrills{Runner: server}
	}
	return s.loadRecord()
}

// loadRecord reads back what an earlier run of this upgrade remembered.
func (s *Session) loadRecord() error {
	if err := s.Jnl.DecodeState(&s.Record); err != nil {
		return err
	}
	if s.Record.FromBundle == "" {
		s.Record.FromBundle = s.From.Bundle
		s.Record.ToBundle = s.Opts.To
	}
	return nil
}

// RollbackPlan describes what a rollback would do, without doing it.
func (s *Session) RollbackPlan() RollbackPlan {
	return PlanRollback(s.Jnl, s.From, s.To, s.Record)
}

// Rollback executes a plan. It never runs automatically: a failed stage
// leaves the cluster where it is and prints both exits, because automatic
// teardown destroys the evidence needed to diagnose the failure and can
// itself fail.
func (s *Session) Rollback(ctx context.Context, plan RollbackPlan) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	switch plan.Mechanism {
	case MechanismNothing:
		return nil
	case MechanismComponents:
		if err := RevertComponents(ctx, server, s.From, s.Reporter, s.Logf); err != nil {
			return err
		}
	case MechanismRestore:
		s.Logf("  restoring datastore snapshot %s", plan.Snapshot)
		if err := RestoreSnapshot(ctx, server, plan.Snapshot); err != nil {
			return err
		}
		// The one residual worth naming: a PVC created during the upgrade
		// window is gone from the restored cluster while its volume still
		// exists on disk. Harmless until someone needs the space and cannot
		// work out what is using it.
		if orphans, err := OrphanedVolumes(ctx, server); err == nil && len(orphans) > 0 {
			s.Logf("\n  %d volume(s) are now orphaned — their claims were created during the upgrade", len(orphans))
			s.Logf("  window and do not exist in the restored cluster. Their DATA IS INTACT:")
			for _, o := range orphans {
				s.Logf("    %s", o)
			}
			s.Logf("  Reclaim or re-bind them deliberately; nothing here deletes them.")
		}
	}

	// The cluster is back on the bundle it came from, and the record must
	// say so: a record that claims the new version after a rollback is worse
	// than no record, because every day-2 operation trusts it.
	if s.API != nil && s.Jnl.ClusterID != "" {
		if err := s.API.PutBundleRecord(ctx, s.Jnl.ClusterID, api.BundleRecord{
			BundleVersion:        s.Record.FromBundle,
			Profiles:             s.Cluster.Profiles,
			HATier:               s.Cluster.HATier,
			VolumeGroupOwnership: s.Cluster.VolumeGroupOwnership,
			InstallJournal:       terminalEntries(s.Jnl),
		}); err != nil {
			return fmt.Errorf("the cluster was rolled back but its record could not be updated: %w", err)
		}
	}
	// The upgrade journal has served its purpose; leaving it would make the
	// next attempt resume into a cluster that no longer matches it.
	return s.Jnl.Remove()
}
