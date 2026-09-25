package controlplane

import (
	"context"
	"fmt"
	"io"
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

	// WantBuild is an exact stamp to require, when the caller knows one.
	WantBuild string

	// StaleBuild is the build stamp the current backend image reports — the
	// one the validation must NOT see after the upgrade. It is set to the
	// pre-upgrade stamp when the chart's backend image differs from the one
	// running, because "the same build is still serving" is then a failure.
	StaleBuild string

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
	fenced, err := Raise(ctx, r, s.Opts.Values)
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
	running, err := FenceValues(s.Opts.Values, FenceOptions{Up: true, BackendReplicas: int32Ptr(replicas)})
	if err != nil {
		return err
	}
	// THE MIGRATION JOB STAYS ENABLED FROM HERE ON. A chart apply that rendered
	// it off makes helm DELETE the Job the migration stage created: the
	// operator-visible record of the schema step would be gone after the
	// upgrade and `kubectl logs job/kubenest-cp-migrate` would have nothing to
	// read (hardware, 2026-09-25). The Job's pod template depends only on the
	// backend image, the pull secrets and the PostgreSQL host, user and
	// database — none of which the later applies change — so keeping it on
	// cannot hit the immutable-field rule.
	running, err = MigrationValues(running)
	if err != nil {
		return err
	}
	revision, err := Apply(ctx, r, running)
	if err != nil {
		return err
	}
	if err := WaitReady(ctx, r, revision, s.Opts.Bundle, s.Opts.Reporter); err != nil {
		return err
	}
	s.Record.ChartRevision = revision
	return s.saveRecord()
}

// stageValidate proves the new backend through the node's port-forward, while
// the fence is still up.
func stageValidate(ctx context.Context, s *UpgradeSession) error {
	r := s.runner(StageValidate)
	return ValidationReporter(ctx, r, s.Opts.Open, ValidationOptions{
		MinContract: s.Opts.Before.Contract,
		StaleBuild:  s.Opts.StaleBuild,
		WantBuild:   s.Opts.WantBuild,
	}, s.Opts.Reporter)
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
