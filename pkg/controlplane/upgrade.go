package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/window"
)

// THE CONTROL-PLANE UPGRADE (PLAN 7.8, T7.0 item 7).
//
// This is the ordered procedure section 7.8 describes, run through the SAME
// staging engine, journal and operation record as every other day-2 verb — not
// a parallel one, because a second engine would mean a second journal format
// and a second definition of a failed stage, and an operator would have to
// learn which one they were looking at.
//
//	gates       every gate runs before the first change
//	fence       the public API route goes to the 503 page
//	checkpoint  a checkpoint is published and WAITED FOR until it is eligible
//	migration   the schema Job runs at this revision, backend stopped
//	chart       the new chart rolls, still fenced
//	validate    the new backend is proven through the node's port-forward
//	unfence     the route is restored and the fence dismantled
//
// TWO PROPERTIES THE ORDER BUYS, and both are acceptance criteria:
//
//	The old code never serves the migrated schema, and no customer request
//	reaches the backend from the moment the fence goes up: the fence is
//	raised BEFORE the migration, and the backend is stopped for it.
//
//	An interrupted run is resumable BY A SECOND LAPTOP. The record of what
//	has happened is the operation record in the cluster (the lock, T2.3) and
//	this journal; every remote action is recorded before it is submitted, so a
//	successor can reconcile actual state instead of repeating a step.
//
// AND BEFORE ANY STAGE RUNS, a caller that finds the fence up over a backend
// stopped at zero replicas recovers one first (RecoverStoppedBackend): every
// read the caller makes before it has a session needs a running control plane,
// and the run that stopped it is not necessarily still alive to put it back.
//
// ROLLING BACK is automatic only while the fence has held continuously: the
// checkpoint taken at stage 3 is the state to return to, and after the fence
// lifts, returning to it is an explicit recovery with its data-loss interval
// stated (T4.7's recovery path), never a silent step of this run.
const (
	// UpgradeKind names this operation in the journal, so a control-plane
	// upgrade journal can never be opened as a cluster upgrade's.
	UpgradeKind = "control-plane-upgrade"

	// The stage names. They are the journal's vocabulary and the wire's
	// payload.stage, so the caller's constants and the contract's enum agree.
	StageGates      = "control-plane-gates"
	StageFence      = "control-plane-fence"
	StageCheckpoint = "control-plane-checkpoint"
	StageMigration  = "control-plane-migration"
	StageChart      = "control-plane-chart"
	StageValidate   = "control-plane-validation"
	StageUnfence    = "control-plane-unfence"
)

// UpgradeStageNames is the order. The fence goes up before anything changes and
// comes down after everything is proven.
var UpgradeStageNames = []string{
	StageGates,
	StageFence,
	StageCheckpoint,
	StageMigration,
	StageChart,
	StageValidate,
	StageUnfence,
}

// Gate is one pre-flight verdict.
//
// The stage runs EVERY gate before its first change, and some of them are the
// caller's to assemble: compatibility and the window need the control-plane
// client and the cluster's stored window, and the recovery kit's presence is a
// fact this machine holds. What is checked here rather than there is the part
// that needs the cluster: the field-ownership assertion and the PostgreSQL pin.
type Gate struct {
	Name   string
	Passed bool
	Detail string
	// Fix is what to do about it. A failed gate without one has told the
	// operator they have a problem and nothing more.
	Fix string
}

func (g Gate) String() string {
	out := g.Name + ": " + g.Detail
	if !g.Passed && g.Fix != "" {
		out += "\n      fix: " + g.Fix
	}
	return out
}

// UpgradeOptions is the request.
type UpgradeOptions struct {
	// Cluster is the management cluster's name, and From/To are the bundle
	// versions this moves between. All three are part of the journal identity:
	// resuming a 1.2->1.3 upgrade with a 1.2->1.4 journal is a different
	// operation wearing the same cluster's name.
	Cluster string
	From    string
	To      string

	// Values is the values document for the NEW chart: the same document a
	// --control-plane install renders, with the bundle's pins in it.
	Values string

	// Bundle is the TARGET bundle manifest, for the timeouts.
	Bundle *manifest.Manifest

	// Server is the SSH connection to the management cluster's server node.
	// Every remote action in this procedure goes through it.
	Server k3s.Runner

	// Open builds the client the validation reads through the node's
	// port-forward with. The caller owns the credential and the CA.
	Open ClientOpener

	// Before is what the control plane reported about itself before the
	// upgrade started. The validation refuses a contract era below it (the
	// counter never goes down) and a build stamp equal to it whenever the
	// chart's backend image has changed.
	Before api.ControlPlaneVersion

	// StaleBuild and WantBuild are NOT fields here, and that is deliberate: the
	// gates stage derives them from the TWO IMAGES (ValidationExpectations),
	// because the caller cannot know before the run which build is legitimate
	// to report. A caller-set StaleBuild was the defect — hardware ran an
	// upgrade that changed nothing and validation passed, because nothing had
	// told it the old build was stale.

	// Gates are the caller's pre-flight verdicts (compatibility, window, kits
	// present, no other operation). They are run by the gates stage, in order,
	// with the cluster-side gates appended.
	Gates []Gate

	// Window and WindowErr are the management cluster's stored maintenance
	// window. A missing or unreadable window is a REFUSAL, and the caller
	// reports it as a Gate like every other gate.
	Window       *window.Window
	WindowErr    error
	BypassWindow bool

	// Reporter and Out are where the run talks.
	Reporter converge.Reporter
	Out      io.Writer

	// Now overrides the clock, for tests.
	Now func() time.Time

	// PollInterval overrides how often a converge wait re-reads the cluster.
	// Zero uses converge's own cadence; a test sets a millisecond so a wait is
	// EXERCISED rather than endured.
	PollInterval time.Duration

	// WaitDeadline overrides the bundle's `component-ready` timeout for this
	// procedure's waits. Zero — the only value production ever sets — uses the
	// manifest's number, which is where every deadline comes from.
	//
	// IT EXISTS FOR THE ARM THAT ASSERTS A WAIT FAILS. A test that proves "a
	// fence whose route never switches is refused" has to let a wait run out,
	// and doing that against the manifest's real minute makes the whole package
	// three minutes slower for everyone who runs `go test ./...`.
	WaitDeadline time.Duration
}

// UpgradeRecord is what an upgrade must remember across a resume beyond its
// journal entries: the checkpoint it can go back to, and the fence's state.
type UpgradeRecord struct {
	FromBundle string `json:"from_bundle"`
	ToBundle   string `json:"to_bundle"`
	// CheckpointMarker identifies the checkpoint this run published, so a
	// resume can tell "the eligible checkpoint is mine" from "an older one
	// happened to be eligible".
	CheckpointMarker string `json:"checkpoint_marker,omitempty"`
	// FenceRaised says the public route was switched. It is written BEFORE the
	// switch, so a resume that reads it knows to reconcile the route rather
	// than assume either state.
	FenceRaised bool `json:"fence_raised,omitempty"`
	// BackendReplicas is how many replicas the backend ran at before the fence
	// stopped it. Restoring it from the record rather than from the chart's
	// default keeps a scaled control plane scaled.
	BackendReplicas int32 `json:"backend_replicas,omitempty"`
	// ChartRevision is the install revision the last successful chart apply
	// used, so a resume can tell whether the new chart is already what runs.
	ChartRevision string `json:"chart_revision,omitempty"`
	// MigratedRevision is the revision whose migration Job completed.
	MigratedRevision string `json:"migrated_revision,omitempty"`
}

// UpgradeSession is one control-plane upgrade run.
type UpgradeSession struct {
	ID   string
	Opts UpgradeOptions
	// PollInterval is Opts.PollInterval, carried here because every wait reads
	// it and none of them should have to reach through the options.
	PollInterval time.Duration
	// WaitDeadline is Opts.WaitDeadline, for the same reason.
	WaitDeadline time.Duration
	Jnl          *stages.Journal
	Emit         stages.Emitter
	Fence        FenceReport
	Record       UpgradeRecord
	FenceState   FenceState

	// Restored is the previous release's rollout, set by the migration stage
	// when its Job failed and the previous chart was put back. It carries the
	// readiness as an observation: see restorePreviousRelease.
	Restored *Rollout

	// Validation is what the validation stage will require, established by the
	// gates stage while the OLD backend is still the one running.
	Validation ValidationOptions

	// handle and skip come from the operation record — the lock — when the
	// command took it. Without them every remote action still happens; with
	// them each one is written down BEFORE it is submitted.
	handle *operation.Handle
	skip   map[string]bool

	// once guards the operations that must happen at most once per process.
	replicasKnown bool
}

// Identity is the part of the request a resume must match exactly.
func (o UpgradeOptions) Identity() stages.Identity {
	return stages.Identity{
		Kind:    UpgradeKind,
		Cluster: o.Cluster,
		Fields: map[string]string{
			"from bundle": o.From,
			"to bundle":   o.To,
		},
	}
}

// JournalPath is where this cluster's CONTROL-PLANE upgrade journal lives.
func JournalPath(cluster string) (string, error) {
	return stages.JournalPath(UpgradeKind, cluster)
}

// RunID identifies this process.
func (s *UpgradeSession) RunID() string { return s.ID }

// Journal is the durable record of stage transitions.
func (s *UpgradeSession) Journal() *stages.Journal { return s.Jnl }

// Emitter publishes transitions. Never nil.
func (s *UpgradeSession) Emitter() stages.Emitter {
	if s.Emit == nil {
		return stages.NopEmitter{}
	}
	return s.Emit
}

// BundleVersion is carried on every event.
func (s *UpgradeSession) BundleVersion() string { return s.Opts.To }

// TotalDeadline bounds the whole procedure, from the bundle's own limits.
func (s *UpgradeSession) TotalDeadline() (time.Duration, error) {
	if s.Opts.Bundle == nil {
		return 0, fmt.Errorf("no target bundle manifest, so this upgrade has no timeouts to obey")
	}
	return s.Opts.Bundle.Limits.Timeouts.For("install-total")
}

// Logf writes narrative that is not a stage transition.
func (s *UpgradeSession) Logf(format string, args ...any) {
	if s.Opts.Out == nil {
		return
	}
	fmt.Fprintf(s.Opts.Out, format+"\n", args...)
}

// ResumeAdvice is what to do after a clean pause.
func (s *UpgradeSession) ResumeAdvice() string {
	return "Re-run the identical command and it will continue from here; completed stages are skipped."
}

// Exits are the supported ways on from a failure.
func (s *UpgradeSession) Exits() []string {
	return []string{
		"resume     fix what the error names, then run the identical command again\n             (completed stages are skipped, and the fence is adopted if it is already up)",
		"roll back  `kubenest platform rollback --control-plane` restores the checkpoint this run\n             published. It is automatic while the fence has held continuously; after the\n             fence lifts it is an explicit recovery and states the data loss interval",
	}
}

func (s *UpgradeSession) saveRecord() error { return s.Jnl.SetState(&s.Record) }

func (s *UpgradeSession) now() time.Time {
	if s.Opts.Now != nil {
		return s.Opts.Now()
	}
	return time.Now()
}

// WithOperation attaches the operation record this run is covered by.
func (s *UpgradeSession) WithOperation(handle *operation.Handle, skip map[string]bool) {
	s.handle = handle
	s.skip = skip
}

// runner is the transport a stage's remote actions go through.
//
// WITH AN OPERATION IT IS THE RECORDING ONE. The Guarded decorator writes each
// action down BEFORE it is submitted and marks it afterwards, which is what
// makes a second laptop's resume able to establish what happened rather than
// guess (PLAN 7.2). Without an operation — a run that could not take the lock,
// which the gates refuse — nothing is recorded and nothing is skipped.
func (s *UpgradeSession) runner(stage string) k3s.Runner {
	if s.handle == nil {
		return s.Opts.Server
	}
	return &operation.Guarded{
		Inner: s.Opts.Server,
		Op:    s.handle,
		Stage: stage,
		Specs: actionSpecs,
		Skip:  s.skip,
	}
}

// Plan is the seven stages, wired.
func Plan(s *UpgradeSession) []stages.Stage {
	bind := func(name string, f func(context.Context, *UpgradeSession) error) stages.StageFunc {
		return func(ctx context.Context) error { return f(ctx, s) }
	}
	return []stages.Stage{
		{Name: StageGates, AlwaysRun: true, Run: bind(StageGates, stageGates)},
		{Name: StageFence, Run: bind(StageFence, stageFence)},
		{Name: StageCheckpoint, AlwaysRun: true, Run: bind(StageCheckpoint, stageCheckpoint)},
		{Name: StageMigration, Run: bind(StageMigration, stageMigration)},
		{Name: StageChart, Run: bind(StageChart, stageChart)},
		{Name: StageValidate, AlwaysRun: true, Run: bind(StageValidate, stageValidate)},
		{Name: StageUnfence, Run: bind(StageUnfence, stageUnfence)},
	}
}

// stageGates runs every gate, and changes nothing.
func stageGates(ctx context.Context, s *UpgradeSession) error {
	r := s.runner(StageGates)
	var failed []string
	for _, gate := range s.Opts.Gates {
		s.Logf("  %s", gate.String())
		if !gate.Passed {
			failed = append(failed, gate.Name)
		}
	}
	for _, gate := range clusterGates(ctx, r, s.Opts.Values) {
		s.Logf("  %s", gate.String())
		if !gate.Passed {
			failed = append(failed, gate.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("this control-plane upgrade is refused before anything is changed: %s. Nothing has been touched, the fence is down and the control plane is running as it was",
			strings.Join(failed, ", "))
	}
	// WHAT THE VALIDATION MUST SEE, established HERE because this is the last
	// moment the backend is still the OLD one: after the fence stage the
	// Deployments are the chart's, and comparing the chart's image with itself
	// would answer "unchanged" for every upgrade.
	expectations, err := ValidationExpectations(ctx, r, s.Opts.Before, s.Opts.Values)
	if err != nil {
		return err
	}
	s.Validation = expectations
	s.Logf("  validation will require: contract era >= %d, build not %q, build prefix %q",
		expectations.MinContract, expectations.StaleBuild, expectations.WantBuild)
	return nil
}

// clusterGates is the part of the pre-flight that needs the cluster: the
// field-ownership assertion (hardware, 2026-09-25) and the PostgreSQL pin.
func clusterGates(ctx context.Context, r k3s.Runner, values string) []Gate {
	gates := []Gate{}
	report, err := CheckFieldOwnership(ctx, r)
	switch {
	case err != nil:
		gates = append(gates, Gate{
			Name: "Chart field ownership", Passed: false, Detail: err.Error(),
			Fix: "this gate fails closed: a chart apply that cannot be checked is not one that was checked, and a field two managers own makes the next apply fail with a conflict instead of upgrading",
		})
	case len(report.Conflicts) > 0:
		refusal := &OwnershipRefusal{Report: report}
		gates = append(gates, Gate{
			Name: "Chart field ownership", Passed: false, Detail: refusal.Error(),
			Fix: "stop the writer that is not helm from owning a chart-rendered field, then run this upgrade again",
		})
	default:
		gates = append(gates, Gate{
			Name:   "Chart field ownership",
			Passed: true,
			Detail: fmt.Sprintf("%d object(s) read, and no chart-rendered field is owned by more than one manager (%s)", report.Objects, strings.Join(report.Managers, ", ")),
		})
	}
	if err := CheckPostgresUnchanged(ctx, r, values); err != nil {
		gates = append(gates, Gate{
			Name: "PostgreSQL unchanged", Passed: false, Detail: err.Error(),
			Fix: "1.2 does not change the PostgreSQL major or distribution; a migration of either is its own tested procedure in a later release",
		})
	} else {
		gates = append(gates, Gate{Name: "PostgreSQL unchanged", Passed: true, Detail: "the chart and the running control plane use the same PostgreSQL distribution and major version"})
	}
	return gates
}

// stageFence raises the fence: the public API route goes to the 503 page.
//
// ITS APPLY CHANGES THE ROUTE AND NOTHING ELSE, and the two things that would
// otherwise change with it are the backend's image and the migration Job
// (FenceOptions.BackendImage, FenceOptions.MigrationOff). This is the FIRST
// apply of the procedure: the checkpoint and the migration both come after it,
// and both must run against the code and the schema the control plane has now.
//
// IT ADOPTS A FENCE THAT IS ALREADY UP. A resume after an interrupted raise
// finds the route already switched, and re-raising while believing it is
// switching would be a step whose failure mode is invisible. An adopted fence
// makes no apply: the values it would apply are the ones this run started from.
func stageFence(ctx context.Context, s *UpgradeSession) error {
	r := s.runner(StageFence)
	status, err := Status(ctx, r)
	if err != nil {
		return err
	}
	if status.State == FenceUp {
		s.Logf("  the fence is already up: %s/<release>-api points at %s, so no customer request reaches the backend", Namespace, status.BackendRef)
		s.Fence = status
		s.FenceState = FenceUp
		// The count the backend ran at before the fence went up. A re-run that
		// adopts the fence has not read it, and by now the Deployment may be at
		// zero (a failed migration's stop apply), so the fence's own record is
		// the source; without it the chart stage would bring back no backend.
		replicas, err := adoptedBackendReplicas(ctx, r, s.Record.BackendReplicas)
		if err != nil {
			return err
		}
		if replicas != s.Record.BackendReplicas {
			s.Record.BackendReplicas = replicas
			return s.saveRecord()
		}
		return nil
	}
	replicas, err := Replicas(ctx, r)
	if err != nil {
		return err
	}
	// THE IMAGE THE BACKEND RUNS NOW, read before anything is applied. The
	// chart this binary carries pins the NEW backend image, so the apply that
	// raises the fence would otherwise roll the backend Deployment — and the
	// checkpoint CronJob, which runs the same image — onto code that has never
	// been migrated. That roll happens BEFORE the checkpoint this procedure
	// exists to take and before the migration that code needs, which is the
	// invariant the whole ordered procedure buys (hardware finding, 2026-09-25).
	// An image that cannot be read is refused, never silently replaced by the
	// chart's new pin.
	image, err := runningBackendImage(ctx, r)
	if err != nil {
		return err
	}
	s.Record.BackendReplicas = replicas
	s.Record.FenceRaised = true
	if err := s.saveRecord(); err != nil {
		return err
	}
	// THE STAMP IS THIS RAISE'S IDENTITY. k3s's deploy controller records the
	// fence manifest as an Addon carrying its checksum and skips an apply whose
	// content matches, so two raises of identical values are ONE raise as far as
	// the cluster is concerned (hardware, 2026-09-25: every second
	// control-plane upgrade failed at the fence). The operation id is stable
	// across a resume — the same raise retried is the same raise — and the run
	// id covers a run that holds no record.
	facts, err := s.fenceFacts(ctx, r, replicas)
	if err != nil {
		return err
	}
	fenced, err := Raise(ctx, r, s.Opts.Values, RaiseOptions{
		Stamp: s.fenceStamp(),
		Facts: facts,
	})
	if err != nil {
		return err
	}
	// THE MIGRATION JOB GOES OFF WITH it, and that is not a formality either:
	// the running values still enable it, so an apply that reproduced them
	// would re-render the release's Job with the new pod template while the
	// previous revision's Job is still there. Job.spec.template is immutable,
	// so helm would try to PATCH it and the upgrade would fail outright,
	// leaving the release failed under failurePolicy: abort (the second half of
	// the same hardware finding). With it off, helm deletes the Job it had
	// rendered, and stageMigration creates it again at this run's revision.
	values, err := FenceValues(fenced, FenceOptions{Up: true, MigrationOff: true, BackendImage: &image})
	if err != nil {
		return err
	}
	if err := s.apply(ctx, StageFence, values); err != nil {
		return err
	}
	// THE ROUTE IS OBSERVED, NOT ASSUMED. The apply writes the HelmChart and
	// helm-controller re-renders the route afterwards, so a stage that returned
	// here reported "the fence is up" while api.<domain> still reached the
	// backend (hardware, 2026-09-25). The checkpoint and the migration must not
	// start until the switch has actually happened.
	if err := s.waitForRoute(ctx, StageFence, FenceService); err != nil {
		return err
	}
	if err := s.waitForFenceReady(ctx, StageFence); err != nil {
		return err
	}
	s.FenceState = FenceUp
	s.Logf("  the fence is up: %s/%s-api answers 503 and the backend stays up behind the node's tunnel, so the checkpoint can still be taken", Namespace, ReleaseName)
	return nil
}

// stageCheckpoint publishes a checkpoint and waits until it is ELIGIBLE.
//
// "PUBLISHED" IS NOT "ELIGIBLE" and the difference is the whole stage: a
// checkpoint becomes eligible only once every object of it is uploaded, and
// rolling a control plane back to a partial checkpoint is rolling it back to
// nothing. The wait is OnDemandCheckpoint's, which returns only when the
// status document names a checkpoint that did not exist before this run.
func stageCheckpoint(ctx context.Context, s *UpgradeSession) error {
	r := s.runner(StageCheckpoint)
	run, err := OnDemandCheckpoint(ctx, r, s.Opts.Bundle, s.Opts.Reporter)
	if err != nil {
		return err
	}
	marker := eligibleMarker(run.Checkpoint)
	if marker == "" {
		return fmt.Errorf("the checkpoint Job %s reported no eligible checkpoint, and an upgrade with no recovery point must not start", run.Job)
	}
	s.Record.CheckpointMarker = marker
	if err := s.saveRecord(); err != nil {
		return err
	}
	s.Logf("  checkpoint eligible BEFORE the migration starts: %s (job %s)", marker, run.Job)
	return nil
}

// eligibleMarker names one eligible checkpoint. The key is the object in the
// bucket; the timestamp distinguishes two checkpoints written under it.
func eligibleMarker(c EligibleCheckpoint) string {
	if c.Key == "" {
		return ""
	}
	if c.At == "" {
		return c.Key
	}
	return c.Key + "@" + c.At
}

// stageMigration runs the schema Job at the revision the new chart renders,
// with the backend stopped.
//
// THE BACKEND IS STOPPED FOR IT, through the chart's own replica value: with
// the Recreate strategy and backoffLimit 0 on the Job, a backend running the
// old code while the schema moves is the exact state this whole procedure
// exists to prevent.
//
// ITS FIRST APPLY TURNS THE MIGRATION JOB OFF (FenceOptions.MigrationOff), and
// it is applied BEFORE clearStaleMigrationJob for a reason: the values this run
// starts from still enable the Job, so an apply that reproduced them would
// re-render the previous revision's Job with a new pod template and fail helm
// on an immutable template before the stale Job was ever removed. With the Job
// off, helm deletes it, and the apply that follows creates it at THIS run's
// revision and runs the NEW image — the chart's own pin, not the fence's.
func stageMigration(ctx context.Context, s *UpgradeSession) error {
	r := s.runner(StageMigration)
	stopped, err := FenceValues(s.Opts.Values, FenceOptions{Up: true, BackendReplicas: int32Ptr(0), MigrationOff: true})
	if err != nil {
		return err
	}
	if err := s.apply(ctx, StageMigration, stopped); err != nil {
		return err
	}
	values, err := MigrationValues(stopped)
	if err != nil {
		return err
	}
	revision, err := Revision(values)
	if err != nil {
		return err
	}
	if err := clearStaleMigrationJob(ctx, r, revision); err != nil {
		return err
	}
	if _, err := Apply(ctx, r, values); err != nil {
		return err
	}
	deadline, err := s.Opts.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	if err := WaitForMigration(ctx, r, revision, deadline, s.Opts.Reporter); err != nil {
		// PUT THE PREVIOUS CHART BACK BEFORE REPORTING, because the migration
		// applied this chart with `backend.replicas: 0` and the NEW image — the
		// chart has one image value for the Job and the Deployment — so a
		// failure otherwise leaves the control plane running NOTHING. Every
		// read then fails, through the fenced route and through the node's
		// tunnel alike, and the advertised way on ("fix what the error names,
		// then run the identical command again") is refused for want of a
		// backend (hardware, 2026-09-26; kn-t70-control-plane-version-identity-4xso.2).
		//
		// The fence STAYS UP: the backend this puts back has not been
		// validated, and the checkpoint that would justify lowering the fence
		// is the one the failed migration was meant to be preceded by.
		if rerr := s.restorePreviousRelease(ctx); rerr != nil {
			return fmt.Errorf("%w; and the previous release could not be put back behind the fence (%v), so the control plane is running nothing: restore it by hand before doing anything else", err, rerr)
		}
		// THE READINESS IS REPORTED SEPARATELY, because it is a different fact:
		// the previous chart IS what runs, and its backend is waiting for the
		// database the failed migration was denied.
		if s.Restored != nil && s.Restored.Ready < s.Restored.Want {
			return fmt.Errorf("%w. The previous release is what runs behind the fence: its backend has rolled out at install revision %s and %d/%d replicas are Ready — the previous backend is running and not Ready yet; it becomes Ready once its database answers. Restore the database and run the identical command again",
				err, s.Restored.Revision, s.Restored.Ready, s.Restored.Want)
		}
		return err
	}
	s.Record.MigratedRevision = revision
	return s.saveRecord()
}

// stageChart rolls the new chart with the fence still up, so the new backend
// is only reachable from the node until it has been validated.
func stageChart(ctx context.Context, s *UpgradeSession) error {
	r := s.runner(StageChart)
	replicas := s.Record.BackendReplicas
	if replicas == 0 {
		// A record written before the fence read the count, or a resumed run
		// that never raised the fence. Read it rather than start the control
		// plane at the chart's default, which is a number this run never saw.
		read, err := Replicas(ctx, r)
		if err != nil {
			return err
		}
		replicas = read
		s.Record.BackendReplicas = read
		if err := s.saveRecord(); err != nil {
			return err
		}
	}
	deadline, err := s.componentReady()
	if err != nil {
		return err
	}
	revision, _, err := applyBehindFence(ctx, r, fencedApply{
		Code:     backendNew,
		Values:   s.Opts.Values,
		Replicas: replicas,
		Deadline: deadline,
		Every:    s.PollInterval,
		Reporter: s.Opts.Reporter,
		Logf:     s.Logf,
	})
	if err != nil {
		return err
	}
	s.Record.ChartRevision = revision
	return s.saveRecord()
}

// stageValidate proves the new backend through the node's port-forward, while
// the fence is still up.
//
// IT VALIDATES AGAINST WHAT THE GATES STAGE ESTABLISHED, not against the era
// alone: see stageGates.
func stageValidate(ctx context.Context, s *UpgradeSession) error {
	r := s.runner(StageValidate)
	deadline, err := s.componentReady()
	if err != nil {
		return err
	}
	every := s.PollInterval
	if every <= 0 {
		every = 5 * time.Second
	}
	return retryUnreachable(ctx, deadline, every, func() error {
		return ValidationReporter(ctx, r, s.Opts.Open, s.Validation, s.Opts.Reporter)
	})
}

// stageUnfence restores the route and dismantles the fence.
func stageUnfence(ctx context.Context, s *UpgradeSession) error {
	r := s.runner(StageUnfence)
	replicas := s.Record.BackendReplicas
	if err := Lower(ctx, r, s.Opts.Values, int32Ptr(replicas),
		func(values string) error {
			// The migration Job stays enabled here too: see stageChart. This is
			// the LAST apply of the run, so a Job rendered off here is a Job
			// helm deletes.
			withMigration, err := MigrationValues(values)
			if err != nil {
				return err
			}
			return s.apply(ctx, StageUnfence, withMigration)
		},
		func() error {
			// AND THE FENCE'S OBJECTS OUTLIVE THE APPLY. The route is still the
			// fence until helm-controller re-renders it; deleting the Service
			// first leaves api.<domain> naming something that is gone.
			deadline, err := s.componentReady()
			if err != nil {
				return err
			}
			return WaitForRouteBackend(ctx, s.runner(StageUnfence), backendService, deadline, s.PollInterval, s.Opts.Reporter)
		}); err != nil {
		return err
	}
	s.Record.FenceRaised = false
	if err := s.saveRecord(); err != nil {
		return err
	}
	s.FenceState = FenceDown
	s.Logf("  the fence is down: %s/%s-api answers from the backend again", Namespace, ReleaseName)
	return nil
}

// apply writes the control-plane chart's HelmChart through the stage's runner.
func (s *UpgradeSession) apply(ctx context.Context, stage, values string) error {
	_, err := Apply(ctx, s.runner(stage), values)
	return err
}

func int32Ptr(v int32) *int32 { return &v }

// checkpointJobIn extracts the Job name from
// `kubectl create job --from=cronjob/<cronjob> <job> -n <ns>`.
func checkpointJobIn(command string) string {
	fields := strings.Fields(command)
	for i, field := range fields {
		if field == "--from=cronjob/"+CheckpointCronJobName && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// actionSpecs names the mutating commands among the actions this operation
// submits, with what a successor must observe to tell whether each happened.
//
// READS ARE NOT ACTIONS. Their postcondition is the read itself, and recording
// every observation would fill the record with things no successor needs and
// could not safely skip. What is here is what changes the cluster.
func actionSpecs(stage, command string) (operation.Spec, bool) {
	switch {
	case strings.Contains(command, "manifests/"+fenceName+".yaml"):
		path := k3s.ManifestDir + "/" + fenceName + ".yaml"
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "the fence's objects are declared in " + path + " on the server node, which is what k3s applies them from",
			Observe:       "sudo -n test -s " + path + " && echo declared",
		}, true
	case strings.Contains(command, "manifests/"+ReleaseName+".yaml"):
		// The control plane's own HelmChart. The revision is IN the document
		// written here, so a resume can establish which apply this was rather
		// than only that some apply happened.
		path := k3s.ManifestDir + "/" + ReleaseName + ".yaml"
		return operation.Spec{
			Kind:          operation.ActionPlan,
			Postcondition: "the control plane's HelmChart declares the install revision this stage rendered",
			Observe:       "sudo -n grep -q installRevision " + path + " && echo declared",
		}, true
	case strings.Contains(command, "create job --from=cronjob/"+CheckpointCronJobName):
		// The checkpoint Job is created by name, so the action's identity is
		// the object a successor would look for.
		job := checkpointJobIn(command)
		if job == "" {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "the checkpoint Job " + job + " exists",
			Observe:       "sudo -n k3s kubectl get job " + job + " -n " + Namespace + " >/dev/null 2>&1 && echo created",
		}, true
	case strings.Contains(command, "delete job "+MigrationJobName):
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "the previous migration Job is gone, so the chart can create this revision's Job",
			Observe:       "sudo -n k3s kubectl get job " + MigrationJobName + " -n " + Namespace + " >/dev/null 2>&1; test $? -ne 0 && echo gone",
		}, true
	case strings.Contains(command, "delete deployment/"+fenceName):
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "the fence's objects are removed and the API route answers from the backend again",
			Observe:       "sudo -n k3s kubectl get service " + fenceName + " -n " + Namespace + " >/dev/null 2>&1; test $? -ne 0 && echo removed",
		}, true
	}
	return operation.Spec{}, false
}

// waitForRoute converges until the public API route names want.
//
// BOTH DIRECTIONS ARE WAITED ON. On the way in, so the fence is real before the
// checkpoint and the migration start; on the way out (through Lower's confirm),
// so the fence's objects are not deleted while api.<domain> still names them.
func (s *UpgradeSession) waitForRoute(ctx context.Context, stage, want string) error {
	deadline, err := s.componentReady()
	if err != nil {
		return err
	}
	return WaitForRouteBackend(ctx, s.runner(stage), want, deadline, s.PollInterval, s.Opts.Reporter)
}

// waitForFenceReady converges until the fence answers 503 itself.
func (s *UpgradeSession) waitForFenceReady(ctx context.Context, stage string) error {
	deadline, err := s.componentReady()
	if err != nil {
		return err
	}
	return WaitForFenceAvailable(ctx, s.runner(stage), deadline, s.PollInterval, s.Opts.Reporter)
}

// restorePreviousRelease puts the PREVIOUS CODE back behind the fence after a
// failed migration.
//
// IT RE-APPLIES THE NEW CHART, NOT THE PREVIOUS ONE, and that is the whole
// subtlety (hardware, 2026-09-26): the previous chart has no fence template — a
// real 1.1 control plane predates the fence entirely — so re-applying it with
// `fence.enabled: true` renders the api route back onto the backend and the
// fence comes DOWN, exposing an unvalidated backend. Only the NEW chart can
// express "the old code, fenced", so the restore applies the new archive with:
//
//	the run's values, unchanged;
//	fence.enabled          TRUE  — the fence stays up, which is the point;
//	migration.enabled      FALSE — the chart has ONE backend image for the Job
//	                               and the Deployment and the Job's pod template
//	                               is immutable;
//	backend.image                the image the backend ran BEFORE the upgrade,
//	                               recorded on the fence at the fence stage;
//	backend.replicas             the count recorded before the fence stopped it.
//
// That is the state from just before the migration, and it is the state to
// return to while the checkpoint is the operator's recovery point.
func (s *UpgradeSession) restorePreviousRelease(ctx context.Context) error {
	r := s.runner(StageMigration)
	facts, ok, err := FenceDeploymentFacts(ctx, r)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the fence's own Deployment is gone, so the image the previous release ran cannot be established")
	}
	replicas := s.Record.BackendReplicas
	if replicas == 0 {
		// A NEW process has no journal, so the count comes from the fence.
		replicas = facts.Replicas
	}
	deadline, err := s.componentReady()
	if err != nil {
		return err
	}
	_, rollout, err := applyBehindFence(ctx, r, fencedApply{
		Code:          backendPrevious,
		Values:        s.Opts.Values,
		PreviousImage: facts.PreviousImage,
		Replicas:      replicas,
		Deadline:      deadline,
		Every:         s.PollInterval,
		Reporter:      s.Opts.Reporter,
		Logf:          s.Logf,
	})
	if err != nil {
		return err
	}
	s.Restored = rollout
	return nil
}

// backendCode says which code a fenced apply brings back.
type backendCode int

const (
	// backendNew is the release this run is moving to: the pin the chart this
	// binary carries declares, and the schema the migration Job created.
	backendNew backendCode = iota
	// backendPrevious is the code the backend ran BEFORE this upgrade. Only the
	// fence knows its image, because after the first apply the reference is
	// nowhere on the cluster: the chart this binary carries pins the new one.
	backendPrevious
)

// fencedApply is one apply of the control-plane chart while the fence is up,
// and how to wait for what it brings back.
type fencedApply struct {
	// Code is which code this apply puts behind the fence.
	Code backendCode
	// Values is the values document the control plane runs with, as the run
	// read it off the cluster.
	Values string
	// PreviousImage is the image the backend ran before the upgrade, recorded
	// on the fence. It is required for backendPrevious and unused for
	// backendNew, which takes the chart's own pin.
	PreviousImage string
	// Replicas is how many backend replicas the control plane ran before the
	// fence stopped it.
	Replicas int32
	// Deadline bounds the wait that follows the apply.
	Deadline time.Duration
	// Every is that wait's poll interval. Zero means the converge default.
	Every time.Duration
	// Reporter publishes the wait's progress.
	Reporter converge.Reporter
	// Logf writes the line that says what came back. Nil writes nothing.
	Logf func(format string, args ...any)
}

// applyBehindFence applies the control-plane chart with the fence up and waits
// for the backend the apply brings back, returning the install revision it
// applied and — for the previous code — how far its rollout got.
//
// IT IS THE ONE IMPLEMENTATION of every apply a fenced control plane takes: the
// migration stage's chart roll onto the new code (stageChart), the restore after
// a failed migration (restorePreviousRelease) and the recovery a resumed run
// performs before its first read (RecoverStoppedBackend). Three copies of these
// values would be three chances for the fence, the image and the migration Job
// to disagree about what is running.
func applyBehindFence(ctx context.Context, r k3s.Runner, a fencedApply) (string, *Rollout, error) {
	values := a.Values
	// ZERO IS NEVER A COUNT TO APPLY. The whole point of this function is that
	// a backend is running when it returns, and a count nobody recorded — a
	// fence raised before any run read one, or a Deployment already at zero
	// because a migration stopped it — starts at the chart's own size of one
	// rather than leaving the control plane with none and every read refused.
	replicas := a.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	if a.Code == backendPrevious {
		if a.PreviousImage == "" {
			return "", nil, fmt.Errorf("the fence records no %s, so the image the previous release ran cannot be established: nothing was recorded at the fence stage", fencePreviousImageAnnotation)
		}
		pinned, err := withBackendImage(values, a.PreviousImage)
		if err != nil {
			return "", nil, err
		}
		values = pinned
	}
	fenced, err := FenceValues(values, FenceOptions{Up: true, BackendReplicas: &replicas})
	if err != nil {
		return "", nil, err
	}
	// THE MIGRATION JOB IS ON FOR THE NEW CODE AND OFF FOR THE PREVIOUS ONE.
	//
	// On, for the new code, from the chart stage onward: a chart apply that
	// rendered it off makes helm DELETE the Job the migration stage created, and
	// the operator-visible record of the schema step would be gone after the
	// upgrade (hardware, 2026-09-25). Its pod template depends only on the
	// backend image, the pull secrets and the PostgreSQL host, user and database
	// — none of which the later applies change — so keeping it on cannot hit the
	// immutable-field rule.
	//
	// Off, for the previous code, because the chart has ONE backend image for
	// the Job and the Deployment and the Job's pod template is immutable: an
	// apply that re-enabled it would render the Job with the new pod template
	// over the Job that already exists, and helm would fail the release.
	if a.Code == backendPrevious {
		fenced, err = migrationValues(fenced, false)
	} else {
		fenced, err = MigrationValues(fenced)
	}
	if err != nil {
		return "", nil, err
	}
	revision, err := Apply(ctx, r, fenced)
	if err != nil {
		return "", nil, err
	}
	if a.Code == backendPrevious {
		// ONLY THE ROLLOUT IS WAITED FOR, NOT READINESS: the migration failed
		// because PostgreSQL went away, so the previous backend's pods start and
		// sit NotReady until the database answers.
		rollout, err := WaitRolledOut(ctx, r, revision, a.Deadline, a.Every, a.Reporter)
		if err != nil {
			return revision, nil, err
		}
		if a.Logf != nil {
			a.Logf("  the previous code is back behind the fence at install revision %s (%s, %d/%d backend replicas ready), with the fence up and the migration off",
				revision, a.PreviousImage, rollout.Ready, rollout.Want)
		}
		return revision, &rollout, nil
	}
	// READINESS IS THE WHOLE WAIT FOR THE NEW CODE: it is the chart this run is
	// moving to, and a backend that has not become Ready is not one any read can
	// use.
	if err := waitReadyWithin(ctx, r, revision, a.Deadline, a.Every, a.Reporter); err != nil {
		return revision, nil, err
	}
	return revision, nil, nil
}

// A RESUME THAT FINDS THE BACKEND STOPPED MUST PUT ONE BACK BEFORE IT READS
// (kn-t70-control-plane-version-identity-4xso.3).
//
// The migration stage's first apply stops the backend — `backend.replicas: 0`
// with the fence up — so the schema moves under no running code. A run
// interrupted after that apply, or a laptop that died, leaves the cluster
// exactly there: the fence up, the backend stopped, and no process left
// anywhere to put one back. A second laptop's `--resume` then dies on its FIRST
// ordinary read — `control plane kubenest-backend is unreachable: ... connect
// failed ("Connection refused")` (hardware, 2026-09-26) — because the record,
// the window and the bundle manifests all have to be read from the control
// plane this procedure has stopped.
//
// WHICH CODE TO BRING BACK IS A QUESTION FOR THE CLUSTER, because the second
// laptop holds no journal: the migration Job is the only witness to how far the
// schema moved. Three answers:
//
//	the Job is running     wait for it, bounded by the bundle's own
//	                       component-ready deadline, and read its outcome. No
//	                       backend is started while it runs: a backend against
//	                       a schema in flight is the state this procedure
//	                       exists to prevent;
//	the Job succeeded      the schema is the new one, so the NEW code goes back
//	                       — fence up, migration on, the replica count the fence
//	                       recorded — exactly as stageChart would apply it;
//	the Job failed, or     the schema has not moved, so the PREVIOUS code goes
//	there is no Job at all back exactly as restorePreviousRelease puts it: the
//	                       image and the replica count the fence recorded,
//	                       migration off, waiting only for the rollout.
//
// THE FENCE STAYS UP in every case. The code this puts back has not been
// validated, and the checkpoint that would justify lowering the fence is the one
// this run has not taken yet; the run that follows continues from the recorded
// stage, which lowers it once the new backend has been proven.
//
// THE JOB IS NOT MATCHED BY ITS INSTALL REVISION, and that is a measurement
// rather than a preference. A migration apply stamps the Job with a hash of the
// values it was composed from, and those values carry the installRevision of the
// apply before it — so the revision a SECOND process computes from the values it
// reads off the cluster is not the one the interrupted process stamped for the
// same chart and the same stage (measured 2026-09-26: 38959d1066e00715 against
// bd33b55a43e0b1e1). What identifies the Job instead is what the chart gives it:
// there is exactly one Job, MigrationJobName, and the only apply that renders it
// ON is the migration stage's — every apply before it renders it off
// (FenceOptions.MigrationOff), which is also what deletes a previous release's
// Job. So a Job that exists while the fence is up over a stopped backend is this
// chart's migration, and its state says which code the schema matches.
//
// THE RUNNING JOB IS WAITED ON WITH THE REVISION IT CARRIES, which is the
// revision-only filter migrate.go's wait can honestly apply: it is a fact about
// this exact Job rather than a value computed at the wrong moment.
//
// THE DEADLINE IS THE CALLER'S, because this function must not read the control
// plane to find one: the bundle's `component-ready` limit, and never a default.
func RecoverStoppedBackend(ctx context.Context, r k3s.Runner, o RecoveryOptions) error {
	code, found, err := stoppedBackendCode(ctx, r, o.Deadline, o.Reporter)
	if err != nil {
		return err
	}
	_, rollout, err := applyBehindFence(ctx, r, fencedApply{
		Code:          code,
		Values:        o.Values,
		PreviousImage: o.Facts.PreviousImage,
		Replicas:      o.Facts.Replicas,
		Deadline:      o.Deadline,
		Every:         o.Every,
		Reporter:      o.Reporter,
	})
	if err != nil {
		return err
	}
	if o.Logf == nil {
		return nil
	}
	// ONE LINE SAYING WHAT WAS FOUND AND WHAT WAS BROUGHT BACK, because the
	// question a second laptop asks here — is this control plane mid-migration,
	// and which code does that leave it needing? — is answered by the cluster
	// and by nothing the operator can read.
	brought := "the new code the migration moved the schema to"
	if code == backendPrevious {
		brought = fmt.Sprintf("the previous code (%s)", o.Facts.PreviousImage)
		if rollout != nil {
			brought += fmt.Sprintf(", %d/%d backend replicas ready", rollout.Ready, rollout.Want)
		}
	}
	o.Logf("  the fence is up and the backend is stopped at zero replicas with %s: brought back %s, and the fence stays up", found, brought)
	return nil
}

// RecoveryOptions is what a run gives the recovery: the values it read off the
// cluster, the facts the fence recorded about the code behind it, and the
// deadline its own bundle manifest names.
type RecoveryOptions struct {
	// Values is the values document the control plane runs with. It is carried
	// onto whichever code comes back rather than re-derived, because the
	// settings, the generated secrets and the control plane's CA are facts about
	// this installation.
	Values string
	// Facts is what the fence records about the control plane behind it.
	Facts FenceFacts
	// Deadline bounds the wait for a running migration and for the backend the
	// recovery brings back. It is the target bundle's `component-ready` limit.
	Deadline time.Duration
	// Every is the poll interval of the wait for the backend this brings back.
	// Zero means the converge default; the wait for a RUNNING migration is
	// migrate.go's and keeps its own.
	Every time.Duration
	// Reporter publishes the waits' progress.
	Reporter converge.Reporter
	// Logf writes the one line that says what was found and what came back.
	Logf func(format string, args ...any)
}

// StoppedBackendBehindAFence reports whether the control plane's backend is
// stopped behind a fence that is still up, and the facts the fence carries about
// the code it is holding back.
//
// TWO READS, BOTH THROUGH THE NODE, and neither needs the control plane's API:
// the fence's own Deployment exists exactly while the fence is up, and the
// backend's desired replica count is a field on the Deployment helm rendered.
// That is what makes this callable BEFORE every read a resume makes.
//
// A fence that is not up is not this: nothing has been taken apart, and a
// backend at the chart's default size is not this either, because the reads the
// caller is about to make reach it.
func StoppedBackendBehindAFence(ctx context.Context, r k3s.Runner) (FenceFacts, bool, error) {
	facts, ok, err := FenceDeploymentFacts(ctx, r)
	if err != nil || !ok {
		return FenceFacts{}, false, err
	}
	replicas, err := Replicas(ctx, r)
	if err != nil {
		return FenceFacts{}, false, err
	}
	if replicas > 0 {
		return FenceFacts{}, false, nil
	}
	return facts, true, nil
}

// stoppedBackendCode decides which code the cluster says a stopped backend
// should come back as, and describes what it found for the narrative line.
//
// THE MIGRATION JOB IS THE ONLY WITNESS. A second laptop's journal says what
// THIS machine did, not what the cluster is, so the Job's own state — and, while
// it runs, its own outcome — is what decides between the two codes.
func stoppedBackendCode(ctx context.Context, r k3s.Runner, deadline time.Duration, rep converge.Reporter) (backendCode, string, error) {
	job, found, err := readMigrationJob(ctx, r)
	if err != nil {
		return backendPrevious, "", err
	}
	if !found {
		// NO JOB: the interrupt came between the stop apply and the Job's
		// creation, so alembic never ran and the schema is the one the previous
		// code matches.
		return backendPrevious, "no migration Job in the cluster, so the schema has not moved", nil
	}
	revision := job.Metadata.Annotations[migrationJobRevisionAnnotation]
	switch {
	case jobFailed(job):
		return backendPrevious, fmt.Sprintf("the migration Job %s failed (install revision %s)", MigrationJobName, jobRevision(revision)), nil
	case jobComplete(job):
		return backendNew, fmt.Sprintf("the migration Job %s completed (install revision %s)", MigrationJobName, jobRevision(revision)), nil
	}
	// RUNNING, OR WITHOUT A VERDICT YET. NOTHING IS STARTED WHILE IT RUNS, so
	// the wait comes first and the decision second: the Job has backoffLimit 0,
	// which makes Failed final, and a schema is not something to guess about.
	if err := WaitForMigration(ctx, r, revision, deadline, rep); err != nil {
		after, still, readErr := readMigrationJob(ctx, r)
		if readErr != nil {
			return backendPrevious, "", readErr
		}
		if still && jobFailed(after) {
			return backendPrevious, fmt.Sprintf("the migration Job %s failed while this run waited for it", MigrationJobName), nil
		}
		// STILL RUNNING — or gone, or stamped for another revision: whatever the
		// reason, the deadline ran out with the schema possibly moving, and a
		// backend started now would be code against a schema nobody knows. The
		// wait's own error says which of the three it was.
		return backendPrevious, "", fmt.Errorf("the migration Job %s has not settled, and no backend is started while the schema may still be moving: %w", MigrationJobName, err)
	}
	return backendNew, fmt.Sprintf("the migration Job %s completed while this run waited for it", MigrationJobName), nil
}

// jobComplete reports whether the Job has ended in success. It is the
// complement of migrate.go's jobFailed and read the same way: from the Job
// controller's own condition, never from a pod's exit or a missing failure.
func jobComplete(job migrationJob) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Status == "True" && condition.Type == "Complete" {
			return true
		}
	}
	return false
}

// jobRevision names the install revision a Job carries, for a line an operator
// reads. A Job that carries none is said rather than printed as nothing.
func jobRevision(revision string) string {
	if revision == "" {
		return "none recorded"
	}
	return revision
}

// withBackendImage pins the chart's backend image.
//
// A failed migration must put back the image the backend RAN, and the values a
// run carries name whatever the chart pins now, so the pin is written here
// explicitly — repository, tag and digest separately, which is what the chart's
// image helper reads.
func withBackendImage(valuesYAML, ref string) (string, error) {
	image := parsePostgresImage(ref)
	if image.repository == "" {
		return "", fmt.Errorf("the recorded previous backend image %q names no repository", ref)
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		return "", fmt.Errorf("reading the control-plane values: %w", err)
	}
	pin := map[string]any{"repository": image.repository, "pullPolicy": "IfNotPresent"}
	if image.tag != "" {
		pin["tag"] = image.tag
	}
	if image.digest != "" {
		pin["digest"] = image.digest
	}
	backend, _ := doc["backend"].(map[string]any)
	if backend == nil {
		backend = map[string]any{}
	}
	backend["image"] = pin
	doc["backend"] = backend
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("rendering the control-plane values: %w", err)
	}
	return string(out), nil
}

// fenceFacts is what this raise records on the fence about the control plane
// behind it, so a re-run whose backend is gone can still establish which control
// plane it is upgrading and inside which window.
func (s *UpgradeSession) fenceFacts(ctx context.Context, r k3s.Runner, replicas int32) (FenceFacts, error) {
	facts := FenceFacts{Contract: s.Opts.Before.Contract, Build: s.Opts.Before.Build, Replicas: replicas}
	// THE IMAGE THE BACKEND RUNS NOW is the one a failed migration puts back, and
	// it is only knowable HERE: after the first apply the chart is the new one and
	// the released image exists nowhere the CLI can read
	// (kn-t70-control-plane-version-identity-4xso.2).
	running, err := RunningBackendImageRef(ctx, r)
	if err != nil {
		return facts, err
	}
	facts.PreviousImage = running
	if s.Opts.Window != nil {
		if spec, err := json.Marshal(s.Opts.Window.Spec()); err == nil {
			facts.Window = string(spec)
		}
	}
	return facts, nil
}

// fenceStamp identifies one raise. The operation id when this run holds the
// record — a resume of the same operation is the same raise — and the run id
// otherwise, which is new on every process. It is never empty: a raise that
// stamped nothing would write the same bytes as the last one.
func (s *UpgradeSession) fenceStamp() string {
	if s.handle != nil && s.handle.OperationID() != "" {
		return s.handle.OperationID()
	}
	return s.ID
}

// componentReady is the bundle's component-ready deadline. Every wait in this
// procedure is bounded by something real, and the number comes from the
// manifest rather than from a default in this binary.
func (s *UpgradeSession) componentReady() (time.Duration, error) {
	if s.WaitDeadline > 0 {
		return s.WaitDeadline, nil
	}
	if s.Opts.Bundle == nil {
		return 0, fmt.Errorf("no target bundle manifest, so this upgrade has no deadlines to obey")
	}
	return s.Opts.Bundle.Limits.Timeouts.For("component-ready")
}

// adoptedBackendReplicas is the backend's replica count for a run that adopts a
// fence it did not raise: the count this run recorded if it has one, else the
// count the fence recorded when it went up.
func adoptedBackendReplicas(ctx context.Context, r k3s.Runner, recorded int32) (int32, error) {
	if recorded > 0 {
		return recorded, nil
	}
	facts, ok, err := FenceDeploymentFacts(ctx, r)
	if err != nil {
		return 0, fmt.Errorf("reading the replica count the fence recorded: %w", err)
	}
	if !ok || facts.Replicas <= 0 {
		return 0, nil
	}
	return facts.Replicas, nil
}
