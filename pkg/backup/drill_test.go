package backup

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/sshx"
)

// The measured defect: on a healthy cluster, `backup now` followed by
// `backup drill` waited out the bundle's two-hour `restore-drill` deadline and
// then failed, because the operator had already decided not to run — the newest
// backup completed before the drill's proof workload became ready, so restoring
// it would prove nothing — and said so only in its own logs.
//
// The operator now records the refusal on the result ConfigMap, in the same
// patch that marks the request done. These tests hold the CLI to reading it:
// with the decline read (the old code fell straight through to
// json.Unmarshal on the result) the wait runs to the deadline, which this test
// sets to two hours, so the red run is a two-hour test, not a one-line diff.
// The test therefore owns a deadline it does not need and asserts a bound it
// cannot fake.

var drillTokenPattern = regexp.MustCompile(`"run-now":"([^"]+)"`)

// declinedResultJSON is the result ConfigMap the operator leaves behind after a
// decline: the request token in the annotations (so the CLI settles), the
// refusal and its sentence beside it, and the untouched never-run evidence.
func declinedResultJSON(t *testing.T, token, code, detail string) string {
	t.Helper()
	record := map[string]any{"status": "never_run"}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode the never-run record: %v", err)
	}
	doc := map[string]any{
		"metadata": map[string]any{
			"name":      DrillResultName,
			"namespace": Namespace,
			"annotations": map[string]string{
				DrillRequestAnnotation:        token,
				DrillDeclinedAnnotation:       code,
				DrillDeclinedDetailAnnotation: detail,
			},
		},
		"data": map[string]string{DrillResultDataKey: string(raw)},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode the result ConfigMap: %v", err)
	}
	return string(out)
}

// newDeclineRunner answers the two commands RequestDrill issues and echoes the
// request token back, because the token is generated inside the function under
// test and the operator's answer has to carry the same one.
func newDeclineRunner(t *testing.T, code, detail string) *fakeRunner {
	t.Helper()
	var token string
	return &fakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.HasPrefix(command, "sudo -n k3s kubectl patch configmap "+DrillConfigName):
			match := drillTokenPattern.FindStringSubmatch(command)
			if match == nil {
				t.Fatalf("the drill request patch carries no token: %q", command)
			}
			token = match[1]
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get configmap "+DrillResultName):
			if token == "" {
				t.Fatal("the result was read before the request token was sent")
			}
			return sshx.Result{Stdout: declinedResultJSON(t, token, code, detail)}, nil
		}
		t.Fatalf("unscripted command: %q", command)
		return sshx.Result{}, nil
	}}
}

func TestDrillCommandPrintsTheRemedyWhenNoBackupIsEligible(t *testing.T) {
	// The sentence the operator writes: the newest completed backup, when it
	// finished, when the proof became ready, and the fix.
	const detail = "The newest completed backup manual-20260921-130959 finished at " +
		"2026-09-21T13:09:59Z, before the restore proof workload became ready at " +
		"2026-09-21T13:10:31Z, so it cannot contain the proof data and restoring it " +
		"would prove nothing. Run `kubenest backup now` and the drill runs on its next poll."

	bundle := testManifest()
	// The manifest is what bounds the wait, and this test hands it a real
	// bundle's two hours so that "returned at once" is measured against the
	// behaviour the bead recorded, not against a test-sized timeout.
	bundle.Limits.Timeouts["restore-drill"] = 2 * time.Hour

	var events []converge.Event
	rep := converge.ReporterFunc(func(e converge.Event) { events = append(events, e) })
	r := newDeclineRunner(t, DrillDeclineBackupPredatesProof, detail)

	// The context is the test's own bound, and it is far shorter than the
	// two-hour deadline on purpose: an implementation that kept waiting would
	// be cut off here and fail the elapsed assertion below, instead of sitting
	// in the suite for two hours.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	result, err := RequestDrill(ctx, r, bundle, rep)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatalf("a declined request returned success with %+v", result)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the declined request took %s; it must settle on the operator's refusal, not on a deadline", elapsed)
	}
	var declined *DrillDeclinedError
	if !errors.As(err, &declined) {
		t.Fatalf("error %v is not a DrillDeclinedError", err)
	}
	if declined.Code != DrillDeclineBackupPredatesProof {
		t.Errorf("code = %q, want %q", declined.Code, DrillDeclineBackupPredatesProof)
	}
	for _, want := range []string{
		"manual-20260921-130959",
		"2026-09-21T13:09:59Z",
		"2026-09-21T13:10:31Z",
		"kubenest backup now",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the operator's refusal does not reach the user: %q lacks %q", err.Error(), want)
		}
	}
	if result.Status == "passed" {
		t.Errorf("a declined drill was reported as a pass: %+v", result)
	}

	// Exactly one observation of the result: the request settles on the first
	// read that carries the token, rather than polling for a result that will
	// never be written.
	gets := 0
	for _, command := range r.Commands() {
		if strings.Contains(command, "get configmap "+DrillResultName) {
			gets++
		}
	}
	if gets != 1 {
		t.Errorf("read the result ConfigMap %d times, want 1", gets)
	}
	// The first thing the user is told is that the request was accepted, before
	// the wait starts. The wait itself settles on its first observation here, so
	// a converging event can only be that notice.
	if len(events) == 0 || events[0].Outcome != converge.Converging {
		t.Errorf("the command did not say the request was accepted before waiting: %+v", events)
	}
	for _, event := range events {
		if event.Outcome == converge.Pass && !strings.Contains(event.State.Status, "declined") {
			t.Errorf("reported a settled pass for a declined request: %+v", event)
		}
	}
}

// An operator that records a code but no sentence must still leave the user with
// the fix, because that is the whole point of the refusal being reported.
func TestDrillDeclinedWithoutDetailStillOffersTheRemedy(t *testing.T) {
	bundle := testManifest()
	bundle.Limits.Timeouts["restore-drill"] = time.Minute
	r := newDeclineRunner(t, DrillDeclineNoCompletedBackup, "")

	result, err := RequestDrill(context.Background(), r, bundle, nil)
	if err == nil {
		t.Fatalf("a declined request returned success with %+v", result)
	}
	if !strings.Contains(err.Error(), "kubenest backup now") {
		t.Errorf("the fallback refusal does not offer the fix: %q", err.Error())
	}
}

func drillWaitJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode drill wait fixture: %v", err)
	}
	return string(raw)
}

func pendingDrillRunner(t *testing.T, restores []any, volumes map[string][]any) *fakeRunner {
	t.Helper()
	result := drillWaitJSON(t, map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{}},
		"data":     map[string]string{DrillResultDataKey: `{"status":"never_run"}`},
	})
	restoreList := drillWaitJSON(t, map[string]any{"items": restores})
	return &fakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.HasPrefix(command, "sudo -n k3s kubectl patch configmap "+DrillConfigName):
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get configmap "+DrillResultName):
			return sshx.Result{Stdout: result}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get restores.velero.io -n "+Namespace):
			return sshx.Result{Stdout: restoreList}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get podvolumerestores.velero.io -n "+Namespace):
			for name, items := range volumes {
				if strings.Contains(command, "velero.io/restore-name="+name+" ") {
					return sshx.Result{Stdout: drillWaitJSON(t, map[string]any{"items": items})}, nil
				}
			}
		}
		t.Fatalf("unscripted drill wait command: %q", command)
		return sshx.Result{}, nil
	}}
}

func drillWaitRestore(name, phase string, created time.Time, drill bool) any {
	labels := map[string]string{}
	if drill {
		labels[DrillRestoreLabelKey] = DrillRestoreLabelValue
	}
	return map[string]any{
		"metadata": map[string]any{
			"name": name, "labels": labels,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"status": map[string]any{"phase": phase},
	}
}

func drillWaitVolume(name, phase string) any {
	return map[string]any{
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{
			"pod":    map[string]string{"namespace": "e2e-restore-ns", "name": "restored-web"},
			"volume": "data",
		},
		"status": map[string]string{"phase": phase},
	}
}

func runPendingDrillWait(t *testing.T, r *fakeRunner) ([]converge.Event, error) {
	t.Helper()
	bundle := testManifest()
	bundle.Limits.Timeouts["restore-drill"] = 20 * time.Millisecond
	var events []converge.Event
	rep := converge.ReporterFunc(func(e converge.Event) { events = append(events, e) })
	_, err := RequestDrill(context.Background(), r, bundle, rep)
	return events, err
}

func TestDrillWaitReportsTheRestoreThatHoldsVelerosWorker(t *testing.T) {
	const drill = "kubenest-restore-drill-queued"
	const foreign = "kubenest-restore-stuck"
	const stuckVolume = "kubenest-restore-stuck-pvr"
	r := pendingDrillRunner(t, []any{
		drillWaitRestore(foreign, "InProgress", time.Now().Add(-23*time.Minute), false),
		drillWaitRestore(drill, "", time.Now(), true),
	}, map[string][]any{
		foreign: {drillWaitVolume(stuckVolume, "New")},
		drill:   {},
	})

	events, err := runPendingDrillWait(t, r)
	if err == nil {
		t.Fatal("the unanswered drill request must expire at its bundle deadline")
	}
	var sawQueued bool
	for _, event := range events {
		if event.Outcome != converge.Converging && event.Outcome != converge.Fail {
			continue
		}
		state := event.State.String()
		if strings.Contains(strings.ToLower(state), "queued") &&
			strings.Contains(state, foreign) && strings.Contains(state, "InProgress") &&
			strings.Contains(state, "23m") && strings.Contains(state, stuckVolume) &&
			strings.Contains(state, "New") && strings.Contains(state, "restored-web") &&
			strings.Contains(strings.ToLower(state), "worker") {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Errorf("no progress event reported the queued drill and worker holder: %+v", events)
	}
	for _, want := range []string{foreign, "InProgress", stuckVolume, "New", "delete", "drill"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("deadline error %q does not carry %q", err, want)
		}
	}
}

func TestDrillWaitReportsItsOwnRestoreAndUnfinishedVolumes(t *testing.T) {
	const drill = "kubenest-restore-drill-working"
	const volume = "kubenest-restore-drill-working-pvr"
	r := pendingDrillRunner(t, []any{
		drillWaitRestore(drill, "InProgress", time.Now().Add(-time.Minute), true),
	}, map[string][]any{
		drill: {drillWaitVolume(volume, "New")},
	})

	events, err := runPendingDrillWait(t, r)
	if err == nil {
		t.Fatal("the unanswered drill request must expire at its bundle deadline")
	}
	var sawDrill bool
	for _, event := range events {
		state := event.State.String()
		if strings.Contains(state, drill) && strings.Contains(state, "InProgress") &&
			strings.Contains(state, volume) && strings.Contains(state, "New") &&
			!strings.Contains(strings.ToLower(state), "queued") {
			sawDrill = true
		}
	}
	if !sawDrill {
		t.Errorf("no progress event reported the active drill and its volume: %+v", events)
	}
	if !strings.Contains(err.Error(), drill) || !strings.Contains(err.Error(), volume) {
		t.Errorf("deadline error does not carry the drill's last state: %v", err)
	}
	if strings.Contains(err.Error(), "kubenest-restore-stuck") {
		t.Errorf("ordinary drill was attributed to a foreign restore: %v", err)
	}
}
