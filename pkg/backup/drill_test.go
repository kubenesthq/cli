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
