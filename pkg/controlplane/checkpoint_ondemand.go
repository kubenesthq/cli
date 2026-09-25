// The on-demand half of the control plane's recovery path (kn-t47):
// `kubenest backup now --control-plane`.
//
// WHAT IT DOES, AND WHY IT IS NOT A SECOND IMPLEMENTATION OF A CHECKPOINT. The
// chart owns how a checkpoint is taken — the digest-pinned backend image, the
// dedicated scratch volume, the service account, the fleet recipient and the
// nightly retention all live in the CronJob it installs — and this creates a
// Job FROM that object, so an on-demand checkpoint cannot drift from a
// scheduled one. The only things this run decides are the Job's name and when
// to stop waiting.
//
// WHY IT WAITS FOR ELIGIBILITY AND NOT FOR THE POD. The runner
// (kubenest-backend app/services/checkpoint_runner.py) publishes the marker in
// the `control-plane-checkpoint-status` ConfigMap as its LAST write: the
// ciphertext is uploaded and read back by size, the manifest is uploaded and
// read back, and only then is the checkpoint marked eligible. A Job that
// reached `Complete` while nothing new is published is therefore an
// INTERRUPTED run, and reporting it as a backup would tell an operator a
// recovery point exists that does not. The wait is bounded by the bundle
// manifest's `limits.timeouts.component-ready`, the same deadline the install
// uses for the components it brings up, and a failed Job is a verdict rather
// than something to converge out of — backoffLimit is 0, so nothing retries it.
package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

const (
	// CheckpointCronJobName is the CronJob the chart installs
	// (kubenest-helm/kubenest/templates/checkpoint-cronjob.yaml,
	// `{{ .Release.Name }}-checkpoint` with ReleaseName = kubenest-cp). The
	// backend derives its security-triggered Jobs from the same object and
	// names it the same way (app/services/checkpoints.py).
	CheckpointCronJobName = ReleaseName + "-checkpoint"

	// CheckpointStatusConfigMap is where the runner publishes what is eligible,
	// and the drill's result. The chart creates it at install so that "no
	// eligible checkpoint yet" is distinguishable from "the CronJob was
	// deleted": an absent ConfigMap reads as unknown, this one as
	// unprotected-with-no-checkpoint. The chart renders it with metadata and
	// labels only — `data.status` is the runner's to patch, and a chart that
	// owned that key failed every upgrade after the first checkpoint
	// (TestTheChartDoesNotOwnTheCheckpointStatus).
	CheckpointStatusConfigMap = "control-plane-checkpoint-status"
	checkpointStatusKey       = "status"

	// checkpointJobReason is the Job name's own word for why it exists. A Job
	// created from the CronJob carries the scheduled reason in its env; the NAME
	// is what tells an operator, looking at `kubectl get jobs`, which run this
	// was.
	checkpointJobReason = "manual"
	// checkpointCheckName is what the wait reports itself as.
	checkpointCheckName = "kubenest-control-plane-checkpoint"
)

// EligibleCheckpoint is the `latest_eligible` entry the checkpoint runner
// publishes: what is in the bucket, sealed, and readable.
//
// Every field is the runner's own; nothing here is derived. `retention_seconds`
// and the Postgres major/image travel in the checkpoint's manifest as well, and
// the CLI reads them from here because the ConfigMap is what the control plane
// and the backend's `backup` verdict also read.
type EligibleCheckpoint struct {
	Key                      string `json:"key"`
	At                       string `json:"at"`
	SizeBytes                int64  `json:"size_bytes"`
	SHA256                   string `json:"sha256"`
	PostgresMajor            int    `json:"postgres_major"`
	PostgresImage            string `json:"postgres_image"`
	ControlPlaneVersion      string `json:"control_plane_version"`
	RetentionSeconds         int64  `json:"retention_seconds"`
	SecurityChangeAt         string `json:"security_change_at"`
	IncludesSecurityChangeAt string `json:"includes_security_change_at"`
}

// CheckpointRun is one on-demand checkpoint: the Job this run created and the
// eligible checkpoint it produced.
type CheckpointRun struct {
	Job        string
	Checkpoint EligibleCheckpoint
}

// ReadEligibleCheckpoint reads the newest eligible checkpoint the runner
// published, or nil when none has been published yet.
//
// An ABSENT ConfigMap IS "NONE YET", not an error: the chart creates it at
// install and the runner creates it if it is missing, so a namespace without one
// is a namespace where nothing has ever been published. A ConfigMap WITH NO
// `status` KEY is the same answer, and it is the shape every install starts in:
// the chart renders metadata and labels only, because `data.status` belongs to
// the runner that patches it. A ConfigMap that is there and unreadable IS an
// error — a status we cannot parse is not "no checkpoint", and reporting it as
// one would turn a broken publish into an on-demand run that silently waits out
// its deadline.
func ReadEligibleCheckpoint(ctx context.Context, r k3s.Runner) (*EligibleCheckpoint, error) {
	out, err := k3s.Kubectl(ctx, r, "get configmap "+CheckpointStatusConfigMap+" -n "+Namespace+" -o json")
	if err != nil {
		if configMapAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading the checkpoint status %s/%s: %w", Namespace, CheckpointStatusConfigMap, err)
	}
	var configMap struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &configMap); err != nil {
		return nil, fmt.Errorf("the checkpoint status ConfigMap %s/%s is not valid JSON: %w", Namespace, CheckpointStatusConfigMap, err)
	}
	// Not published yet: the chart's ConfigMap carries no `data` until the first
	// run writes one, so "no checkpoint has ever been recorded here" is the
	// honest baseline — `backup now --control-plane` waits for the first one
	// rather than refusing to start.
	raw, published := configMap.Data[checkpointStatusKey]
	if !published || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var document struct {
		LatestEligible *EligibleCheckpoint `json:"latest_eligible"`
	}
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return nil, fmt.Errorf("the %s key of ConfigMap %s/%s holds unreadable JSON: %w", checkpointStatusKey, Namespace, CheckpointStatusConfigMap, err)
	}
	return document.LatestEligible, nil
}

// configMapAbsent reports whether a read failed because the object is not there.
// kubectl's message is the only shape the CLI gets for this, and a 404 is not a
// failure of the read.
func configMapAbsent(err error) bool {
	return err != nil &&
		strings.Contains(err.Error(), "(NotFound)") &&
		strings.Contains(err.Error(), CheckpointStatusConfigMap)
}

// OnDemandCheckpoint creates a Job from the chart's checkpoint CronJob and
// waits until the checkpoint it produced is eligible.
//
// The wait is bounded by the bundle manifest's `component-ready` timeout: the
// number comes from the manifest and never from a default in this binary,
// because a bogus deadline is how a job that was going to succeed gets killed.
func OnDemandCheckpoint(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) (CheckpointRun, error) {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return CheckpointRun{}, err
	}
	return onDemandCheckpoint(ctx, r, deadline, 0, rep)
}

// onDemandCheckpoint is OnDemandCheckpoint with the poll interval exposed:
// converge's own default (5s) when zero, and a millisecond in tests, so the
// wait's logic is exercised without making every test pay the real cadence.
func onDemandCheckpoint(ctx context.Context, r k3s.Runner, deadline, interval time.Duration, rep converge.Reporter) (CheckpointRun, error) {
	// THE BASELINE FIRST. "The checkpoint it produced" is decided against what
	// was eligible BEFORE the Job existed, and reading it first is also what
	// keeps this from creating a Job it cannot judge: an unreadable status
	// fails here, with nothing started.
	before, err := ReadEligibleCheckpoint(ctx, r)
	if err != nil {
		return CheckpointRun{}, err
	}

	job := newCheckpointJobName(time.Now())
	res, err := k3s.Kubectl(ctx, r, "create job --from=cronjob/"+CheckpointCronJobName+" "+job+" -n "+Namespace)
	if err != nil {
		return CheckpointRun{}, fmt.Errorf(
			"creating a checkpoint Job from CronJob %s/%s: %w. The chart installs that CronJob only when the install was given a backup target, and a disabled one has to be enabled before the control plane can be backed up",
			Namespace, CheckpointCronJobName, err)
	}
	_ = res // kubectl prints the created Job; the name is the one we asked for

	wait := &checkpointWait{runner: r, job: job, before: before}
	probe := func(ctx context.Context) (bool, converge.State, error) {
		return wait.observe(ctx)
	}
	result, err := converge.Wait(ctx, probe, converge.Options{
		Name:     checkpointCheckName,
		Deadline: deadline,
		Interval: interval,
		Reporter: rep,
	})
	if err != nil {
		return CheckpointRun{Job: job}, err
	}
	if wait.failure != "" {
		// The wait saw the Job SETTLE, so it reports a pass; the terminal has to
		// hear the truth as well, because a green check followed by a failed
		// backup is a log nobody can read.
		if rep != nil {
			rep.Report(converge.Event{
				Check:    checkpointCheckName,
				Outcome:  converge.Fail,
				State:    result.Last,
				Elapsed:  result.Elapsed,
				Deadline: deadline,
			})
		}
		return CheckpointRun{Job: job}, fmt.Errorf("%s: %s", checkpointCheckName, wait.failure)
	}
	if err := result.Err(); err != nil {
		return CheckpointRun{Job: job}, fmt.Errorf(
			"%s: the checkpoint Job %s did not produce an eligible checkpoint: %w (read it with `kubectl logs job/%s -n %s`)",
			checkpointCheckName, job, err, job, Namespace)
	}
	if wait.done == nil {
		return CheckpointRun{Job: job}, fmt.Errorf("%s: the wait settled without an eligible checkpoint from Job %s", checkpointCheckName, job)
	}
	return CheckpointRun{Job: job, Checkpoint: *wait.done}, nil
}

// checkpointWait observes the Job and the eligibility marker together. Both
// have to hold: the Job completing is a fact about a pod, and the marker moving
// is a fact about the objects in the bucket.
type checkpointWait struct {
	runner k3s.Runner
	job    string
	before *EligibleCheckpoint

	// failure is set when the Job reached a Failed verdict, and done when a new
	// eligible checkpoint was published.
	failure string
	done    *EligibleCheckpoint
}

func (w *checkpointWait) observe(ctx context.Context) (bool, converge.State, error) {
	state, complete, failure, err := checkpointJobState(ctx, w.runner, w.job)
	if err != nil {
		return false, state, err
	}
	if failure != "" {
		// A verdict, not a state to converge out of: backoffLimit 0 means
		// Failed is final, and burning the deadline would hide the reason. The
		// message names the log, because the reason a pod failed is in it.
		w.failure = fmt.Sprintf(
			"the checkpoint Job %s failed: %s. The previous eligible checkpoint is still the newest one, so nothing has been lost; read the failure with `kubectl logs job/%s -n %s`",
			w.job, failure, w.job, Namespace)
		return true, state, nil
	}
	if !complete {
		return false, state, nil
	}
	marker, err := ReadEligibleCheckpoint(ctx, w.runner)
	if err != nil {
		return false, state, err
	}
	if !checkpointIsNew(marker, w.before) {
		state.Status = "Complete"
		state.Detail = "the Job finished, but no checkpoint has become eligible yet: eligibility is published only after every object is uploaded and read back"
		return false, state, nil
	}
	w.done = marker
	state.Status = "Complete"
	state.Detail = fmt.Sprintf("checkpoint %s eligible (%d bytes sealed)", marker.Key, marker.SizeBytes)
	return true, state, nil
}

// checkpointJobState observes the Job once. `complete` is true only on the Job
// controller's own Complete condition, and a Failed condition comes back as the
// reason to report — the same shape the migration step's wait uses.
func checkpointJobState(ctx context.Context, r k3s.Runner, job string) (converge.State, bool, string, error) {
	object := "job " + job + " in " + Namespace
	var observed struct {
		Status struct {
			Succeeded  int32 `json:"succeeded"`
			Failed     int32 `json:"failed"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	out, err := k3s.Kubectl(ctx, r, "get job "+job+" -n "+Namespace+" -o json")
	if err != nil {
		// The Job was created a moment ago, or the API server is briefly
		// unreachable; both are observations, not verdicts.
		return converge.State{Object: object, Status: "not found yet"}, false, "", err
	}
	if err := json.Unmarshal([]byte(out), &observed); err != nil {
		return converge.State{Object: object, Status: "unparsable"}, false, "", err
	}
	for _, condition := range observed.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		switch condition.Type {
		case "Complete":
			return converge.State{Object: object, Status: "Complete"}, true, "", nil
		case "Failed":
			reason := condition.Message
			if reason == "" {
				reason = condition.Reason
			}
			return converge.State{Object: object, Status: "Failed", Detail: reason}, false, reason, nil
		}
	}
	if observed.Status.Failed > 0 && observed.Status.Succeeded == 0 {
		return converge.State{Object: object, Status: fmt.Sprintf("%d pod(s) failed, no verdict yet", observed.Status.Failed)}, false, "", nil
	}
	return converge.State{Object: object, Status: "running"}, false, "", nil
}

// checkpointIsNew reports whether marker is a checkpoint this run could have
// produced, given what was eligible before the Job was created.
func checkpointIsNew(marker, before *EligibleCheckpoint) bool {
	if marker == nil || marker.Key == "" {
		return false
	}
	if before == nil || before.Key == "" {
		return true
	}
	if marker.Key != before.Key {
		return true
	}
	// The key is `<prefix><second-precision stamp>-<reason>`, so two runs of the
	// same reason inside one second share one key. The `at` the runner published
	// then decides; a marker that did not move is the checkpoint we already had,
	// and reporting it as this run's would be a lie.
	return marker.At > before.At
}

// OnDemandJobPrefix begins the name of every Job an on-demand run creates. It
// is exported because it is the one handle an operator — or a gate on real
// hardware — has for telling this run's Job apart from the schedule's and from
// the backend's security-triggered ones, and the two must not spell it
// differently.
func OnDemandJobPrefix() string { return CheckpointCronJobName + "-" + checkpointJobReason + "-" }

// newCheckpointJobName is unique per run. It has to be: the CronJob's Jobs, a
// previous on-demand run's Job and the backend's security-triggered Jobs all
// live in the same namespace, and two of them may be alive at once.
func newCheckpointJobName(at time.Time) string {
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		// crypto/rand failing is a broken host, and a name that is not unique
		// is a Job that does not get created. Fall back to nanoseconds rather
		// than to a fixed name.
		return fmt.Sprintf("%s%s-%d", OnDemandJobPrefix(), at.UTC().Format("20060102-150405"), at.UnixNano())
	}
	return fmt.Sprintf("%s%s-%s", OnDemandJobPrefix(), at.UTC().Format("20060102-150405"), hex.EncodeToString(suffix))
}
