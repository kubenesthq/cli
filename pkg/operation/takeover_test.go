package operation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/sshx"
)

// The tests below are the acceptance criterion for kn-yzuv: an operation whose
// executor died without recording its stop has to be continuable, and the
// permission is the operator's own assertion rather than anything the CLI
// infers.
//
// What a green run does NOT prove: that an operator is right about the machine
// they are asserting about (the CLI cannot see the difference between a laptop
// that is asleep and one that is off, and 7.2 says so), that a real API server
// returns the conflict text fakeKube returns, and nothing here proves that any
// verb adopted the flag — the verb-level tests in pkg/cmd, pkg/node, pkg/backup
// and pkg/upgrade are what say that.

// TestADeadExecutorsRecordIsTakenOverOnlyWithTheOperatorsAssertion: an executor
// that died mid-action — the measured case (kn-yzuv, lab w3, 2026-09-27) — can
// never write `stopped` itself, so the record is taken over on the operator's
// explicit assertion and on nothing else.
//
// PLANTED NEGATIVE: today's code. The reconcile at step 1 establishes every
// action's outcome, and the claim that follows is refused because the record
// still says `running` — the node stays cordoned and every later command on the
// cluster is refused with "another operation holds the record".
func TestADeadExecutorsRecordIsTakenOverOnlyWithTheOperatorsAssertion(t *testing.T) {
	k := newFakeKube(t)
	ana := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, ana, testRequest("host-1"))

	const command = "sudo -n k3s kubectl cordon node-1"
	const observe = "sudo -n k3s kubectl get node node-1 -o jsonpath={.spec.unschedulable}"
	const postcondition = "node-1 is cordoned"
	id := ActionID("kubernetes", command)
	if err := h.RecordAction(ctx, id, "kubernetes", Spec{Kind: ActionSSH, Postcondition: postcondition, Observe: observe}); err != nil {
		t.Fatalf("recording the action: %v", err)
	}
	if err := h.SubmitAction(ctx, id); err != nil {
		t.Fatalf("submitting the action: %v", err)
	}
	// The laptop dies here: the action happened, its outcome was never written
	// back, and nothing wrote `stopped` — only the dead executor's own token
	// could have.
	k.script(observe, sshx.Result{})

	other := newStore(t, k, "bo@other-laptop")
	rv, token := k.resourceVersion(Name), h.Token()

	// 1. THE PLANTED NEGATIVE. The second laptop reconciles the record — that
	// part works and is not the bug — and the claim is then refused.
	plan, err := Resume(ctx, other, h.OperationID())
	if err != nil {
		t.Fatalf("reconciling the dead executor's record: %v", err)
	}
	if !plan.Skip()[id] {
		t.Fatalf("the recorded postcondition did not hold, so this test is not about the take-over rules: %+v", plan.Steps)
	}
	if _, err := TakeOver(ctx, other, h.OperationID()); !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("a resume of a record left running by a dead executor returned %v, want ErrTakeOverRefused", err)
	} else if !strings.Contains(err.Error(), "--take-over "+h.OperationID()) {
		t.Fatalf("the refusal does not name the way on (--take-over %s): %v", h.OperationID(), err)
	}
	if k.resourceVersion(Name) != rv || k.liveRecord(Name).Executor.Token != token {
		t.Fatal("a refused claim wrote to the record")
	}

	// 2. THE ASSERTION IS THE OPERATOR'S. A --take-over without its confirmation
	// is refused: the CLI must not make this assertion on anybody's behalf.
	if _, err := (Recovery{TakeOver: h.OperationID()}).Claim(ctx, other); !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("a take-over without the operator's confirmation returned %v, want ErrTakeOverRefused", err)
	} else if !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("the refusal does not name the confirmation it needs: %v", err)
	}
	if k.liveRecord(Name).Executor.Token != token {
		t.Fatal("an unconfirmed take-over wrote to the record")
	}

	// 3. With the assertion, the record changes hands and the successor carries
	// on from the reconciliation above.
	h2, err := (Recovery{TakeOver: h.OperationID(), Confirm: true}).Claim(ctx, other)
	if err != nil {
		t.Fatalf("an asserted take-over of a dead executor's record was refused: %v", err)
	}
	if h2.OperationID() != h.OperationID() || h2.Token() == token {
		t.Fatalf("the take-over returned operation %s token %s", h2.OperationID(), h2.Token())
	}
	rec := k.liveRecord(Name)
	if rec.Executor.Token != h2.Token() || rec.Executor.Operator != "bo@other-laptop" || rec.Executor.State != ExecutorRunning {
		t.Fatalf("the taken-over record's executor is %+v", rec.Executor)
	}
	if k.resourceVersion(Name) == rv {
		t.Fatal("the take-over did not write the record")
	}
	// The dead executor cannot come back and keep writing.
	if err := other.Update(ctx, h, func(r *Record) error { return nil }); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("the taken-over-from token wrote the record: %v", err)
	}
}

// TestTheTakeOverIsRecordedInTheRecord: a take-over replaces an executor the
// CLI could not observe, so the record has to say who asserted it, when, and
// which executor the assertion replaced. A claim that recorded nothing would be
// indistinguishable from theft.
func TestTheTakeOverIsRecordedInTheRecord(t *testing.T) {
	k := newFakeKube(t)
	ctx := context.Background()

	acquired := time.Date(2026, 9, 27, 4, 20, 0, 0, time.UTC)
	ana, setAna := clockStore(t, k, "ana@laptop")
	setAna(acquired)
	h := acquire(t, ana, testRequest("host-1"))
	deceased := h.Token()

	// The second laptop's clock, so the recorded instant is the assertion's and
	// not the reading of any heartbeat.
	assertedAt := acquired.Add(3*time.Hour + 17*time.Minute)
	bo, setBo := clockStore(t, k, "bo@other-laptop")
	setBo(assertedAt)
	h2, err := (Recovery{TakeOver: h.OperationID(), Confirm: true}).Claim(ctx, bo)
	if err != nil {
		t.Fatalf("the take-over was refused: %v", err)
	}

	rec := k.liveRecord(Name)
	if len(rec.TakeOvers) != 1 {
		t.Fatalf("the record holds %d take-over assertion(s), want exactly 1: %+v", len(rec.TakeOvers), rec.TakeOvers)
	}
	got := rec.TakeOvers[0]
	if got.Operator != "bo@other-laptop" {
		t.Errorf("the assertion is attributed to %q, want the operator who made it", got.Operator)
	}
	if !got.At.Equal(assertedAt) {
		t.Errorf("the assertion is recorded at %s, want %s", got.At, assertedAt)
	}
	if got.Replaced.Token != deceased || got.Replaced.Operator != "ana@laptop" {
		t.Errorf("the record does not name the executor the assertion replaced: %+v", got.Replaced)
	}
	if got.Replaced.State != ExecutorRunning || !got.Replaced.Heartbeat.Equal(acquired) {
		t.Errorf("the replaced executor is recorded as %+v, want the dead executor's own last state and heartbeat", got.Replaced)
	}
	if rec.Executor.Token != h2.Token() || rec.Executor.Operator != "bo@other-laptop" {
		t.Errorf("the executor the assertion installed is %+v, want this one", rec.Executor)
	}
}

// TestResumeOfARunningRecordNamesTakeOver: a resume continues an operation whose
// record says its executor stopped. A record that says `running` is refused, and
// the refusal has to name the command that does continue it — otherwise an
// operator whose laptop died is told "no" with no way on (kn-yzuv).
func TestResumeOfARunningRecordNamesTakeOver(t *testing.T) {
	k := newFakeKube(t)
	ana := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, ana, testRequest("host-1"))

	other := newStore(t, k, "bo@other-laptop")
	_, err := TakeOver(ctx, other, h.OperationID())
	if !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("a resume of a running record returned %v, want ErrTakeOverRefused", err)
	}
	for _, want := range []string{"--take-over " + h.OperationID(), "--confirm", "running"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if k.liveRecord(Name).Executor.Token != h.Token() {
		t.Fatal("the refused resume changed the record's owner")
	}
}

// TestATakeOverOfAStoppedOrTerminalRecordIsRefusedWithTheRightVerb: the
// assertion is for a record that cannot say `stopped` itself. One that already
// says it needs a resume, and a finished record needs nothing at all — a
// take-over that accepted either would be the wrong verb, and an operator
// following it would not do what they meant.
func TestATakeOverOfAStoppedOrTerminalRecordIsRefusedWithTheRightVerb(t *testing.T) {
	k := newFakeKube(t)
	ana := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, ana, testRequest("host-1"))
	other := newStore(t, k, "bo@other-laptop")

	if err := ana.Stop(ctx, h); err != nil {
		t.Fatalf("stopping the executor: %v", err)
	}
	token := k.liveRecord(Name).Executor.Token
	_, err := (Recovery{TakeOver: h.OperationID(), Confirm: true}).Claim(ctx, other)
	if !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("a take-over of a stopped record returned %v, want ErrTakeOverRefused", err)
	}
	if !strings.Contains(err.Error(), "--resume "+h.OperationID()) {
		t.Errorf("the refusal does not name --resume as the verb that continues it: %v", err)
	}
	if strings.Contains(err.Error(), "--take-over") {
		t.Errorf("the refusal suggests a take-over of a record that already says stopped: %v", err)
	}
	if k.liveRecord(Name).Executor.Token != token {
		t.Fatal("the refused take-over of a stopped record wrote to the record")
	}
	// The same record is a plain resume's, and that one works.
	if _, err := TakeOver(ctx, other, h.OperationID()); err != nil {
		t.Fatalf("the resume of a stopped record was refused: %v", err)
	}

	// A terminal record has nothing left to take over.
	k2 := newFakeKube(t)
	s2 := newStore(t, k2, "ana@laptop")
	h2 := acquire(t, s2, testRequest("host-1"))
	if err := s2.Complete(ctx, h2, ResultSucceeded); err != nil {
		t.Fatalf("completing the record: %v", err)
	}
	rv2 := k2.resourceVersion(Name)
	if _, err := (Recovery{TakeOver: h2.OperationID(), Confirm: true}).Claim(ctx, newStore(t, k2, "bo@other-laptop")); !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("a take-over of a terminal record returned %v, want ErrTakeOverRefused", err)
	} else if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("the refusal does not say the record is terminal: %v", err)
	}
	if k2.resourceVersion(Name) != rv2 {
		t.Fatal("the refused take-over of a terminal record wrote to the record")
	}
}

// TestAResumeAndATakeOverCannotBothBeGiven: the two flags name the same
// operation in two different ways, and accepting both would leave the run
// deciding which one the operator meant.
func TestAResumeAndATakeOverCannotBothBeGiven(t *testing.T) {
	err := (Recovery{Resume: "abcdef01", TakeOver: "fedcba98", Confirm: true}).Validate()
	if !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("--resume with --take-over returned %v, want a refusal", err)
	}
	for _, want := range []string{"--resume", "--take-over"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if id := (Recovery{Resume: "abcdef01"}).ID(); id != "abcdef01" {
		t.Errorf("a resume's operation is %q", id)
	}
	if id := (Recovery{TakeOver: "fedcba98", Confirm: true}).ID(); id != "fedcba98" {
		t.Errorf("a take-over's operation is %q", id)
	}
	if rec := (Recovery{}).ID(); rec != "" {
		t.Errorf("an empty recovery names operation %q, want none", rec)
	}
}
