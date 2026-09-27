package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/upgrade"
)

// The tests below are the verb-level half of kn-yzuv: a record a dead executor
// left `running` is taken over by `--take-over <id> --confirm`, and `--resume`
// alone keeps refusing it while naming the way on.
//
// What they do NOT prove: that the SSH transport or the API server behave the
// way these fakes do (e2e/ and the lab hosts are what say that), and — because
// these drive one verb — nothing about the other verbs' flag surfaces, which
// have their own tests in pkg/node, pkg/backup and pkg/upgrade.

// TestARebootTakeOverReachesReconciliationOnARunningRecord.
//
// The record is the one a reboot that lost its laptop leaves: the verb's OWN
// immutable request, still `running` because the process died between the
// reboot and the record's closing write — the measured state on lab w3
// (2026-09-27), where the node stayed cordoned and every later command was
// refused with "another operation holds the record".
func TestARebootTakeOverReachesReconciliationOnARunningRecord(t *testing.T) {
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	ran := []string{}
	// The first run gets as far as the reboot; the host then never comes back,
	// so it leaves a record with the verb's request in it.
	first := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost()},
		server: server,
		gates:  permissiveGates(&ran),
	})
	first.dialer.fail = func(call int) error {
		if call <= 1 {
			return nil
		}
		return fmt.Errorf("connect to %s: connection refused", testServerAddr)
	}
	if out, err := first.run(t); err == nil {
		t.Fatalf("the planted reboot failure did not fail the run:\n%s", out)
	}

	// THE DEAD EXECUTOR. Its laptop died before the record's closing write, so
	// the record still says `running` and its request is untouched.
	dead := server.liveRecord(t)
	dead.Executor.State = operation.ExecutorRunning
	dead.Terminal, dead.Result = false, ""
	seedLiveRecord(t, server, dead)

	again := func(flags NodeRebootFlags) (string, error) {
		flags.Cluster, flags.Node, flags.Confirm, flags.Now = testCluster, testServerAddr, true, true
		fixture := newRebootFixture(t, rebootFixtureOpts{
			flags: flags, hosts: []api.HostRecord{serverHost()}, server: server, gates: permissiveGates(&ran),
		})
		return fixture.run(t)
	}

	// 1. --resume alone does not claim it: it is refused, and the refusal names
	// --take-over.
	out, err := again(NodeRebootFlags{Resume: dead.OperationID})
	if err == nil {
		t.Fatalf("--resume accepted a record whose executor is still running:\n%s", out)
	}
	for _, want := range []string{"--take-over " + dead.OperationID, "--confirm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if strings.Contains(out, "Taking over") {
		t.Errorf("--resume took the record over:\n%s", out)
	}

	// 2. --take-over --confirm reconciles first and then takes the record.
	out, _ = again(NodeRebootFlags{TakeOver: dead.OperationID})
	if !strings.Contains(out, "Taking over operation "+dead.OperationID) {
		t.Fatalf("--take-over did not reach the reconciliation and claim the record:\n%s", out)
	}
	rec := server.liveRecord(t)
	if len(rec.TakeOvers) != 1 {
		t.Fatalf("the record holds %d take-over assertion(s), want exactly 1: %+v", len(rec.TakeOvers), rec.TakeOvers)
	}
	if rec.TakeOvers[0].Replaced.Token != dead.Executor.Token {
		t.Errorf("the recorded assertion replaced %q, want the dead executor's token %q", rec.TakeOvers[0].Replaced.Token, dead.Executor.Token)
	}
	if rec.Executor.Token == dead.Executor.Token {
		t.Error("the record's ownership token did not change, so the take-over did not claim it")
	}
}

// TestPlatformUpgradeRefusesTheTakeOverFlagCombinations: the flags reach ONE
// rule (pkg/operation's Recovery), and the refusal happens before anything is
// read — a take-over without the operator's confirmation, and the two recovery
// flags at once, are both refused at the command's own surface.
func TestPlatformUpgradeRefusesTheTakeOverFlagCombinations(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		wants []string
	}{
		{
			name:  "a take-over without the assertion",
			args:  []string{"--cluster", "prod-1", "--to", "1.1", "--take-over", "abcdef0123456789"},
			wants: []string{"--take-over", "--confirm"},
		},
		{
			name:  "a resume and a take-over at once",
			args:  []string{"--cluster", "prod-1", "--to", "1.1", "--control-plane", "--resume", "abcdef0123456789", "--take-over", "fedcba9876543210", "--confirm"},
			wants: []string{"--resume", "--take-over"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newPlatformUpgradeCommand()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("the flag combination was accepted")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}
}

// upgradeKube is the in-memory kube the workload upgrade's record needs: the
// one ConfigMap, its resourceVersion, and the commands the store issues.
type upgradeKube struct {
	mu  sync.Mutex
	rv  int
	doc []byte
}

func (k *upgradeKube) Run(_ context.Context, command string) (sshx.Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if strings.Contains(command, "get configmap "+operation.Name) {
		if k.doc == nil {
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "kubenest-operation" not found`}, nil
		}
		body, err := json.Marshal(map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": operation.Name, "resourceVersion": fmt.Sprint(k.rv)},
			"data":     map[string]any{"record.json": string(k.doc)},
		})
		if err != nil {
			return sshx.Result{}, err
		}
		return sshx.Result{Stdout: string(body)}, nil
	}
	return sshx.Result{}, nil
}

func (k *upgradeKube) RunInput(_ context.Context, command string, stdin io.Reader) (sshx.Result, error) {
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
	k.mu.Lock()
	defer k.mu.Unlock()
	k.doc = []byte(object.Data["record.json"])
	k.rv++
	return sshx.Result{Stdout: fmt.Sprintf(`{"metadata":{"resourceVersion":"%d"}}`, k.rv)}, nil
}

// leaveRunning rewrites the live record as a process that died before its
// closing write left it: still `running`. Nothing in this CLI produces that
// state, which is why only the operator's assertion may continue it (kn-yzuv).
func (k *upgradeKube) leaveRunning(t *testing.T) operation.Record {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	var rec operation.Record
	if err := json.Unmarshal(k.doc, &rec); err != nil {
		t.Fatalf("the record on the cluster is not a record: %v", err)
	}
	rec.Terminal, rec.Result = false, ""
	rec.Executor.State = operation.ExecutorRunning
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	k.doc = out
	return rec
}

// record decodes the record the fake cluster holds.
func (k *upgradeKube) record(t *testing.T) operation.Record {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	var rec operation.Record
	if err := json.Unmarshal(k.doc, &rec); err != nil {
		t.Fatalf("the record on the cluster is not a record: %v", err)
	}
	return rec
}

// TestAWorkloadUpgradeTakeOverReachesReconciliationOnARunningRecord: the
// workload cluster's `platform upgrade --take-over <id> --confirm` claims the
// record an interrupted upgrade left and continues the SAME operation — the
// request is the one the dead run wrote, so a take-over is not a new upgrade
// wearing the same cluster's name.
func TestAWorkloadUpgradeTakeOverReachesReconciliationOnARunningRecord(t *testing.T) {
	ctx := context.Background()
	kube := &upgradeKube{}
	session := &upgrade.Session{
		Opts:  upgrade.Options{Cluster: "prod-1", To: "1.1"},
		From:  &manifest.Manifest{Bundle: "1.0"},
		To:    &manifest.Manifest{Bundle: "1.1"},
		Nodes: []upgrade.Node{{Address: "10.0.0.1", Server: true, Runner: kube}},
	}

	// A --wait run takes the record; then its laptop dies.
	_, handle, err := upgradeLock(ctx, io.Discard, session, UpgradeFlags{Cluster: "prod-1", To: "1.1", Wait: true})
	if err != nil {
		t.Fatalf("the first run could not take the record: %v", err)
	}
	if handle == nil {
		t.Fatal("--wait did not take the record, so this test is not about a recorded upgrade")
	}
	dead := kube.leaveRunning(t)

	// 1. --resume is a control-plane verb, so a workload run without it still
	// takes nothing — what matters here is that `--take-over` is not a resume
	// and does reach the record.
	var out strings.Builder
	store, h2, err := upgradeLock(ctx, &out, session, UpgradeFlags{Cluster: "prod-1", To: "1.1", TakeOver: dead.OperationID, Confirm: true})
	if err != nil {
		t.Fatalf("--take-over --confirm was refused: %v", err)
	}
	if store == nil || h2 == nil {
		t.Fatal("--take-over returned no handle, so the run would take no record at all")
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
	if after.OperationID != dead.OperationID || after.Request.Kind != operation.KindUpgrade {
		t.Errorf("the take-over did not continue the same upgrade: %s %s", after.OperationID, after.Request.Kind)
	}

	// 2. And a run with neither flag takes no record at all: nothing else about
	// the upgrade's record-taking changed.
	none, noHandle, err := upgradeLock(ctx, io.Discard, session, UpgradeFlags{Cluster: "prod-1", To: "1.1"})
	if err != nil || none != nil || noHandle != nil {
		t.Errorf("a run with no recovery flag took the record (%v, %v, %v)", none, noHandle, err)
	}
}

// TestAControlPlaneUpgradeTakeOverReachesReconciliationOnARunningRecord: the
// same thing through `platform upgrade --control-plane`, whose lock helper is
// the verb-level seam. The operation it continues is the one the first run
// began, request and all — so the take-over does not become a different
// operation wearing the same record.
func TestAControlPlaneUpgradeTakeOverReachesReconciliationOnARunningRecord(t *testing.T) {
	kube := &recordKube{}
	ctx := context.Background()
	session := &upgrade.Session{
		From:  &manifest.Manifest{Bundle: "1.0"},
		To:    &manifest.Manifest{Bundle: "1.1"},
		Nodes: []upgrade.Node{{Address: "10.0.0.11", Server: true}},
	}
	started := api.ControlPlaneVersion{Contract: 3, Build: "c121ed887750b1d3196d54fe9fd8368791a1bf03"}
	f := UpgradeFlags{Cluster: "prod-1", To: "1.1", ControlPlane: true}

	// The first run takes the record and its laptop dies: nothing closes the
	// record, so it stays `running`.
	_, handle, _, err := controlPlaneLock(ctx, kube, f, session, io.Discard, started, "")
	if err != nil {
		t.Fatalf("the first run could not take the record: %v", err)
	}

	// 1. --resume alone: the reconcile runs and the claim is refused, naming
	// the way on.
	var out strings.Builder
	resuming := f
	resuming.Resume = handle.OperationID()
	if _, _, _, err := controlPlaneLock(ctx, kube, resuming, session, &out, started, ""); err == nil {
		t.Fatalf("--resume of a running record was accepted:\n%s", out.String())
	} else if !strings.Contains(err.Error(), "--take-over "+handle.OperationID()) {
		t.Fatalf("the refusal does not name --take-over: %v", err)
	}

	// 2. --take-over --confirm reaches the reconciliation and claims it.
	taking := f
	taking.TakeOver = handle.OperationID()
	taking.Confirm = true
	out.Reset()
	_, h2, _, err := controlPlaneLock(ctx, kube, taking, session, &out, started, "")
	if err != nil {
		t.Fatalf("--take-over --confirm was refused: %v\n%s", err, out.String())
	}
	if h2.Token() == handle.Token() {
		t.Fatal("the take-over did not change the record's ownership token")
	}
	if !strings.Contains(out.String(), "Taking over operation "+handle.OperationID()) {
		t.Fatalf("the take-over did not reach the reconciliation:\n%s", out.String())
	}
	var rec operation.Record
	if err := json.Unmarshal(kube.doc, &rec); err != nil {
		t.Fatalf("the record on the cluster is not a record: %v", err)
	}
	if len(rec.TakeOvers) != 1 || rec.TakeOvers[0].Replaced.Token != handle.Token() {
		t.Fatalf("the take-over is not recorded in the record: %+v", rec.TakeOvers)
	}
	if rec.Executor.Token != h2.Token() {
		t.Errorf("the record's executor is %s, want the take-over's own token", rec.Executor.Token)
	}
}
