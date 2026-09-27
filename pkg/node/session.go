package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/interlock"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/window"
)

// MetadataWriter refreshes the cluster's recovery metadata in S3 (T4.6): the
// recovery set a recovery selects a kit and a backup by.
//
// A completed node operation refreshes it so that a recovery never depends on
// an install-time copy of what the cluster is (PLAN 7.1). The seam is an
// interface because the write needs the bucket credentials and the local kit,
// which live in the command layer — and because a laptop that has neither CAN
// still perform the node operation: it reports that the write is still owed
// rather than failing the work on the machines.
type MetadataWriter interface {
	// Refresh returns a sentence describing what it wrote, or an error naming
	// why it could not write.
	Refresh(ctx context.Context, cluster string) (string, error)
}

// Session is what `node add` and `node remove` have in common: the seams a
// test drives, the cluster facts the run resolves, and the locks both hold.
//
// The command layer builds one of these; the staging engine drives the verb
// built on top of it (PLAN 7.2: one operation, journals under
// stages.JournalPath, the operation record as the CLI-vs-CLI lock, kured's
// lock as the OS-reboot interlock).
type Session struct {
	// ID identifies this process across its stages.
	ID string
	// Cluster is the cluster's name as the operator typed it; ClusterID is
	// the control plane's own id for it, which the record's mirror needs.
	Cluster   string
	ClusterID string

	// Records is where the cluster's record is read and written back: the
	// control plane it registered with. Every node verb reads the machine set
	// from there and never from flags.
	Records Records
	// Bundle is the manifest of the bundle the cluster RECORDED, fetched by
	// the command layer: every version and every deadline comes from it.
	Bundle *manifest.Manifest
	// Catalog and Egress are what preflight checks the request and the new
	// host's reachability against.
	Catalog preflight.Catalog
	Egress  []preflight.EgressTarget

	Jnl  *stages.Journal
	Emit stages.Emitter
	Out  io.Writer

	// Window is the cluster's stored maintenance window; nil means none is
	// set, which the window rule REFUSES rather than reading as "any time".
	Window    *window.Window
	WindowErr error

	// Now, Sleep and Poll are the run's clock, so a test measures a timeout
	// instead of enduring it.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Poll  time.Duration
	// Dial opens one SSH connection to an inventory host.
	Dial func(ctx context.Context, host api.HostRecord) (Transport, error)
	// Store builds the operation record's store.
	Store func(runner k3s.Runner) *operation.Store
	// Metadata refreshes the recovery metadata; nil means this machine cannot
	// write it, which is reported and recorded rather than treated as a
	// failure.
	Metadata MetadataWriter

	// resolved during the run
	// Record is the cluster's record as it was read: the bundle version, the
	// profile set, the tier, and the revision every write must carry back.
	Record   api.ClusterBundle
	Hosts    []api.HostRecord
	Revision int
	// Server and ServerConn are the Ready server every cluster read and every
	// kubectl write goes through.
	Server     api.HostRecord
	ServerConn Transport
	// Host and Conn are the machine this verb acts on; Node is its Node
	// object, once the cluster reports one.
	Host api.HostRecord
	Conn Transport
	Node ClusterNode
	// Nodes is what the cluster reported when the run started, which is the
	// baseline a join is compared against.
	Nodes []ClusterNode

	store  *operation.Store
	handle *operation.Handle
	skip   map[string]bool
	// interlockHeld records whether THIS run placed kured's lock, so it
	// reports only what it did; interlockNode is the node name it was placed
	// under, which is what a release must match.
	interlockHeld bool
	interlockNode string
}

// The staging engine's Controller.
func (s *Session) RunID() string            { return s.ID }
func (s *Session) Journal() *stages.Journal { return s.Jnl }
func (s *Session) Emitter() stages.Emitter {
	if s.Emit == nil {
		return stages.NopEmitter{}
	}
	return s.Emit
}

func (s *Session) BundleVersion() string {
	if s.Bundle == nil {
		return ""
	}
	return s.Bundle.Bundle
}

// TotalDeadline bounds a node operation, from the bundle: a node operation
// that has not finished inside the install's own budget is not going to, and
// the budget is the manifest's rather than a constant here.
func (s *Session) TotalDeadline() (time.Duration, error) {
	if s.Bundle == nil {
		return 0, fmt.Errorf("no bundle manifest was loaded, so this operation has no deadline")
	}
	return s.Bundle.Limits.Timeouts.For("install-total")
}

// ResumeAdvice is what to do after a clean pause. A node operation's only
// pause is the maintenance window, and the wait for it happens inside the
// window stage holding nothing, so there is nothing to resume from.
func (s *Session) ResumeAdvice() string { return "" }

// Exits are the supported ways on from a failure, in the verb's own words.
// Every failure message the engine produces prints them.
var nodeExits = []string{
	"re-run the same command: the stages that completed are carried over from the journal, and work that already happened is not repeated",
	"continue a live operation record with --resume <operation-id>, as the record names it",
}

func (s *Session) Exits() []string { return nodeExits }

// Logf writes narrative that is not a stage transition.
func (s *Session) Logf(format string, args ...any) {
	if s.Out == nil {
		return
	}
	fmt.Fprintf(s.Out, format+"\n", args...)
}

// mergeState overlays v's JSON fields on the journal's state document and saves
// it.
//
// A JOURNAL HAS ONE STATE FIELD, and an operation that composes two verbs needs
// both of them in it: `node replace` records the order it committed to and the
// machine it is replacing, while the `node add` half it composes records the
// node that joined. A write that replaced the whole document would erase the
// other half's facts exactly when a resume needs them, so writes MERGE — every
// field of v is set, and a field this caller does not name is left as it was.
func (s *Session) mergeState(v any) error {
	if s.Jnl == nil {
		return nil
	}
	fields := map[string]json.RawMessage{}
	if len(s.Jnl.State) > 0 {
		// A document that cannot be decoded is replaced rather than merged: it
		// is not this operation's, and refusing here would refuse work that has
		// not happened.
		_ = json.Unmarshal(s.Jnl.State, &fields)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	mine := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &mine); err != nil {
		return err
	}
	for key, value := range mine {
		fields[key] = value
	}
	merged, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	s.Jnl.State = merged
	return s.Jnl.Save()
}

// now is the session's clock.
func (s *Session) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// sleep waits on the session's clock.
func (s *Session) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// poll is how long between two observations while something converges.
func (s *Session) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return 5 * time.Second
}

// Close releases every connection this run opened. Safe to call twice.
func (s *Session) Close() {
	for _, t := range []Transport{s.Conn, s.ServerConn} {
		if t != nil {
			t.Close()
		}
	}
	s.Conn, s.ServerConn = nil, nil
}

// resolveCluster reads what the cluster IS and finds the machine to act on.
//
// The inventory comes from the record and nowhere else: an operation that
// guessed its machines from flags would take down whatever the operator
// mistyped. A record with no inventory is refused in the one wording that
// already exists for it (pkg/upgrade), because "assume" is never an answer.
func (s *Session) resolveCluster(ctx context.Context) error {
	record, err := s.Records.Load(ctx)
	if err != nil {
		return err
	}
	s.Record = record
	if len(record.Hosts) == 0 {
		// ONE WORDING FOR "NO INVENTORY", and it is not "assume": the hosts
		// are never inferred from --server/--agent flags, because what an
		// operator typed is what they think the cluster is, and the record is
		// what it is.
		return fmt.Errorf("the cluster record carries no host inventory, so there is no host to act on: this cluster's record was written before the inventory existed, or its record stage has not run since. Re-run the install's record stage, or run `kubenest node add` to adopt the host explicitly — the hosts are never inferred from --server/--agent flags")
	}
	s.Hosts = record.Hosts
	s.Revision = record.Revision
	if s.Record.BundleVersion == "" {
		return fmt.Errorf("this cluster's record names no bundle version, so the versions a node must match are unknown. Re-run the install's record stage")
	}
	if s.Bundle == nil {
		return fmt.Errorf("bundle %s is what this cluster records, and its manifest could not be fetched", s.Record.BundleVersion)
	}
	if s.Bundle.Bundle != s.Record.BundleVersion {
		return fmt.Errorf("this cluster records bundle %s and the manifest loaded is %s: a node operation must match the cluster it joins, so nothing was changed",
			s.Record.BundleVersion, s.Bundle.Bundle)
	}
	// In 1.2 a node verb acts on the AGENTS of a single-server cluster. The ha
	// tier is a preview until its promotion (T6.1/1.3), and its node
	// operations are part of that work: refusing here, once, is better than a
	// tier-specific half-implementation nobody can test.
	switch s.Record.HATier {
	case "ha":
		return fmt.Errorf("this cluster's tier is %q, whose node operations arrive with its promotion (T6.1, bundle 1.3). In 1.2 a node verb acts on the agents of a single-server cluster", s.Record.HATier)
	case "single-server", "":
	default:
		return fmt.Errorf("this cluster's record names tier %q, which is not a tier this CLI knows: nothing was changed", s.Record.HATier)
	}
	return nil
}

// writeInventory writes the cluster's host inventory back, carrying the
// revision this run read — the compare-and-swap that stops two operators from
// silently overwriting each other's inventory.
//
// EVERY FIELD THE CONTROL PLANE REQUIRES ON THE WRITE is round-tripped from
// the record that was read, so a node operation cannot erase the bundle
// version, the profiles or the volume-group ownership it did not intend to
// touch: the record is written whole, and an omitted field would mean
// something nobody asked for.
func (s *Session) writeInventory(ctx context.Context, hosts []api.HostRecord) error {
	record := api.BundleRecord{
		BundleVersion:        s.Record.BundleVersion,
		Profiles:             s.Record.Profiles,
		HATier:               s.Record.HATier,
		VolumeGroupOwnership: s.Record.VolumeGroupOwnership,
		Hosts:                hosts,
		Revision:             s.Revision,
	}
	if err := s.Records.Save(ctx, record); err != nil {
		return fmt.Errorf("writing the cluster's host inventory at revision %d: %w. The control plane refuses a write based on a revision another operator has moved on from, so re-read the record and re-apply", s.Revision, err)
	}
	s.Hosts = hosts
	s.Revision++
	return nil
}

// findHost looks the machine up in the inventory by anything the operator
// could have typed: the host ID, the SSH address, the join address, or the
// Node UID.
func (s *Session) findHost(want string) (api.HostRecord, bool) {
	return FindHost(s.Hosts, want)
}

// windowRule applies the cluster's maintenance window (PLAN 7.4 item 3).
//
//	--now   bypasses THE WINDOW ONLY, and says so. Quorum, storage and the
//	        interlock still run: asking to act now is not asking to act on a
//	        cluster that cannot take it.
//	--wait  holds until the window opens while holding NOTHING — no operation
//	        record, no cluster change — which is why the record is taken by a
//	        later stage.
//	otherwise it refuses outside the window, naming the next opening in local
//	time and UTC.
func (s *Session) windowRule(ctx context.Context, now bool, wait bool) error {
	if now {
		s.Logf("--now: the maintenance window is bypassed for this run. Every other check still runs.")
		return nil
	}
	if wait {
		return s.waitForWindow(ctx)
	}
	switch {
	case s.WindowErr != nil:
		return fmt.Errorf("the cluster's maintenance window could not be read, so whether now is inside it is unknown: %w", s.WindowErr)
	case s.Window == nil:
		return fmt.Errorf("%s: %s", window.NoWindow, window.NoWindowFix)
	}
	if err := s.Window.Outside(s.now()); err != nil {
		return fmt.Errorf("%s\n%s", err.Error(), window.OutsideFix)
	}
	s.Logf("Inside %s (now %s).", s.Window, s.Window.Moment(s.now()))
	return nil
}

// Records is the cluster's record in the control plane, as a node verb uses
// it: read it, and write the inventory back carrying the revision that was
// read.
//
// It is declared HERE rather than taken from pkg/upgrade because pkg/upgrade's
// own tests import pkg/install, which imports this package — a node verb that
// depended on pkg/upgrade would close that loop for every `go vet ./...`.
type Records interface {
	// Load reads the cluster's record.
	Load(ctx context.Context) (api.ClusterBundle, error)
	// Save writes the inventory back. The revision it carries is the one Load
	// returned, and the control plane refuses a write based on a revision
	// another operator has moved on from.
	Save(ctx context.Context, record api.BundleRecord) error
}

// waitForWindow holds the run until the cluster's maintenance window is open,
// HOLDING NOTHING while it waits: no operation record, no cluster change, so an
// interrupted wait is simply started again and a second operator is free to
// work meanwhile.
//
// A missing or unreadable window is a refusal with the fix, never an opening to
// wait for and never "any time".
func (s *Session) waitForWindow(ctx context.Context) error {
	if s.WindowErr != nil {
		return fmt.Errorf("the cluster's maintenance window could not be read, so there is no opening to wait for: %w", s.WindowErr)
	}
	if s.Window == nil {
		return fmt.Errorf("%s: %s", window.NoWindow, window.NoWindowFix)
	}
	if s.Window.Contains(s.now()) {
		s.Logf("inside %s (now %s).", s.Window, s.Window.Moment(s.now()))
		return nil
	}
	at, ok := s.Window.NextOpen(s.now())
	if !ok {
		return fmt.Errorf("the maintenance window %s has no opening within the next week, so waiting would never finish: change it with `kubenest cluster set-window`", s.Window)
	}
	s.Logf("%s is closed (now %s); it next opens %s.\nWaiting. Nothing is held while this waits — no operation record is created and the cluster is not touched, so an interrupted wait can simply be re-run.",
		s.Window, s.Window.Moment(s.now()), s.Window.Moment(at))
	// The wait is bounded by the opening itself: if the clock reaches it and
	// the window still is not open — a stored window that cannot be
	// represented in the zone this host uses, a clock that jumped — the wait
	// says so instead of spinning for a week.
	deadline := at.Add(5 * time.Minute)
	for !s.Window.Contains(s.now()) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stopped waiting for the maintenance window: %w", err)
		}
		if s.now().After(deadline) {
			return fmt.Errorf("waited past %s and the maintenance window %s still has not opened, so waiting would never finish: check the window's timezone against this host's clock, or change it with `kubenest cluster set-window`",
				s.Window.Moment(deadline), s.Window)
		}
		// Bounded steps rather than one long sleep: a window that opens EARLIER
		// than the computed opening — a clock correction, a changed window, a
		// DST transition — is then noticed instead of slept through.
		step := time.Minute
		if remaining := at.Sub(s.now()); remaining < step {
			step = remaining
		}
		if step <= 0 {
			step = time.Second
		}
		if err := s.sleep(ctx, step); err != nil {
			return err
		}
	}
	s.Logf("inside %s (now %s). Every check runs from here, against the cluster as it is now, before anything is changed.", s.Window, s.Window.Moment(s.now()))
	return nil
}

// takeInterlock takes kured's own lock (T5.1), which is what stops an
// automatic reboot and this operation from both taking a node down.
//
// IT IS TAKEN BEFORE THE FIRST DISRUPTIVE STEP and released on terminal state.
// The TTL comes from the bundle rather than from here: it must outlive the
// longest operation that holds it, and only the manifest knows how long that
// is.
func (s *Session) takeInterlock(ctx context.Context, node string, ttl time.Duration) error {
	// ONE OPERATION HOLDS KURED'S LOCK ONCE. The lock is a single document
	// naming one node, and an operation that acts on two machines — a replace
	// takes one node down while it brings another up — would otherwise have its
	// second take refused by the lock its own first take wrote. For a verb with
	// one machine this changes nothing: the second take named the same node,
	// and kured's own rule already made that a re-take.
	if s.interlockHeld {
		s.Logf("  interlock: kured's lock is already held by this operation (for %s), so %s is covered by it too", s.interlockNode, node)
		return nil
	}
	held, holder, err := interlock.Acquire(ctx, s.ServerConn, node, ttl)
	if err != nil {
		return fmt.Errorf("kured's lock could not be taken: %w", err)
	}
	if !held {
		return fmt.Errorf("kured's lock is held by %s, so a reboot of %s may already be under way: nothing was changed. Wait for that reboot to finish, then re-run",
			holder, holder)
	}
	s.interlockHeld = true
	s.interlockNode = node
	s.Logf("  interlock: kured's lock taken for %s, so kured cannot reboot any node while this operation runs", ttl)
	return nil
}

// releaseInterlock removes kured's lock. It reports a failure and never hides
// the run's own error: a lock left behind is bounded by its TTL, and an
// operation report that swallowed a failure would hide it for exactly that
// long.
func (s *Session) releaseInterlock(ctx context.Context, node string, ttl time.Duration) {
	if !s.interlockHeld || s.ServerConn == nil {
		return
	}
	if err := interlock.Release(ctx, s.ServerConn, node); err != nil {
		s.Logf("warning: kured's lock could not be released: %v (it expires %s after it was taken)", err, ttl)
		return
	}
	s.interlockHeld = false
	s.Logf("  interlock: kured's lock released")
}

// lockTTL is how long this operation's hold on kured's lock lasts: the
// longest operation that holds one, which is a node's drain plus its reboot
// (PLAN 7.3). A missing manifest timeout is an error, never a default.
func (s *Session) lockTTL() (time.Duration, error) {
	return interlock.LockTTLFor(s.Bundle)
}

// openRecord creates the operation record — the lock and the resume path — or
// takes up the one an interrupted run left behind, by resume or by the
// operator's take-over assertion.
//
// The record exists BEFORE the first side effect: it is what a second laptop
// refuses on, and what a resume reads to find out what already happened. The
// kind is the verb's, so an add can never adopt a remove's record.
func (s *Session) openRecord(ctx context.Context, kind operation.Kind, request operation.Request, recovery operation.Recovery) error {
	s.store = s.Store(s.ServerConn)
	if recovery.ID() == "" {
		handle, err := s.store.Acquire(ctx, request)
		if err != nil {
			return err
		}
		s.handle = handle
		s.Logf("  record:    %s (resume with --resume %s if this is interrupted)", handle.OperationID(), handle.OperationID())
		return nil
	}
	// A resume — and a take-over — reconciles BEFORE anything is repeated: the
	// probes are read-only, and what a reconciliation cannot establish stops it
	// rather than guessing (PLAN 7.2). A take-over adds nothing here except the
	// operator's assertion: the reconcile is the same one.
	plan, err := operation.Resume(ctx, s.store, recovery.ID())
	if err != nil {
		return err
	}
	if err := plan.Verify(request); err != nil {
		return err
	}
	s.Logf("%s", recovery.Progress(plan))
	handle, err := recovery.Claim(ctx, s.store)
	if err != nil {
		return err
	}
	s.handle = handle
	s.skip = plan.Skip()
	return nil
}

// finishRecord closes the operation record the way the run ended. An
// interrupted run is marked stopped rather than finished, so a successor may
// take it over (a terminal record cannot be resumed by anyone).
func (s *Session) finishRecord(ctx context.Context, runErr error, interrupted bool) {
	if s.store == nil || s.handle == nil {
		return
	}
	if interrupted {
		if err := s.store.Stop(ctx, s.handle); err != nil {
			s.Logf("warning: the operation record %s could not be marked stopped: %v", s.handle.OperationID(), err)
		}
		s.Logf("  record:    %s marked stopped; continue it with --resume %s", s.handle.OperationID(), s.handle.OperationID())
		return
	}
	result := operation.ResultSucceeded
	if runErr != nil {
		result = operation.ResultFailed
	}
	if err := s.store.Complete(ctx, s.handle, result); err != nil {
		s.Logf("warning: the operation record %s could not be closed: %v", s.handle.OperationID(), err)
		return
	}
	s.Logf("  record:    %s marked %s", s.handle.OperationID(), result)
}

// guard wraps a runner so that what it submits is written down before it is
// submitted. Reads are not actions: the postcondition of a read is the read.
func (s *Session) guard(inner k3s.Runner, stage string, specs operation.Specs) k3s.Runner {
	if s.handle == nil {
		return inner
	}
	return &operation.Guarded{Inner: inner, Op: s.handle, Stage: stage, Specs: specs, Skip: s.skip}
}

// owe records a write the operation owes and has not discharged, so a run that
// dies with the write outstanding is identifiable rather than invisible.
func (s *Session) owe(ctx context.Context, kind, target, detail string) error {
	if s.handle == nil {
		return nil
	}
	return s.handle.PendingWrite(ctx, kind, target, detail)
}

// owed discharges a recorded write. A write the record does not owe is
// refused: a ledger that can be satisfied by inventing an entry records
// nothing.
func (s *Session) owed(ctx context.Context, kind, target string) error {
	if s.handle == nil {
		return nil
	}
	return s.handle.PendingWriteDone(ctx, kind, target)
}

// refreshMetadata discharges the recovery-metadata write a completed node
// operation owes (PLAN 7.1).
//
// IT IS NOT ALLOWED TO FAIL THE OPERATION: the node is in the cluster and
// recorded, and a bucket this laptop cannot reach does not change that. What
// it does is stay OWED in the operation record, with the reason, so the next
// operator sees work still outstanding rather than reading a completed
// operation as if nothing were left.
func (s *Session) refreshMetadata(ctx context.Context) {
	if s.Metadata == nil {
		s.Logf("  recovery:  no bucket credentials on this machine, so the cluster's recovery metadata was not refreshed; run `kubenest recovery-kit check` from wherever the install journal is")
		return
	}
	if err := s.owe(ctx, "recovery-metadata", s.Cluster, "the recovery set a recovery selects a kit and a backup by"); err != nil {
		s.Logf("warning: the operation record could not be told that the recovery metadata is owed: %v", err)
	}
	detail, err := s.Metadata.Refresh(ctx, s.Cluster)
	if err != nil {
		s.Logf("  recovery:  the cluster's recovery metadata was NOT refreshed: %v. The operation record keeps it owed", err)
		return
	}
	if err := s.owed(ctx, "recovery-metadata", s.Cluster); err != nil {
		s.Logf("warning: the operation record could not be told that the recovery metadata was written: %v", err)
	}
	s.Logf("  recovery:  %s", detail)
}

// liftHold removes the reboot hold this operation placed on an agent node.
//
// It runs LAST — after the node is Ready and its inventory entry is written —
// because a node that carries the hold is a node kured does not touch, and the
// point of the hold is that a node which joined but is not yet recorded cannot
// be rebooted. A SERVER is never touched here: every server carries the hold
// permanently in 1.2 (a server never reboots by itself), so lifting it would
// be this verb overruling the cluster's own configuration.
func (s *Session) liftHold(ctx context.Context, node ClusterNode, role Role) error {
	if role == RoleServer {
		return nil
	}
	if node.Labels[day2.NoAutoRebootLabel] != day2.NoAutoRebootValue {
		return nil
	}
	guarded := s.guard(s.ServerConn, "lift-hold", nodeSpecs)
	if _, err := k3s.Kubectl(ctx, guarded, "label node "+node.Name+" "+day2.NoAutoRebootLabel+"-"); err != nil {
		return fmt.Errorf("lifting the reboot hold from %s: %w", node.Name, err)
	}
	s.Logf("  hold:      %s is back in kured's pool (%s removed)", node.Name, day2.NoAutoRebootLabel)
	return nil
}

// waitReporter turns converge events into lines an operator can follow, so a
// readiness wait prints what it is waiting for instead of going quiet.
func (s *Session) waitReporter() converge.Reporter {
	return converge.NewTextReporter(s.Out)
}
