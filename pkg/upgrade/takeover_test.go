package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
)

// The test below is the upgrade's half of kn-yzuv: `platform upgrade
// --take-over <id> --confirm` reconciles the record an interrupted upgrade left
// and claims it, so the run continues the same operation.
//
// What it does NOT prove: that the stages then behave on a real cluster (that
// is the lab and e2e/), and nothing about the control-plane path, whose test is
// in pkg/cmd.

// recordRunner is an in-memory kube-system for the record: the one ConfigMap,
// its resourceVersion, and the commands pkg/operation's store issues.
type recordRunner struct {
	mu  sync.Mutex
	rv  int
	doc []byte
}

func (r *recordRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Contains(command, "get configmap "+operation.Name) {
		if r.doc == nil {
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "kubenest-operation" not found`}, nil
		}
		body, err := json.Marshal(map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": operation.Name, "resourceVersion": fmt.Sprint(r.rv)},
			"data":     map[string]any{"record.json": string(r.doc)},
		})
		if err != nil {
			return sshx.Result{}, err
		}
		return sshx.Result{Stdout: string(body)}, nil
	}
	return sshx.Result{}, nil
}

func (r *recordRunner) RunInput(_ context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	body, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	var object struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &object); err != nil {
		return sshx.Result{}, err
	}
	if object.Data["record.json"] == "" {
		return sshx.Result{}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.doc = []byte(object.Data["record.json"])
	r.rv++
	return sshx.Result{Stdout: fmt.Sprintf(`{"metadata":{"resourceVersion":"%d"}}`, r.rv)}, nil
}

// record decodes the record the fake cluster holds.
func (r *recordRunner) record(t *testing.T) operation.Record {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var rec operation.Record
	if err := json.Unmarshal(r.doc, &rec); err != nil {
		t.Fatalf("the record on the cluster is not a record: %v", err)
	}
	return rec
}

// leaveRunning rewrites the live record as a process that died before its
// closing write left it: still `running`, request and actions untouched. No
// code path in this CLI produces that state, which is why only the operator's
// assertion can continue it (kn-yzuv).
func (r *recordRunner) leaveRunning(t *testing.T) operation.Record {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var rec operation.Record
	if err := json.Unmarshal(r.doc, &rec); err != nil {
		t.Fatalf("the record on the cluster is not a record: %v", err)
	}
	rec.Terminal, rec.Result = false, ""
	rec.Executor.State = operation.ExecutorRunning
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	r.doc = out
	return rec
}

func TestAnUpgradeTakeOverReachesReconciliationOnARunningRecord(t *testing.T) {
	ctx := context.Background()
	kube := &recordRunner{}
	session := &Session{
		Opts:  Options{Cluster: "prod-1", To: "1.1"},
		From:  parseManifest(t, "bundle: \"1.0\"\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n"),
		To:    parseManifest(t, "bundle: \"1.1\"\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n"),
		Nodes: []Node{{Address: "10.0.0.1", Server: true, Runner: kube}},
	}

	// The first run takes the record, and its laptop dies.
	_, handle, err := session.LockOperation(ctx)
	if err != nil {
		t.Fatalf("taking the record: %v", err)
	}
	dead := kube.leaveRunning(t)

	// 1. --resume alone: the reconcile runs and the claim is refused, naming
	// --take-over.
	var out bytes.Buffer
	if _, _, err := session.TakeOverOperation(ctx, &out, operation.Recovery{Resume: dead.OperationID}); err == nil {
		t.Fatalf("--resume claimed a record whose executor is still running:\n%s", out.String())
	} else if !strings.Contains(err.Error(), "--take-over "+dead.OperationID) {
		t.Fatalf("the refusal does not name --take-over: %v", err)
	}

	// 2. --take-over --confirm reaches the reconciliation and claims the record.
	out.Reset()
	_, h2, err := session.TakeOverOperation(ctx, &out, operation.Recovery{TakeOver: dead.OperationID, Confirm: true})
	if err != nil {
		t.Fatalf("--take-over --confirm was refused: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Taking over operation "+dead.OperationID) {
		t.Fatalf("the take-over did not reach the reconciliation:\n%s", out.String())
	}
	if h2.Token() == handle.Token() {
		t.Fatal("the take-over did not change the record's ownership token")
	}
	after := kube.record(t)
	if len(after.TakeOvers) != 1 || after.TakeOvers[0].Replaced.Token != handle.Token() {
		t.Fatalf("the take-over is not recorded against the executor it replaced: %+v", after.TakeOvers)
	}
	if after.Executor.Token != h2.Token() {
		t.Errorf("the record's executor is not the take-over's own")
	}
	if after.Request.Kind != operation.KindUpgrade || after.OperationID != dead.OperationID {
		t.Errorf("the take-over did not continue the same operation: %+v", after.Request)
	}
}
