package controlplane

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/sshx"
)

// The on-demand checkpoint (kn-t47): `kubenest backup now --control-plane`.
//
// TWO PROPERTIES, and the first is the one a recovery depends on:
//
//   - the Job is DERIVED FROM THE CHART'S CronJob — the image digest, the
//     scratch volume, the service account and the fleet recipient all come from
//     the object the chart installed — under a name unique to this run; and
//   - the wait is for the CHECKPOINT, not for the Job. A Job that reaches
//     `Complete` while nothing new is eligible has produced nothing: the
//     runner publishes eligibility as its LAST write, after every object is
//     uploaded and read back, so completion without a new eligible marker is
//     exactly the interrupted run this must not report as a backup.
//
// The second test is the other verdict: a failed Job is final (backoffLimit 0)
// and is reported with the command that reads its log, rather than waited out.

var (
	checkpointStatusCmd = "sudo -n k3s kubectl get configmap " + CheckpointStatusConfigMap +
		" -n " + Namespace + " -o json"
	checkpointCreatePrefix = "sudo -n k3s kubectl create job --from=cronjob/" + CheckpointCronJobName
	checkpointJobGetPrefix = "sudo -n k3s kubectl get job "
)

// checkpointMarkerJSON answers `kubectl get configmap ... -o json` with the
// document the chart creates and the runner merges into. A nil marker is the
// install-time document: the field is null, which is "no checkpoint has been
// published yet" and not "an unreadable status".
func checkpointMarkerJSON(t *testing.T, marker map[string]any) string {
	t.Helper()
	status, err := json.Marshal(map[string]any{
		"management_cluster_id": "0f2e7c1a-0000-4000-8000-00000000cafe",
		"latest_eligible":       marker,
		"drill":                 map[string]any{"status": "never_run"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": CheckpointStatusConfigMap, "namespace": Namespace},
		"data":       map[string]any{"status": string(status)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func checkpointMarker(key string, sizeBytes int64) map[string]any {
	return map[string]any{
		"key":                         key,
		"at":                          "2026-09-25T12:00:00Z",
		"size_bytes":                  sizeBytes,
		"sha256":                      "e3b0c44298fc1c149afbf4c8996fb924",
		"postgres_major":              16,
		"postgres_image":              "postgres:16@sha256:deadbeef",
		"control_plane_version":       "3.0.0",
		"retention_seconds":           14 * 24 * 3600,
		"security_change_at":          "",
		"includes_security_change_at": "",
	}
}

// checkpointJobJSON is the answer `kubectl get job <name> -o json` gives.
func checkpointJobJSON(t *testing.T, name, condition, message string) string {
	t.Helper()
	status := map[string]any{"succeeded": 0, "failed": 0}
	if condition != "" {
		status["conditions"] = []any{map[string]any{
			"type": condition, "status": "True", "reason": "BackoffLimitExceeded", "message": message,
		}}
		if condition == "Complete" {
			status["succeeded"] = 1
		} else {
			status["failed"] = 1
		}
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": Namespace},
		"status":   status,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// jobNameOf reads the Job name back out of the create command the run issued.
var jobNameOf = regexp.MustCompile(`^sudo -n k3s kubectl create job --from=cronjob/` + CheckpointCronJobName + ` (\S+) -n ` + Namespace + `$`)

const (
	oldCheckpointKey = "control-plane/2026-09-25T020000Z-nightly"
	newCheckpointKey = "control-plane/2026-09-25T120000Z-nightly"
)

// The whole property: the Job comes from the CronJob under a name of its own,
// and "done" means a NEW eligible checkpoint — never just a finished pod.
func TestOnDemandCheckpointCreatesAJobFromTheCronJobAndWaitsForEligibility(t *testing.T) {
	var (
		created     []string
		jobReads    int
		markerReads int
	)
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case command == checkpointStatusCmd:
			markerReads++
			// The baseline, then the OLD marker twice more: the second of
			// those is the observation the planted negative reports as done —
			// a Job that completed while nothing was published.
			if markerReads >= 4 {
				return sshx.Result{Stdout: checkpointMarkerJSON(t, checkpointMarker(newCheckpointKey, 4096))}, nil
			}
			return sshx.Result{Stdout: checkpointMarkerJSON(t, checkpointMarker(oldCheckpointKey, 1024))}, nil
		case strings.HasPrefix(command, checkpointCreatePrefix):
			created = append(created, command)
			return sshx.Result{}, nil
		case strings.HasPrefix(command, checkpointJobGetPrefix):
			jobReads++
			name := strings.TrimSuffix(strings.TrimPrefix(command, checkpointJobGetPrefix), " -n "+Namespace+" -o json")
			if jobReads == 1 {
				return sshx.Result{Stdout: checkpointJobJSON(t, name, "", "")}, nil
			}
			return sshx.Result{Stdout: checkpointJobJSON(t, name, "Complete", "Job completed")}, nil
		default:
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{}, nil
	}}

	// 1 ms between observations: the wait is the same code the CLI runs, only
	// not against a 5 s poll.
	run, err := onDemandCheckpoint(context.Background(), r, 30*time.Second, time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(created) != 1 {
		t.Fatalf("created %d Jobs, want exactly one:\n%v", len(created), created)
	}
	match := jobNameOf.FindStringSubmatch(created[0])
	if match == nil {
		t.Fatalf("the Job was not created from the chart's CronJob %s:\n%s", CheckpointCronJobName, created[0])
	}
	job := match[1]
	if job == CheckpointCronJobName {
		t.Errorf("the Job is named after the CronJob (%s): a second run would collide with the first", job)
	}
	if run.Job != job {
		t.Errorf("reported Job %q, want the one created (%q)", run.Job, job)
	}

	// THE LOAD-BEARING ASSERTION. The Job was Complete before the new
	// checkpoint was published, so a wait that accepted completion alone would
	// return the marker it already had — and an interrupted upload would be
	// reported as a backup that exists.
	if run.Checkpoint.Key != newCheckpointKey {
		t.Errorf("reported checkpoint %q, want %q: completion is not eligibility", run.Checkpoint.Key, newCheckpointKey)
	}
	if run.Checkpoint.SizeBytes != 4096 {
		t.Errorf("reported size %d, want the published marker's 4096", run.Checkpoint.SizeBytes)
	}
	if markerReads < 4 {
		t.Errorf("the eligibility marker was read %d time(s): the wait never looked at what a checkpoint run publishes", markerReads)
	}
	if jobReads < 2 {
		t.Errorf("the Job was observed %d time(s), want it polled until it completed", jobReads)
	}
}

// A failed Job is a verdict: backoffLimit 0 means nothing retries it, so the
// run says what failed and how to read it instead of burning the deadline.
func TestOnDemandCheckpointReportsAFailedJob(t *testing.T) {
	const message = "Job has reached the specified backoff limit"
	start := time.Now()
	var jobReads int
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case command == checkpointStatusCmd:
			return sshx.Result{Stdout: checkpointMarkerJSON(t, checkpointMarker(oldCheckpointKey, 1024))}, nil
		case strings.HasPrefix(command, checkpointCreatePrefix):
			return sshx.Result{}, nil
		case strings.HasPrefix(command, checkpointJobGetPrefix):
			jobReads++
			name := strings.TrimSuffix(strings.TrimPrefix(command, checkpointJobGetPrefix), " -n "+Namespace+" -o json")
			return sshx.Result{Stdout: checkpointJobJSON(t, name, "Failed", message)}, nil
		default:
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{}, nil
	}}

	run, err := onDemandCheckpoint(context.Background(), r, 10*time.Second, time.Millisecond, nil)
	if err == nil {
		t.Fatal("a failed checkpoint Job was reported as success: the operator would believe a recovery point exists")
	}
	if run.Job == "" {
		t.Error("the failure does not name the Job that failed")
	}
	for _, want := range []string{run.Job, message, "kubectl logs job/" + run.Job + " -n " + Namespace} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q must name %q so the log can be read without guessing", err, want)
		}
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the failure took %s: a Job that has already failed must not be waited out to the deadline", time.Since(start))
	}
	if jobReads != 1 {
		t.Errorf("the failed Job was observed %d times, want one observation to reach the verdict", jobReads)
	}
}

// The wait has no built-in deadline: it is the bundle's component-ready
// timeout, so a manifest that does not carry one is refused rather than given a
// constant, and no Job is created first.
func TestOnDemandCheckpointNeedsTheBundlesDeadline(t *testing.T) {
	bundle := installBundle(t)
	bundle.Limits.Timeouts = nil

	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		t.Fatalf("a command was run before the deadline was known: %q", command)
		return sshx.Result{}, nil
	}}
	if _, err := OnDemandCheckpoint(context.Background(), r, bundle, converge.ReporterFunc(func(converge.Event) {})); err == nil {
		t.Fatal("an on-demand checkpoint without a bundle deadline was accepted")
	}
	if len(r.Commands()) != 0 {
		t.Errorf("commands were run with no deadline to bound them: %v", r.Commands())
	}
}
