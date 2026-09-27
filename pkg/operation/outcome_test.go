package operation

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/sshx"
)

// The tests below are the acceptance criterion for kn-x0wv.7: an interrupt
// between an action's return and the write that records its outcome.
//
// What a green run does NOT prove: that a real SSH transport survives a
// cancelled run the way fakeKube does (the lab, and e2e/restore_namespace_test.go's
// S4 arm, is what says that), and nothing here proves that any verb wires its
// actions through Guarded — the verbs' own tests are what say that.

// cancellingRunner is a remote action that cancels the run's context AS IT
// RETURNS: the interrupt that lands in the window between the action returning
// and the record being told (hardware, kn-x0wv.7, S4 on lab w3, 2026-09-27 —
// `creating Velero Restore …: the action returned; the outcome could not be
// written to the operation record, which still says it was submitted`).
type cancellingRunner struct {
	cancel context.CancelFunc
	calls  int
}

func (r *cancellingRunner) Run(context.Context, string) (sshx.Result, error) {
	r.calls++
	r.cancel()
	return sshx.Result{}, nil
}

func (r *cancellingRunner) RunInput(ctx context.Context, command string, _ io.Reader) (sshx.Result, error) {
	return r.Run(ctx, command)
}

// TestAnOutcomeIsWrittenEvenWhenTheContextIsCancelledBetweenTheReturnAndTheWrite.
//
// THE ACTION HAS RETURNED: what is left is telling the record, and that write
// must not die with the run's context. The three writes that record something
// that already happened are covered — the action's outcome, the executor's own
// stop, and the operation's completion (with the history copy) — and the write
// that comes BEFORE an action is covered too, from the other side: it must
// still obey the run's context, because a cancelled run must submit nothing.
func TestAnOutcomeIsWrittenEvenWhenTheContextIsCancelledBetweenTheReturnAndTheWrite(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := acquire(t, s, testRequest("host-1"))

	// A write the operation owes, recorded while the run is still live: its
	// discharge is one of the after-the-fact writes below.
	if err := h.PendingWrite(ctx, "inventory", "h-1", "the host recorded as joining"); err != nil {
		t.Fatalf("recording the owed write: %v", err)
	}

	const command = "sudo -n k3s kubectl -n velero create -f -"
	const postcondition = "the Velero Restore exists"
	id := ActionID("restore", command)
	runner := &cancellingRunner{cancel: cancel}
	g := &Guarded{Inner: runner, Op: h, Stage: "restore", Specs: sshSpecs(postcondition, "true")}
	if _, err := g.Run(ctx, command); err != nil {
		t.Fatalf("the action itself returned cleanly, so the run must not fail: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("%d calls to the action, want exactly 1", runner.calls)
	}

	rec := k.liveRecord(Name)
	action, ok := rec.ActionByID(id)
	if !ok {
		t.Fatalf("the action was not recorded at all: %+v", rec.Actions)
	}
	if action.Status != ActionSucceeded {
		t.Fatalf("the action returned and the record says %q: the outcome write died with the run's context, which is the state a successor can only reconcile by hand (kn-x0wv.7)",
			action.Status)
	}
	if action.FinishedAt == nil {
		t.Error("the outcome is recorded without a finishing instant")
	}

	// The executor's own stop, with the same cancelled context.
	if err := s.Stop(ctx, h); err != nil {
		t.Fatalf("the stop died with the run's context: %v", err)
	}
	if live := k.liveRecord(Name); live.Executor.State != ExecutorStopped {
		t.Fatalf("the record's executor is %q after a stop, want %q", live.Executor.State, ExecutorStopped)
	}

	// The owed write that landed.
	if err := h.PendingWriteDone(ctx, "inventory", "h-1"); err != nil {
		t.Fatalf("the discharge of an owed write died with the run's context: %v", err)
	}
	if owed := k.liveRecord(Name).Outstanding(); len(owed) != 0 {
		t.Fatalf("the discharged write is still outstanding: %+v", owed)
	}

	// A write that comes BEFORE an action still obeys the run's context: a
	// cancelled run must not submit anything, and "it was never sent" is only
	// knowable if recording it first can fail with the cancellation.
	if err := h.RecordAction(ctx, ActionID("kubernetes", "sudo -n k3s kubectl cordon node-1"), "kubernetes",
		Spec{Kind: ActionSSH, Postcondition: "node-1 is cordoned"}); err == nil {
		t.Error("a write that precedes an action was accepted on a cancelled run: the record-before-submit rule is what makes 'never sent' knowable")
	}

	// And the operation completes, history copy and all.
	if err := s.Complete(ctx, h, ResultFailed); err != nil {
		t.Fatalf("the completion died with the run's context: %v", err)
	}
	if !k.exists(HistoryPrefix + h.OperationID()) {
		t.Fatal("the completion did not copy the record under its own id")
	}
}

// TestAResumeOfAStoppedRecordSettlesASubmittedActionWhosePostconditionHolds:
// the record an interrupt left behind — executor stopped, one action submitted
// with its outcome unwritten — is continued by the resume it advertises. The
// action's recorded postcondition is what settles it, and the claim writes the
// outcome it established in the same compare-and-swap that takes ownership.
func TestAResumeOfAStoppedRecordSettlesASubmittedActionWhosePostconditionHolds(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	const command = "sudo -n k3s kubectl -n velero create -f -"
	const observe = "sudo -n k3s kubectl -n velero get restore kubenest-restore-1 -o json"
	const postcondition = "the Velero Restore kubenest-restore-1 exists"
	id := ActionID("restore", command)
	if err := h.RecordAction(ctx, id, "restore", Spec{Kind: ActionRestore, Postcondition: postcondition, Observe: observe}); err != nil {
		t.Fatalf("recording the action: %v", err)
	}
	if err := h.SubmitAction(ctx, id); err != nil {
		t.Fatalf("submitting the action: %v", err)
	}
	// The restore exists, so the recorded postcondition holds.
	k.script(observe, sshx.Result{})
	if err := s.Stop(ctx, h); err != nil {
		t.Fatalf("stopping the executor: %v", err)
	}

	other := newStore(t, k, "bo@other-laptop")
	plan, err := Resume(ctx, other, h.OperationID())
	if err != nil {
		t.Fatalf("the reconcile of a stopped record with a submitted action failed: %v", err)
	}
	if !plan.Skip()[id] {
		t.Fatalf("the reconcile did not establish the submitted action: %+v", plan.Steps)
	}

	h2, err := TakeOver(ctx, other, h.OperationID())
	if err != nil {
		t.Fatalf("a resume of a stopped record with an established outcome was refused: %v", err)
	}
	rec := k.liveRecord(Name)
	action, _ := rec.ActionByID(id)
	if action.Status != ActionSucceeded || action.FinishedAt == nil {
		t.Fatalf("the claim did not write the outcome it established: the action is %q with finished_at %v", action.Status, action.FinishedAt)
	}
	if rec.Executor.Token != h2.Token() || rec.Executor.State != ExecutorRunning {
		t.Fatalf("the record's executor is %+v, want this claim", rec.Executor)
	}

	// And the operation completes.
	if err := other.Complete(ctx, h2, ResultSucceeded); err != nil {
		t.Fatalf("completing the continued operation: %v", err)
	}
	stored, err := other.Find(ctx, h.OperationID())
	if err != nil {
		t.Fatalf("reading the record back: %v", err)
	}
	if !stored.Record.Terminal || stored.Record.Result != string(ResultSucceeded) {
		t.Fatalf("the record is terminal=%v result=%q, want a finished operation", stored.Record.Terminal, stored.Record.Result)
	}
}

// TestAResumeOfAnActionWhoseOutcomeCannotBeEstablishedNamesARealNextStep: the
// documented rule (PLAN 7.2) is that an outcome that cannot be established
// STOPS the resume and names the reconciliation step. The claim that follows
// must name that same step — and never the command the operator is already
// running, which is the loop kn-x0wv.7 measured.
func TestAResumeOfAnActionWhoseOutcomeCannotBeEstablishedNamesARealNextStep(t *testing.T) {
	const command = "sudo -n k3s kubectl -n velero create -f -"
	const observe = "sudo -n k3s kubectl -n velero get restore kubenest-restore-1 -o json"
	const postcondition = "the Velero Restore kubenest-restore-1 exists"

	cases := []struct {
		name      string
		observe   string
		scripted  sshx.Result
		wants     []string
		reconcile string
	}{
		{
			name:      "the recorded probe does not establish it",
			observe:   observe,
			scripted:  sshx.Result{ExitCode: 1, Stderr: "Error from server (NotFound): restores.velero.io \"kubenest-restore-1\" not found\n"},
			wants:     []string{observe, postcondition, "exited 1"},
			reconcile: observe,
		},
		{
			name:      "the record holds no probe at all",
			observe:   "",
			wants:     []string{postcondition, "no read-only command"},
			reconcile: postcondition,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := newFakeKube(t)
			s := newStore(t, k, "ana@laptop")
			ctx := context.Background()
			h := acquire(t, s, testRequest("host-1"))

			id := ActionID("restore", command)
			if err := h.RecordAction(ctx, id, "restore", Spec{Kind: ActionRestore, Postcondition: postcondition, Observe: tc.observe}); err != nil {
				t.Fatalf("recording the action: %v", err)
			}
			if err := h.SubmitAction(ctx, id); err != nil {
				t.Fatalf("submitting the action: %v", err)
			}
			if tc.observe != "" {
				k.script(tc.observe, tc.scripted)
			}
			if err := s.Stop(ctx, h); err != nil {
				t.Fatalf("stopping the executor: %v", err)
			}

			other := newStore(t, k, "bo@other-laptop")
			// The documented rule: the resume STOPS and names the step.
			plan, err := Resume(ctx, other, h.OperationID())
			if !errors.Is(err, ErrReconcile) {
				t.Fatalf("an outcome that cannot be established returned %v, want ErrReconcile", err)
			}
			if plan.Blocked == nil || !strings.Contains(plan.Blocked.Reconciliation, tc.reconcile) {
				t.Fatalf("the resume stopped without naming the step that settles it: %+v", plan.Blocked)
			}

			// And the claim names the same step, not the command that just
			// refused the operator.
			rv := k.resourceVersion(Name)
			_, err = TakeOver(ctx, other, h.OperationID())
			if !errors.Is(err, ErrTakeOverRefused) {
				t.Fatalf("the claim returned %v, want ErrTakeOverRefused", err)
			}
			for _, want := range append(tc.wants, "establish") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "--resume") {
				t.Errorf("the refusal points back at the command that just refused the operator: %v", err)
			}
			if k.resourceVersion(Name) != rv {
				t.Fatal("a refused claim wrote to the record")
			}
			if status := mustAction(t, k, id).Status; status != ActionSubmitted {
				t.Errorf("the action is recorded %q after a refusal, want the state the dead executor left", status)
			}
		})
	}
}

func mustAction(t *testing.T, k *fakeKube, id string) Action {
	t.Helper()
	a, ok := k.liveRecord(Name).ActionByID(id)
	if !ok {
		t.Fatalf("action %s is not in the record", id)
	}
	return a
}
