package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

// The tests below are the node verbs' half of kn-yzuv: a record a dead executor
// left `running` is taken over by the operator's assertion — `--take-over <id>
// --confirm` — and `--resume` alone keeps refusing it while naming the way on.
//
// What they do NOT prove: that a real host's `kubectl` returns what this fake
// returns, and nothing about the other verbs, whose own tests are in pkg/cmd,
// pkg/backup and pkg/upgrade.

// TestANodeVerbTakeOverReachesReconciliationOnARunningRecord: the whole verb.
//
// A `node add` whose laptop died mid-join leaves a record that still says its
// executor is `running`, because the only writer of `stopped` needs the dead
// executor's own token. `--resume` refuses it; `--take-over --confirm`
// reconciles the recorded actions and continues THAT operation — the stage the
// dead run never got past is run again, and the host ends up in the cluster.
func TestANodeVerbTakeOverReachesReconciliationOnARunningRecord(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	const newNode = "prod-1-agt-2"
	installs := 0
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		installs++
		if installs == 1 {
			// The dead run's last act: the installer could not be fetched, and
			// then the laptop went away before the record could be closed.
			return sshx.Result{ExitCode: 1, Stderr: "The installer could not be downloaded"}, nil
		}
		f.server.setNodes(joinedNodes(t, newNode, true))
		return sshx.Result{}, nil
	})

	if _, err := f.runAdd(f.newAdd(AddOptions{})); err == nil {
		t.Fatalf("the planted install failure did not fail the run:\n%s", f.out.String())
	}
	if installs != 1 {
		t.Fatalf("%d install attempts in the first run, want exactly 1", installs)
	}
	dead := killRecord(t, f.server)

	// 1. --resume alone is refused, and names --take-over. THE REFUSAL LEAVES
	// THE RECORD WHERE IT WAS: nothing claimed it, so nothing may close it.
	resuming := freshSession(t, f, "run-2")
	resumeErr := drive(resuming, AddOptions{Agent: testAgentAddr, Resume: dead.OperationID})
	if resumeErr == nil {
		t.Fatalf("--resume accepted a record whose executor is still running:\n%s", f.out.String())
	}
	if !strings.Contains(resumeErr.Error(), "--take-over "+dead.OperationID) {
		t.Fatalf("the refusal does not name the way on (--take-over %s): %v", dead.OperationID, resumeErr)
	}
	if installs != 1 {
		t.Errorf("the refused resume re-ran the install (%d attempts)", installs)
	}
	if after := readRecord(t, f.server); after.Terminal || after.Executor.Token != dead.Executor.Token {
		t.Fatalf("the refused resume terminalized or re-owned the record (terminal=%v, token %q): the take-over it names would find nothing to take over",
			after.Terminal, after.Executor.Token)
	}

	// 2. --take-over --confirm reconciles, claims the record, and continues the
	// operation: the stage the dead run failed in is run again, and the host
	// joins.
	f.out.Reset()
	taking := freshSession(t, f, "run-3")
	if err := drive(taking, AddOptions{Agent: testAgentAddr, TakeOver: dead.OperationID, Confirm: true}); err != nil {
		t.Fatalf("--take-over failed: %v\n%s", err, f.out.String())
	}
	if !strings.Contains(f.out.String(), "Taking over operation "+dead.OperationID) {
		t.Fatalf("--take-over did not reach the reconciliation:\n%s", f.out.String())
	}
	if installs != 2 {
		t.Errorf("%d install attempts after the take-over, want 2: the take-over continues the operation, and the stage that failed is repeated", installs)
	}
	joiner := hostWithAddress(f.records.inventory(), testAgentAddr)
	if joiner.LifecycleState != string(StateActive) {
		t.Errorf("the take-over left the host as %q, want %q", joiner.LifecycleState, StateActive)
	}
	// THE ASSERTION IS IN THE RECORD: who made it, and which executor it
	// replaced.
	after := readRecord(t, f.server)
	if len(after.TakeOvers) != 1 {
		t.Fatalf("the record holds %d take-over assertion(s), want exactly 1: %+v", len(after.TakeOvers), after.TakeOvers)
	}
	if after.TakeOvers[0].Replaced.Token != dead.Executor.Token {
		t.Errorf("the recorded assertion replaced %q, want the dead executor's token %q", after.TakeOvers[0].Replaced.Token, dead.Executor.Token)
	}
	if after.Executor.Token == dead.Executor.Token {
		t.Error("the record's ownership token did not change, so the take-over did not claim it")
	}
}

// drive runs one node verb over the fixture with the options under test, the
// way the command layer does: one stages.Execute, then the run's own ending.
func drive(s *Session, opts AddOptions) error {
	add := &Add{Session: s, Opts: opts}
	ctx := context.Background()
	_, err := stages.Execute(ctx, add, PlanAdd(add))
	add.Finish(ctx, err, false)
	return err
}

// killRecord leaves the live record the way a process that died before its
// closing write left it: still `running`, with its request and its actions
// untouched. NOTHING IN THIS CLI PRODUCES THIS STATE — the closing write is
// what says `stopped` — which is exactly why the operator's assertion is the
// only way a later laptop may continue it (kn-yzuv, lab w3, 2026-09-27).
func killRecord(t *testing.T, h *fakeHost) operation.Record {
	t.Helper()
	return rewriteRecord(t, h, func(rec *operation.Record) {
		rec.Terminal = false
		rec.Result = ""
		rec.Executor.State = operation.ExecutorRunning
	})
}

// rewriteRecord applies one change to the record the fake cluster holds.
func rewriteRecord(t *testing.T, h *fakeHost, mutate func(*operation.Record)) operation.Record {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	obj, ok := h.objects[operation.Name]
	if !ok {
		t.Fatal("no operation record was created on the cluster")
	}
	data, _ := obj["data"].(map[string]any)
	raw, _ := data["record.json"].(string)
	var rec operation.Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("the record on the cluster is not a record: %v", err)
	}
	mutate(&rec)
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	data["record.json"] = string(out)
	return rec
}

// readRecord decodes the live record the fake cluster holds.
func readRecord(t *testing.T, h *fakeHost) operation.Record {
	t.Helper()
	obj := h.liveRecord(t)
	data, _ := obj["data"].(map[string]any)
	raw, _ := data["record.json"].(string)
	var rec operation.Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("the record on the cluster is not a record: %v", err)
	}
	return rec
}

// freshSession is a session from ANOTHER process: the same world and the same
// journal, and no handle — a process that has not taken the record holds
// nothing, so its ending cannot write one (the bug a shallow copy of the
// fixture's session would plant).
func freshSession(t *testing.T, f *nodeFixture, id string) *Session {
	t.Helper()
	s := f.sessionFor(t, id)
	s.handle, s.store, s.skip = nil, nil, nil
	return s
}

// TestEveryNodeVerbTakesOverThroughTheSameRecordPath: `node add`, `node remove`
// and `node replace` share ONE record path (Session.openRecord), so the
// take-over reaches the reconcile for all three kinds.
//
// It drives that path directly because the request a verb builds on a resumed
// resolution is that verb's own business: what is being tested here is the
// record path, which all three call.
func TestEveryNodeVerbTakesOverThroughTheSameRecordPath(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []operation.Kind{operation.KindNodeAdd, operation.KindNodeRemove, operation.KindNodeReplace} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t, serverHost(), agentHost())
			request := operation.Request{
				Kind:    kind,
				Cluster: "prod-1",
				Targets: []operation.Target{{HostID: "h-agt", NodeUID: "uid-agt"}},
			}
			// The dead executor's record: `running`, with one remote action it
			// submitted and whose outcome was never written back.
			dead, err := f.session.Store(f.server).Acquire(ctx, request)
			if err != nil {
				t.Fatalf("taking the record: %v", err)
			}
			const command = "sudo -n k3s kubectl cordon node-1"
			const observe = "sudo -n k3s kubectl get node node-1 -o jsonpath={.spec.unschedulable}"
			id := operation.ActionID("hold", command)
			if err := dead.RecordAction(ctx, id, "hold", operation.Spec{
				Kind: operation.ActionPlan, Postcondition: "node-1 is cordoned", Observe: observe,
			}); err != nil {
				t.Fatalf("recording the action: %v", err)
			}
			if err := dead.SubmitAction(ctx, id); err != nil {
				t.Fatalf("submitting the action: %v", err)
			}

			// 1. A resume does not claim it.
			resuming := recordSession(f, "run-2")
			err = resuming.openRecord(ctx, kind, request, operation.Recovery{Resume: dead.OperationID()})
			if err == nil {
				t.Fatalf("openRecord resumed a record whose executor is still running")
			}
			if !strings.Contains(err.Error(), "--take-over "+dead.OperationID()) {
				t.Fatalf("the refusal does not name --take-over: %v", err)
			}

			// 2. The operator's assertion does, after the same reconcile.
			f.out.Reset()
			taking := recordSession(f, "run-3")
			if err := taking.openRecord(ctx, kind, request, operation.Recovery{TakeOver: dead.OperationID(), Confirm: true}); err != nil {
				t.Fatalf("openRecord refused an asserted take-over: %v", err)
			}
			out := f.out.String()
			if !strings.Contains(out, "Taking over operation "+dead.OperationID()) {
				t.Fatalf("the take-over did not reach the reconciliation:\n%s", out)
			}
			if !f.log.hasCommand(observe) {
				t.Errorf("the take-over did not run the recorded postcondition probe, so it did not reconcile the action the dead executor left:\n%s", out)
			}
			after := readRecord(t, f.server)
			if len(after.TakeOvers) != 1 || after.TakeOvers[0].Replaced.Token != dead.Token() {
				t.Fatalf("the take-over is not recorded against the executor it replaced: %+v", after.TakeOvers)
			}
			if taking.handle == nil || after.Executor.Token != taking.handle.Token() {
				t.Errorf("the record's executor token is not the take-over's own")
			}
		})
	}
}

// recordSession is a session that can reach the record and nothing else: the
// seam the three node verbs open their record through.
func recordSession(f *nodeFixture, id string) *Session {
	return &Session{
		ID:         id,
		Cluster:    "prod-1",
		ServerConn: f.server,
		Out:        f.out,
		Store: func(runner k3s.Runner) *operation.Store {
			return &operation.Store{Runner: runner, Operator: "bo@other-laptop"}
		},
	}
}
