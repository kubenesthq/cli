package backup

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/operation"
)

// The test below is the restore's half of kn-yzuv: a restore whose laptop died
// leaves a record that still says its executor is `running` (the only writer of
// `stopped` needs the dead executor's own token), `--resume` refuses it, and
// `--take-over <id> --confirm` reconciles the recorded actions and continues
// the same restore.
//
// What it does NOT prove: that Velero or a real cluster behaves as this fake
// does, and nothing about activation, which is a separate command with its own
// tests.

func TestARestoreTakeOverReachesReconciliationOnARunningRecord(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	// The first run's restore never reaches a terminal phase: the wait runs out
	// and the process is gone before it can close its own record.
	f.cluster.outcome = &RestoreOutcome{Name: "r", Phase: "InProgress"}
	if _, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, ""); err == nil {
		t.Fatal("the interrupted run must fail")
	}
	opID := f.kube.recordFor(operation.Name).OperationID
	dead := leaveRunning(t, f.kube, opID)

	// The cluster is where the interrupted run left it: the pause is up and the
	// namespace is deleted. This is the second laptop, seeing the same world.
	resumed := newRestoreFixture(t, facts)
	resumed.kube = f.kube
	resumed.cluster.deleted = f.cluster.deleted
	resumed.cluster.namespace = f.cluster.namespace
	resumed.cluster.annotations = f.cluster.annotations

	// 1. --resume alone is refused, names --take-over, and leaves the record
	// exactly where the dead executor left it.
	out, err := runRestorePlan(t, resumed, RestoreOptions{Namespace: "payments", Resume: opID}, "")
	if err == nil {
		t.Fatalf("--resume accepted a record whose executor is still running:\n%s", out)
	}
	for _, want := range []string{"--take-over " + opID, "--confirm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if rec := f.kube.recordFor(operation.Name); rec.Terminal || rec.Executor.Token != dead.Executor.Token {
		t.Fatalf("the refused resume terminalized or re-owned the record (terminal=%v): the take-over it names would find nothing to take over", rec.Terminal)
	}

	// 2. --take-over --confirm reconciles and finishes the restore.
	out, err = runRestorePlan(t, resumed, RestoreOptions{Namespace: "payments", TakeOver: opID, Confirm: true}, "")
	if err != nil {
		t.Fatalf("--take-over failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Taking over operation "+opID) {
		t.Fatalf("--take-over did not reach the reconciliation:\n%s", out)
	}
	if !strings.Contains(out, RestoredStage) {
		t.Errorf("the take-over did not finish the restore at %q:\n%s", RestoredStage, out)
	}
	if resumed.cluster.annotations[PauseAnnotationKey] != opID {
		t.Errorf("the take-over lifted the pause: taking over is not activating")
	}
	rec := f.kube.recordFor(operation.Name)
	if len(rec.TakeOvers) != 1 {
		t.Fatalf("the record holds %d take-over assertion(s), want exactly 1: %+v", len(rec.TakeOvers), rec.TakeOvers)
	}
	if rec.TakeOvers[0].Replaced.Token != dead.Executor.Token {
		t.Errorf("the recorded assertion replaced %q, want the dead executor's token %q", rec.TakeOvers[0].Replaced.Token, dead.Executor.Token)
	}
	if !rec.TakeOvers[0].At.After(time.Time{}) || rec.TakeOvers[0].Operator == "" {
		t.Errorf("the assertion does not say who made it and when: %+v", rec.TakeOvers[0])
	}
}

// leaveRunning rewrites the record the fake cluster holds as a process that
// died before its closing write left it: still `running`, its request and its
// actions untouched. NOTHING IN THIS CLI PRODUCES THAT STATE, which is why only
// the operator's assertion can continue it (kn-yzuv, lab w3, 2026-09-27).
func leaveRunning(t *testing.T, k *fakeOpKube, opID string) operation.Record {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	obj, ok := k.objects[operation.Name]
	if !ok {
		t.Fatal("no operation record was created on the cluster")
	}
	data, _ := obj["data"].(map[string]any)
	raw, _ := data["record.json"].(string)
	var rec operation.Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("the record on the cluster is not a record: %v", err)
	}
	if rec.OperationID != opID {
		t.Fatalf("the live record is operation %s, want %s", rec.OperationID, opID)
	}
	rec.Terminal, rec.Result = false, ""
	rec.Executor.State = operation.ExecutorRunning
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	data["record.json"] = string(out)
	return rec
}
