package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
)

// `kubenest backup restore` (plan 7.5, kn-x0wv / kn-t43).
//
// Two modes, one command. Mode 1 puts a whole NAMESPACE back into the live
// cluster: it chooses an eligible backup, prints a plan of what will change,
// waits for the operator's confirmation, pauses the project's reconcilers and
// stops the namespace's scheduled work and any Job still running, takes a
// safety backup of the namespace as it stands, stops the writers, deletes the
// namespace and restores it. Mode 2 (restore_volumes.go) does the node-loss
// path: one workload's stranded claims, refilled in place while the volumes of
// the same workload that are still fine keep their newer data.
//
// THREE RULES SHAPE EVERYTHING HERE.
//
//   - Restoring on top of live objects is destructive in a way the
//     scratch-namespace drill is not, so the plan is printed and confirmed
//     before the first side effect, and the identities the plan was built
//     against (the namespace UID and every claim's UID) are re-read immediately
//     before the destructive step. Kubernetes timestamps are never treated as
//     proof that a volume holds no newer data.
//   - The operation record (pkg/operation) is the lock. It is written before
//     the first change, it is what a second laptop's `--resume` reads, and it
//     is what holds the identities, the prior replica counts and the suspend
//     value the backup had for each CronJob this restore suspended.
//   - A non-Completed Velero Restore is never reported as done, and Velero's
//     own "Completed with 0 errors" is not proof that a volume was filled:
//     every PodVolumeRestore this restore created must be Completed, which is
//     the same assertion the operator's drill makes (op3/pkg/restoredrill).
//
// The reconcile hold, mirrored from op3/api/v1/project_types.go and
// op3/internal/controller/reconcile_hold.go. The CLI writes the annotation; the
// operator acknowledges it with the condition, and the CLI waits for that
// acknowledgement before its first destructive step (T4.2).
const (
	// PauseAnnotationKey is the annotation whose value is the operation id that
	// asked for the pause.
	PauseAnnotationKey = "kubenest.io/reconcile-paused"
	// ActivateAnnotationKey releases a project that recovery mode holds. A
	// restore's own pause is released by REMOVING PauseAnnotationKey.
	ActivateAnnotationKey = "kubenest.io/reconcile-activated"
	// ProjectCRNamespace is where every Project CR lives, which is NOT the
	// project's own namespace — that is what lets the pause outlive the
	// namespace's deletion.
	ProjectCRNamespace = "kubenest-system"
	// ConditionReconcilePaused is the condition the operator sets while a pause
	// holds the project.
	ConditionReconcilePaused = "ReconcilePaused"
	// ReasonPausedByOperation names the operation id in PauseAnnotationKey.
	ReasonPausedByOperation = "PausedByOperation"
)

// MinOperatorPauseVersion is the first kubenest-operator-2 chart whose operator
// acknowledges the reconcile pause, and therefore the first an operator must be
// running for this command to hold a project at all.
//
// The pause and the condition the CLI waits for arrived in op3 418280a (T4.2,
// plan 7.5), and the first chart carrying them is 2.7.0-rc.1, which bundle 1.2
// pins. No older operator will EVER acknowledge, so a restore against one wrote
// the pause, waited the bundle's whole acknowledgement deadline and failed,
// having destroyed nothing and learned nothing — measured on the demo cluster
// (bundle 1.1, operator chart 2.6.17, 2026-09-27, kn-x0wv.6).
//
// THE OPERATOR'S OWN VERSION IS THE SIGNAL, NOT THE BUNDLE'S. Lab w3 runs
// bundle 1.1 with the 2.7.0-rc.1 operator installed by hand and restores
// correctly, and a cluster can run an operator newer than the one its bundle
// pinned; only the chart the operator reports says what can be waited for.
//
// It is a MINIMUM and a pre-release, so it sorts below 2.7.0 (semver §11): a
// 2.7.0-rc.1 cluster proceeds, and so does a 2.7.0 one.
const MinOperatorPauseVersion = "2.7.0-rc.1"

// minOperatorPause is MinOperatorPauseVersion parsed once. A constant this
// build cannot parse is an error here and not a condition of the cluster, so it
// is raised where it is made rather than on a customer's cluster.
var minOperatorPause = mustChartVersion(MinOperatorPauseVersion)

func mustChartVersion(raw string) chartVersion {
	version, err := parseChartVersion(raw)
	if err != nil {
		panic(fmt.Sprintf("MinOperatorPauseVersion %q is not a version: %v", raw, err))
	}
	return version
}

// chartVersion is a chart version: the three numbers and the pre-release
// identifiers semver orders by (§11). Build metadata is parsed and dropped,
// which is what the spec says it is for.
type chartVersion struct {
	major, minor, patch int
	pre                 []string
}

// operatorChartVersion reads the version out of a helm.sh/chart label, which
// Helm writes as "<chart name>-<chart version>".
//
// THE NAME IS MATCHED, NOT SEARCHED FOR. The operator chart's name carries a
// dash and a digit of its own ("kubenest-operator-2") and a version's
// pre-release carries dashes ("2.7.0-rc.1"), so "the first dash whose remainder
// parses as a version" would read a label from a DIFFERENT chart as a version
// of this one. The prefix is the one place the chart's name is spelled for a
// reader, in pkg/component/agent, beside the Deployment it also names.
func operatorChartVersion(label string) (chartVersion, string, error) {
	text, found := strings.CutPrefix(label, agent.ChartName+"-")
	if !found {
		return chartVersion{}, "", fmt.Errorf("the label does not name the operator chart %s", agent.ChartName)
	}
	version, err := parseChartVersion(text)
	if err != nil {
		return chartVersion{}, text, err
	}
	return version, text, nil
}

// parseChartVersion reads a chart version: major.minor.patch, an optional
// pre-release, and build metadata that is parsed and ignored. A leading "v" is
// accepted because Helm accepts one and normalizes it away, and the numbers are
// read as the integers they are, so a padded "2.07.0" is the version it says.
// Everything else — a missing component, a letter where a number belongs, an
// empty pre-release identifier — is refused rather than guessed at: this gate
// decides whether a cluster can be held, and a version it cannot read is not a
// version it may treat as new enough.
func parseChartVersion(raw string) (chartVersion, error) {
	text := strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V")
	text, _, _ = strings.Cut(text, "+")
	numbers, pre, hasPre := strings.Cut(text, "-")
	parts := strings.Split(numbers, ".")
	if len(parts) != 3 {
		return chartVersion{}, fmt.Errorf("%q is not major.minor.patch", raw)
	}
	version := chartVersion{}
	fields := []*int{&version.major, &version.minor, &version.patch}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return chartVersion{}, fmt.Errorf("%q has %q where a number belongs", raw, part)
		}
		*fields[i] = value
	}
	if !hasPre {
		return version, nil
	}
	if pre == "" {
		return chartVersion{}, fmt.Errorf("%q marks a pre-release and names none", raw)
	}
	version.pre = strings.Split(pre, ".")
	for _, identifier := range version.pre {
		if identifier == "" || strings.Trim(identifier, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
			return chartVersion{}, fmt.Errorf("%q has %q where a pre-release identifier belongs", raw, identifier)
		}
	}
	return version, nil
}

// compareChartVersions orders two versions the way semver does.
//
// THE ONE RULE THAT MATTERS FOR THIS GATE is §11's: a pre-release is LOWER than
// the release it precedes, so 2.7.0-rc.1 is older than 2.7.0 — which is what
// makes a minimum of "2.7.0-rc.1" accept both that release candidate and the
// release itself, while refusing 2.7.0-rc.0 and everything below it.
//
// Numeric identifiers compare numerically (2.7.0-rc.10 is NEWER than
// 2.7.0-rc.9, not older), and a numeric identifier is lower than an
// alphanumeric one.
func compareChartVersions(left, right chartVersion) int {
	if c := compareInts(left.major, right.major); c != 0 {
		return c
	}
	if c := compareInts(left.minor, right.minor); c != 0 {
		return c
	}
	if c := compareInts(left.patch, right.patch); c != 0 {
		return c
	}
	// A PRE-RELEASE IS LOWER THAN THE RELEASE IT PRECEDES, so an empty
	// pre-release outranks any list of identifiers — this is the comparison the
	// gate turns on, and getting it backwards would refuse the release that
	// fixes the bug.
	switch {
	case len(left.pre) == 0 && len(right.pre) == 0:
		return 0
	case len(left.pre) == 0:
		return 1
	case len(right.pre) == 0:
		return -1
	}
	for i := 0; i < len(left.pre) && i < len(right.pre); i++ {
		leftIdentifier, rightIdentifier := left.pre[i], right.pre[i]
		if leftIdentifier == rightIdentifier {
			continue
		}
		leftNumber, leftErr := strconv.Atoi(leftIdentifier)
		rightNumber, rightErr := strconv.Atoi(rightIdentifier)
		switch {
		case leftErr == nil && rightErr == nil:
			return compareInts(leftNumber, rightNumber)
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		default:
			return strings.Compare(leftIdentifier, rightIdentifier)
		}
	}
	// The shared identifiers are equal, so the SHORTER list is lower
	// (2.7.0-rc.1 precedes 2.7.0-rc.1.1).
	return compareInts(len(left.pre), len(right.pre))
}

func compareInts(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	}
	return 0
}

// requirePauseCapableOperator refuses a restore whose operator cannot
// acknowledge the reconcile pause, BEFORE the pause is written.
//
// IT IS A READ WITH NO SIDE EFFECT, and it runs on every run that reaches for
// the hold — mode 1's and mode 2's (both call ensurePause, restore_volumes.go),
// and a resume, which must not re-wait on an operator that was downgraded since
// the interruption.
func (r *restoreRun) requirePauseCapableOperator(ctx context.Context) error {
	label, err := r.deps.Cluster.OperatorChart(ctx)
	if err != nil {
		return err
	}
	if label == "" {
		return pauseUnsupported(fmt.Sprintf("the operator Deployment %s/%s is not on this cluster, or carries no helm.sh/chart label, so this restore cannot tell which operator version is running", ProjectCRNamespace, agent.DeploymentName))
	}
	version, text, err := operatorChartVersion(label)
	if err != nil {
		return pauseUnsupported(fmt.Sprintf("the operator Deployment %s/%s reports %q, which names no operator chart version this command can read (%v)", ProjectCRNamespace, agent.DeploymentName, label, err))
	}
	if compareChartVersions(version, minOperatorPause) < 0 {
		return pauseUnsupported(fmt.Sprintf("the operator running here is chart %s (version %s), older than the %s this restore needs", label, text, MinOperatorPauseVersion))
	}
	fmt.Fprintf(r.out, "  operator:      %s acknowledges the reconcile pause\n", label)
	return nil
}

// pauseUnsupported builds the one refusal for every way an operator can fail to
// support the pause, with what was found in front of it: the version found, the
// version needed, that nothing was changed, and the path that fixes it.
//
// THE UPGRADE PATH IS PROSE, NOT A COPYABLE COMMAND LINE. The bundle whose
// operator carries the pause may not be the one a reader can install yet, and a
// command line naming an unreleased version is a command that fails when it is
// pasted.
func pauseUnsupported(found string) error {
	return fmt.Errorf("%s. The reconcile pause this restore needs — the operator's own %s condition, which holds reconciliation for the few minutes a restore takes — first ships in operator chart %s, the one bundle 1.2 pins: an operator older than that never writes the condition, so the restore would write the pause, wait out the acknowledgement deadline and fail, with the namespace untouched. Nothing has been changed. The path is `kubenest platform upgrade`, which moves this cluster to a bundle whose operator chart carries the pause; run the restore again afterwards",
		found, ConditionReconcilePaused, MinOperatorPauseVersion)
}

// The restore's own labels and constants.
const (
	// SafetyBackupTTL is how long the pre-restore safety backup is kept (plan
	// 7.5: "kept for 7 days").
	SafetyBackupTTL = 168 * time.Hour
	// SafetyBackupPurposeLabel marks the backup this command takes before it
	// destroys anything, so an operator can tell it from a scheduled one.
	SafetyBackupPurposeLabel = "kubenest.io/purpose"
	// SafetyBackupPurpose is the value of that label.
	SafetyBackupPurpose = "safety-backup"
	// OperationIDLabel records the operation a produced object belongs to.
	OperationIDLabel = "kubenest.io/operation-id"
	// CronJobSuspendedAnnotationKey records, on a restored CronJob, whether
	// spec.suspend was true in the BACKUP. A restore's resource modifier writes
	// it while it suspends the CronJob, because by the time anything else can
	// read the object the object is already suspended and the backup's own
	// value is gone (see cronJobModifierDocument).
	CronJobSuspendedAnnotationKey = "kubenest.io/restore-cronjob-was-suspended"
	// RestoredStage is the stage the record holds while the data is back and
	// nothing is allowed to run yet.
	RestoredStage = "restored — awaiting activation"
	// StopCronJobWrite and StopJobWrite are the record kinds naming the work
	// the stop step did on one object of the namespace as it stood. They are
	// written ALREADY DISCHARGED (operation.WriteDone): they answer "what did
	// the stop step do", and they are not work activation owes.
	StopCronJobWrite = "stop-cronjob"
	StopJobWrite     = "stop-job"
	// StoppedCronJobTarget and StoppedJobTarget are where those record entries
	// point. Each is its own target — never "cronjob/<name>" or "job/<name>" —
	// so a stop-step entry can never be read back as the value activation puts
	// a restored object back to: those come from the backup, in kn-x0wv.3's
	// annotation.
	StoppedCronJobTarget = "cronjob-stop/"
	StoppedJobTarget     = "job-stop/"
)

// The marker the operator's restore drill puts on ITS Velero Restore. Both
// literals are mirrored from op3/pkg/restoredrill/result.go
// (DrillRestoreLabelKey / DrillRestoreLabelValue / DrillRestoreNamePrefix):
// the CLI and the operator are separate Go modules, so they cannot share them.
//
// A drill holds Velero's only restore worker — the first run of probe P5 lost
// its node while a drill was restoring onto it and every later restore queued
// behind that drill for ever — so mode 2 refuses to start while one is in
// progress and says how to free the worker.
const (
	DrillRestoreLabelKey   = "kubenest.io/restore-drill"
	DrillRestoreLabelValue = "scratch"
	DrillRestoreNamePrefix = "kubenest-restore-drill-"
)

// The Velero Restore phases this command treats as terminal. The same set the
// operator's drill waits out (op3/pkg/restoredrill/runner.go).
var restoreTerminalPhases = []string{"Completed", "Failed", "PartiallyFailed", "FailedValidation"}

// restorePhaseTerminal reports whether a Velero Restore phase is one a run
// stops on. It is one function because two places reason about it — the drill
// in progress and this run's own restore — and they must not disagree.
func restorePhaseTerminal(phase string) bool {
	for _, terminal := range restoreTerminalPhases {
		if phase == terminal {
			return true
		}
	}
	return false
}

// ErrNoBackup is a lookup for a backup the cluster does not have.
var ErrNoBackup = errors.New("no such backup")

// VolumeRef names one claim a backup was expected to cover, or one it did not
// cover. The UID is the identity that matters: a claim deleted and recreated
// under the same name is a different volume.
type VolumeRef struct {
	Namespace string
	Name      string
	UID       string
	Detail    string
}

// BackupFacts is one Velero Backup as this command judges it, scoped to the
// namespace the restore is about.
//
// The judgement is T2.10's, mirrored from the operator's producer: eligibility
// is `completed && missing == [] && failed == []`, plus the two facts that look
// like completion but are not — a backup whose expected set was never recorded
// cannot be shown to cover anything, and a backup whose storage location is not
// Available is not off-cluster. See op3/pkg/restoredrill/backup.go.
type BackupFacts struct {
	Name                 string
	Phase                string
	CaptureStartedAt     string
	CompletedAt          string
	StorageLocation      string
	StorageLocationPhase string
	ConsistencyMethod    string
	Consistency          string
	// Expected is the recorded expected set for this namespace, Missing are its
	// volumes with no copy record at all, and Failed those whose copy is not
	// successful. A volume in the expected set with no copy is MISSING, never
	// "nothing to copy".
	Expected        []VolumeRef
	Missing         []VolumeRef
	Failed          []VolumeRef
	CoverageUnknown bool
}

// Completed reports whether Velero reached the Completed phase.
func (b BackupFacts) Completed() bool { return b.Phase == "Completed" }

// IneligibleReason names the one piece that makes this backup unusable for the
// namespace, in the operator's own vocabulary, or "" when it is eligible.
func (b BackupFacts) IneligibleReason() string {
	switch {
	case !b.Completed():
		return "not-completed"
	case b.CoverageUnknown:
		return "coverage-unknown"
	case len(b.Missing) > 0 || len(b.Failed) > 0:
		return "incomplete-coverage"
	case b.StorageLocationPhase != "Available":
		return "storage-location-unavailable"
	}
	return ""
}

// Age is the CONSERVATIVE data age: measured from the backup's capture start
// and never from its completion, because a backup that ran for hours may hold
// files copied near its beginning and completion would understate the loss by
// the whole duration of the backup.
func (b BackupFacts) Age(now time.Time) (time.Duration, bool) {
	started, err := time.Parse(time.RFC3339, b.CaptureStartedAt)
	if err != nil {
		return 0, false
	}
	age := now.Sub(started)
	if age < 0 {
		age = 0
	}
	return age, true
}

// BackupSource answers what backups the cluster holds, judged for one
// namespace. The command fills it from Velero over `k3s kubectl`; tests fill it
// with facts they choose, which is why the plan's refusals are testable with no
// cluster.
type BackupSource interface {
	// TerminalBackups lists every terminal Velero Backup, newest first.
	TerminalBackups(ctx context.Context, namespace string) ([]BackupFacts, error)
	// NamedBackup reads one backup by name. It returns ErrNoBackup when the
	// cluster has none by that name.
	NamedBackup(ctx context.Context, namespace, name string) (BackupFacts, error)
}

// RecoveryPointPolicy is the age the bundle lets a recovery point reach before
// the restore needs the operator to accept that age explicitly.
type RecoveryPointPolicy struct {
	Max time.Duration
	// Source names the manifest key the threshold came from, so the plan can
	// say which promise is being judged.
	Source string
}

// RecoveryPointPolicyFor reads the bundle's recovery-point policy: T2.0's
// `health.backup.recovery-point-age` when the bundle declares it, and
// otherwise the same bundle's `health.backup.max-backup-age` — the age at which
// this bundle already says a recovery point is stale, which is what the reboot
// gate uses. Bundle 1.0 and 1.1 predate the dedicated key; a number invented in
// this binary is exactly what the manifest rule forbids.
func RecoveryPointPolicyFor(bundle *manifest.Manifest) (RecoveryPointPolicy, error) {
	if bundle == nil {
		return RecoveryPointPolicy{}, fmt.Errorf("no bundle manifest: the recovery-point policy is the bundle's, never a default in this binary")
	}
	if age := bundle.Health.Backup.RecoveryPointAge.Duration(); age > 0 {
		return RecoveryPointPolicy{Max: age, Source: "health.backup.recovery-point-age"}, nil
	}
	if age := bundle.Health.Backup.MaxBackupAge.Duration(); age > 0 {
		return RecoveryPointPolicy{
			Max:    age,
			Source: "health.backup.max-backup-age (this bundle declares no health.backup.recovery-point-age)",
		}, nil
	}
	return RecoveryPointPolicy{}, fmt.Errorf("bundle %s declares neither health.backup.recovery-point-age nor health.backup.max-backup-age, so the age a recovery point may reach cannot be judged", bundle.Bundle)
}

// RestoreOptions is one `kubenest backup restore` run: the flags, plus the
// manifest and the confirmation path.
type RestoreOptions struct {
	// Cluster is the cluster's name, as recorded at install. It is what the
	// record and every message name.
	Cluster string
	// Namespace is the mode selector: always required.
	Namespace string
	// From names one backup; Latest picks the newest eligible one.
	From   string
	Latest bool
	// Replace allows restoring over a namespace that exists. An absent
	// namespace needs no --replace.
	Replace bool
	// PVCs selects mode 2 and the stranded claims to refill.
	PVCs []string
	// IncludeJobs restores Jobs deliberately (they run at activation). Mode 1
	// only.
	IncludeJobs bool
	// Resume, Activate and Abort are the three follow-on commands. They are
	// mutually exclusive and take no other mode flags.
	Resume   string
	Activate string
	Abort    string
	// TakeOver is the fourth way to take up an interrupted restore: it claims
	// a record whose executor is still `running` on the operator's assertion
	// that the previous executor and its outstanding actions have stopped
	// (PLAN 7.2, kn-yzuv). It needs Confirm, and it reconciles exactly as a
	// resume does.
	TakeOver string
	// AcceptDataAge is the operator's explicit acceptance of a backup older
	// than the bundle's recovery-point policy.
	AcceptDataAge bool
	// Confirm is the non-interactive path, matching `kubenest platform
	// restore --confirm`.
	Confirm bool
	// KeepRestored / KeepDesired answer activation's one question — which side
	// wins when the restored workload configuration differs from the current
	// desired state — without a prompt.
	KeepRestored bool
	KeepDesired  bool
	// Bundle is the cluster's bundle manifest.
	Bundle *manifest.Manifest
}

// recovery is the two recovery flags as pkg/operation's Recovery, where their
// rules live: mutually exclusive, and --take-over requires --confirm. It is
// empty for a new restore and for a follow-on that acts on a record without
// taking it up (--activate, --abort).
//
// --confirm means the same thing here as it does for a new restore: the
// operator affirming the thing this run is about, which for --take-over is that
// the previous executor and its outstanding actions have stopped.
func (o RestoreOptions) recovery() operation.Recovery {
	return operation.Recovery{Resume: o.Resume, TakeOver: o.TakeOver, Confirm: o.Confirm}
}

// Mode names what this run will do, for messages and for the record's version
// map. A take-over is a resume with the operator's assertion behind it: it
// continues the recorded request, so it names the same mode.
func (o RestoreOptions) Mode() string {
	switch {
	case len(o.PVCs) > 0:
		return "volumes"
	case o.Activate != "":
		return "activate"
	case o.Abort != "":
		return "abort"
	case o.Resume != "" || o.TakeOver != "":
		return "resume"
	default:
		return "namespace"
	}
}

// RestoreDeps is what a restore run needs from outside itself. Every field is
// an interface so a test can drive the whole run with no cluster: the plan's
// refusals, the identity re-verification and the PartiallyFailed verdict are
// all decided in this package.
type RestoreDeps struct {
	// Cluster is every read and change the run performs on the cluster.
	Cluster RestoreCluster
	// Backups is the eligibility source.
	Backups BackupSource
	// Store is the operation record, built by the caller from the same SSH
	// transport. It is what makes this run resumable.
	Store *operation.Store
	// Runner is the raw transport the recorded actions are submitted over. The
	// run decorates it with pkg/operation's Guarded for the stages that change
	// the cluster, so every action is written down before it is submitted.
	Runner k3s.Runner
	// Now, Poll and Sleep are the run's clock, so a test measures a deadline
	// instead of enduring one.
	Now   func() time.Time
	Poll  time.Duration
	Sleep func(ctx context.Context, d time.Duration) error
}

// RunRestore is the whole verb. It dispatches to the three follow-on commands
// first, because `--activate`/`--abort`/`--resume` act on a record rather than
// on a new plan.
func RunRestore(ctx context.Context, out io.Writer, in io.Reader, opts RestoreOptions, deps RestoreDeps) error {
	r := &restoreRun{out: out, in: in, opts: opts, deps: deps}
	if r.deps.Now == nil {
		r.deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if r.deps.Poll == 0 {
		r.deps.Poll = 5 * time.Second
	}
	if r.deps.Sleep == nil {
		r.deps.Sleep = sleepCtx
	}
	switch {
	case opts.Activate != "":
		return r.activate(ctx)
	case opts.Abort != "":
		return r.abort(ctx)
	}
	if len(opts.PVCs) > 0 {
		return r.runVolumeRestore(ctx)
	}
	return r.runNamespaceRestore(ctx)
}

// restoreRun is one run: the flags, the seams, and the resolved bundle facts.
type restoreRun struct {
	out  io.Writer
	in   io.Reader
	opts RestoreOptions
	deps RestoreDeps

	policy RecoveryPointPolicy
	// plan is mode 1's plan; volumePlan is mode 2's. Exactly one of them is set.
	plan       *restorePlan
	volumePlan *volumePlan

	handle *operation.Handle
	// skip names the recorded actions a resume established, so Guarded does not
	// submit them again.
	skip map[string]bool
	// skippedStages is the same decision read by STAGE rather than by action
	// id. A step whose stage is here is done and must not be waited for again:
	// the deletion of a namespace is the one postcondition that can be undone
	// under the operation (the reconcilers recreate the namespace), so
	// "wait until it is gone" is not a fact a resume can re-establish.
	skippedStages map[string]bool
	// backupErrTimeout is the deadline for the safety backup's settle wait,
	// from limits.timeouts.backup.
	backupErrTimeout time.Duration
	// restoreTimeout is the deadline for the Velero Restore, from
	// limits.timeouts.restore-drill (or a dedicated `restore` timeout when a
	// later bundle declares one).
	restoreTimeout time.Duration
}

func sleepCtx(ctx context.Context, d time.Duration) error {
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

// timeout reads one deadline from the bundle's limits.timeouts. It refuses to
// invent one: every wait's deadline is the bundle's, and a missing one is an
// error rather than a default in this binary.
func (r *restoreRun) timeout(name string) (time.Duration, error) {
	if r.opts.Bundle == nil {
		return 0, fmt.Errorf("no bundle manifest")
	}
	d, err := r.opts.Bundle.Limits.Timeouts.For(name)
	if err != nil {
		return 0, err
	}
	return d, nil
}

// restoreTimeoutFor reads the deadline the Velero Restore is waited within. The
// bundle has no dedicated `restore` timeout today (platform-1.1.yaml names
// limits.timeouts.restore-drill); a later bundle that adds one is read here
// rather than shadowed by a constant.
func (r *restoreRun) restoreTimeoutFor() (time.Duration, error) {
	if r.opts.Bundle != nil {
		if d, ok := r.opts.Bundle.Limits.Timeouts["restore"]; ok && d > 0 {
			return d, nil
		}
	}
	return r.timeout("restore-drill")
}

// runNamespaceRestore is mode 1.
//
// A RESUME PLANS FROM THE RECORD, not from the operator's flags: the request it
// continues is immutable, so the backup and the identities are the ones the
// interrupted run wrote down, and there is nothing new to choose or confirm.
func (r *restoreRun) runNamespaceRestore(ctx context.Context) error {
	if err := r.resolve(ctx); err != nil {
		return err
	}
	if r.opts.recovery().ID() == "" {
		if err := r.chooseBackup(ctx); err != nil {
			return err
		}
		if err := r.buildPlan(ctx); err != nil {
			return err
		}
		r.renderPlan()
		if !r.opts.Confirm {
			ok, err := r.prompt()
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("nothing has been changed: the restore was not confirmed")
			}
		}
	}
	if err := r.openRecord(ctx); err != nil {
		return err
	}
	runErr := r.execute(ctx)
	r.release(ctx, runErr)
	return runErr
}

// resolve reads the bundle facts every run needs: the policy threshold and the
// two deadlines.
func (r *restoreRun) resolve(ctx context.Context) error {
	policy, err := RecoveryPointPolicyFor(r.opts.Bundle)
	if err != nil {
		return err
	}
	r.policy = policy
	if r.backupErrTimeout, err = r.timeout("backup"); err != nil {
		return err
	}
	if r.restoreTimeout, err = r.restoreTimeoutFor(); err != nil {
		return err
	}
	return nil
}

// Backups returns every terminal backup, newest first, or an error naming the
// read that failed.
func (r *restoreRun) backups(ctx context.Context) ([]BackupFacts, error) {
	list, err := r.deps.Backups.TerminalBackups(ctx, r.opts.Namespace)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool {
		return backupCompletedAt(list[i]).After(backupCompletedAt(list[j]))
	})
	return list, nil
}

func backupCompletedAt(b BackupFacts) time.Time {
	at, err := time.Parse(time.RFC3339, b.CompletedAt)
	if err != nil {
		return time.Time{}
	}
	return at
}

// restorePlan is everything the operator is shown, and the identities the run
// re-checks before it destroys anything.
type restorePlan struct {
	Namespace string
	Backup    BackupFacts
	// Skipped names the newer backups --latest passed over, with the reason, so
	// "the latest" is never silently "an older one".
	Skipped []string
	// NamespaceUID is the live namespace's UID, or "" when it does not exist.
	NamespaceUID string
	Exists       bool
	Claims       []VolumeRef
	Workloads    []WorkloadState
	CronJobs     []CronJobState
	Applications []ApplicationState
	// Discard names what will be lost when the namespace goes.
	Discard  []string
	Age      time.Duration
	AgeKnown bool
	Policy   RecoveryPointPolicy
	// AgeAccepted reports whether the operator explicitly accepted a backup
	// older than the policy.
	AgeAccepted bool
	Replace     bool
	// PVCs is mode 2's named set, and Mode names which mode the plan is for.
	PVCs []string
	Mode string
}

// chooseBackup picks the backup the run will restore, named or latest eligible.
func (r *restoreRun) chooseBackup(ctx context.Context) error {
	if r.opts.From != "" && r.opts.Latest {
		return fmt.Errorf("--from and --latest are mutually exclusive: name the backup or let the command pick the newest eligible one")
	}
	plan := &restorePlan{Namespace: r.opts.Namespace, Mode: r.opts.Mode(), PVCs: append([]string(nil), r.opts.PVCs...)}
	r.plan = plan

	switch {
	case r.opts.From != "":
		facts, err := r.deps.Backups.NamedBackup(ctx, r.opts.Namespace, r.opts.From)
		if err != nil {
			if errors.Is(err, ErrNoBackup) {
				return fmt.Errorf("backup %s is not on this cluster: nothing has been changed", r.opts.From)
			}
			return err
		}
		if reason := facts.IneligibleReason(); reason != "" {
			// NOTHING IS TOUCHED, and the refusal names the piece: the operator
			// chose this backup, so "it is not eligible" is not an answer.
			return fmt.Errorf("backup %s cannot restore namespace %s: %s. Nothing has been changed", facts.Name, r.opts.Namespace, describeIneligible(facts, reason))
		}
		plan.Backup = facts
	case r.opts.Latest:
		list, err := r.backups(ctx)
		if err != nil {
			return err
		}
		var chosen *BackupFacts
		for i := range list {
			if list[i].IneligibleReason() == "" {
				chosen = &list[i]
				break
			}
		}
		if chosen == nil {
			if len(list) == 0 {
				return fmt.Errorf("this cluster has no terminal backup for namespace %s: nothing has been changed. Take one with `kubenest backup now`", r.opts.Namespace)
			}
			return fmt.Errorf("no backup can restore namespace %s: every terminal backup is ineligible (%s). Nothing has been changed", r.opts.Namespace, ineligibleSummary(list))
		}
		for i := range list {
			if list[i].Name == chosen.Name {
				break
			}
			plan.Skipped = append(plan.Skipped, fmt.Sprintf("%s: %s", list[i].Name, describeIneligible(list[i], list[i].IneligibleReason())))
		}
		plan.Backup = *chosen
	default:
		return fmt.Errorf("choose a backup: pass --from NAME or --latest")
	}

	age, known := plan.Backup.Age(r.deps.Now())
	plan.Age, plan.AgeKnown = age, known
	return nil
}

// buildPlan reads the live state the plan is about, and applies the two
// pre-flight refusals that must happen BEFORE the safety backup: an over-policy
// data age without --accept-data-age, and restoring over an existing namespace
// without --replace.
//
// THE OPERATOR'S PAUSE CAPABILITY IS A PRE-FLIGHT REFUSAL TOO, and the first
// one: a cluster whose operator cannot acknowledge the pause has no plan worth
// reading, and refusing HERE — before the operation record is taken — is what
// keeps a refusal from leaving a stopped record that blocks the cluster's next
// operation until someone aborts it (measured on prod-2, kn-x0wv.6).
func (r *restoreRun) buildPlan(ctx context.Context) error {
	if err := r.requirePauseCapableOperator(ctx); err != nil {
		return err
	}
	plan := r.plan
	plan.Policy = r.policy
	plan.AgeAccepted = r.opts.AcceptDataAge

	state, err := r.deps.Cluster.Namespace(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	if state != nil {
		plan.Exists = true
		plan.NamespaceUID = state.UID
	}
	claims, err := r.deps.Cluster.Claims(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	plan.Claims = claims
	workloads, err := r.deps.Cluster.Workloads(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	plan.Workloads = workloads
	cronjobs, err := r.deps.Cluster.CronJobs(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	plan.CronJobs = cronjobs
	apps, err := r.deps.Cluster.Applications(ctx, plan.Namespace)
	if err != nil {
		return err
	}
	plan.Applications = apps

	// What will be discarded: the objects the namespace holds now. A namespace
	// that does not exist discards nothing; the reconcilers may have already
	// recreated it empty, which is the ordinary case (plan 7.5, mode 1).
	if plan.Exists {
		plan.Discard = append(plan.Discard, fmt.Sprintf("namespace %s (uid %s)", plan.Namespace, plan.NamespaceUID))
		for _, claim := range plan.Claims {
			plan.Discard = append(plan.Discard, fmt.Sprintf("persistentvolumeclaim %s/%s (uid %s)%s", claim.Namespace, claim.Name, claim.UID, claimDetail(claim)))
		}
		for _, w := range plan.Workloads {
			plan.Discard = append(plan.Discard, fmt.Sprintf("%s %s (replicas %d)", w.Kind, w.Name, w.Replicas))
		}
	}

	if !plan.Exists && !r.opts.Replace {
		// An absent namespace needs no --replace, and saying so beats a silent
		// success the operator cannot explain.
		fmt.Fprintf(r.out, "namespace %s does not exist, so no --replace is needed: the restore recreates it\n", plan.Namespace)
	}
	if plan.Exists && !r.opts.Replace {
		return fmt.Errorf("namespace %s exists (uid %s), and restoring over live objects discards what is in it: pass --replace to restore it anyway. Nothing has been changed", plan.Namespace, plan.NamespaceUID)
	}
	if !plan.AgeKnown {
		return fmt.Errorf("backup %s records no capture start, so the age of the data it holds is unknown and cannot be judged against the %s this bundle allows. Nothing has been changed", plan.Backup.Name, plan.Policy.Max)
	}
	if plan.Age > plan.Policy.Max && !plan.AgeAccepted {
		// REFUSED, AND THE BEST AVAILABLE DATA IS STILL PRINTED: the operator
		// has to be able to decide, and "older than policy" without the age and
		// the alternative is not a decision anyone can make.
		r.renderPlan()
		return fmt.Errorf("backup %s holds data %s old (measured from its capture start %s), older than the %s this bundle allows (%s): pass --accept-data-age to restore it anyway. Nothing has been changed",
			plan.Backup.Name, plan.Age.Round(time.Minute), plan.Backup.CaptureStartedAt, plan.Policy.Max, plan.Policy.Source)
	}
	return nil
}

// renderPlan prints what the run will do and what it will destroy, before any
// side effect.
func (r *restoreRun) renderPlan() {
	if r.plan == nil {
		r.renderVolumePlan()
		return
	}
	p := r.plan
	if p == nil {
		return
	}
	fmt.Fprintf(r.out, "Restore plan for namespace %s of %s.\n", p.Namespace, r.opts.Cluster)
	fmt.Fprintf(r.out, "  backup:        %s\n", p.Backup.Name)
	fmt.Fprintf(r.out, "  completed:     %s\n", orUnknown(p.Backup.CompletedAt))
	if p.AgeKnown {
		fmt.Fprintf(r.out, "  data age:      %s, measured from the capture start %s (a long backup may hold files copied near its beginning)\n",
			p.Age.Round(time.Minute), p.Backup.CaptureStartedAt)
	} else {
		fmt.Fprintf(r.out, "  data age:      UNKNOWN: the backup records no capture start\n")
	}
	fmt.Fprintf(r.out, "  policy:        %s, from %s\n", p.Policy.Max, p.Policy.Source)
	if p.AgeAccepted {
		fmt.Fprintf(r.out, "  data age:      accepted explicitly by --accept-data-age\n")
	}
	fmt.Fprintf(r.out, "  coverage:      %s\n", describeCoverage(p.Backup))
	fmt.Fprintf(r.out, "  consistency:   %s\n", describeConsistency(p.Backup))
	fmt.Fprintf(r.out, "  storage:       %s (%s)\n", orUnknown(p.Backup.StorageLocation), orUnknown(p.Backup.StorageLocationPhase))
	if p.Exists {
		fmt.Fprintf(r.out, "  namespace:     exists, uid %s\n", p.NamespaceUID)
	} else {
		fmt.Fprintf(r.out, "  namespace:     does not exist; the restore recreates it\n")
	}
	if len(p.Skipped) > 0 {
		fmt.Fprintf(r.out, "  --latest:      passed over %d newer backup(s):\n", len(p.Skipped))
		for _, s := range p.Skipped {
			fmt.Fprintf(r.out, "                   - %s\n", s)
		}
	}
	if len(p.Claims) == 0 {
		fmt.Fprintf(r.out, "  claims:        none\n")
	}
	for _, claim := range p.Claims {
		fmt.Fprintf(r.out, "  claim:         %s/%s (uid %s)%s\n", claim.Namespace, claim.Name, claim.UID, claimDetail(claim))
	}
	if len(p.Workloads) == 0 && p.Exists {
		fmt.Fprintf(r.out, "  workloads:     none\n")
	}
	for _, w := range p.Workloads {
		fmt.Fprintf(r.out, "  workload:      %s %s, %d replica(s)\n", w.Kind, w.Name, w.Replicas)
	}
	if len(p.CronJobs) > 0 {
		for _, c := range p.CronJobs {
			fmt.Fprintf(r.out, "  cronjob:       %s (%s), suspended=%t\n", c.Name, c.Schedule, c.Suspend)
		}
	}
	if len(p.Discard) > 0 {
		fmt.Fprintf(r.out, "  discarded:     what namespace %s holds now, after a safety backup:\n", p.Namespace)
		for _, d := range p.Discard {
			fmt.Fprintf(r.out, "                   - %s\n", d)
		}
	}
	if p.Mode == "volumes" {
		fmt.Fprintf(r.out, "  mode:          volumes — refilling %s in place; every other volume of the same workload keeps its current contents\n", strings.Join(p.PVCs, ", "))
	} else {
		fmt.Fprintf(r.out, "  mode:          namespace — the namespace is deleted and restored; its CronJobs and any Job still running are suspended as soon as the pause is written, Jobs are %s, and every CronJob the restore creates is suspended by the restore itself (a Velero resource modifier), so none can fire before activation\n", includeJobsWord(r.opts.IncludeJobs))
		fmt.Fprintf(r.out, "  job pods:      excluded (pods carrying %s), so the pod of a Job that was running when the backup was taken is not restored and its work does not run again before activation\n", JobPodLabelKey)
		fmt.Fprintf(r.out, "  restored pods: every ReplicaSet the backup holds at 0 replicas comes back with its pod-template-hash renamed, in the selector and the template both, so it cannot adopt and delete the pod the backup copied a volume for before that pod's PodVolumeRestore fills the volume (a backup taken mid-rollout). A ReplicaSet the backup holds with replicas keeps its selector and adopts its restored pod, so that pod is the workload's own; activation deletes only the restored pods no controller owns, and the workload's own controller creates the pods that take the claims over\n")
	}
}

func includeJobsWord(include bool) string {
	if include {
		return "restored (--include-jobs)"
	}
	return "excluded"
}

func claimDetail(claim VolumeRef) string {
	if claim.Detail == "" {
		return ""
	}
	return " — " + claim.Detail
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// describeCoverage states the coverage honestly: what is expected, what got no
// copy, and what copy failed.
func describeCoverage(b BackupFacts) string {
	switch {
	case b.CoverageUnknown:
		return "UNKNOWN: no expected-coverage record was written for this backup, so it cannot be shown to hold every volume it claimed"
	case len(b.Missing) == 0 && len(b.Failed) == 0:
		return fmt.Sprintf("complete: every one of the %d expected volume(s) has a successful copy", len(b.Expected))
	default:
		parts := []string{}
		for _, v := range b.Missing {
			parts = append(parts, fmt.Sprintf("%s/%s got no copy", v.Namespace, v.Name))
		}
		for _, v := range b.Failed {
			detail := v.Detail
			if detail == "" {
				detail = "the copy did not complete"
			}
			parts = append(parts, fmt.Sprintf("%s/%s: %s", v.Namespace, v.Name, detail))
		}
		return "INCOMPLETE: " + strings.Join(parts, "; ")
	}
}

func describeConsistency(b BackupFacts) string {
	method := b.ConsistencyMethod
	if method == "" {
		method = "unknown"
	}
	text := b.Consistency
	if text == "" {
		text = "not recorded"
	}
	return fmt.Sprintf("%s (%s)", method, text)
}

// describeIneligible names the specific missing piece for one backup, in the
// words an operator can act on.
func describeIneligible(b BackupFacts, reason string) string {
	switch reason {
	case "not-completed":
		return fmt.Sprintf("it reached phase %s, not Completed", orUnknown(b.Phase))
	case "coverage-unknown":
		return "no expected-coverage record was written for it, so it cannot be shown to hold the volumes it claimed"
	case "incomplete-coverage":
		parts := []string{}
		for _, v := range b.Missing {
			parts = append(parts, fmt.Sprintf("volume %s/%s has no copy record", v.Namespace, v.Name))
		}
		for _, v := range b.Failed {
			detail := v.Detail
			if detail == "" {
				detail = "its copy did not succeed"
			}
			parts = append(parts, fmt.Sprintf("volume %s/%s: %s", v.Namespace, v.Name, detail))
		}
		return strings.Join(parts, "; ")
	case "storage-location-unavailable":
		return fmt.Sprintf("its storage location %s is %s, so its data is not proven to be in the bucket", orUnknown(b.StorageLocation), orUnknown(b.StorageLocationPhase))
	}
	return reason
}

func ineligibleSummary(list []BackupFacts) string {
	parts := make([]string, 0, len(list))
	for _, b := range list {
		parts = append(parts, fmt.Sprintf("%s: %s", b.Name, describeIneligible(b, b.IneligibleReason())))
	}
	return strings.Join(parts, "; ")
}

// prompt asks for confirmation on stdin. Anything but an explicit yes is a
// refusal: an empty line is not consent to delete a namespace.
func (r *restoreRun) prompt() (bool, error) {
	fmt.Fprintf(r.out, "Type yes to restore %s into namespace %s, destroying what is there now: ", r.plan.Backup.Name, r.plan.Namespace)
	if r.in == nil {
		return false, fmt.Errorf("this run has no input to confirm on: pass --confirm for a non-interactive restore")
	}
	reader := bufio.NewReader(r.in)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}

// identityOf renders the identities a plan is pinned to, for the record and for
// the re-verification refusal.
func identityOf(plan *restorePlan) map[string]string {
	out := map[string]string{}
	if plan.Exists {
		out["namespace/"+plan.Namespace] = plan.NamespaceUID
	}
	for _, claim := range plan.Claims {
		out["persistentvolumeclaim/"+claim.Namespace+"/"+claim.Name] = claim.UID
	}
	for _, pvc := range plan.PVCs {
		key := "named-persistentvolumeclaim/" + plan.Namespace + "/" + pvc
		if _, ok := out[key]; !ok {
			out[key] = namedClaimUID(plan, pvc)
		}
	}
	return out
}

func namedClaimUID(plan *restorePlan, name string) string {
	for _, claim := range plan.Claims {
		if claim.Name == name {
			return claim.UID
		}
	}
	return ""
}

// sameIdentity compares a recorded identity set with a freshly read one and
// names every identity that moved.
//
// A key in `consumed` is not compared at all: it is an identity a COMPLETED
// step of this operation destroyed, so whatever holds that name now is the
// restore's own doing and not the change this check exists to catch. An
// interrupted restore, resumed after its delete, must not be refused by its own
// success — and the object the restore put back legitimately has a new UID,
// which is exactly why activation re-reads and prints it.
//
// A CONSUMED NAMESPACE CONSUMES WHAT WAS IN IT. Deleting a namespace destroys
// every claim in it, and mode 1 records that as the one `delete-namespace/<ns>`
// action, not as one action per claim. So a recorded claim under a namespace
// this operation deleted is an identity its own delete destroyed: the
// reconcilers recreating the namespace, and a claim in it under the recorded
// name, is the ordinary state after that step (S4 on lab w3, 2026-09-27) and
// not a moved identity. A claim the record never held is refused by name,
// separately, because that one is not the operation's own doing.
func sameIdentity(recorded, live map[string]string, consumed map[string]bool) []string {
	var moved []string
	keys := make([]string, 0, len(recorded))
	for k := range recorded {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if consumed[k] {
			continue
		}
		if namespace := identityNamespaceOf(k); namespace != "" && consumed["namespace/"+namespace] {
			continue
		}
		now := live[k]
		if now == "" {
			moved = append(moved, fmt.Sprintf("%s is gone (its UID was %s)", k, recorded[k]))
			continue
		}
		if now != recorded[k] {
			moved = append(moved, fmt.Sprintf("%s (uid %s) is now uid %s", k, recorded[k], now))
		}
	}
	return moved
}

// identityNamespaceOf names the namespace a recorded identity key belongs to,
// or "" for a key that names none.
func identityNamespaceOf(key string) string {
	if rest, ok := strings.CutPrefix(key, "namespace/"); ok {
		return rest
	}
	namespace, _ := claimIdentityOf(key)
	return namespace
}

// claimIdentityOf splits a recorded claim identity key into its namespace and
// claim name. Both shapes count: mode 1 records every claim in the namespace it
// is about, and mode 2 records the claims it was asked to refill.
func claimIdentityOf(key string) (namespace, name string) {
	for _, prefix := range []string{"persistentvolumeclaim/", "named-persistentvolumeclaim/"} {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		namespace, name, found := strings.Cut(rest, "/")
		if !found {
			return "", ""
		}
		return namespace, name
	}
	return "", ""
}

// consumedIdentities reads which identities a record says its own steps already
// destroyed: the namespace this operation deleted, and each claim it deleted.
func consumedIdentities(record *operation.Record, namespace string) map[string]bool {
	consumed := map[string]bool{}
	if record == nil {
		return consumed
	}
	for _, action := range record.Actions {
		if action.Status != operation.ActionSucceeded {
			continue
		}
		switch {
		case strings.HasPrefix(action.Stage, "delete-namespace/"):
			consumed["namespace/"+namespace] = true
		case strings.HasPrefix(action.Stage, "delete-claim/"):
			parts := strings.Split(strings.TrimPrefix(action.Stage, "delete-claim/"), "/")
			if len(parts) == 2 {
				consumed["persistentvolumeclaim/"+parts[0]+"/"+parts[1]] = true
				consumed["named-persistentvolumeclaim/"+parts[0]+"/"+parts[1]] = true
			}
		}
	}
	return consumed
}

// request is the operation's immutable identity: what was asked for, and the
// identities the plan was built against.
//
// THE IDENTITIES ARE ARTIFACTS, and that is what catches the planted negative
// this command is accepted on: an artifact whose digest changed underneath is a
// different operation wearing the same name, so a resume whose namespace or
// claim moved is refused by `Verify` rather than executed against the wrong
// objects. The backup name and the mode go in the version map, which is the
// free-form part of the request a resume also compares.
func (r *restoreRun) request() operation.Request {
	versions := map[string]string{
		"backup": r.chosenBackup(),
		"mode":   r.opts.Mode(),
	}
	if r.opts.Bundle != nil {
		versions["bundle"] = r.opts.Bundle.Bundle
	}
	artifacts := []operation.Artifact{}
	for name, uid := range r.planIdentities() {
		artifacts = append(artifacts, operation.Artifact{Name: name, Digest: uid})
	}
	return operation.Request{
		Kind:      r.recordKind(),
		Cluster:   r.opts.Cluster,
		Artifacts: artifacts,
		Versions:  versions,
	}
}

// chosenBackup names the backup the run is restoring from, in whichever mode
// planned it.
func (r *restoreRun) chosenBackup() string {
	switch {
	case r.plan != nil:
		return r.plan.Backup.Name
	case r.volumePlan != nil:
		return r.volumePlan.Backup.Name
	}
	return ""
}

// planIdentities is the identity set the record is pinned to: mode 1's
// namespace and claims, or mode 2's named claims.
func (r *restoreRun) planIdentities() map[string]string {
	if r.plan != nil {
		return identityOf(r.plan)
	}
	out := map[string]string{}
	if r.volumePlan != nil {
		for _, claim := range r.volumePlan.Claims {
			out["named-persistentvolumeclaim/"+claim.Namespace+"/"+claim.Name] = claim.UID
		}
	}
	return out
}

func (r *restoreRun) recordKind() operation.Kind {
	if len(r.opts.PVCs) > 0 {
		return operation.KindRestoreVolume
	}
	return operation.KindRestoreNamespace
}

// openRecord takes the operation lock — or takes up the record an interrupted
// run left behind, by resume or by the operator's take-over assertion — BEFORE
// the first change this run makes.
func (r *restoreRun) openRecord(ctx context.Context) error {
	if r.deps.Store == nil {
		return fmt.Errorf("a restore needs the operation record: it is the lock that keeps two operators from driving one cluster at once")
	}
	recovery := r.opts.recovery()
	if recovery.ID() == "" {
		handle, err := r.deps.Store.Acquire(ctx, r.request())
		if err != nil {
			return err
		}
		r.handle = handle
		fmt.Fprintf(r.out, "  record:        %s (resume with `kubenest backup restore --resume %s` if this is interrupted)\n", handle.OperationID(), handle.OperationID())
		return nil
	}
	return r.resumeRecord(ctx, recovery)
}

// resumeRecord reconciles an interrupted restore before repeating anything.
//
// The order matters and is the plan's (7.2): read the record, establish what
// happened from the recorded postconditions, verify that the request and the
// identities are still the ones this operation was planned against, and only
// then take the record up — by the resume's claim, or by the operator's
// take-over assertion when the record cannot say its executor stopped.
func (r *restoreRun) resumeRecord(ctx context.Context, recovery operation.Recovery) error {
	opID := recovery.ID()
	plan, err := operation.Resume(ctx, r.deps.Store, opID)
	if err != nil {
		return err
	}
	if plan.Terminal {
		return fmt.Errorf("operation %s is terminal (%s): there is nothing to resume", opID, plan.Record.Result)
	}
	if kind := plan.Record.Request.Kind; kind != operation.KindRestoreNamespace && kind != operation.KindRestoreVolume {
		return fmt.Errorf("operation %s is a %s, not a restore: it cannot be resumed by this command", opID, kind)
	}
	// THE OPERATOR HAS TO BE ABLE TO ANSWER the pause this operation already
	// wrote, AND THIS IS DECIDED BEFORE THE RECORD IS RE-CLAIMED: a resume whose
	// operator was downgraded leaves the stopped record exactly as it found it,
	// so the upgrade can resume the same operation id instead of finding a
	// record this refusal moved (kn-x0wv.6). Nothing below this line writes.
	if err := r.requirePauseCapableOperator(ctx); err != nil {
		return err
	}
	// THE RECORD FILLS IN WHAT THE FLAGS CANNOT. `--resume` takes no
	// `--namespace` (the CLI refuses it alongside --resume), so the namespace —
	// and the cluster name every message uses — are the ones the interrupted
	// run wrote into its immutable request. Reading the live namespace by an
	// empty name is what produced "namespace  reports no UID, so the plan cannot
	// be pinned to the namespace it was built against" on hardware
	// (kn-x0wv.4, lab w3, 2026-09-27).
	if err := r.adoptRecordedRequest(plan.Record); err != nil {
		return err
	}
	r.skip = plan.Skip()
	r.skippedStages = skippedStages(plan)

	recorded := recordedIdentities(plan.Record)
	live, err := r.liveIdentities(ctx)
	if err != nil {
		return err
	}
	if moved := sameIdentity(recorded, live, r.consumedIdentitiesFor(plan.Record)); len(moved) > 0 {
		return fmt.Errorf("operation %s was planned against identities that have moved, so resuming it would restore into different objects:\n  %s",
			opID, strings.Join(moved, "\n  "))
	}
	if err := r.refuseClaimsTheRecordDidNotCreate(ctx, plan.Record, recorded); err != nil {
		return err
	}
	// The resume continues the SAME request, so the backup and the mode are the
	// record's and not this run's flags, and the plan is rebuilt from what the
	// record wrote down.
	backup := plan.Record.Request.Versions["backup"]
	switch plan.Record.Request.Kind {
	case operation.KindRestoreVolume:
		volumePlan, err := r.volumePlanFromRecord(ctx, recorded)
		if err != nil {
			return err
		}
		r.volumePlan = volumePlan
	default:
		r.plan = &restorePlan{
			Namespace:    r.opts.Namespace,
			Mode:         plan.Record.Request.Versions["mode"],
			NamespaceUID: recorded["namespace/"+r.opts.Namespace],
			Exists:       recorded["namespace/"+r.opts.Namespace] != "",
		}
		for name, uid := range recorded {
			if rest, ok := strings.CutPrefix(name, "persistentvolumeclaim/"); ok {
				namespace, claim, found := strings.Cut(rest, "/")
				if !found {
					continue
				}
				r.plan.Claims = append(r.plan.Claims, VolumeRef{Namespace: namespace, Name: claim, UID: uid})
			}
		}
		sort.Slice(r.plan.Claims, func(i, j int) bool { return r.plan.Claims[i].Name < r.plan.Claims[j].Name })
	}
	if backup != "" {
		facts, err := r.deps.Backups.NamedBackup(ctx, r.opts.Namespace, backup)
		if err != nil {
			return fmt.Errorf("the backup this operation restores (%s) cannot be read back: %w", backup, err)
		}
		if r.plan != nil {
			r.plan.Backup = facts
			// The resumed plan is rendered like the first one was, so the
			// policy and the conservative age come from the same places.
			r.plan.Policy = r.policy
			age, known := facts.Age(r.deps.Now())
			r.plan.Age, r.plan.AgeKnown = age, known
		}
		if r.volumePlan != nil {
			r.volumePlan.Backup = facts
		}
	}
	fmt.Fprintln(r.out, recovery.Progress(plan))
	for _, step := range plan.Steps {
		fmt.Fprintf(r.out, "  %s %s (%s): %s\n", step.Decision, step.ActionID, step.Stage, step.Reason)
	}
	handle, err := recovery.Claim(ctx, r.deps.Store)
	if err != nil {
		return err
	}
	r.handle = handle
	r.renderPlan()
	return nil
}

// adoptRecordedRequest takes the namespace, and the cluster name, from the
// record this resume continues. `--resume` takes no `--namespace`: the run it
// continues is the immutable request the record holds, and the namespace in a
// mode-1 record is the one the plan was pinned to.
func (r *restoreRun) adoptRecordedRequest(record *operation.Record) error {
	namespace := restoreNamespaceOf(record)
	if namespace == "" {
		return fmt.Errorf("operation %s records no namespace, so the restore it interrupted cannot be pinned to one: nothing has been changed", record.OperationID)
	}
	if r.opts.Namespace != "" && r.opts.Namespace != namespace {
		fmt.Fprintf(r.out, "  namespace:     the record is about %s, not the %s this run named: the record is the request this resume continues\n", namespace, r.opts.Namespace)
	}
	r.opts.Namespace = namespace
	if r.opts.Cluster == "" {
		r.opts.Cluster = record.Request.Cluster
	}
	return nil
}

// skippedStages reads a resume plan's decisions by STAGE, which is the shape
// the steps ask the question in. Plan.Skip is keyed by action id, which only
// the transport's Guarded decorator matches against.
func skippedStages(plan *operation.Plan) map[string]bool {
	done := map[string]bool{}
	for _, step := range plan.Steps {
		if step.Decision == operation.DecisionSkip {
			done[step.Stage] = true
		}
	}
	return done
}

// consumedIdentitiesFor is consumedIdentities plus the stages this resume's own
// plan established as done: an action whose postcondition the resume proved
// holds is as destroyed as one the record says succeeded, and the namespace
// deletion is the step whose postcondition the reconcilers can undo.
func (r *restoreRun) consumedIdentitiesFor(record *operation.Record) map[string]bool {
	consumed := consumedIdentities(record, r.opts.Namespace)
	if r.skippedStages["delete-namespace/"+r.opts.Namespace] {
		consumed["namespace/"+r.opts.Namespace] = true
	}
	return consumed
}

// refuseClaimsTheRecordDidNotCreate refuses a resume whose namespace now holds
// a claim this operation never saw.
//
// THE NAMESPACE DELETION IS TREATED AS DONE. After it, a namespace that is gone
// or that the reconcilers recreated does not fail the UID pin — the recorded
// namespace and the claims that went with it were destroyed by this operation's
// own delete, and a claim recreated under the same name is the ordinary state
// (S4 on lab w3: the recreated namespace holds an empty claim before the resume
// starts). What is not ordinary is a claim the record never held: it carries
// data this operation did not destroy and never showed the operator, and
// restoring into that namespace would leave it beside the restored objects. The
// refusal names every one of them, and it is decided BEFORE anything changes.
func (r *restoreRun) refuseClaimsTheRecordDidNotCreate(ctx context.Context, record *operation.Record, recorded map[string]string) error {
	if !r.consumedIdentitiesFor(record)["namespace/"+r.opts.Namespace] {
		return nil
	}
	known := map[string]bool{}
	for key := range recorded {
		if namespace, name := claimIdentityOf(key); namespace == r.opts.Namespace && name != "" {
			known[name] = true
		}
	}
	claims, err := r.deps.Cluster.Claims(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	var foreign []string
	for _, claim := range claims {
		if claim.Namespace != r.opts.Namespace || known[claim.Name] {
			continue
		}
		foreign = append(foreign, fmt.Sprintf("persistentvolumeclaim/%s/%s (uid %s)", claim.Namespace, claim.Name, claim.UID))
	}
	if len(foreign) == 0 {
		return nil
	}
	sort.Strings(foreign)
	return fmt.Errorf("operation %s deleted namespace %s, and what holds that name now carries %d claim(s) this restore did not create:\n  %s\nResuming would restore into them, and the safety backup holds what this operation destroyed rather than these. Nothing has been changed and the pause is left in place: look at them, and either `kubenest backup restore --abort %s`, or remove what does not belong and resume again",
		record.OperationID, r.opts.Namespace, len(foreign), strings.Join(foreign, "\n  "), record.OperationID)
}

// volumePlanFromRecord rebuilds mode 2's plan from what the record wrote down,
// with each claim's live detail filled in where the claim is still there. A
// claim this operation already deleted is expected to be absent — the record's
// own delete says so — and is still the claim whose refill this run verifies.
func (r *restoreRun) volumePlanFromRecord(ctx context.Context, recorded map[string]string) (*volumePlan, error) {
	plan := &volumePlan{Namespace: r.opts.Namespace, Mode: "volumes"}
	live, err := r.deps.Cluster.Claims(ctx, r.opts.Namespace)
	if err != nil {
		return nil, err
	}
	for name, uid := range recorded {
		rest, ok := strings.CutPrefix(name, "named-persistentvolumeclaim/")
		if !ok {
			continue
		}
		_, claimName, found := strings.Cut(rest, "/")
		if !found {
			continue
		}
		ref := VolumeRef{Namespace: r.opts.Namespace, Name: claimName, UID: uid}
		for _, claim := range live {
			if claim.Name == claimName {
				ref = claim
			}
		}
		plan.Claims = append(plan.Claims, ref)
		plan.PVCs = append(plan.PVCs, claimName)
	}
	sort.Slice(plan.Claims, func(i, j int) bool { return plan.Claims[i].Name < plan.Claims[j].Name })
	sort.Strings(plan.PVCs)
	return plan, nil
}

// recordedIdentities reads an operation's recorded artifact digests back into
// the map shape the comparison uses.
func recordedIdentities(rec *operation.Record) map[string]string {
	out := map[string]string{}
	for _, artifact := range rec.Request.Artifacts {
		out[artifact.Name] = artifact.Digest
	}
	return out
}

// liveIdentities re-reads the identities the plan is pinned to. Mode 1 reads
// the namespace and its claims; mode 2 reads the named claims.
func (r *restoreRun) liveIdentities(ctx context.Context) (map[string]string, error) {
	live := map[string]string{}
	state, err := r.deps.Cluster.Namespace(ctx, r.opts.Namespace)
	if err != nil {
		return nil, err
	}
	if state != nil {
		live["namespace/"+r.opts.Namespace] = state.UID
	}
	claims, err := r.deps.Cluster.Claims(ctx, r.opts.Namespace)
	if err != nil {
		return nil, err
	}
	for _, claim := range claims {
		live["persistentvolumeclaim/"+claim.Namespace+"/"+claim.Name] = claim.UID
	}
	for _, name := range r.opts.PVCs {
		key := "named-persistentvolumeclaim/" + r.opts.Namespace + "/" + name
		for _, claim := range claims {
			if claim.Name == name {
				live[key] = claim.UID
			}
		}
	}
	return live, nil
}

// release hands the record back at the end of a run.
//
// A run that got the data back leaves the record NON-terminal at
// `restored — awaiting activation` and marks its executor stopped, so
// `--activate`, `--resume` and `--abort` can take it over. A run that failed
// does the same: the operation is resumable, which is only true if a successor
// may take the record.
func (r *restoreRun) release(ctx context.Context, runErr error) {
	if r.handle == nil {
		return
	}
	// An interrupted run is released too: its own context is cancelled by the
	// interrupt, a write made with it never lands, and a record left "running"
	// refuses the --resume the interruption is for (hardware, 2026-09-27).
	ctx = context.WithoutCancel(ctx)
	if runErr != nil {
		if err := r.handle.Heartbeat(ctx, "failed: "+oneLine(runErr.Error())); err != nil {
			fmt.Fprintf(r.out, "warning: the operation record could not be told the failing stage: %v\n", err)
		}
	}
	if err := r.deps.Store.Stop(ctx, r.handle); err != nil {
		fmt.Fprintf(r.out, "warning: the operation record %s could not be released: %v (a later --resume will be refused while it still looks live)\n", r.handle.OperationID(), err)
		return
	}
	if runErr != nil {
		fmt.Fprintf(r.out, "  record:        %s released at %q; nothing was activated, and the project stays paused\n",
			r.handle.OperationID(), oneLine(runErr.Error()))
	}
}

func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// stage binds the cluster's CHANGES to the operation record for one stage, so
// every action is recorded with a stable identity and a postcondition before it
// is submitted, and a resume can tell whether it happened.
func (r *restoreRun) stage(name string) RestoreCluster {
	if r.handle == nil {
		return r.deps.Cluster
	}
	inner := r.deps.Runner
	if inner == nil {
		inner = r.deps.Store.Runner
	}
	return r.deps.Cluster.WithRunner(&operation.Guarded{
		Inner: inner,
		Op:    r.handle,
		Stage: name,
		Specs: restoreSpecs,
		Skip:  r.skip,
	})
}

// holdTimeout is the deadline for the bundle-owned waits around the hold: the
// operator acknowledging the pause, and a namespace or its pods going away.
// `component-ready` is the bundle's deadline for a component reaching its
// stated state, which is what all three are; the bundle carries no dedicated
// one and this binary invents no number.
func (r *restoreRun) holdTimeout() (time.Duration, error) {
	return r.timeout("component-ready")
}

// ensurePause writes the pause annotation on the in-cluster Project, stops
// everything in the namespace that can start work, and waits for the operator
// to acknowledge the pause.
//
// THE STOP COMES BEFORE THE WAIT AND IS RE-ASSERTED AFTER IT. The wait can run
// to the bundle's component-ready deadline, and a reconciler that is still
// finishing can act during it; a Job created in that window runs against the
// namespace the restore is about to empty (measured on hardware, S4 on lab w3,
// 2026-09-27).
//
// WHETHER THE OPERATOR CAN ACKNOWLEDGE AT ALL IS DECIDED EARLIER, and
// deliberately not here: by the time this runs the operation record exists, and
// a refusal that leaves a stopped record behind blocks the cluster's next
// operation until someone aborts it. The plan's pre-flight refuses instead —
// buildPlan for mode 1, buildVolumePlan for mode 2, and resumeRecord before the
// record is re-claimed — so a fresh run that cannot pause leaves nothing at all
// (measured on prod-2, kn-x0wv.6).
func (r *restoreRun) ensurePause(ctx context.Context) error {
	opID := r.handle.OperationID()
	if err := r.stage("pause").AnnotateProject(ctx, r.opts.Namespace, PauseAnnotationKey, opID); err != nil {
		return fmt.Errorf("%w. Without the hold, reconciliation would recreate the namespace or sync over the restore", err)
	}
	fmt.Fprintf(r.out, "  pause:         %s=%s written on Project %s/%s\n", PauseAnnotationKey, opID, ProjectCRNamespace, r.opts.Namespace)
	if err := r.stopWork(ctx, " before the pause is acknowledged"); err != nil {
		return err
	}
	if err := r.waitPauseAcknowledged(ctx, opID); err != nil {
		return err
	}
	// RE-ASSERTED AFTER THE ACKNOWLEDGEMENT, because the wait is exactly the
	// window in which a reconciler can have recreated a CronJob, or created a
	// Job, that the annotation did not stop it from making.
	return r.stopWork(ctx, " again after the acknowledgement")
}

// waitPauseAcknowledged waits for the operator's own acknowledgement AND for
// Argo CD to have no sync in progress on the target.
//
// BOTH HALVES ARE LOAD-BEARING. The condition proves the operator read the
// annotation; an Application with an operation still Running or Terminating
// proves a sync that started BEFORE the pause has not finished, and turning off
// future syncs is not proof that a running one did (plan 7.5, "Holding the
// reconcilers").
func (r *restoreRun) waitPauseAcknowledged(ctx context.Context, opID string) error {
	deadline, err := r.holdTimeout()
	if err != nil {
		return err
	}
	end := r.deps.Now().Add(deadline)
	var last string
	for {
		hold, err := r.deps.Cluster.ProjectHold(ctx, r.opts.Namespace)
		switch {
		case err != nil:
			last = err.Error()
		case hold == nil:
			last = fmt.Sprintf("Project %s/%s does not exist, so nothing can acknowledge the pause", ProjectCRNamespace, r.opts.Namespace)
		case !strings.Contains(hold.ConditionMessage, opID):
			last = fmt.Sprintf("the operator has not acknowledged operation %s yet (condition %s=%s reason %s)", opID, ConditionReconcilePaused, orUnknown(hold.ConditionStatus), orUnknown(hold.ConditionReason))
		case hold.ConditionStatus != "True" || hold.ConditionReason != ReasonPausedByOperation:
			last = fmt.Sprintf("the operator reports %s=%s (%s), not %s=%s", ConditionReconcilePaused, orUnknown(hold.ConditionStatus), orUnknown(hold.ConditionReason), ConditionReconcilePaused, ReasonPausedByOperation)
		default:
			inProgress, apps, err := r.syncInProgress(ctx)
			if err != nil {
				last = err.Error()
				break
			}
			if inProgress {
				last = fmt.Sprintf("Argo CD is still running an operation on %s", strings.Join(apps, ", "))
				break
			}
			fmt.Fprintf(r.out, "  pause:         acknowledged by the operator, and no sync is in progress on %s\n", r.opts.Namespace)
			return nil
		}
		if r.deps.Now().After(end) {
			return fmt.Errorf("the reconcilers did not acknowledge the pause within %s (limits.timeouts.component-ready): %s. Nothing has been destroyed, and the pause annotation is left in place", deadline, last)
		}
		if err := r.deps.Sleep(ctx, r.deps.Poll); err != nil {
			return err
		}
	}
}

// syncInProgress reports whether any Application that writes into the target
// namespace has an operation in flight.
func (r *restoreRun) syncInProgress(ctx context.Context) (bool, []string, error) {
	apps, err := r.deps.Cluster.Applications(ctx, r.opts.Namespace)
	if err != nil {
		return false, nil, err
	}
	var busy []string
	for _, app := range apps {
		switch app.Phase {
		case "Running", "Terminating":
			busy = append(busy, fmt.Sprintf("%s (%s)", app.Name, app.Phase))
		}
	}
	return len(busy) > 0, busy, nil
}

// verifyIdentities re-reads the identities the plan was built against,
// IMMEDIATELY before the first destructive step.
//
// KUBERNETES TIMESTAMPS ARE NEVER TREATED AS PROOF that a volume holds no newer
// data, and neither is a plan read minutes ago: a namespace deleted and
// recreated under the same name, or a claim recreated by the reconcilers, is a
// different object wearing the same name, and restoring into it is not the
// restore the operator confirmed.
func (r *restoreRun) verifyIdentities(ctx context.Context) error {
	recorded := identityOf(r.plan)
	live, err := r.liveIdentities(ctx)
	if err != nil {
		return err
	}
	// On a resume, a namespace or claim this SAME operation already destroyed is
	// not a moved identity: it is the step the record says is done.
	consumed := map[string]bool{}
	if r.handle != nil {
		consumed = r.consumedIdentitiesFor(r.handle.Record())
	}
	if moved := sameIdentity(recorded, live, consumed); len(moved) > 0 {
		return fmt.Errorf("the namespace or one of its claims changed since the plan was confirmed, so this is no longer the restore that was planned:\n  %s\nNothing has been destroyed, and the pause is left in place. Re-run the command to plan against what is there now",
			strings.Join(moved, "\n  "))
	}
	fmt.Fprintf(r.out, "  identities:    re-read and unchanged (%d identit(y/ies))\n", len(recorded))
	return nil
}

// safetyBackup takes the pre-restore backup of the namespace as it stands, and
// waits for it within limits.timeouts.backup.
//
// IT IS NOT TakeBackup. That one excludes the platform's own namespaces and
// derives its TTL from the schedule; this one must cover THIS namespace
// whatever its name, and keeps seven days. The wait is the same converge probe,
// so the two cannot drift apart.
func (r *restoreRun) safetyBackup(ctx context.Context) error {
	name := "kubenest-safety-" + r.handle.OperationID()
	doc, err := safetyBackupDocument(name, r.opts.Namespace, r.handle.OperationID(), SafetyBackupTTL)
	if err != nil {
		return err
	}
	if err := r.stage("backup/"+name).CreateBackup(ctx, name, doc); err != nil {
		return fmt.Errorf("taking the safety backup of %s before restoring over it: %w", r.opts.Namespace, err)
	}
	result, err := converge.Wait(ctx, backupSettledProbe(r.runner(), name), converge.Options{
		Name:     "safety-backup-" + name + "-settled",
		Deadline: r.backupErrTimeout,
		Interval: r.deps.Poll,
		Reporter: converge.NewTextReporter(r.out),
	})
	if err != nil {
		return fmt.Errorf("the safety backup %s did not settle within %s (limits.timeouts.backup): %w", name, r.backupErrTimeout, err)
	}
	if result.Last.Status != "Completed" {
		return fmt.Errorf("the safety backup %s settled as %s (%s), so nothing can be restored if this goes wrong: the restore has not started. Nothing has been destroyed",
			name, result.Last.Status, orUnknown(result.Last.Detail))
	}
	fmt.Fprintf(r.out, "  safety:        %s completed (kept %s), so the namespace as it stands can be brought back\n", name, SafetyBackupTTL)
	return nil
}

func (r *restoreRun) runner() k3s.Runner {
	if r.deps.Runner != nil {
		return r.deps.Runner
	}
	if r.deps.Store != nil {
		return r.deps.Store.Runner
	}
	return nil
}

// recordPending records work this operation still owes activation: putting the
// workloads back to their prior replica counts, and putting each CronJob back
// to the suspend value the BACKUP held (read out of the annotation the
// restore's resource modifier wrote, because the object itself is suspended by
// then).
//
// It is a PendingWrite rather than a version, because the version map is part
// of the request identity a resume compares: the replica counts legitimately
// change during the run (the workloads are scaled to zero), so they cannot live
// where a difference is a refusal.
func (r *restoreRun) recordPending(ctx context.Context, target, kind, detail string) error {
	return r.deps.Store.Update(ctx, r.handle, func(rec *operation.Record) error {
		now := time.Now().UTC()
		for i := range rec.Pending {
			if rec.Pending[i].Target == target && rec.Pending[i].Kind == kind {
				rec.Pending[i].Detail = detail
				rec.Pending[i].Status = operation.WritePending
				rec.Pending[i].At = now
				rec.Pending[i].DoneAt = nil
				return nil
			}
		}
		rec.Pending = append(rec.Pending, operation.PendingWrite{
			Kind:   kind,
			Target: target,
			Detail: detail,
			Status: operation.WritePending,
			At:     now,
		})
		return nil
	})
}

// scaleToZero stops the namespace's writers before it is deleted, recording the
// counts activation puts back.
func (r *restoreRun) scaleToZero(ctx context.Context) error {
	workloads, err := r.deps.Cluster.Workloads(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	for _, w := range workloads {
		if err := r.recordPending(ctx, w.Kind+"/"+w.Name, "restore-replicas", fmt.Sprintf("%d", w.Replicas)); err != nil {
			return err
		}
		if err := r.stage("scale/"+w.Kind+"/"+w.Name).ScaleWorkload(ctx, w.Kind, w.Name, r.opts.Namespace, 0); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  stop:          %s %s scaled to 0 (was %d)\n", w.Kind, w.Name, w.Replicas)
	}
	return nil
}

// stopWork stops everything in the namespace that can START work: every
// CronJob, so no schedule creates a Job, and every Job that has NOT finished,
// so a Job already running stops its pods instead of writing into claims the
// restore is about to empty.
//
// IT RUNS TWICE, AROUND THE ACKNOWLEDGEMENT, AND THAT IS THE POINT. The first
// run is as soon as the pause annotation is written and BEFORE the operator's
// acknowledgement is waited for; the second re-asserts both after the
// acknowledgement, in case a reconciler acted during that wait.
//
// WHY IT IS NOT ENOUGH TO DO THIS AFTER THE ACKNOWLEDGEMENT. On hardware (S4,
// lab w3, 2026-09-27, after the stop step already suspended CronJobs before the
// safety backup) a sentinel call still arrived: the recreated namespace's live
// CronJob created a Job at 18:30:00, its pod called the sentinel at 18:30:31,
// and the run's stop step ran only after the acknowledgement — so suspending
// the CronJob then could not stop a Job it had already created. A CronJob's Job
// starts its own pod, which is why scaling the workloads to zero does not stop
// it, and why a Job that is already running has to be suspended itself.
//
// SCALING THE WORKLOADS IS NOT PART OF THIS. They are scaled to zero after the
// safety backup, because Velero's file-level copy of a claim needs the pod that
// mounts it to be running; a Job's pod is not the workload's, and its writes
// are what must stop now.
//
// WHAT IT RECORDS IS NOT WHAT ACTIVATION USES. Each object's value before this
// step goes into the record under its own target, so "what did the stop step do"
// is answerable. Activation puts each RESTORED object back from the operation's
// own restore records — a CronJob's value comes from the BACKUP, read out of
// the annotation the restore's resource modifier wrote (kn-x0wv.3) — and never
// from here.
//
// A JOB THIS SUSPENDS IS NOT ACTIVATION'S BUSINESS. The namespace deletion
// destroys it with everything else the namespace held, and Jobs are excluded
// from the restore unless --include-jobs brought them back, so there is no
// object left for activation to un-suspend: the "job-stop/<name>" entry is a
// record for the operator, not owed work. When --include-jobs DID restore a
// Job, activation starts the RESTORED object, from its own entry
// ("job/<name>", written by holdRestoredWork) — never from this one.
func (r *restoreRun) stopWork(ctx context.Context, when string) error {
	cronjobs, err := r.deps.Cluster.CronJobs(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	for _, job := range cronjobs {
		was := "false"
		if job.Suspend {
			was = "true"
		}
		if err := r.stage("stop-cronjob/"+job.Name).SuspendCronJob(ctx, r.opts.Namespace, job.Name, true); err != nil {
			return fmt.Errorf("suspending CronJob %s in namespace %s%s: %w", job.Name, r.opts.Namespace, when, err)
		}
		if err := r.recordStoppedWork(ctx, StoppedCronJobTarget+job.Name, StopCronJobWrite, was); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  stop:          CronJob %s suspended%s (it was suspended=%s); activation takes its value from the backup, not from this step\n", job.Name, when, was)
	}
	jobs, err := r.deps.Cluster.Jobs(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	unfinished := 0
	for _, job := range jobs {
		if job.Finished {
			// A Job that completed or failed terminally creates no pod and
			// writes nothing: suspending it would change the object's spec away
			// from what the backup holds for no gain.
			continue
		}
		unfinished++
		was := "false"
		if job.Suspend {
			was = "true"
		}
		if err := r.stage("stop-job/"+job.Name).SuspendJob(ctx, r.opts.Namespace, job.Name, true); err != nil {
			return fmt.Errorf("suspending Job %s in namespace %s%s: %w", job.Name, r.opts.Namespace, when, err)
		}
		if err := r.recordStoppedWork(ctx, StoppedJobTarget+job.Name, StopJobWrite, was); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  stop:          Job %s suspended%s (it was suspended=%s): a suspended Job's pods stop and the object stays for the safety backup\n", job.Name, when, was)
	}
	if len(cronjobs) == 0 && unfinished == 0 {
		fmt.Fprintf(r.out, "  stop:          nothing in namespace %s can start work%s (no CronJob, no Job that has not finished)\n", r.opts.Namespace, when)
	}
	return nil
}

// recordStoppedWork writes down, in the operation, that the stop step suspended
// one object and what its value was before, as a write that is already
// DISCHARGED: it is a record of what happened rather than work activation owes.
// It is written AFTER the patch returned, so the record never claims a
// suspension that did not happen.
//
// ITS TARGET IS ITS OWN ("cronjob-stop/<name>", "job-stop/<name>"). Activation
// reads the values it puts restored objects back to from "cronjob/<name>",
// "job/<name>", "deployment/<name>" and "statefulset/<name>", so a record entry
// here can never be mistaken for one of those.
func (r *restoreRun) recordStoppedWork(ctx context.Context, target, kind, was string) error {
	now := r.deps.Now()
	return r.deps.Store.Update(ctx, r.handle, func(rec *operation.Record) error {
		for i := range rec.Pending {
			if rec.Pending[i].Target != target || rec.Pending[i].Kind != kind {
				continue
			}
			rec.Pending[i].Detail = was
			rec.Pending[i].Status = operation.WriteDone
			rec.Pending[i].DoneAt = &now
			return nil
		}
		rec.Pending = append(rec.Pending, operation.PendingWrite{
			Kind:   kind,
			Target: target,
			Detail: was,
			Status: operation.WriteDone,
			At:     now,
			DoneAt: &now,
		})
		return nil
	})
}

// namespaceDeletionDone reports whether this operation's own delete of the
// namespace is established: the record says the action succeeded, or the
// resume's plan proved the postcondition holds. A resumed run does not delete
// it again and does not wait for it to be gone.
func (r *restoreRun) namespaceDeletionDone() bool {
	stage := "delete-namespace/" + r.opts.Namespace
	if r.skippedStages[stage] {
		return true
	}
	if r.handle == nil {
		return false
	}
	for _, action := range r.handle.Record().Actions {
		if action.Stage == stage && action.Status == operation.ActionSucceeded {
			return true
		}
	}
	return false
}

// deleteNamespace deletes the namespace and waits until the API reports it
// gone: restoring into a terminating namespace is how a restore comes back
// half-applied.
//
// A DELETION THIS OPERATION ALREADY DID IS NOT WAITED FOR AGAIN. The
// reconcilers recreate the namespace as soon as it is gone — that is the
// ordinary state the pause exists to stop mid-flight — so on a resume the
// namespace can exist again with a new UID, and "wait until it is gone" would
// never come true. The record's own succeeded action is the fact; the resume
// goes on to the Restore the record names.
func (r *restoreRun) deleteNamespace(ctx context.Context) error {
	if r.namespaceDeletionDone() {
		fmt.Fprintf(r.out, "  namespace:     %s was deleted by this operation (the step is recorded as done): not deleting it again, and not waiting for the name to stay empty — the reconcilers may have recreated it\n", r.opts.Namespace)
		return nil
	}
	if state, err := r.deps.Cluster.Namespace(ctx, r.opts.Namespace); err != nil {
		return err
	} else if state == nil {
		fmt.Fprintf(r.out, "  namespace:     %s is already gone; there is nothing to delete\n", r.opts.Namespace)
		return nil
	}
	if err := r.stage("delete-namespace/"+r.opts.Namespace).DeleteNamespace(ctx, r.opts.Namespace); err != nil {
		return err
	}
	deadline, err := r.holdTimeout()
	if err != nil {
		return err
	}
	if err := r.waitUntil(ctx, deadline, "namespace "+r.opts.Namespace+" to be gone", func(ctx context.Context) (bool, string, error) {
		state, err := r.deps.Cluster.Namespace(ctx, r.opts.Namespace)
		if err != nil {
			return false, err.Error(), err
		}
		if state != nil {
			return false, fmt.Sprintf("namespace %s is still terminating (uid %s)", r.opts.Namespace, state.UID), nil
		}
		return true, "namespace gone", nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "  namespace:     %s deleted\n", r.opts.Namespace)
	return nil
}

// waitUntil polls one condition to a deadline, reporting the last observation
// when it runs out.
func (r *restoreRun) waitUntil(ctx context.Context, deadline time.Duration, what string, probe func(ctx context.Context) (bool, string, error)) error {
	end := r.deps.Now().Add(deadline)
	var last string
	for {
		done, detail, err := probe(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		last = detail
		if r.deps.Now().After(end) {
			return fmt.Errorf("waited %s for %s and it did not happen: %s", deadline, what, last)
		}
		if err := r.deps.Sleep(ctx, r.deps.Poll); err != nil {
			return err
		}
	}
}

// runVeleroRestore creates the restore this mode asks for, waits for it within
// the bundle's restore deadline, and refuses a verdict that is not Completed.
func (r *restoreRun) runVeleroRestore(ctx context.Context, name string, spec restoreRequest) ([]VolumeRestoreState, error) {
	spec.Name = name
	spec.OperationID = r.handle.OperationID()
	doc, err := restoreDocument(spec)
	if err != nil {
		return nil, err
	}
	if spec.ResourceModifier != nil {
		if err := r.stage("modifier/"+spec.ResourceModifier.Name).Apply(ctx, "the restore resource modifier "+spec.ResourceModifier.Name, spec.ResourceModifier.Doc); err != nil {
			return nil, err
		}
	}
	stage := r.stage("restore/" + name)
	if err := stage.CreateRestore(ctx, name, doc); err != nil {
		return nil, err
	}
	fmt.Fprintf(r.out, "  restore:       %s requested from backup %s (waited within %s)\n", name, spec.Backup, r.restoreTimeout)
	outcome, err := r.waitRestore(ctx, name)
	if err != nil {
		return nil, err
	}
	restores, err := r.deps.Cluster.VolumeRestores(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := checkVolumeRestores(outcome, restores, spec.NamedVolumes, spec.RemovedVolumes); err != nil {
		return nil, err
	}
	fmt.Fprintf(r.out, "  restore:       %s finished as %s with %d error(s) and %d warning(s)%s\n",
		name, outcome.Phase, outcome.Errors, outcome.Warnings, partialNote(len(restores)))
	// ONE LINE NAMING WHAT WAS TOLERATED. A PartiallyFailed verdict with errors
	// in it is exactly what a reader will stop on, so the run says which errors
	// they were and why they were accepted — the volume the modifier removed
	// from the restored pod, which is the volume whose live contents this
	// restore exists to leave alone.
	if tolerated := removedVolumeFailures(restores, spec.RemovedVolumes); len(tolerated) > 0 {
		pairs := make([]string, 0, len(tolerated))
		for _, vr := range tolerated {
			pairs = append(pairs, vr.Pod+"/"+vr.Volume)
		}
		fmt.Fprintf(r.out, "  tolerated:     %d of those error(s) are the modifier's own: %s — %q, so the volume stays as it was and that error is not counted against the restore\n",
			len(tolerated), strings.Join(pairs, ", "), volumeNotFoundInPod)
	}
	return restores, nil
}

// partialNote says how many PodVolumeRestores Velero wrote for this restore,
// beside the phase. IT IS PRINTED EVEN WHEN THE COUNT IS ZERO: a Completed
// restore that wrote no PodVolumeRestore restored nothing at all — probe P5's
// run 4 — and that count is the one thing a reader needs beside "Completed with
// 0 errors". It counts what Velero WROTE and not what completed, because the
// accepted PartiallyFailed verdict has a Failed one among them on purpose.
func partialNote(volumeRestores int) string {
	return fmt.Sprintf(" (%d PodVolumeRestore(s) Velero wrote)", volumeRestores)
}

// waitRestore polls one Velero Restore until it reaches a terminal phase.
//
// IT ALSO REFUSES A RESTORE THAT CANNOT FINISH, WITHIN A POLL OR TWO. A Velero
// Restore waits for every PodVolumeRestore it created, and a PodVolumeRestore
// whose pod the cluster no longer has waits for ever — Velero's own
// itemOperationTimeout is four hours, and it holds Velero's only restore worker
// the whole time (measured on lab w3, 2026-09-27, kn-x0wv.2). So each poll also
// asks whether the pod any outstanding PodVolumeRestore targets is still there
// and still wanted, and stops with a refusal that names it.
func (r *restoreRun) waitRestore(ctx context.Context, name string) (*RestoreOutcome, error) {
	end := r.deps.Now().Add(r.restoreTimeout)
	// missingPods are the PodVolumeRestores whose target pod was absent at the
	// PREVIOUS poll. One absence is not evidence — the pod may not be created
	// yet — and two in a row is, because Velero creates a pod and its
	// PodVolumeRestore together.
	missingPods := map[string]bool{}
	for {
		outcome, err := r.deps.Cluster.RestoreOutcome(ctx, name)
		if err != nil {
			return nil, err
		}
		if outcome.Terminal() {
			return outcome, nil
		}
		if r.deps.Now().After(end) {
			return nil, fmt.Errorf("Velero Restore %s did not reach a terminal phase within %s (limits.timeouts.restore-drill): it is %s. Nothing is reported as done, and the operation is resumable with `kubenest backup restore --resume %s`",
				name, r.restoreTimeout, orUnknown(outcome.Phase), r.handle.OperationID())
		}
		if err := r.refuseRestoresWhosePodsCannotBeFilled(ctx, name, missingPods); err != nil {
			return nil, err
		}
		if err := r.deps.Sleep(ctx, r.deps.Poll); err != nil {
			return nil, err
		}
	}
}

// refuseRestoresWhosePodsCannotBeFilled stops a wait that can never end.
//
// TWO SHAPES, ONE OUTCOME. Either the pod a PodVolumeRestore targets is not in
// the namespace any more — the restore's own pod, adopted and deleted by a
// zero-replica ReplicaSet, is the measured case — or it is there and already
// owned by a ReplicaSet whose desired count is zero, which will delete it on
// its next reconcile. In both, the volume cannot be filled and the restore
// cannot finish, so the wait ends with a refusal instead of after hours.
//
// THE MEASURED CAUSE IS NOW REMOVED BEFORE IT CAN ARISE: the restore's own
// resource modifier renames a zero-replica ReplicaSet's selector out of the way
// as Velero creates it (heldReplicaSetRule, namespaceModifierDocument), so that
// ReplicaSet no longer selects the restored pod it would have deleted. This
// guard is kept as the last line of defence, because a pod can go another way —
// deleted by hand, rescheduled elsewhere — and a wait that can never end must
// still end with a refusal and a name rather than after hours.
//
// ONLY OUTSTANDING RESTORES COUNT: a Completed PodVolumeRestore has its data,
// and a Failed one is Velero's own verdict, which checkVolumeRestores reports
// with far more detail than this guard could.
func (r *restoreRun) refuseRestoresWhosePodsCannotBeFilled(ctx context.Context, restore string, missingPods map[string]bool) error {
	volumes, err := r.deps.Cluster.VolumeRestores(ctx, restore)
	if err != nil {
		return err
	}
	outstanding := make([]VolumeRestoreState, 0, len(volumes))
	for _, volume := range volumes {
		if volume.Pod == "" || podVolumeRestoreSettled(volume.Phase) {
			delete(missingPods, volume.Name)
			continue
		}
		outstanding = append(outstanding, volume)
	}
	if len(outstanding) == 0 {
		return nil
	}
	pods, err := r.deps.Cluster.Pods(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	live := map[string]PodState{}
	for _, pod := range pods {
		live[pod.Name] = pod
	}
	var sets []ReplicaSetState
	for _, volume := range outstanding {
		pod, present := live[volume.Pod]
		if present {
			delete(missingPods, volume.Name)
			// THE OWNER THAT IS ABOUT TO DELETE IT. Velero strips a restored
			// pod's owner references, so this is a pod some controller has
			// adopted since — and a ReplicaSet at zero deletes what it adopts.
			owner := controllerOwnerOf(pod)
			if owner == nil || !strings.EqualFold(owner.Kind, "replicaset") {
				continue
			}
			if sets == nil {
				if sets, err = r.deps.Cluster.ReplicaSets(ctx, r.opts.Namespace); err != nil {
					return err
				}
			}
			if set, found := replicaSetByName(sets, owner.Name); found && set.Replicas == 0 {
				return r.refuseUnfillableVolume(restore, volume, set, "the pod is still there, adopted by a ReplicaSet whose desired count is 0, which deletes what it adopts")
			}
			continue
		}
		if !missingPods[volume.Name] {
			// The FIRST poll that misses it: it may not be created yet.
			missingPods[volume.Name] = true
			continue
		}
		if sets == nil {
			if sets, err = r.deps.Cluster.ReplicaSets(ctx, r.opts.Namespace); err != nil {
				return err
			}
		}
		set, found := replicaSetByName(sets, replicaSetOfPodName(volume.Pod))
		return r.refuseUnfillableVolume(restore, volume, set,
			fmt.Sprintf("the pod is gone from namespace %s%s", r.opts.Namespace, replicaSetEvidence(set, found)))
	}
	return nil
}

// refuseUnfillableVolume is the refusal for a PodVolumeRestore whose volume
// cannot be filled, naming the pod, the ReplicaSet the pod belonged to and what
// did it, and ending with the two ways out.
func (r *restoreRun) refuseUnfillableVolume(restore string, volume VolumeRestoreState, set *ReplicaSetState, shape string) error {
	owner := ""
	if set != nil {
		owner = fmt.Sprintf(" ReplicaSet %s is on the cluster at %d replica(s)%s.", set.Name, set.Replicas, ownerPhrase(set))
	}
	return fmt.Errorf("Velero Restore %s cannot finish: PodVolumeRestore %s was created for pod %s (volume %s of claim %s), and %s.%s The backup was taken while the workload was rolling out — the pod it holds a volume copy for belongs to a ReplicaSet the backup holds at zero, and a restored pod is adopted by its ReplicaSet's SELECTOR, not by its owner, so the adoption happens even though Velero strips owner references. That is the usual cause: if it is not this one, the pod went another way. Nothing is reported as done, and the operation is resumable (`kubenest backup restore --resume %s`) or can be given up (`--abort %s`); this run has stopped waiting, but Velero Restore %s still holds Velero's only restore worker until its own timeout. Restoring this backup will run into the same adoption: take a fresh backup once the rollout has settled, or restore one taken when it was not in progress",
		restore, volume.Name, volume.Pod, orUnknown(volume.Volume), orUnknown(volume.ClaimName), shape, owner,
		r.handle.OperationID(), r.handle.OperationID(), restore)
}

// podVolumeRestoreSettled reports whether a PodVolumeRestore has an outcome the
// run does not wait on: Completed (the volume is filled) or Failed (Velero's own
// verdict, which checkVolumeRestores reports in full).
func podVolumeRestoreSettled(phase string) bool {
	switch phase {
	case "Completed", "Failed", "FailedValidation":
		return true
	}
	return false
}

// controllerOwnerOf reads a pod's controller owner, or nil.
func controllerOwnerOf(pod PodState) *OwnerRef {
	for _, owner := range pod.Owners {
		if owner.Controller {
			return &owner
		}
	}
	return nil
}

// replicaSetByName finds one ReplicaSet by name.
func replicaSetByName(sets []ReplicaSetState, name string) (*ReplicaSetState, bool) {
	for i := range sets {
		if sets[i].Name == name {
			return &sets[i], true
		}
	}
	return nil, false
}

// replicaSetOfPodName names the ReplicaSet a pod's name was built from.
//
// IT IS THE NAMING CONVENTION, NOT AN IDENTITY. A pod created by a ReplicaSet is
// named "<replicaset>-<suffix>", so the part before the last dash is the
// ReplicaSet — and this is only ever used to look that name up among the
// ReplicaSets the cluster actually has (replicaSetByName), which is what makes
// the refusal say "ReplicaSet X" about a ReplicaSet that is there. When the name
// matches nothing, the refusal says so instead of guessing.
func replicaSetOfPodName(pod string) string {
	if at := strings.LastIndexByte(pod, '-'); at > 0 {
		return pod[:at]
	}
	return ""
}

// replicaSetEvidence says what could be established about the ReplicaSet the pod
// belonged to, for the refusal. It is a sentence of its own so the refusal reads
// the same for a pod that is there and one that is gone.
func replicaSetEvidence(set *ReplicaSetState, found bool) string {
	switch {
	case !found:
		return ", and no ReplicaSet its name is built from is on the cluster either, so it went with the pod"
	case set.Replicas == 0:
		return fmt.Sprintf(", and ReplicaSet %s — the one its name is built from — is on the cluster at 0 replicas: it adopted the pod and deleted it", set.Name)
	default:
		return fmt.Sprintf(", and ReplicaSet %s — the one its name is built from — is on the cluster at %d replica(s), so something other than its own rollout deleted it", set.Name, set.Replicas)
	}
}

// ownerPhrase names the Deployment above a ReplicaSet, when the cluster still
// has one.
func ownerPhrase(set *ReplicaSetState) string {
	if set.Owner == nil {
		return ""
	}
	return fmt.Sprintf(" %s %s owns it.", set.Owner.Kind, set.Owner.Name)
}

// volumeNotFoundInPod is Velero's own wording for the one error a mode-2 restore
// of a SUBSET of a pod's volumes expects: the strip patch took the unnamed
// volume out of the restored pod, and Velero's restore-wait init container —
// which mounts every backed-up volume — finds nothing to expose for it.
//
// It is the wording P5 measured and the wording hardware reported again (lab w3,
// 2026-09-27: PodVolumeRestore …-9m8ks for pod sel-5df6d6d955-78hxc, volume b:
// "error getting volume directory name for volume b in pod
// sel-5df6d6d955-78hxc: volume not found in pod"). Judging an error by another
// program's wording is worth it here and only here: the alternative is refusing
// every selective restore, and this is the one string that separates a volume we
// chose to keep from a volume that failed to come back.
const volumeNotFoundInPod = "volume not found in pod"

// removedVolumeFailures are the PodVolumeRestores the strip patch is responsible
// for: Failed, for a (pod, volume) pair the patch removed, carrying Velero's
// "volume not found in pod" wording. All three are required — the same message
// on a volume this restore did not remove, or a Failed volume it did not touch,
// is a real failure and not this.
func removedVolumeFailures(restores []VolumeRestoreState, removed []RemovedVolume) []VolumeRestoreState {
	var out []VolumeRestoreState
	for _, vr := range restores {
		if isRemovedVolumeFailure(vr, removed) {
			out = append(out, vr)
		}
	}
	return out
}

func isRemovedVolumeFailure(vr VolumeRestoreState, removed []RemovedVolume) bool {
	if vr.Phase != "Failed" || !strings.Contains(vr.Message, volumeNotFoundInPod) {
		return false
	}
	for _, pair := range removed {
		if pair.Pod == vr.Pod && pair.Volume == vr.Volume {
			return true
		}
	}
	return false
}

// checkVolumeRestores decides whether a Velero restore's verdict is one this run
// may act on.
//
// "COMPLETED WITH 0 ERRORS" IS NOT PROOF. Run 4 of probe P5 reported exactly
// that and restored nothing: without persistentvolumes in the type filter Velero
// creates no PodVolumeRestore at all, injects its restore-wait init container
// anyway, and the pod waits for ever. So every NAMED volume must have a
// Completed PodVolumeRestore of its own, whatever the phase says.
//
// PARTIALLYFAILED IS THE ONE OTHER VERDICT MODE 2 EXPECTS, and only for the
// price of the strip patch. Removing an unnamed volume from a restored pod
// makes Velero report one error for that (pod, volume) pair, and that error is
// what keeps the volume's live contents: it is tolerated ONLY when every one of
// these holds —
//
//   - every named claim has a Completed PodVolumeRestore;
//   - every PodVolumeRestore that did not complete is Failed, is for a pair the
//     patch removed, and carries Velero's "volume not found in pod" wording;
//   - the restore's error count is exactly the number of those pair errors;
//   - there is no failureReason.
//
// ANYTHING ELSE IS REFUSED, and mode 1 — which removes nothing, so removed is
// empty — keeps the strict rule: only a restore whose volumes all completed is
// acted on.
func checkVolumeRestores(outcome *RestoreOutcome, restores []VolumeRestoreState, named []VolumeRef, removed []RemovedVolume) error {
	var failed []string
	var tolerated []string
	var toleratedPairs []string
	completed := map[string]bool{}
	for _, vr := range restores {
		if vr.Phase == "Completed" {
			completed[vr.ClaimName] = true
			completed[vr.Volume] = true
			continue
		}
		detail := fmt.Sprintf("%s (pod %s, volume %s, phase %s: %s)", vr.Name, vr.Pod, vr.Volume, orUnknown(vr.Phase), orUnknown(vr.Message))
		if isRemovedVolumeFailure(vr, removed) {
			tolerated = append(tolerated, detail)
			toleratedPairs = append(toleratedPairs, vr.Pod+"/"+vr.Volume)
			continue
		}
		failed = append(failed, detail)
	}
	// The volumes with no Completed PodVolumeRestore of their own, BY NAME: a
	// count tells an operator that something is wrong, and these names tell
	// them what.
	var unfilled []string
	for _, volume := range named {
		if !completed[volume.Name] {
			unfilled = append(unfilled, volume.Namespace+"/"+volume.Name)
		}
	}

	// THE ONE NON-COMPLETED VERDICT THIS MODE ACTS ON: the strip patch's own
	// errors, the named claims filled, nothing else wrong.
	if outcome.Phase == "PartiallyFailed" && len(tolerated) > 0 && len(failed) == 0 && len(unfilled) == 0 &&
		outcome.FailureReason == "" && int64(len(tolerated)) == outcome.Errors {
		return nil
	}

	if outcome.Phase == "Completed" {
		if notCompleted := append(append([]string(nil), failed...), tolerated...); len(notCompleted) > 0 {
			return fmt.Errorf("Velero Restore %s reports Completed, but these PodVolumeRestores did not: %s", outcome.Name, strings.Join(notCompleted, "; "))
		}
		if len(unfilled) > 0 {
			return fmt.Errorf("Velero Restore %s reports Completed with %d error(s), but these volume(s) have no Completed PodVolumeRestore, so nothing proves they were filled: %s",
				outcome.Name, outcome.Errors, strings.Join(unfilled, ", "))
		}
		return nil
	}

	detail := fmt.Sprintf("Velero Restore %s is %s, not Completed: %d error(s), %d warning(s)", outcome.Name, outcome.Phase, outcome.Errors, outcome.Warnings)
	if outcome.FailureReason != "" {
		detail += ", failureReason: " + outcome.FailureReason
	}
	if notCompleted := append(append([]string(nil), failed...), tolerated...); len(notCompleted) > 0 {
		detail += "; PodVolumeRestores that did not complete: " + strings.Join(notCompleted, "; ")
	}
	if len(unfilled) > 0 {
		detail += "; volumes with no Completed PodVolumeRestore: " + strings.Join(unfilled, ", ")
	}
	if len(removed) > 0 {
		// The patch removed volumes, so errors ARE expected to be its own: say
		// what it removed, which of this verdict's errors were accepted as
		// theirs, and what refuses the verdict — "PartiallyFailed" on its own
		// tells an operator nothing about which errors to look at.
		expected := make([]string, 0, len(removed))
		for _, pair := range removed {
			expected = append(expected, pair.Pod+"/"+pair.Volume)
		}
		var lacking []string
		if len(tolerated) == 0 {
			lacking = append(lacking, fmt.Sprintf("no Failed PodVolumeRestore for one of the removed pairs carrying %q", volumeNotFoundInPod))
		}
		if outcome.Phase != "PartiallyFailed" {
			lacking = append(lacking, "a phase of "+orUnknown(outcome.Phase)+" where the removed volumes' errors make PartiallyFailed")
		}
		if len(failed) > 0 {
			lacking = append(lacking, "PodVolumeRestores that are not a removed volume's error")
		}
		if len(unfilled) > 0 {
			lacking = append(lacking, "named volumes with no Completed PodVolumeRestore")
		}
		if outcome.FailureReason != "" {
			lacking = append(lacking, "a failureReason")
		}
		if int64(len(tolerated)) != outcome.Errors {
			lacking = append(lacking, fmt.Sprintf("%d error(s) where the removed volumes account for %d", outcome.Errors, len(tolerated)))
		}
		accepted := "none"
		if len(toleratedPairs) > 0 {
			accepted = strings.Join(toleratedPairs, ", ")
		}
		detail += fmt.Sprintf("; the strip patch removed %s, and %q is tolerated for each — accepted as the patch's own: %s; refused for: %s",
			strings.Join(expected, ", "), volumeNotFoundInPod, accepted, strings.Join(lacking, ", "))
	}
	return errors.New(detail)
}

// cronJobModifierDocument renders the resource modifier mode 1's restore
// carries: it suspends every CronJob AS VELERO CREATES IT, and records on the
// object the value the backup held.
//
// WHY IN THE RESTORE AND NOT AFTER IT. The suspension used to be a
// `kubectl patch` this command applied once the restore had finished, and a
// CronJob restored unsuspended can create a Job in that window — measured on
// hardware, 2026-09-27, S4 on lab w3: `s4-sentinel` came back at 16:27:58 and
// created its Job at 16:28:00, before the patch landed. A resource modifier is
// applied to the object BEFORE Velero creates it, so the CronJob never exists
// unsuspended. There is only one mechanism: the shapes below are the ones mode
// 2 already uses (restore_volumes.go), and runVeleroRestore applies both the
// same way.
//
// TWO RULES, AND THE ORDER IS LOAD-BEARING. Velero applies every rule whose
// conditions match, in the order the ConfigMap lists them, and — when the
// ConfigMap holds more than one rule — it matches each rule's conditions
// against the ORIGINAL object, so a later rule's condition cannot see an
// earlier rule's patch (internal/resourcemodifiers/resource_modifiers.go,
// ApplyResourceModifierRules, v1.18.1 — the version chart 12.1.0 in
// pkg/bundles/manifests/platform-1.1.yaml ships):
//
//   - the FIRST rule matches every CronJob, suspends it and records "false".
//     A rule conditional on `suspend: false` cannot do this job: `spec.suspend`
//     unset and `spec.suspend: false` are the same state to Kubernetes, the
//     field is optional with no default, and a `matches` entry for a path the
//     object does not carry is simply not a match — matchConditions runs the
//     entries as JSON Patch `test` operations and reads ErrTestFailed and
//     ErrMissing as "no match".
//   - the SECOND rule matches only a CronJob the BACKUP had suspended and
//     overwrites that record with "true". It is matched against the object as
//     the backup holds it, which is what makes the recorded value the BACKUP's
//     own and not the value the rule above just wrote.
//
// The patch type is a JSON merge patch, not a JSON patch. A JSON patch's
// `value` is a string unless it is escaped or looks like a number, boolean,
// null, object or array (json_patch.go, addQuotes), and adding
// `/metadata/annotations/<key>` to an object with no annotations at all — an
// ordinary CronJob, since `suspend` is not the only field absent by default —
// fails. Merge patches carry JSON types and create the missing intermediate
// objects.
func cronJobModifierDocument(operationID string) (*modifierConfigMap, error) {
	return modifierDocument(operationID, cronJobModifierRules())
}

// cronJobModifierRules are the two rules that suspend a restored CronJob as
// Velero creates it and record the value the BACKUP held.
func cronJobModifierRules() []any {
	// recorded is the merge patch that writes, on the CronJob, the value the
	// backup held for spec.suspend. It is a real JSON string in the object,
	// not the boolean spec.suspend is.
	recorded := func(wasSuspended string) string {
		return `{"metadata":{"annotations":{"` + CronJobSuspendedAnnotationKey + `":"` + wasSuspended + `"}}}`
	}
	return []any{
		map[string]any{
			"conditions": map[string]any{"groupResource": "cronjobs.batch"},
			"mergePatches": []any{
				map[string]any{"patchData": `{"spec":{"suspend":true}}`},
				map[string]any{"patchData": recorded("false")},
			},
		},
		map[string]any{
			"conditions": map[string]any{
				"groupResource": "cronjobs.batch",
				"matches":       []any{map[string]any{"path": "/spec/suspend", "value": "true"}},
			},
			"mergePatches": []any{
				map[string]any{"patchData": recorded("true")},
			},
		},
	}
}

// HeldReplicaSetHash is the pod-template-hash a ReplicaSet the backup holds at
// zero replicas is renamed to, so that it selects none of the restored pods.
//
// IT IS NOT A HASH AND DOES NOT PRETEND TO BE: nothing selects on the value, and
// the point is only that it is not the hash that ReplicaSet's own pods carry.
// The value is reserved for this one job, so no ReplicaSet's live selector can
// ever hold it.
const HeldReplicaSetHash = "kubenest-held"

// heldReplicaSetRule is the rule that keeps a zero-replica ReplicaSet out of the
// selector of the pods a namespace restore brings back.
//
// WHY THE REPLICASET AND NOT THE POD. kn-x0wv.2 renamed every restored pod the
// backup had copied a volume for, and that renamed too much. A ReplicaSet WITH
// replicas adopts the restored pod as one of its own replicas — which is the
// ordinary case and creates no second pod — but the rename stopped it doing so,
// so the ReplicaSet found itself one replica short and created a NEW pod in the
// same second. That new pod carries no Velero restore-wait init container, it
// mounts the claim first, and the restored pod can then never mount it (on lab
// w1, 2026-09-28: "MountVolume.SetUp failed ... verifyMount: device already
// mounted", OpenEBS LVM refusing a second mount of one volume). The restored
// pod's init container never ran, its PodVolumeRestore stayed in phase "", and
// the restore sat InProgress for an ORDINARY one-replica Deployment.
//
// THE DANGER IS ONLY EVER THE ZERO-REPLICA REPLICASET. In a backup taken while
// a Deployment was rolling out, the PodVolumeBackup belongs to the OLD pod,
// whose ReplicaSet the backup holds at zero replicas; Velero restores that pod
// and strips its owner references, and the ReplicaSet adopts it BY ITS SELECTOR
// and deletes it at once, because its desired count is zero. So the rule moves
// the hash on the ReplicaSet whose count is zero and leaves every pod's labels
// alone.
//
// `matches` IS HOW "THE COUNT IS ZERO" IS READ. Velero evaluates a rule's
// `matches` entries as JSON Patch `test` operations against the object as the
// BACKUP holds it, and reads a failed or missing test as "this rule does not
// match" (internal/resourcemodifiers/resource_modifiers.go, matchConditions,
// v1.18.1 — the version chart 12.1.0 in pkg/bundles/manifests/platform-1.1.yaml
// ships). The CronJob rules above use the same shape for /spec/suspend. The
// value is written as the string "0" because Velero sends it as the number 0,
// which is what spec.replicas is.
//
// BOTH HALVES OF THE HASH, OR THE API REFUSES THE OBJECT: a ReplicaSet's
// spec.selector must match its pod template, so the merge patch writes the held
// hash into spec.selector.matchLabels AND spec.template.metadata.labels. A
// merge patch — not a JSON patch `replace` — for the same reason mode 2 uses
// one: it creates the missing intermediate objects and carries JSON types.
//
// WHAT IT BUYS. A zero-replica ReplicaSet restored this way selects none of the
// restored pods, so it cannot adopt and delete the pod its PodVolumeRestore
// fills; a ReplicaSet with replicas is never matched, keeps its selector, and
// adopts its restored pod as the replica it is.
func heldReplicaSetRule() map[string]any {
	held := `"pod-template-hash":"` + HeldReplicaSetHash + `"`
	return map[string]any{
		"conditions": map[string]any{
			"groupResource": "replicasets.apps",
			"matches":       []any{map[string]any{"path": "/spec/replicas", "value": "0"}},
		},
		"mergePatches": []any{
			map[string]any{"patchData": `{"spec":{"selector":{"matchLabels":{` + held + `}},"template":{"metadata":{"labels":{` + held + `}}}}}`},
		},
	}
}

// namespaceModifierDocument is mode 1's resource modifier: the CronJob rules,
// and the rule that holds a zero-replica ReplicaSet out of the restored pods'
// selector.
//
// THE REPLICASET RULE IS ALWAYS IN THE DOCUMENT. It reads nothing — not the
// backup, not the namespace — and Velero simply finds no match when the backup
// holds no zero-replica ReplicaSet, so carrying it is harmless. Being static is
// also what lets a resumed run build exactly the document the interrupted run
// already applied, without re-reading the backup: the old rule named the pods
// the backup had copied a volume for, and re-deriving that list was the only
// reason the document depended on the backup at all.
func namespaceModifierDocument(operationID string) (*modifierConfigMap, error) {
	return modifierDocument(operationID, append(cronJobModifierRules(), heldReplicaSetRule()))
}

// recordSuspendedCronJobs reads the CronJobs the restore brought back, records
// what activation must put back, and refuses — naming them — any that are not
// suspended.
//
// THE OBJECT'S OWN VALUE IS NOT THE RECORD. The restore's resource modifier
// suspended every CronJob as Velero created it, so reading spec.suspend here
// would record "true" for all of them and activation would leave the
// namespace's scheduled work switched off. The backup's value is the annotation
// that same modifier wrote.
//
// THE REFUSAL IS THE POINT, and the run does not patch its way out of it. A
// CronJob that is unsuspended can create a Job before activation, which is the
// bug this restore was changed to close; a write from here is exactly the
// window the modifier exists to remove, and by the time it is noticed a Job may
// already have run. So the CronJobs are named, the project stays paused, and
// the operator — not this run — decides what the namespace's state should be.
func (r *restoreRun) recordSuspendedCronJobs(ctx context.Context) error {
	jobs, err := r.deps.Cluster.CronJobs(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	var unsuspended, unrecorded []string
	for _, job := range jobs {
		switch {
		case !job.Suspend:
			unsuspended = append(unsuspended, job.Name)
		case job.RecordedSuspend != "true" && job.RecordedSuspend != "false":
			unrecorded = append(unrecorded, job.Name)
		}
	}
	if len(unsuspended) > 0 {
		return fmt.Errorf("the restore left CronJob(s) %s in namespace %s unsuspended, so nothing stops them creating Jobs before activation. The restore's resource modifier is what suspends every CronJob as Velero creates it, and this operation does not patch them afterwards: that write is the window the modifier exists to close, and a Job these CronJobs created may already have run. Nothing has been activated and the namespace stays paused, so suspend them by hand and check what they created; if the namespace should not be in this state at all, `kubenest backup restore --abort %s` gives up on the operation and leaves the project paused",
			strings.Join(unsuspended, ", "), r.opts.Namespace, r.handle.OperationID())
	}
	if len(unrecorded) > 0 {
		return fmt.Errorf("CronJob(s) %s in namespace %s are suspended but carry no %s annotation, so the value the backup held for them is unknown and this operation cannot put them back at activation. The restore's resource modifier writes that annotation as it suspends each CronJob, so these did not come back through it; `kubenest backup restore --abort %s` gives up on the operation if that is not the state the backup should have brought back. Nothing has been activated and the namespace stays paused",
			strings.Join(unrecorded, ", "), r.opts.Namespace, CronJobSuspendedAnnotationKey, r.handle.OperationID())
	}
	for _, job := range jobs {
		if err := r.recordPending(ctx, "cronjob/"+job.Name, "restore-cronjob", job.RecordedSuspend); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  cronjob:       %s is suspended by the restore (the backup had it suspended=%s)\n", job.Name, job.RecordedSuspend)
	}
	return nil
}

// holdRestoredWork holds what must not run before activation: every CronJob the
// restore brought back — already suspended by the restore's own resource
// modifier, whose record of the backup's value is checked and kept here — and,
// when --include-jobs restored them on purpose, every Job, because a Job object
// that appears unsuspended starts its pod at once and Velero has no modifier
// step of ours in the way for it.
func (r *restoreRun) holdRestoredWork(ctx context.Context) error {
	if err := r.recordSuspendedCronJobs(ctx); err != nil {
		return err
	}
	if !r.opts.IncludeJobs {
		return nil
	}
	jobs, err := r.deps.Cluster.Jobs(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		was := "false"
		if job.Suspend {
			was = "true"
		}
		if err := r.recordPending(ctx, "job/"+job.Name, "restore-job", was); err != nil {
			return err
		}
		if err := r.stage("suspend-job/"+job.Name).SuspendJob(ctx, r.opts.Namespace, job.Name, true); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  job:           %s suspended (it was suspended=%s before); it runs at activation\n", job.Name, was)
	}
	return nil
}

// markAwaitingActivation records the state the operation stops in: the data is
// back, the project is still paused, and nothing may run until an operator
// says so.
func (r *restoreRun) markAwaitingActivation(ctx context.Context) error {
	if err := r.deps.Store.Update(ctx, r.handle, func(rec *operation.Record) error {
		rec.Stage = RestoredStage
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "  state:         %s — the project stays paused\n", RestoredStage)
	fmt.Fprintf(r.out, "  next:          kubenest backup restore --activate %s\n", r.handle.OperationID())
	fmt.Fprintf(r.out, "                 kubenest backup restore --resume %s\n", r.handle.OperationID())
	fmt.Fprintf(r.out, "                 kubenest backup restore --abort %s\n", r.handle.OperationID())
	return nil
}

// activate is `--activate <operation-id>`: the explicit decision to let the
// namespace run as before.
//
// IT TAKES THE LOCK, which means it obeys the same ownership rules as any other
// operation: a live executor still holding the record is a refusal, and a
// take-over needs that executor stopped with its actions reconciled.
func (r *restoreRun) activate(ctx context.Context) error {
	if err := r.resolve(ctx); err != nil {
		return err
	}
	record, err := r.restoreRecord(ctx, r.opts.Activate)
	if err != nil {
		return err
	}
	if r.opts.Namespace == "" {
		r.opts.Namespace = restoreNamespaceOf(record)
	}
	handle, err := operation.TakeOver(ctx, r.deps.Store, r.opts.Activate)
	if err != nil {
		return err
	}
	r.handle = handle
	record = handle.Record()

	hold, err := r.deps.Cluster.ProjectHold(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	if hold == nil || !strings.Contains(hold.ConditionMessage, r.opts.Activate) {
		return fmt.Errorf("Project %s/%s does not report a hold by operation %s, so activating it would lift a pause this operation does not hold. Nothing has been changed",
			ProjectCRNamespace, r.opts.Namespace, r.opts.Activate)
	}
	// THE RESTORED IDENTITIES ARE READ AGAIN, and they are different objects
	// from the ones the plan was pinned to: the restore recreated the namespace
	// and its claims, so their UIDs are new. Printing them is what lets an
	// operator see that activation is about the objects the restore made and
	// not about whatever was there before it.
	state, err := r.deps.Cluster.Namespace(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("namespace %s does not exist, so there is nothing to activate: the restore it belongs to has not put it back", r.opts.Namespace)
	}
	claims, err := r.deps.Cluster.Claims(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "  identity:      namespace %s is uid %s\n", r.opts.Namespace, state.UID)
	for _, claim := range claims {
		fmt.Fprintf(r.out, "  identity:      claim %s/%s is uid %s\n", claim.Namespace, claim.Name, claim.UID)
	}

	// What will start: the CronJobs this operation suspended, and any Jobs it
	// restored deliberately.
	jobs, err := r.deps.Cluster.CronJobs(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Activating operation %s for namespace %s.\n", r.opts.Activate, r.opts.Namespace)
	scheduled := 0
	for _, job := range jobs {
		state := r.pendingDetail(record, "cronjob/"+job.Name)
		if state == "" {
			continue
		}
		fmt.Fprintf(r.out, "  scheduled:     CronJob %s (%s) goes back to suspended=%s\n", job.Name, job.Schedule, state)
		scheduled++
	}
	if scheduled == 0 {
		fmt.Fprintf(r.out, "  scheduled:     no CronJob was suspended by this operation\n")
	}
	started := 0
	for _, pending := range record.Pending {
		if !strings.HasPrefix(pending.Target, "deployment/") && !strings.HasPrefix(pending.Target, "statefulset/") {
			continue
		}
		fmt.Fprintf(r.out, "  workload:      %s goes back to %s replica(s)\n", pending.Target, pending.Detail)
		started++
	}
	if started == 0 {
		fmt.Fprintf(r.out, "  workload:      no workload was scaled down by this operation\n")
	}

	differences, err := r.configurationDifferences(ctx)
	if err != nil {
		return err
	}
	if len(differences) > 0 {
		fmt.Fprintf(r.out, "  configuration: the restored state differs from the current desired state:\n")
		for _, d := range differences {
			fmt.Fprintf(r.out, "                   - %s\n", d)
		}
		switch {
		case r.opts.KeepDesired:
			fmt.Fprintf(r.out, "  configuration: --keep-desired: the desired state wins, and reconciliation will apply it\n")
		case r.opts.KeepRestored:
			fmt.Fprintf(r.out, "  configuration: --keep-restored: the restored state wins. Commit it to your GitOps repository before the next sync, because reconciliation rewrites it otherwise\n")
		default:
			confirmed, err := r.askWhichStateWins()
			if err != nil {
				return err
			}
			if !confirmed {
				return fmt.Errorf("the restored configuration differs from the current desired state and neither was chosen, so activating would let reconciliation decide. Nothing has been changed: re-run with --keep-restored or --keep-desired")
			}
		}
	}

	for _, job := range jobs {
		was := r.pendingDetail(record, "cronjob/"+job.Name)
		if was == "" {
			continue
		}
		if err := r.stage("activate-cronjob/"+job.Name).SuspendCronJob(ctx, r.opts.Namespace, job.Name, was == "true"); err != nil {
			return err
		}
	}
	// The ONE-SHOT work --include-jobs restored, started now and not before:
	// this is the "what may run" an operator is shown before they agree to it.
	restored, err := r.deps.Cluster.Jobs(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	for _, job := range restored {
		was := r.pendingDetail(record, "job/"+job.Name)
		if was == "" {
			continue
		}
		fmt.Fprintf(r.out, "  one-shot:      Job %s runs now (it was suspended=%s before the restore)\n", job.Name, was)
		if err := r.stage("activate-job/"+job.Name).SuspendJob(ctx, r.opts.Namespace, job.Name, was == "true"); err != nil {
			return err
		}
	}
	for _, pending := range record.Pending {
		if !strings.HasPrefix(pending.Target, "deployment/") && !strings.HasPrefix(pending.Target, "statefulset/") {
			continue
		}
		parts := strings.SplitN(pending.Target, "/", 2)
		replicas, err := parseReplicas(pending.Detail)
		if err != nil {
			return err
		}
		if err := r.stage("activate-"+pending.Target).ScaleWorkload(ctx, parts[0], parts[1], r.opts.Namespace, replicas); err != nil {
			return err
		}
	}
	// THE RESTORED PODS THE RESTORE OWES WORK FOR, WITH ONE DECISION EACH. A pod
	// a controller owns now is the workload's own pod — a ReplicaSet adopted it
	// as one of its replicas — so deleting it would only restart the workload,
	// and it stays. A pod no controller owns is the restore's own, which existed
	// to fill the volumes, so activation deletes it, in the same step that puts
	// the workloads back. A pod that is already gone is done: there is nothing
	// to delete, and the owed write is discharged below either way.
	var live map[string]PodState
	for _, pending := range record.Pending {
		if pending.Kind != "restore-pod" {
			continue
		}
		pod := strings.TrimPrefix(pending.Target, "pod/")
		if live == nil {
			pods, err := r.deps.Cluster.Pods(ctx, r.opts.Namespace)
			if err != nil {
				return err
			}
			live = make(map[string]PodState, len(pods))
			for _, restoredPod := range pods {
				live[restoredPod.Name] = restoredPod
			}
		}
		restoredPod, present := live[pod]
		if !present {
			continue
		}
		if owner := controllerOwnerOf(restoredPod); owner != nil {
			fmt.Fprintf(r.out, "  pod:           %s is the workload's own pod now (owned by %s %s), so it stays\n", pod, owner.Kind, owner.Name)
			continue
		}
		fmt.Fprintf(r.out, "  pod:           deleting restored pod %s — it existed only to fill the volumes\n", pod)
		if err := r.stage("activate-pod-"+pod).Delete(ctx, "pod "+pod+" -n "+r.opts.Namespace+" --wait=false"); err != nil {
			return err
		}
	}
	// The work this operation owed is now discharged, and a record that kept it
	// pending would report owed work that has been done.
	if err := r.deps.Store.Update(ctx, handle, func(rec *operation.Record) error {
		now := r.deps.Now()
		for i := range rec.Pending {
			if rec.Pending[i].Status == operation.WriteDone {
				continue
			}
			rec.Pending[i].Status = operation.WriteDone
			rec.Pending[i].DoneAt = &now
		}
		return nil
	}); err != nil {
		return err
	}
	if err := r.stage("activate-pause").ClearProjectAnnotation(ctx, r.opts.Namespace, PauseAnnotationKey); err != nil {
		return err
	}
	if err := r.deps.Store.Complete(ctx, handle, operation.ResultSucceeded); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Activated: the pause on Project %s/%s is lifted and the operation is closed\n", ProjectCRNamespace, r.opts.Namespace)
	return nil
}

func parseReplicas(raw string) (int32, error) {
	var n int32
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, fmt.Errorf("the record's replica count %q is not a number, so activating cannot put the workload back", raw)
	}
	return n, nil
}

// pendingDetail reads one owed write's detail out of the record.
func (r *restoreRun) pendingDetail(record *operation.Record, target string) string {
	for _, pending := range record.Pending {
		if pending.Target == target && pending.Status != operation.WriteDone {
			return pending.Detail
		}
	}
	return ""
}

// configurationDifferences reports how the restored state differs from the
// current desired state, as far as the cluster can tell: an Argo CD Application
// whose reported sync status is OutOfSync is the reconcilers saying exactly
// that.
func (r *restoreRun) configurationDifferences(ctx context.Context) ([]string, error) {
	apps, err := r.deps.Cluster.Applications(ctx, r.opts.Namespace)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, app := range apps {
		if app.SyncStatus == "" || app.SyncStatus == "Synced" {
			continue
		}
		out = append(out, fmt.Sprintf("%s reports %s", app.Name, app.SyncStatus))
	}
	return out, nil
}

func (r *restoreRun) askWhichStateWins() (bool, error) {
	fmt.Fprintf(r.out, "Which state wins — type restored or desired: ")
	if r.in == nil {
		return false, nil
	}
	line, err := bufio.NewReader(r.in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "restored":
		r.opts.KeepRestored = true
	case "desired":
		r.opts.KeepDesired = true
	default:
		return false, nil
	}
	return true, nil
}

// abort is `--abort <operation-id>`: give up on a restore that has not
// activated.
//
// IT LEAVES THE PROJECT PAUSED. Aborting is not activating, and a namespace
// left half-restored must not be reconciled over. The safety backup is never
// deleted here either: it is the last copy of what the namespace held.
func (r *restoreRun) abort(ctx context.Context) error {
	record, err := r.restoreRecord(ctx, r.opts.Abort)
	if err != nil {
		return err
	}
	if r.opts.Namespace == "" {
		r.opts.Namespace = restoreNamespaceOf(record)
	}
	handle, err := operation.TakeOver(ctx, r.deps.Store, r.opts.Abort)
	if err != nil {
		return err
	}
	r.handle = handle
	record = handle.Record()
	fmt.Fprintf(r.out, "Aborting operation %s for namespace %s. The project stays paused.\n", r.opts.Abort, r.opts.Namespace)
	for _, action := range record.Actions {
		fmt.Fprintf(r.out, "  %-10s %s: %s\n", action.Status, action.Stage, action.Postcondition)
	}
	fmt.Fprintf(r.out, "  project:       %s/%s keeps %s=%s, so nothing reconciles over what is there\n",
		ProjectCRNamespace, r.opts.Namespace, PauseAnnotationKey, r.opts.Abort)
	if err := r.deps.Store.Complete(ctx, handle, operation.Result("aborted")); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "  record:        %s marked aborted. The safety backup is kept; put the replicas and CronJobs you want back with\n", r.opts.Abort)
	fmt.Fprintf(r.out, "                 kubenest backup restore --activate %s   (after checking the namespace is in a state you want)\n", r.opts.Abort)
	return nil
}

// restoreRecord reads one restore operation's record and refuses anything that
// is not a restore or is already finished.
func (r *restoreRun) restoreRecord(ctx context.Context, operationID string) (*operation.Record, error) {
	stored, err := r.deps.Store.Find(ctx, operationID)
	if err != nil {
		return nil, err
	}
	if stored.Record.Terminal {
		return nil, fmt.Errorf("operation %s is terminal (%s): there is nothing to do for it", operationID, stored.Record.Result)
	}
	kind := stored.Record.Request.Kind
	if kind != operation.KindRestoreNamespace && kind != operation.KindRestoreVolume {
		return nil, fmt.Errorf("operation %s is a %s, not a restore", operationID, kind)
	}
	return stored.Record, nil
}

// restoreNamespaceOf reads the namespace a record is about. Mode 1 records it
// as an identity of its own; mode 2 records only the claims it refills, and
// both shapes name the namespace.
func restoreNamespaceOf(record *operation.Record) string {
	for _, artifact := range record.Request.Artifacts {
		if rest, ok := strings.CutPrefix(artifact.Name, "namespace/"); ok {
			return rest
		}
	}
	for _, artifact := range record.Request.Artifacts {
		if namespace, _ := claimIdentityOf(artifact.Name); namespace != "" {
			return namespace
		}
	}
	return ""
}

// restoreSpecs decides which of the commands this verb submits are ACTIONS —
// written down before submission, with a postcondition a successor can check —
// and what a successor can establish about each.
//
// Reads are not actions: the postcondition of a read is the read. Every stage
// name carries the object it is about (backup/<name>, scale/deployment/web),
// because "which of the twenty scale commands is this" is not a question a
// resume should have to answer from a hash.
func restoreSpecs(stage, command string) (operation.Spec, bool) {
	switch {
	case stage == "pause":
		if !strings.Contains(command, "annotate project ") {
			return operation.Spec{}, false
		}
		project := wordAfter(command, "project")
		opID := valueOf(command, PauseAnnotationKey+"=")
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: fmt.Sprintf("Project %s/%s carries %s=%s", ProjectCRNamespace, project, PauseAnnotationKey, opID),
			Observe: fmt.Sprintf(`test "$(sudo -n k3s kubectl get project %s -n %s -o jsonpath='{.metadata.annotations.kubenest\.io/reconcile-paused}')" = %s`,
				project, ProjectCRNamespace, opID),
		}, true
	case strings.HasPrefix(stage, "backup/"):
		name := strings.TrimPrefix(stage, "backup/")
		return operation.Spec{
			Kind:          operation.ActionRestore,
			Postcondition: "Velero Backup " + name + " exists",
			Observe:       "sudo -n k3s kubectl get backup " + name + " -n " + Namespace + " -o name",
		}, true
	case strings.HasPrefix(stage, "restore/"):
		name := strings.TrimPrefix(stage, "restore/")
		return operation.Spec{
			Kind:          operation.ActionRestore,
			Postcondition: "Velero Restore " + name + " exists",
			Observe:       "sudo -n k3s kubectl get restore " + name + " -n " + Namespace + " -o name",
		}, true
	case strings.HasPrefix(stage, "modifier/"):
		name := strings.TrimPrefix(stage, "modifier/")
		return operation.Spec{
			Kind:          operation.ActionRestore,
			Postcondition: "the restore resource modifier " + name + " exists",
			Observe:       "sudo -n k3s kubectl get configmap " + name + " -n " + Namespace + " -o name",
		}, true
	case strings.HasPrefix(stage, "scale/"):
		if !strings.Contains(command, "scale ") {
			return operation.Spec{}, false
		}
		kind := wordAfter(stage, "scale")
		name := lastWordOf(stage)
		namespace := wordAfter(command, "--namespace", "-n")
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: kind + " " + name + " in " + namespace + " has 0 replicas",
			Observe: fmt.Sprintf(`test "$(sudo -n k3s kubectl get %s %s -n %s -o jsonpath='{.spec.replicas}')" = 0`,
				kind, name, namespace),
		}, true
	case strings.HasPrefix(stage, "delete-namespace/"):
		name := strings.TrimPrefix(stage, "delete-namespace/")
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "namespace " + name + " is gone",
			Observe:       `test -z "$(sudo -n k3s kubectl get namespace ` + name + ` -o name 2>/dev/null)"`,
		}, true
	case strings.HasPrefix(stage, "delete-claim/"):
		namespace, name, ok := strings.Cut(strings.TrimPrefix(stage, "delete-claim/"), "/")
		if !ok {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "claim " + namespace + "/" + name + " is gone",
			Observe: fmt.Sprintf(`test -z "$(sudo -n k3s kubectl get persistentvolumeclaim %s -n %s -o name 2>/dev/null)"`,
				name, namespace),
		}, true
	case strings.HasPrefix(stage, "suspend-job/"):
		name := strings.TrimPrefix(stage, "suspend-job/")
		namespace := wordAfter(command, "-n")
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "Job " + namespace + "/" + name + " is suspended",
			Observe: fmt.Sprintf(`test "$(sudo -n k3s kubectl get job %s -n %s -o jsonpath='{.spec.suspend}')" = true`,
				name, namespace),
		}, true
	}
	return operation.Spec{}, false
}

// wordAfter returns the token that follows the LAST of the given flag words.
//
// It is the last one on purpose: every command this verb submits begins with
// `sudo -n k3s kubectl`, so the first "-n" is sudo's and the namespace flag is
// the closing one.
func wordAfter(command string, flags ...string) string {
	fields := strings.Fields(command)
	found := ""
	for i, field := range fields {
		for _, flag := range flags {
			if field == flag && i+1 < len(fields) {
				found = fields[i+1]
			}
		}
	}
	return found
}

func lastWordOf(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// valueOf returns the value of a KEY=value token, up to the next space.
func valueOf(command, key string) string {
	i := strings.Index(command, key)
	if i < 0 {
		return ""
	}
	rest := command[i+len(key):]
	if j := strings.IndexAny(rest, " \t\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// execute is mode 1: everything that changes something, in the plan's order.
//
// The record exists before the first line of it (openRecord), and the hold is
// the first change — a reconcile that recreated the namespace between the plan
// and the delete would restore into objects nobody planned against. The hold's
// own step stops everything in the namespace that can start work, before it
// waits for the acknowledgement and again after it (ensurePause); the writers
// themselves are scaled to zero later, because Velero's file-level copy of a
// claim needs the pod that mounts it running.
//
// EVERY STEP IS "DO IT, THEN MAKE SURE IT HAPPENED", which is what makes a
// `--resume` converge: a step whose action the record already holds is not
// submitted again, and the wait after it re-establishes the same fact.
func (r *restoreRun) execute(ctx context.Context) error {
	if err := r.ensurePause(ctx); err != nil {
		return err
	}
	if err := r.verifyIdentities(ctx); err != nil {
		return err
	}
	// THE MODIFIER IS BUILT BEFORE ANYTHING CHANGES and reads nothing: its
	// rules are static, so what it writes is the same before the safety backup,
	// after the namespace is gone, and on a resumed run.
	modifier, err := namespaceModifierDocument(r.handle.OperationID())
	if err != nil {
		return err
	}
	if err := r.safetyBackup(ctx); err != nil {
		return err
	}
	if err := r.scaleToZero(ctx); err != nil {
		return err
	}
	if err := r.deleteNamespace(ctx); err != nil {
		return err
	}
	restores, err := r.runVeleroRestore(ctx, "kubenest-restore-"+r.handle.OperationID(), restoreRequest{
		Backup:      r.plan.Backup.Name,
		Namespace:   r.opts.Namespace,
		IncludeJobs: r.opts.IncludeJobs,
		// THE POD A JOB CREATED IS NEVER RESTORED, with or without
		// --include-jobs (JobPodsExcluded): Velero skips only Succeeded or
		// Failed pods when it takes the backup, so a Job that was running then
		// has its pod in the backup, and excludedResources drops the Job and not
		// its pod — the orphan pod runs the Job's work again before activation.
		LabelExpressions: JobPodsExcluded(),
		// Mode 1's type filter is the DEFAULT one: the namespace comes back as
		// the backup holds it. The claims are what this restore is asked to
		// fill, and every one of them must have a Completed PodVolumeRestore.
		NamedVolumes: r.plan.Claims,
		// THE MODIFIER CARRIES BOTH: the CronJob suspension, so no restored
		// CronJob can fire between the restore and activation, and the
		// ReplicaSet hold, so a backup taken mid-rollout cannot have its
		// restored pod adopted and deleted by a zero-replica ReplicaSet before
		// its volume is filled (namespaceModifierDocument, heldReplicaSetRule).
		// A ReplicaSet with replicas is left alone: it adopts its restored pod
		// rather than starting a second one.
		ResourceModifier: modifier,
	})
	if err != nil {
		return err
	}
	// THE PODS THE RESTORE FILLED VOLUMES THROUGH ARE NOW OWED WORK, exactly as
	// mode 2's are: activation deletes the ones no controller owns, and the
	// workload's own controller creates the pods that take the claims over.
	if err := r.recordRestoredPods(ctx, restores); err != nil {
		return err
	}
	if err := r.holdRestoredWork(ctx); err != nil {
		return err
	}
	return r.markAwaitingActivation(ctx)
}

// recordRestoredPods records, as owed work, the pods the restore filled volumes
// through — the pods the PodVolumeRestores name.
//
// NOT EVERY ONE OF THEM IS THE RESTORE'S TO DELETE, and activation decides that
// when it runs (activate) by reading the pod's controller owner. A ReplicaSet
// the backup held with replicas keeps its selector, so it adopts the restored
// pod as one of its own replicas; that pod IS the workload's own pod, and
// deleting it only restarts the workload — on a claim a single node can mount
// once (OpenEBS LVM) the pod that restart creates cannot mount the claim while
// the deleted pod's mount is still going away. So the record is not a list of
// deletions: it is the list of what may still be owed.
func (r *restoreRun) recordRestoredPods(ctx context.Context, restores []VolumeRestoreState) error {
	seen := map[string]bool{}
	for _, restore := range restores {
		if restore.Pod == "" || seen[restore.Pod] {
			continue
		}
		seen[restore.Pod] = true
		if err := r.recordPending(ctx, "pod/"+restore.Pod, "restore-pod", restore.Pod); err != nil {
			return err
		}
	}
	return nil
}
