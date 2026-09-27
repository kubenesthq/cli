package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/window"
)

// strandedClaim is one workload's share of the volumes a machine is holding:
// the claims that must come back TOGETHER, because restoring one claim of a pod
// while another stays stranded leaves the pod unable to start.
type strandedClaim struct {
	Namespace string
	Workload  string
	PVCs      []string
}

// strands makes the cluster report BOUND local volumes on node, held by the
// workloads the caller names.
//
// One call per fixture: the scripted machine answers the FIRST rule that
// matches, so the whole stranded set goes into one answer, the way the cluster
// gives it.
func (f *nodeFixture) strands(t *testing.T, node string, claims ...strandedClaim) {
	t.Helper()
	var pvs, pods []string
	for _, claim := range claims {
		var mounted []string
		for _, pvc := range claim.PVCs {
			pvs = append(pvs, `{"metadata":{"name":"pv-`+pvc+`"},"spec":{`+
				`"claimRef":{"namespace":"`+claim.Namespace+`","name":"`+pvc+`"},`+
				`"csi":{"driver":"local.csi.openebs.io"},`+
				`"nodeAffinity":{"required":{"nodeSelectorTerms":[{"matchExpressions":[`+
				`{"key":"kubernetes.io/hostname","operator":"In","values":["`+node+`"]}]}]}}},`+
				`"status":{"phase":"Bound"}}`)
			mounted = append(mounted, `{"persistentVolumeClaim":{"claimName":"`+pvc+`"}}`)
		}
		pods = append(pods, `{"metadata":{"namespace":"`+claim.Namespace+`",`+
			`"ownerReferences":[{"kind":"StatefulSet","name":"`+claim.Workload+`"}]},`+
			`"spec":{"volumes":[`+strings.Join(mounted, ",")+`]}}`)
	}
	f.server.on("get pv -o json", ok(`{"items":[`+strings.Join(pvs, ",")+`]}`))
	f.server.on("get pods --all-namespaces -o json", ok(`{"items":[`+strings.Join(pods, ",")+`]}`))
}

// replaceFixture is the world a replace runs in: the cluster's server and the
// agent being replaced, the machine that replaces it (a fixture host that is
// not a node of this cluster yet), and whether the machine being replaced
// answers at all.
func replaceFixture(t *testing.T, oldReachable, oldReady bool) *nodeFixture {
	t.Helper()
	old := agentHost()
	f := newFixture(t, serverHost(), old)
	f.server.setNodes(nodesJSON(t,
		testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{old.SSHAddress}, Ready: oldReady},
	))
	if oldReachable {
		// The machine being replaced answers SSH with the host key the
		// inventory recorded for it.
		f.addHost(old.SSHAddress, old.HostKeyFingerprint, nodesJSON(t))
	}
	return f
}

// runReplace drives the real engine over the real plan, and closes the
// operation the way the command does — so what these tests exercise is the verb
// and not a rehearsal of it.
func (f *nodeFixture) runReplace(r *Replace) (stages.Result, error) {
	f.t.Helper()
	result, err := RunReplace(context.Background(), r)
	r.Finish(context.Background(), err, false)
	return result, err
}

func (f *nodeFixture) newReplace(opts ReplaceOptions) *Replace {
	if opts.Node == "" {
		// The machine being replaced, named the way an operator names it when
		// the machine has several names: its inventory host ID.
		opts.Node = "h-agt"
	}
	if opts.With == "" {
		opts.With = testAgentAddr
	}
	return NewReplace(f.session, opts)
}

// stageIndex is the position of a stage in the order the run reports, or -1.
func stageIndex(result stages.Result, name string) int {
	for i, ran := range result.Ran {
		if ran == name {
			return i
		}
	}
	return -1
}

// withAddress finds an inventory entry by the address it is reached at.
func withAddress(hosts []api.HostRecord, address string) (api.HostRecord, bool) {
	for _, h := range hosts {
		if h.SSHAddress == address {
			return h, true
		}
	}
	return api.HostRecord{}, false
}

// A server is not replaceable by this command either: the single-server tier is
// recovered by S6, and the ha tier's server operations arrive with its
// promotion. Both are named, and the refusal costs nothing — neither machine is
// touched.
func TestReplaceRefusesAServerNode(t *testing.T) {
	f := replaceFixture(t, true, true)

	_, err := f.runReplace(f.newReplace(ReplaceOptions{Node: testServerAddr}))
	if err == nil {
		t.Fatal("a server was accepted as the machine to replace")
	}
	for _, want := range []string{"S6", "promotion"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if len(f.log.entries) != 0 {
		t.Errorf("the refusal touched something before refusing:\n%s", strings.Join(f.log.commands(testServerAddr), "\n"))
	}
	if f.log.hasCommand("get.k3s.io") || f.log.hasCommand("cordon") || f.log.hasCommand("drain") {
		t.Error("a machine was changed for a replace that may not happen")
	}
	if len(f.records.savedRecords()) != 0 {
		t.Error("the inventory was written for a replace that may not happen")
	}
}

// A REACHABLE machine that holds local volumes is refused BEFORE the replacement
// is added: adding first would strand a second set of volumes while the first is
// still unresolved. The refusal carries the migration to run instead — the
// backup, and one restore command per workload with every claim of it grouped.
func TestReplaceReachableWithLocalVolumesRefusesBeforeTheSpareJoins(t *testing.T) {
	f := replaceFixture(t, true, true)
	f.strands(t, testAgentNode, strandedClaim{Namespace: "db", Workload: "pg", PVCs: []string{"data-pg-0", "data-pg-1"}})

	_, err := f.runReplace(f.newReplace(ReplaceOptions{}))
	if err == nil {
		t.Fatal("a reachable machine holding local volumes was replaced")
	}
	command := "kubenest backup restore --cluster prod-1 --namespace db --pvc data-pg-0 --pvc data-pg-1"
	if !strings.Contains(err.Error(), command) {
		t.Errorf("the refusal does not carry the grouped restore command %q:\n%v", command, err)
	}
	if n := strings.Count(err.Error(), "kubenest backup restore"); n != 1 {
		t.Errorf("%d restore commands for one workload's two claims, want 1: the pod cannot start unless both are restored together", n)
	}
	// The planned migration names what to run, in order: back up, restore,
	// replace. Naming them is the point of the refusal — an operator who is
	// told only "no" has to invent the rest.
	for _, want := range []string{"kubenest backup now", "kubenest node replace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the migration does not name %q:\n%v", want, err)
		}
	}
	// NOTHING WAS ADDED AND NOTHING WAS DRAINED, which is the whole ordering
	// claim: the spare must not have been touched, so no install ran anywhere
	// and no inventory write happened.
	if f.log.hasCommand("get.k3s.io") {
		t.Error("the replacement machine was joined before the volume disposition was settled")
	}
	if f.log.hasCommand("cordon") || f.log.hasCommand("drain") {
		t.Error("the machine was cordoned or drained by a replace that refused")
	}
	if len(f.records.savedRecords()) != 0 {
		t.Error("the inventory was written by a replace that refused")
	}
	if f.log.hasCommand("create -f -") {
		t.Error("an operation record was created by a replace that refused")
	}
}

// A reachable machine that holds no local volume is replaced ADD FIRST: the
// replacement joins, is Ready and is written down as active, and only then is
// the machine it replaces drained and removed. Capacity for the workload set is
// never reduced to zero.
func TestReplaceReachableWithoutLocalVolumesAddsBeforeItRemoves(t *testing.T) {
	f := replaceFixture(t, true, true)
	const newNode = "prod-1-agt-2"
	f.joins(t, newNode)

	result, err := f.runReplace(f.newReplace(ReplaceOptions{}))
	if err != nil {
		t.Fatalf("the replace failed: %v\n%s", err, f.out.String())
	}
	if stageIndex(result, StageReplaceAddRecord) > stageIndex(result, StageReplaceRemoveCordon) {
		t.Errorf("the removal's stages ran before the join was recorded: %v", result.Ran)
	}
	joined := f.log.indexOf("get.k3s.io")
	if joined < 0 {
		t.Fatal("the replacement machine was never joined")
	}
	cordon := f.log.indexOf("cordon ")
	if cordon < 0 {
		t.Fatal("the machine being replaced was never cordoned")
	}
	if joined > cordon {
		t.Error("the machine being replaced was cordoned before the replacement joined")
	}
	// The spare is RECORDED ACTIVE — Ready, its volume group checked, its
	// inventory entry written — before the drain starts, which is what makes the
	// drain safe to run at all.
	spare, found := withAddress(f.records.inventory(), testAgentAddr)
	if !found {
		t.Fatalf("the replacement machine is not in the inventory: %+v", f.records.inventory())
	}
	active := f.log.indexOf("inventory-write " + spare.HostID + " " + string(StateActive))
	if active < 0 || active > f.log.indexOf("drain ") {
		t.Errorf("the replacement was not recorded active before the drain: active at %d, drain at %d", active, f.log.indexOf("drain "))
	}
	// Each half acted on ITS OWN machine: the join on the replacement, the k3s
	// uninstall on the machine being replaced.
	if f.log.indexOf("get.k3s.io") != firstOn(f.log, testAgentAddr, "get.k3s.io") {
		t.Errorf("the join did not run on the replacement machine")
	}
	if !hasOn(f.log, agentHost().SSHAddress, "/usr/local/bin/k3s-agent-uninstall.sh") {
		t.Errorf("k3s was not uninstalled from the machine being replaced: %v", f.log.commands(agentHost().SSHAddress))
	}
	if hasOn(f.log, testAgentAddr, "/usr/local/bin/k3s-agent-uninstall.sh") {
		t.Error("the k3s uninstall of the replaced machine ran on the replacement")
	}
	// The machine being replaced is gone from the cluster and recorded removed,
	// with its identity kept.
	if !f.log.hasCommand("delete node " + testAgentNode) {
		t.Error("the Node object of the machine being replaced was not deleted")
	}
	if got := f.lastState("h-agt"); got != string(StateRemoved) {
		t.Errorf("the replaced host is recorded as %q, want removed", got)
	}
	old, _ := f.hostIn("h-agt")
	if old.HostID != "h-agt" || old.HostKeyFingerprint != agentHost().HostKeyFingerprint {
		t.Errorf("the replaced host lost its identity: %+v", old)
	}
	if f.log.hasCommand("kubenest backup restore") {
		t.Error("a restore command was printed for a machine that held no local volume")
	}
}

// An unreachable machine is replaced REMOVE FIRST: it is taken out of the
// cluster, and only then does the replacement join — etcd's own order, and the
// only order that works when the old machine may come back.
func TestReplaceUnreachableRemovesBeforeItAdds(t *testing.T) {
	f := replaceFixture(t, false, false)
	const newNode = "prod-1-agt-2"
	f.joins(t, newNode)

	result, err := f.runReplace(f.newReplace(ReplaceOptions{ConfirmIsolated: true}))
	if err != nil {
		t.Fatalf("the replace of an unreachable machine failed: %v\n%s", err, f.out.String())
	}
	if stageIndex(result, StageReplaceRemoveCordon) > stageIndex(result, StageReplaceAddResolve) {
		t.Errorf("the join ran before the removal: %v", result.Ran)
	}
	cordon := f.log.indexOf("cordon ")
	joined := f.log.indexOf("get.k3s.io")
	if cordon < 0 || joined < 0 || cordon > joined {
		t.Errorf("the removal (cordon at %d) did not run before the join (at %d)", cordon, joined)
	}
	if f.lastState("h-agt") != string(StateRemoved) {
		t.Errorf("the replaced host is recorded as %q, want removed", f.lastState("h-agt"))
	}
	if !f.log.hasCommand("delete node "+testAgentNode) || f.log.hasCommand("delete node "+newNode) {
		t.Error("the wrong Node object was deleted")
	}
}

// The order that removes first is the operator's decision, not the command's:
// without --confirm-isolated it refuses, naming the machine by host ID and
// address, and NOTHING is removed, added or even locked.
func TestReplaceUnreachableRequiresTheIsolationConfirmation(t *testing.T) {
	f := replaceFixture(t, false, false)

	_, err := f.runReplace(f.newReplace(ReplaceOptions{}))
	if err == nil {
		t.Fatal("an unreachable machine was replaced without confirming its isolation")
	}
	for _, want := range []string{"h-agt", agentHost().SSHAddress} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "--confirm-isolated") {
		t.Errorf("the refusal does not say how to confirm: %v", err)
	}
	for _, destructive := range []string{"cordon", "drain", "delete node", "get.k3s.io", "inventory-write"} {
		if f.log.hasCommand(destructive) {
			t.Errorf("%q was issued for a replace whose isolation nobody confirmed", destructive)
		}
	}
	if f.log.hasCommand("create -f -") {
		t.Error("an operation record was created for a replace whose isolation nobody confirmed")
	}
}

// The machine that is gone leaves one restore command per affected workload,
// run AFTER the replacement has joined — and every stranded claim of a workload
// is in the same command, because restoring one claim of a pod while another
// stays stranded leaves the pod unable to start.
func TestReplacePrintsOneRestoreCommandPerWorkloadWithEveryStrandedClaimGrouped(t *testing.T) {
	f := replaceFixture(t, false, false)
	const newNode = "prod-1-agt-2"
	f.joins(t, newNode)
	f.strands(t, testAgentNode,
		strandedClaim{Namespace: "db", Workload: "pg", PVCs: []string{"data-pg-0", "data-pg-1"}},
		strandedClaim{Namespace: "web", Workload: "cache", PVCs: []string{"cache-0"}},
	)

	result, err := f.runReplace(f.newReplace(ReplaceOptions{ConfirmIsolated: true}))
	if err != nil {
		t.Fatalf("the replace failed: %v\n%s", err, f.out.String())
	}
	out := f.out.String()
	grouped := "kubenest backup restore --cluster prod-1 --namespace db --pvc data-pg-0 --pvc data-pg-1"
	if !strings.Contains(out, grouped) {
		t.Errorf("the two claims of one pod are not in one command:\n%s", out)
	}
	if n := strings.Count(out, "kubenest backup restore"); n != 2 {
		t.Errorf("%d restore commands for two workloads, want one each:\n%s", n, out)
	}
	if !strings.Contains(out, "--namespace web --pvc cache-0") {
		t.Errorf("the second workload's own claim is not in its own command:\n%s", out)
	}
	// The commands come last, so the volumes they refill land on a live node.
	if stageIndex(result, StageReplaceRestoreCommands) < stageIndex(result, StageReplaceAddRecord) {
		t.Errorf("the restore commands were printed before the replacement joined: %v", result.Ran)
	}
	if f.log.indexOf("get.k3s.io") < 0 {
		t.Fatal("the replacement was never joined")
	}
}

// A resume continues the order the operation STARTED in, even when the machine
// has answered since — the order is part of the operation's immutable request —
// and a resume that names a different machine is refused rather than performed.
func TestReplaceResumeKeepsItsBranchAndTargets(t *testing.T) {
	f := replaceFixture(t, false, false)
	const newNode = "prod-1-agt-2"
	f.joins(t, newNode)
	// The removal gets as far as the drain and the run is INTERRUPTED there: a
	// stopped operation, which is what a resume continues (a failed one is
	// finished, and nobody may take it over).
	ctx, stop := context.WithCancel(context.Background())
	f.server.on("drain ", func(_ *fakeHost, _ string) (sshx.Result, error) {
		stop()
		return fail(1, "error: unable to drain node")(nil, "")
	})
	first := f.newReplace(ReplaceOptions{ConfirmIsolated: true})
	_, err := RunReplace(ctx, first)
	if err == nil {
		t.Fatalf("the replace was expected to stop at the drain:\n%s", f.out.String())
	}
	first.Finish(context.Background(), err, true)
	operationID := f.session.handle.OperationID()
	if operationID == "" {
		t.Fatal("the interrupted replace left no operation record")
	}
	if f.lastState("h-agt") == string(StateRemoved) {
		t.Fatal("the machine was fully removed, so this resume would prove nothing")
	}

	// THE MACHINE COMES BACK: it answers SSH and the cluster reports it Ready
	// again. Add-first is now the tempting reading of the facts, and a resume
	// must not take it.
	old := agentHost()
	f.server.setNodes(nodesJSON(t,
		testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{old.SSHAddress}, Ready: true},
	))
	f.addHost(old.SSHAddress, old.HostKeyFingerprint, nodesJSON(t))
	// The drain that stopped the first run is not replayed: the interruption
	// was the process's, not the cluster's.
	f.server.rules = nil
	f.joins(t, newNode)

	// A resume that names a DIFFERENT machine is refused: the request is
	// immutable, and continuing it onto another machine is how a cluster stops
	// matching its own record.
	other := "10.0.4.9"
	f.addHost(other, "SHA256:other", nodesJSON(t))
	before := len(f.log.entries)
	wrong := NewReplace(f.sessionFor(t, "run-2a"), ReplaceOptions{ConfirmIsolated: true, Resume: operationID, Node: "h-agt", With: other})
	if _, err := RunReplace(context.Background(), wrong); err == nil {
		t.Fatal("a resume naming another machine was accepted")
	} else if !strings.Contains(err.Error(), "with") {
		t.Errorf("the refusal does not say which part of the request changed: %v", err)
	}
	refused := f.log.entries[before:]
	for _, destructive := range []string{"cordon", "drain", "delete node", "get.k3s.io"} {
		if indexIn(refused, destructive) >= 0 {
			t.Errorf("%q ran for a resume that changed its target", destructive)
		}
	}

	// The identical resume continues, and it continues REMOVE FIRST: the
	// removal's remaining stages run before the replacement joins.
	resume := NewReplace(f.sessionFor(t, "run-2b"), ReplaceOptions{ConfirmIsolated: true, Resume: operationID, Node: "h-agt", With: testAgentAddr})
	start := len(f.log.entries)
	result, err := RunReplace(context.Background(), resume)
	resume.Finish(context.Background(), err, false)
	if err != nil {
		t.Fatalf("the resume failed: %v\n%s", err, f.out.String())
	}
	rest := f.log.entries[start:]
	drain, joined := indexIn(rest, "drain "), indexIn(rest, "get.k3s.io")
	if drain < 0 || joined < 0 || drain > joined {
		t.Errorf("the resume did not continue remove-first: drain at %d, join at %d", drain, joined)
	}
	if stageIndex(result, StageReplaceAddRecord) < 0 {
		t.Errorf("the resume did not finish the join: %v", result.Ran)
	}
	if f.lastState("h-agt") != string(StateRemoved) {
		t.Errorf("the replaced host is recorded as %q, want removed", f.lastState("h-agt"))
	}
	spare, found := withAddress(f.records.inventory(), testAgentAddr)
	if !found || spare.LifecycleState != string(StateActive) {
		t.Errorf("the replacement is not recorded active after the resume: %+v", spare)
	}
}

// A replace that has already JOINED its replacement is still resumable. The
// machine it added is an ACTIVE host of this cluster because THIS operation
// added it, and a resume that refused it as "nothing to add" would refuse the
// operation it is resuming — with a node in the cluster that nothing may reboot
// (it still carries the hold) and a machine that was never taken out.
func TestReplaceResumeContinuesAfterTheReplacementWasRecorded(t *testing.T) {
	f := replaceFixture(t, true, true)
	const newNode = "prod-1-agt-2"
	f.joins(t, newNode)
	// The join runs to the end — the replacement is Ready, recorded active and
	// its hold lifted — and the run is interrupted at the first thing the
	// removal does.
	ctx, stop := context.WithCancel(context.Background())
	f.server.on("cordon ", func(_ *fakeHost, _ string) (sshx.Result, error) {
		stop()
		return fail(1, "error: unable to cordon")(nil, "")
	})
	first := f.newReplace(ReplaceOptions{})
	_, err := RunReplace(ctx, first)
	if err == nil {
		t.Fatalf("the replace was expected to stop at the cordon:\n%s", f.out.String())
	}
	first.Finish(context.Background(), err, true)
	operationID := first.Session.handle.OperationID()
	if operationID == "" {
		t.Fatal("the interrupted replace left no operation record")
	}
	spare, found := withAddress(f.records.inventory(), testAgentAddr)
	if !found || spare.LifecycleState != string(StateActive) {
		t.Fatalf("the replacement was not recorded active before the interruption: %+v", spare)
	}
	if f.lastState("h-agt") != string(StateRemoving) {
		t.Fatalf("the removal did not get as far as recording the machine as removing: %q", f.lastState("h-agt"))
	}

	// The resume continues the removal, with the machine that replaces already
	// an active host of this cluster.
	f.server.rules = nil
	resume := NewReplace(f.sessionFor(t, "run-2"), ReplaceOptions{Node: "h-agt", With: testAgentAddr, Resume: operationID})
	result, err := RunReplace(context.Background(), resume)
	resume.Finish(context.Background(), err, false)
	if err != nil {
		t.Fatalf("the resume was refused although the replacement is a host this operation added: %v\n%s", err, f.out.String())
	}
	if f.lastState("h-agt") != string(StateRemoved) {
		t.Errorf("the replaced host is recorded as %q, want removed", f.lastState("h-agt"))
	}
	if !f.log.hasCommand("delete node " + testAgentNode) {
		t.Error("the removal did not finish: the Node object of the replaced machine is still there")
	}
	spare, found = withAddress(f.records.inventory(), testAgentAddr)
	if !found || spare.LifecycleState != string(StateActive) {
		t.Errorf("the replacement is no longer recorded active: %+v", spare)
	}
	if stageIndex(result, StageReplaceAddRecord) < 0 || stageIndex(result, StageReplaceRemoveRecord) < 0 {
		t.Errorf("the resume did not carry both halves to their records: %v", result.Ran)
	}
}

// --wait holds until the window opens, holding nothing while it waits, and reads
// the facts the ORDER depends on only then: the operation record and the volume
// disposition are both taken once the window is open, not hours earlier when the
// operator typed the command.
func TestReplaceWaitTakesItsFactsAndLocksWhenTheWindowOpens(t *testing.T) {
	f := replaceFixture(t, true, true)
	// The window opens at 10:00; the fixture's clock reads 09:00 on a Sunday.
	opening := &window.Window{Days: []time.Weekday{time.Sunday}, Start: 10 * 60, End: 23 * 60, Location: time.UTC}
	f.session.Window = opening
	f.joins(t, "prod-1-agt-2")

	if _, err := f.runReplace(f.newReplace(ReplaceOptions{Wait: true})); err != nil {
		t.Fatalf("a --wait run that the window opened for failed: %v\n%s", err, f.out.String())
	}
	if len(*f.slept) == 0 {
		t.Fatal("the run did not wait at all, so this test proves nothing")
	}
	for _, what := range []string{"create -f - -o json", "get pv -o json"} {
		at, ok := f.log.at(what)
		if !ok {
			t.Fatalf("%s never happened", what)
		}
		if !opening.Contains(at) {
			t.Errorf("%s happened at %s, which is outside the window %s", what, at, opening)
		}
	}
	if f.lastState("h-agt") != string(StateRemoved) {
		t.Errorf("the replaced host is recorded as %q, want removed", f.lastState("h-agt"))
	}
}

// joins makes the fixture's spare host join this cluster when the agent
// installer runs on it, which is what a real join looks like from the API: a
// node that was not there when the run started.
func (f *nodeFixture) joins(t *testing.T, node string) {
	t.Helper()
	f.agent.on("get.k3s.io", func(_ *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(nodesJSON(t,
			testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{agentHost().SSHAddress}, Ready: true},
			testNode{Name: node, UID: "uid-new", Addresses: []string{testAgentAddr}, Ready: true},
		))
		return sshx.Result{}, nil
	})
}

// firstOn is the logbook position of the first command containing substr on one
// host, or -1.
func firstOn(log *logbook, host, substr string) int {
	for i, e := range log.entries {
		if e.host == host && strings.Contains(e.command, substr) {
			return i
		}
	}
	return -1
}

// hasOn reports whether substr ran on one host.
func hasOn(log *logbook, host, substr string) bool { return firstOn(log, host, substr) >= 0 }

// indexIn is the position of the first command containing substr in one slice of
// the logbook, or -1. It is how a test reads the SECOND run over a world the
// first run already wrote to.
func indexIn(entries []entry, substr string) int {
	for i, e := range entries {
		if strings.Contains(e.command, substr) {
			return i
		}
	}
	return -1
}

// `node replace`'s add half is `node add`'s own record-joining write, so it
// carried the same empty volume_group_ownership: the replacement's joining
// entry is written before the volume group exists on it, and the control plane
// requires one of the two enum values on every entry.
func TestReplaceRecordsTheVolumeGroupOwnershipInTheJoiningEntry(t *testing.T) {
	cases := []struct {
		name   string
		device string
		want   string
	}{
		{"no --storage-device: the operator created it", "", string(storage.CustomerCreated)},
		{"--storage-device: the installer will create it", testDevice, string(storage.InstallerCreated)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := replaceFixture(t, true, true)
			if tc.device != "" {
				// The replacement is a fresh host: no volume group yet.
				f.agent.on("vgs", fail(1, "Volume group \"kubenest-vg\" not found"))
			}
			const newNode = "prod-1-agt-2"
			f.joins(t, newNode)

			if _, err := f.runReplace(f.newReplace(ReplaceOptions{StorageDevice: tc.device})); err != nil {
				t.Fatalf("the replace failed: %v\n%s", err, f.out.String())
			}
			entry := joiningEntryFor(t, f.records.savedRecords(), testAgentAddr)
			if entry.VolumeGroupOwnership != tc.want {
				t.Errorf("the replacement's joining entry carries volume_group_ownership %q, want %q", entry.VolumeGroupOwnership, tc.want)
			}
		})
	}
}
