package cmd

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

// recordKube is the smallest in-memory kube the operation record needs: the one
// ConfigMap, its resourceVersion, and the three commands the Store issues.
//
// IT REFUSES TO RUN ANYTHING ON A DONE CONTEXT, which is what the SSH transport
// and the API server both do — and it is the whole point of the test below.
type recordKube struct {
	mu  sync.Mutex
	rv  int
	doc []byte
}

func (k *recordKube) Run(ctx context.Context, command string) (sshx.Result, error) {
	if err := ctx.Err(); err != nil {
		return sshx.Result{}, err
	}
	if strings.HasPrefix(command, "sudo -n k3s kubectl get configmap ") {
		k.mu.Lock()
		defer k.mu.Unlock()
		if k.doc == nil {
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "kubenest-operation" not found`}, nil
		}
		body, err := json.Marshal(map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "kubenest-operation", "resourceVersion": fmt.Sprint(k.rv)},
			"data":     map[string]any{"record.json": string(k.doc)},
		})
		if err != nil {
			return sshx.Result{}, err
		}
		return sshx.Result{Stdout: string(body)}, nil
	}
	return sshx.Result{}, nil
}

func (k *recordKube) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	if err := ctx.Err(); err != nil {
		return sshx.Result{}, err
	}
	body, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	// The store writes a CONFIGMAP whose data["record.json"] is the record; the
	// fake keeps that string, because that is what a read hands back.
	var object struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &object); err != nil {
		return sshx.Result{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.doc = []byte(object.Data["record.json"])
	k.rv++
	return sshx.Result{Stdout: fmt.Sprintf(`{"metadata":{"resourceVersion":"%d"}}`, k.rv)}, nil
}

// An interrupt must be able to close its own record.
//
// THE RUN'S CONTEXT IS CANCELLED WHEN THE OPERATOR INTERRUPTS IT, and the record
// must be closed with a context that is not: on hardware (2026-09-25) the Stop
// failed with `reading the operation record kube-system/kubenest-operation:
// context canceled`, the record stayed `executor.state: running`, and the second
// laptop's take-over — which requires the executor STOPPED — was refused. The
// operator was left with an operation that could not be continued by anyone.
func TestAnInterruptedUpgradeStopsItsRecordEvenThoughItsContextIsCancelled(t *testing.T) {
	kube := &recordKube{}
	store := &operation.Store{Runner: kube, Operator: "ana@laptop"}

	live, cancel := context.WithCancel(context.Background())
	handle, err := store.Acquire(live, operation.Request{
		Kind:    operation.KindControlPlaneUpgrade,
		Cluster: "prod-1",
		Versions: map[string]string{
			"bundle": "1.1 -> 1.2",
		},
	})
	if err != nil {
		t.Fatalf("acquiring the record: %v", err)
	}

	// THE INTERRUPT.
	cancel()

	var out bytes.Buffer
	// The run's own error is the cancellation, which is what "interrupted" is.
	endControlPlaneOperation(live, &out, store, handle, context.Canceled, true)

	stored, err := store.Find(context.Background(), handle.OperationID())
	if err != nil {
		t.Fatalf("reading the record back after the interrupt: %v", err)
	}
	if stored.Record.Executor.State != operation.ExecutorStopped {
		t.Fatalf("the record's executor is %q after an interrupt, want %q: a take-over requires the previous executor STOPPED, so a second laptop cannot continue this operation (warning printed: %q)",
			stored.Record.Executor.State, operation.ExecutorStopped, out.String())
	}
	if stored.Record.Terminal {
		t.Error("an interrupt ended the record, so the state the second laptop continues from is gone")
	}
}

// A FAILED run closes its record and marks it terminal: the operator's fix-then-
// rerun starts a new operation over a cluster the record describes, while an
// interrupt has to stay continuable.
func TestAFailedUpgradeEndsItsRecord(t *testing.T) {
	kube := &recordKube{}
	store := &operation.Store{Runner: kube, Operator: "ana@laptop"}
	ctx := context.Background()
	handle, err := store.Acquire(ctx, operation.Request{Kind: operation.KindControlPlaneUpgrade, Cluster: "prod-1"})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	endControlPlaneOperation(ctx, &out, store, handle, fmt.Errorf("the migration Job failed"), false)

	stored, err := store.Find(ctx, handle.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Record.Terminal || stored.Record.Result != string(operation.ResultFailed) {
		t.Errorf("a failed run left the record terminal=%v result=%q, want it ended and failed",
			stored.Record.Terminal, stored.Record.Result)
	}
}
