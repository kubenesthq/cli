package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
)

// `kubenest backup restore` (plan 7.5, kn-x0wv / kn-t43).
//
// Two modes, one command. Mode 1 puts a whole NAMESPACE back into the live
// cluster: it chooses an eligible backup, prints a plan of what will change,
// waits for the operator's confirmation, pauses the project's reconcilers,
// stops the namespace's scheduled work and takes a safety backup of the
// namespace as it stands, stops the writers, deletes the namespace and restores
// it. Mode 2 (restore_volumes.go) does the node-loss path: one workload's
// stranded claims, refilled in place while the volumes of the same workload
// that are still fine keep their newer data.
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
	// StopCronJobWrite is the record kind naming the work the stop step did on
	// one CronJob of the namespace as it stood. It is written ALREADY
	// DISCHARGED (operation.WriteDone): it answers "what did the stop step do",
	// and it is not work activation owes.
	StopCronJobWrite = "stop-cronjob"
	// StoppedCronJobTarget is where that record entry points. It is its own
	// target, never "cronjob/<name>", so it can never be read back as the value
	// activation puts a restored CronJob back to — those come from the backup,
	// in kn-x0wv.3's annotation.
	StoppedCronJobTarget = "cronjob-stop/"
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
func (r *restoreRun) buildPlan(ctx context.Context) error {
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
		fmt.Fprintf(r.out, "  mode:          namespace — the namespace is deleted and restored; its CronJobs are suspended before the safety backup, Jobs are %s, and every CronJob the restore creates is suspended by the restore itself (a Velero resource modifier), so none can fire before activation\n", includeJobsWord(r.opts.IncludeJobs))
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

// ensurePause writes the pause annotation on the in-cluster Project and waits
// for the operator to acknowledge it.
func (r *restoreRun) ensurePause(ctx context.Context) error {
	opID := r.handle.OperationID()
	if err := r.stage("pause").AnnotateProject(ctx, r.opts.Namespace, PauseAnnotationKey, opID); err != nil {
		return fmt.Errorf("%w. Without the hold, reconciliation would recreate the namespace or sync over the restore", err)
	}
	fmt.Fprintf(r.out, "  pause:         %s=%s written on Project %s/%s\n", PauseAnnotationKey, opID, ProjectCRNamespace, r.opts.Namespace)
	return r.waitPauseAcknowledged(ctx, opID)
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

// suspendCronJobs stops the namespace's SCHEDULED work as part of the stop step.
//
// WHY IT RUNS BEFORE THE SAFETY BACKUP. A CronJob left running creates a Job
// whenever its schedule comes due, and the namespace is not deleted until after
// the safety backup — so a scheduled run can start against the emptied
// namespace while the restore is under way. On hardware (S4, lab w3,
// 2026-09-27) the recreated namespace's `s4-sentinel` fired in exactly that
// window, and the Job it created went with the namespace, leaving no record of
// it. Scaling the workloads to zero does not stop that: a CronJob's Job starts
// its own pod. So the schedules are suspended first, and the workloads are
// scaled to zero after the safety backup, because Velero's file-level copy of a
// claim needs the pod that mounts it to be running.
//
// WHAT IT RECORDS IS NOT WHAT ACTIVATION USES. Each CronJob's value before this
// step goes into the record under its own target, so "what did the stop step do"
// is answerable; activation puts each RESTORED CronJob back to the value the
// BACKUP held, read from the annotation the restore's own resource modifier
// wrote (kn-x0wv.3), and never from here.
func (r *restoreRun) suspendCronJobs(ctx context.Context) error {
	jobs, err := r.deps.Cluster.CronJobs(ctx, r.opts.Namespace)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Fprintf(r.out, "  stop:          no CronJob in namespace %s to suspend\n", r.opts.Namespace)
		return nil
	}
	for _, job := range jobs {
		was := "false"
		if job.Suspend {
			was = "true"
		}
		if err := r.stage("stop-cronjob/"+job.Name).SuspendCronJob(ctx, r.opts.Namespace, job.Name, true); err != nil {
			return fmt.Errorf("suspending CronJob %s in namespace %s before anything is captured or destroyed: %w", job.Name, r.opts.Namespace, err)
		}
		if err := r.recordStoppedCronJob(ctx, job.Name, was); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "  stop:          CronJob %s suspended (it was suspended=%s); activation takes its value from the backup, not from this step\n", job.Name, was)
	}
	return nil
}

// recordStoppedCronJob writes down, in the operation, that the stop step
// suspended one CronJob and what its value was before, as a write that is
// already DISCHARGED: it is a record of what happened rather than work
// activation owes. It is written AFTER the patch returned, so the record never
// claims a suspension that did not happen.
//
// ITS TARGET IS ITS OWN ("cronjob-stop/<name>"). Activation reads the value it
// puts a restored CronJob back to from "cronjob/<name>", so a record entry here
// can never be mistaken for it.
func (r *restoreRun) recordStoppedCronJob(ctx context.Context, name, was string) error {
	target := StoppedCronJobTarget + name
	now := r.deps.Now()
	return r.deps.Store.Update(ctx, r.handle, func(rec *operation.Record) error {
		for i := range rec.Pending {
			if rec.Pending[i].Target != target || rec.Pending[i].Kind != StopCronJobWrite {
				continue
			}
			rec.Pending[i].Detail = was
			rec.Pending[i].Status = operation.WriteDone
			rec.Pending[i].DoneAt = &now
			return nil
		}
		rec.Pending = append(rec.Pending, operation.PendingWrite{
			Kind:   StopCronJobWrite,
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
	if err := checkVolumeRestores(outcome, restores, spec.NamedVolumes); err != nil {
		return nil, err
	}
	fmt.Fprintf(r.out, "  restore:       %s finished as %s with %d error(s) and %d warning(s)%s\n",
		name, outcome.Phase, outcome.Errors, outcome.Warnings, partialNote(len(restores)))
	return restores, nil
}

// partialNote explains the ONE non-Completed verdict mode 2 expects: removing a
// volume from a restored pod makes Velero report an error for it, and that
// error is the price of the unnamed claim keeping its newer data (probe P5).
func partialNote(volumeRestores int) string {
	if volumeRestores == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d PodVolumeRestore(s) completed)", volumeRestores)
}

// waitRestore polls one Velero Restore until it reaches a terminal phase.
func (r *restoreRun) waitRestore(ctx context.Context, name string) (*RestoreOutcome, error) {
	end := r.deps.Now().Add(r.restoreTimeout)
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
		if err := r.deps.Sleep(ctx, r.deps.Poll); err != nil {
			return nil, err
		}
	}
}

// checkVolumeRestores refuses a restore that is not Completed, and refuses one
// that is Completed without having filled every volume it was asked to fill.
//
// "COMPLETED WITH 0 ERRORS" IS NOT PROOF. Run 4 of probe P5 reported exactly
// that and restored nothing: without persistentvolumes in the type filter
// Velero creates no PodVolumeRestore at all, injects its restore-wait init
// container anyway, and the pod waits for ever. So every named volume must have
// a Completed PodVolumeRestore of its own.
func checkVolumeRestores(outcome *RestoreOutcome, restores []VolumeRestoreState, named []VolumeRef) error {
	var incomplete []string
	completed := map[string]bool{}
	for _, vr := range restores {
		if vr.Phase == "Completed" {
			completed[vr.ClaimName] = true
			completed[vr.Volume] = true
			continue
		}
		incomplete = append(incomplete, fmt.Sprintf("%s (pod %s, volume %s, phase %s: %s)", vr.Name, vr.Pod, vr.Volume, orUnknown(vr.Phase), orUnknown(vr.Message)))
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
	if outcome.Phase != "Completed" {
		detail := fmt.Sprintf("Velero Restore %s is %s, not Completed: %d error(s), %d warning(s)", outcome.Name, outcome.Phase, outcome.Errors, outcome.Warnings)
		if outcome.FailureReason != "" {
			detail += ", failureReason: " + outcome.FailureReason
		}
		if len(incomplete) > 0 {
			detail += "; PodVolumeRestores that did not complete: " + strings.Join(incomplete, "; ")
		}
		if len(unfilled) > 0 {
			detail += "; volumes with no Completed PodVolumeRestore: " + strings.Join(unfilled, ", ")
		}
		return errors.New(detail)
	}
	if len(incomplete) > 0 {
		return fmt.Errorf("Velero Restore %s reports Completed, but these PodVolumeRestores did not: %s", outcome.Name, strings.Join(incomplete, "; "))
	}
	if len(unfilled) > 0 {
		return fmt.Errorf("Velero Restore %s reports Completed with %d error(s), but these volume(s) have no Completed PodVolumeRestore, so nothing proves they were filled: %s",
			outcome.Name, outcome.Errors, strings.Join(unfilled, ", "))
	}
	return nil
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
	// recorded is the merge patch that writes, on the CronJob, the value the
	// backup held for spec.suspend. It is a real JSON string in the object,
	// not the boolean spec.suspend is.
	recorded := func(wasSuspended string) string {
		return `{"metadata":{"annotations":{"` + CronJobSuspendedAnnotationKey + `":"` + wasSuspended + `"}}}`
	}
	rules := []any{
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
	return modifierDocument(operationID, rules)
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
	for _, pending := range record.Pending {
		if pending.Kind != "restore-pod" {
			continue
		}
		pod := strings.TrimPrefix(pending.Target, "pod/")
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
// and the delete would restore into objects nobody planned against.
//
// EVERY STEP IS "DO IT, THEN MAKE SURE IT HAPPENED", which is what makes a
// `--resume` converge: a step whose action the record already holds is not
// submitted again, and the wait after it re-establishes the same fact.
//
// THE STOP STEP STRADDLES THE SAFETY BACKUP. The namespace's CronJobs are
// suspended BEFORE it, because a due schedule can otherwise start a Job against
// the emptied namespace in the window between the backup and the delete; the
// workloads are scaled to zero AFTER it, because a claim's file-level copy
// needs the pod that mounts it running.
func (r *restoreRun) execute(ctx context.Context) error {
	if err := r.ensurePause(ctx); err != nil {
		return err
	}
	if err := r.verifyIdentities(ctx); err != nil {
		return err
	}
	if err := r.suspendCronJobs(ctx); err != nil {
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
	modifier, err := cronJobModifierDocument(r.handle.OperationID())
	if err != nil {
		return err
	}
	if _, err := r.runVeleroRestore(ctx, "kubenest-restore-"+r.handle.OperationID(), restoreRequest{
		Backup:      r.plan.Backup.Name,
		Namespace:   r.opts.Namespace,
		IncludeJobs: r.opts.IncludeJobs,
		// Mode 1's type filter is the DEFAULT one: the namespace comes back as
		// the backup holds it. The claims are what this restore is asked to
		// fill, and every one of them must have a Completed PodVolumeRestore.
		NamedVolumes: r.plan.Claims,
		// THE SUSPENSION TRAVELS WITH THE RESTORE (cronJobModifierDocument):
		// Velero suspends every CronJob as it creates it, so there is no
		// instant in which a restored CronJob can fire between the restore and
		// activation.
		ResourceModifier: modifier,
	}); err != nil {
		return err
	}
	if err := r.holdRestoredWork(ctx); err != nil {
		return err
	}
	return r.markAwaitingActivation(ctx)
}
