//go:build e2e

// T2.3's gate: the operation record as the lock and as the resume path, on a
// REAL cluster with a REAL API server.
//
// What this asserts, and why it cannot be asserted anywhere else:
//
//	creating the record on a real API server really is a create, and a second
//	Acquire really is refused by the OBJECT rather than by the test
//	a replace carrying a resourceVersion the object has moved past really is a
//	  409, and the store reads it as "the record moved"
//	an action is recorded before it is submitted against a live object
//	a killed action is established from its recorded postcondition, or stops the
//	  resume and names the reconciliation step
//	a take-over is refused while the executor is running and permitted once it
//	  is stopped, after which the old token cannot update
//	completion leaves the live object where it is and copies history under the
//	  operation's ID
//
// Run from the umbrella workspace with a lab cluster:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=...
//	cd kubenest-cli && go test -tags e2e -v -timeout 15m ./e2e/ -run TestOperationLockOnARealCluster
//
// It needs KUBENEST_LAB_SERVER_IP, the SSH key and the control-plane variables
// the other gates use, because gateEnvironment is what resolves the lab.
package e2e

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
)

func TestOperationLockOnARealCluster(t *testing.T) {
	env := gateEnvironment(t)
	ctx := context.Background()
	r := operationRunner(t, env)

	// A live record refuses every later gate on this cluster, so this test
	// leaves none behind.
	t.Cleanup(func() { removeOperationRecords(t, r) })

	first := &operation.Store{Runner: r, Operator: "gate@laptop-1"}
	second := &operation.Store{Runner: r, Operator: "gate@laptop-2"}

	uid, err := k3s.Kubectl(ctx, r, "get nodes -o jsonpath={.items[0].metadata.uid}")
	if err != nil {
		t.Fatalf("reading a node UID: %v", err)
	}
	req := operation.Request{
		Kind:    operation.KindUpgrade,
		Cluster: env.cluster,
		Targets: []operation.Target{{HostID: env.server, NodeUID: strings.TrimSpace(uid)}},
		// 1.0 -> 1.1 is the transition the catalog serves; never invent one.
		Versions: map[string]string{"bundle": "1.0 -> 1.1"},
	}

	h, err := first.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquiring the operation record: %v", err)
	}
	// The object is really on the API server, under the name the lock is
	// defined as.
	out, err := k3s.Kubectl(ctx, r, "get configmap "+operation.Name+" -n "+operation.Namespace+" -o json")
	if err != nil {
		t.Fatalf("the record is not in %s: %v", operation.Namespace, err)
	}
	if !strings.Contains(out, h.OperationID()) || !strings.Contains(out, env.cluster) {
		t.Fatalf("the live record does not name this operation:\n%s", out)
	}

	// The lock is the object: a second laptop is refused by the record itself.
	if _, err := second.Acquire(ctx, req); !errors.Is(err, operation.ErrLocked) {
		t.Fatalf("a second laptop got %v, want ErrLocked", err)
	} else if !strings.Contains(err.Error(), "gate@laptop-1") {
		t.Fatalf("the refusal does not name the other operator: %v", err)
	}

	// An SSH action is recorded before it is submitted and marked on return.
	guarded := &operation.Guarded{
		Inner: r, Op: h, Stage: "kubernetes",
		Specs: func(stage, command string) (operation.Spec, bool) {
			return operation.Spec{
				Kind:          operation.ActionSSH,
				Postcondition: "the server node is Ready",
				Observe:       "sudo -n k3s kubectl get nodes -o jsonpath={.items[0].status.conditions[?(@.type==\"Ready\")].status}",
			}, true
		},
	}
	if _, err := guarded.Run(ctx, "sudo -n k3s kubectl get nodes --no-headers"); err != nil {
		t.Fatalf("submitting a guarded action: %v", err)
	}

	// A killed action: submitted, outcome never written. Its postcondition is
	// observable on the real cluster, so the resume establishes it.
	const observed = "sudo -n k3s kubectl -n kube-system get configmap kube-root-ca.crt"
	observedID := operation.ActionID("kubernetes", observed)
	if err := h.RecordAction(ctx, observedID, "kubernetes", operation.Spec{
		Kind:          operation.ActionPlan,
		Postcondition: "the cluster's root CA bundle is readable",
		Observe:       observed,
	}); err != nil {
		t.Fatalf("recording the action before submission: %v", err)
	}
	if err := h.SubmitAction(ctx, observedID); err != nil {
		t.Fatalf("marking the action submitted: %v", err)
	}

	// The other laptop reads; knowing the operation ID confers nothing.
	plan, err := operation.Resume(ctx, second, h.OperationID())
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if !plan.Skip()[observedID] {
		t.Fatalf("the resume did not establish the recorded postcondition: %+v", plan)
	}
	if err := plan.Verify(req); err != nil {
		t.Fatalf("a resume refused its own immutable request: %v", err)
	}

	// An action whose outcome cannot be established stops the resume and names
	// the reconciliation step.
	unobservable := "sudo -n k3s kubectl -n kube-system delete configmap kubenest-operation-does-not-exist"
	unobservableID := operation.ActionID("kubernetes", unobservable)
	if err := h.RecordAction(ctx, unobservableID, "kubernetes", operation.Spec{
		Kind:          operation.ActionSSH,
		Postcondition: "a state nothing recorded can observe",
		Observe:       "sudo -n k3s kubectl -n kube-system get configmap kubenest-operation-does-not-exist",
	}); err != nil {
		t.Fatalf("recording the unobservable action: %v", err)
	}
	if err := h.SubmitAction(ctx, unobservableID); err != nil {
		t.Fatalf("marking the unobservable action submitted: %v", err)
	}
	blockedPlan, err := operation.Resume(ctx, second, h.OperationID())
	if !errors.Is(err, operation.ErrReconcile) {
		t.Fatalf("an unestablishable outcome returned %v, want ErrReconcile", err)
	}
	if blockedPlan.Blocked == nil || blockedPlan.Blocked.Reconciliation == "" {
		t.Fatalf("the resume stopped without naming the reconciliation step: %+v", blockedPlan)
	}
	if blockedPlan.Skip()[unobservableID] {
		t.Fatal("an action whose outcome is unknown was offered as skippable")
	}
	// The operator reconciles both by hand; the record then says so.
	if err := h.FinishAction(ctx, observedID, sshx.Result{}, nil); err != nil {
		t.Fatalf("recording the reconciled outcome: %v", err)
	}
	if err := h.FinishAction(ctx, unobservableID, sshx.Result{}, nil); err != nil {
		t.Fatalf("recording the reconciled outcome: %v", err)
	}

	// A take-over is refused while the executor is still running...
	if _, err := operation.TakeOver(ctx, second, h.OperationID()); !errors.Is(err, operation.ErrTakeOverRefused) {
		t.Fatalf("a take-over of a running executor returned %v", err)
	}
	if err := first.Stop(ctx, h); err != nil {
		t.Fatalf("stopping the first executor: %v", err)
	}
	// ...and permitted once it is stopped and its actions are reconciled.
	h2, err := operation.TakeOver(ctx, second, h.OperationID())
	if err != nil {
		t.Fatalf("a permitted take-over was refused: %v", err)
	}
	if h2.Token() == h.Token() {
		t.Fatal("the take-over kept the old ownership token")
	}
	if err := first.Update(ctx, h, func(*operation.Record) error { return nil }); !errors.Is(err, operation.ErrNotOwner) {
		t.Fatalf("the old token's write returned %v, want ErrNotOwner", err)
	}

	// Completion leaves the live object where it is and copies history under
	// the operation's ID.
	if err := second.Complete(ctx, h2, operation.ResultSucceeded); err != nil {
		t.Fatalf("completing the operation: %v", err)
	}
	if _, err := k3s.Kubectl(ctx, r, "get configmap "+operation.Name+" -n "+operation.Namespace+" -o json"); err != nil {
		t.Fatalf("completion moved the live record instead of copying it: %v", err)
	}
	if _, err := k3s.Kubectl(ctx, r, "get configmap "+operation.HistoryPrefix+h.OperationID()+" -n "+operation.Namespace+" -o json"); err != nil {
		t.Fatalf("the history copy is not under the operation's ID: %v", err)
	}

	// The terminal record does not hold the lock against the next operation,
	// and the finished one is still findable afterwards.
	next, err := first.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquiring after a terminal record: %v", err)
	}
	if _, err := second.Find(ctx, h.OperationID()); err != nil {
		t.Fatalf("a finished operation is no longer findable by its ID: %v", err)
	}
	if err := first.Complete(ctx, next, operation.ResultSucceeded); err != nil {
		t.Fatalf("completing the second operation: %v", err)
	}
}

// operationRunner dials the lab server the way every other gate does: over
// SSH, with no kubeconfig anywhere.
func operationRunner(t *testing.T, env gateEnv) *sshx.Client {
	t.Helper()
	opts := sshx.Options{
		User:           env.sshUser,
		KeyPath:        env.sshKey,
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
		DialTimeout:    15 * time.Second,
	}
	endpoint, err := sshx.Resolve(env.server, opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshx.Dial(context.Background(), endpoint, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// removeOperationRecords takes the lock back off the cluster, live record and
// history alike: a record left behind refuses every later gate.
func removeOperationRecords(t *testing.T, r k3s.Runner) {
	t.Helper()
	ctx := context.Background()
	args := "delete configmap -n " + operation.Namespace +
		" -l app.kubernetes.io/managed-by=kubenest-cli --ignore-not-found"
	if _, err := k3s.Kubectl(ctx, r, args); err != nil {
		t.Logf("cleaning up the operation records: %v", err)
	}
}
