package node

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

// otherMachineIdentity is a journal identity for the SAME kind and cluster as
// the fixture's, and a different machine: exactly the shape the next run of a
// verb finds on disk when an earlier run of it in the same cluster finished
// first. The real identity also carries the bundle and the storage flag, and
// any of them differing is what used to refuse the run.
func otherMachineIdentity() stages.Identity {
	return stages.Identity{
		Kind: "node-test", Cluster: "prod-1",
		Fields: map[string]string{"machine": "167.233.20.250"},
	}
}

// aFinishedJournalsEntries is what today's CLI leaves behind after a run that
// got all the way through: every stage's LAST word is `completed`.
func aFinishedJournalsEntries() []stages.Entry {
	return []stages.Entry{
		{Stage: "preflight", Status: stages.StatusCompleted},
		{Stage: "record", Status: stages.StatusCompleted},
	}
}

// anUnfinishedJournalsEntries is the same file after a run that did not finish:
// one stage failed and another never completed.
func anUnfinishedJournalsEntries() []stages.Entry {
	return []stages.Entry{
		{Stage: "preflight", Status: stages.StatusCompleted},
		{Stage: "join", Status: stages.StatusFailed},
		{Stage: "record", Status: stages.StatusStarted},
	}
}

// assertNoJournal is the finish-path side of the fix: a finished operation must
// not leave its journal behind, because the next run of the same verb on the
// cluster reads it as a different resume.
func assertNoJournal(t *testing.T, f *nodeFixture) {
	t.Helper()
	if _, err := os.Stat(f.journalPath); !os.IsNotExist(err) {
		t.Errorf("a finished operation left its journal behind (%s): the next run of this verb on this cluster would be refused as a different resume", f.journalPath)
	}
}

// joinTheNextSpare scripts the machine being added so that a run can complete.
func joinTheNextSpare(t *testing.T, f *nodeFixture, node string) {
	t.Helper()
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(joinedNodes(t, node, true))
		return sshx.Result{}, nil
	})
}

// A journal left by a FINISHED add is not a resume target: the operation is
// over. It is replaced, so the next add runs, and the replacement is said in one
// line. Planted negative: the engine's OpenJournal refuses it as "a different
// node-add", which is what made each verb work once per cluster from one laptop.
func TestAddReplacesAFinishedJournalAndSaysSo(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	f.seedJournal(t, otherMachineIdentity(), aFinishedJournalsEntries()...)
	const newNode = "prod-1-agt-2"
	joinTheNextSpare(t, f, newNode)

	session, note, err := f.reopen(t, "run-2")
	if err != nil {
		t.Fatalf("a journal from a finished add must not refuse the next add: %v", err)
	}
	if note == "" {
		t.Error("replacing a finished journal must be said in one line")
	}
	add := &Add{Session: session, Opts: AddOptions{Agent: testAgentAddr}}
	if _, err := f.runAdd(add); err != nil {
		t.Fatalf("the add after a finished journal failed: %v\n%s", err, f.out.String())
	}
	if !strings.Contains(f.out.String(), note) {
		t.Errorf("the one line was not printed:\n%s", f.out.String())
	}
	assertNoJournal(t, f)
}

// The same for `node remove`: a finished remove's journal must not bind the
// next removal, which is the hardware story — two removals of different nodes
// on one cluster from one laptop.
func TestRemoveReplacesAFinishedJournalAndSaysSo(t *testing.T) {
	f := removeFixture(t)
	f.seedJournal(t, otherMachineIdentity(), aFinishedJournalsEntries()...)

	session, note, err := f.reopen(t, "run-2")
	if err != nil {
		t.Fatalf("a journal from a finished remove must not refuse the next remove: %v", err)
	}
	if note == "" {
		t.Error("replacing a finished journal must be said in one line")
	}
	remove := &Remove{Session: session, Opts: RemoveOptions{Node: "h-agt"}}
	if _, err := f.runRemove(remove); err != nil {
		t.Fatalf("the remove after a finished journal failed: %v\n%s", err, f.out.String())
	}
	if !strings.Contains(f.out.String(), note) {
		t.Errorf("the one line was not printed:\n%s", f.out.String())
	}
	assertNoJournal(t, f)
}

// And for `node replace`, whose journal is opened for the same kind: cluster
// pair.
func TestReplaceReplacesAFinishedJournalAndSaysSo(t *testing.T) {
	f := replaceFixture(t, true, true)
	const newNode = "prod-1-agt-2"
	f.joins(t, newNode)
	f.seedJournal(t, otherMachineIdentity(), aFinishedJournalsEntries()...)

	session, note, err := f.reopen(t, "run-2")
	if err != nil {
		t.Fatalf("a journal from a finished replace must not refuse the next replace: %v", err)
	}
	if note == "" {
		t.Error("replacing a finished journal must be said in one line")
	}
	replace := NewReplace(session, ReplaceOptions{Node: "h-agt", With: testAgentAddr})
	if _, err := f.runReplace(replace); err != nil {
		t.Fatalf("the replace after a finished journal failed: %v\n%s", err, f.out.String())
	}
	if !strings.Contains(f.out.String(), note) {
		t.Errorf("the one line was not printed:\n%s", f.out.String())
	}
	assertNoJournal(t, f)
}

// A journal with ANY stage unfinished keeps today's behaviour: it is the record
// of a cluster that changed, so a run with different arguments is refused rather
// than silently starting over.
func TestAnUnfinishedJournalStillRefusesADifferentOperation(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	f.seedJournal(t, otherMachineIdentity(), anUnfinishedJournalsEntries()...)

	if _, _, err := f.reopen(t, "run-2"); err == nil {
		t.Fatal("a half-finished journal must still bind its identity")
	} else if !strings.Contains(err.Error(), "records a different") {
		t.Fatalf("want the engine's identity refusal, got %v", err)
	}
}

// A run that finishes leaves no journal: that is what makes the next run of the
// verb on this cluster possible at all.
func TestFinishedOperationsLeaveNoJournal(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		f := newFixture(t, serverHost(), agentHost())
		const newNode = "prod-1-agt-2"
		joinTheNextSpare(t, f, newNode)
		if _, err := f.runAdd(f.newAdd(AddOptions{})); err != nil {
			t.Fatalf("the add failed: %v\n%s", err, f.out.String())
		}
		assertNoJournal(t, f)
	})
	t.Run("remove", func(t *testing.T) {
		f := removeFixture(t)
		if _, err := f.runRemove(f.newRemove(RemoveOptions{})); err != nil {
			t.Fatalf("the remove failed: %v\n%s", err, f.out.String())
		}
		assertNoJournal(t, f)
	})
	t.Run("replace", func(t *testing.T) {
		f := replaceFixture(t, true, true)
		const newNode = "prod-1-agt-2"
		f.joins(t, newNode)
		if _, err := f.runReplace(f.newReplace(ReplaceOptions{})); err != nil {
			t.Fatalf("the replace failed: %v\n%s", err, f.out.String())
		}
		assertNoJournal(t, f)
	})
}

// A failed run KEEPS its journal — that is the record to resume from — and the
// identical command still resumes it; once the resume finishes, the journal goes.
func TestAFailedRunKeepsItsJournalAndTheIdenticalCommandResumesIt(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	const newNode = "prod-1-agt-2"
	joinTheNextSpare(t, f, newNode)
	// The FIRST run's active write does not land: the host joined and the record
	// does not say so yet.
	f.records.failOn = 2

	if _, err := f.runAdd(f.newAdd(AddOptions{})); err == nil {
		t.Fatal("the planted inventory-write failure did not fail the run")
	}
	if _, err := os.Stat(f.journalPath); err != nil {
		t.Fatalf("a failed run must keep its journal: %v", err)
	}

	session, note, err := f.reopen(t, "run-2")
	if err != nil {
		t.Fatalf("the identical command must still resume a failed run: %v", err)
	}
	if note != "" {
		t.Errorf("an unfinished journal is not replaced, so there is nothing to say: %q", note)
	}
	next := &Add{Session: session, Opts: AddOptions{Agent: testAgentAddr}}
	_, resumeErr := stages.Execute(context.Background(), next, PlanAdd(next))
	next.Finish(context.Background(), resumeErr, false)
	if resumeErr != nil {
		t.Fatalf("the resume failed: %v\n%s", resumeErr, f.out.String())
	}
	assertNoJournal(t, f)
}

// verbIdentity is the journal identity prepareNodeSession gives one node verb on
// the fixture's cluster: the verb, the cluster, the bundle its record names, the
// machine as the operator typed it, and no storage device.
func verbIdentity(kind operation.Kind, machine string) stages.Identity {
	return stages.Identity{Kind: string(kind), Cluster: "prod-1", Fields: map[string]string{
		"bundle": "1.1", "machine": machine, "storage": "",
	}}
}

// openAs is one CLI invocation in the fixture's world whose journal is opened
// the way prepareNodeSession opens it, under the identity that invocation's
// flags give. It returns the open error, which is where a binding journal
// refuses.
func (f *nodeFixture) openAs(t *testing.T, runID string, identity stages.Identity) (*Session, error) {
	t.Helper()
	journal, note, err := stages.OpenJournalReplacingFinished(f.journalPath, identity)
	if err != nil {
		return nil, err
	}
	if note != "" {
		fmt.Fprintln(f.out, note)
	}
	next := *f.session
	next.ID = runID
	next.Jnl = journal
	next.Window = openWindow()
	return &next, nil
}

// refusedAt asserts that the journal on disk records the shape the hardware run
// left: the resolve stage COMPLETED and a later check refused. Today's rule
// already frees a journal in which no stage completed; this is the one it did
// not.
func refusedAt(t *testing.T, f *nodeFixture, stage string) {
	t.Helper()
	j, err := stages.ReadJournal(f.journalPath)
	if err != nil {
		t.Fatalf("the refused run left no journal: %v", err)
	}
	if _, ok := j.Completed("resolve"); !ok {
		t.Errorf("resolve did not complete, so this is not the hardware shape: %+v", j.Entries)
	}
	if last, ok := j.LastFailure(); !ok || last.Stage != stage {
		t.Errorf("the run was not refused by stage %s: %+v", stage, j.Entries)
	}
}

// A REMOVE REFUSED BEFORE IT CHANGED ANYTHING DOES NOT BIND THE NEXT REMOVE.
// Found on hardware by the S7 gate (lab w3, 2026-11-12, cli a462512):
// `node remove --node A --now` was rightly refused because A holds bound local
// volumes. Its resolve stage completed and its volumes stage refused, and both
// only read. The next `node remove --node B --now`, for another node, was then
// refused as "a different node-remove" until the journal was deleted by hand.
func TestARemoveRefusedForItsVolumesDoesNotBindTheNextRemove(t *testing.T) {
	const machineA, machineB = "10.0.3.8", "10.0.3.10"
	b := agentHost()
	b.HostID, b.SSHAddress, b.NodeUID, b.HostKeyFingerprint = "h-agt-b", machineB, "uid-agt-b", "SHA256:agent-b"
	f := removeFixture(t, serverHost(), agentHost(), b)
	f.server.setNodes(nodesJSON(t,
		testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{machineA}, Ready: true},
		testNode{Name: "prod-1-agt-b", UID: "uid-agt-b", Addresses: []string{machineB}, Ready: true},
	))
	f.addHost(machineB, "SHA256:agent-b", nodesJSON(t))
	f.boundLocalVolume(t, "db", "data-pg-0", "pg") // on A's node only

	first, err := f.openAs(t, "run-1", verbIdentity(operation.KindNodeRemove, machineA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.runRemove(&Remove{Session: first, Opts: RemoveOptions{Node: machineA, Now: true}}); err == nil || !strings.Contains(err.Error(), "--abandon-volumes") {
		t.Fatalf("the remove of a node holding a bound local volume must be refused for that volume: %v", err)
	}
	refusedAt(t, f, StageRemoveVolumes)

	second, err := f.openAs(t, "run-2", verbIdentity(operation.KindNodeRemove, machineB))
	if err != nil {
		t.Fatalf("a remove refused before it changed anything bound the next remove of another node: %v", err)
	}
	if _, err := f.runRemove(&Remove{Session: second, Opts: RemoveOptions{Node: machineB, Now: true}}); err != nil {
		t.Fatalf("the remove of the other node failed: %v\n%s", err, f.out.String())
	}
	if got := f.lastState("h-agt-b"); got != string(StateRemoved) {
		t.Errorf("node B is recorded as %q, want %q", got, StateRemoved)
	}
	if got := f.lastState("h-agt"); got != string(StateActive) {
		t.Errorf("node A, whose removal was refused, is recorded as %q, want %q", got, StateActive)
	}
	assertNoJournal(t, f)
}

// THE PROTECTION THAT STAYS. A remove that took its lock, held the node, wrote
// it down as removing and cordoned it, and then failed to drain, has left a
// half-removed node behind. Its journal still binds: a remove of another node is
// refused as a different operation, exactly as before.
func TestARemoveThatChangedTheClusterStillBindsTheNextRemove(t *testing.T) {
	f := removeFixture(t)
	f.server.on("drain "+testAgentNode, fail(1, "error: cannot evict pod as it would violate the pod's disruption budget"))

	first, err := f.openAs(t, "run-1", verbIdentity(operation.KindNodeRemove, "10.0.3.8"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.runRemove(&Remove{Session: first, Opts: RemoveOptions{Node: "10.0.3.8", Now: true}}); err == nil {
		t.Fatal("the planted drain failure did not fail the run")
	}
	j, err := stages.ReadJournal(f.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Completed(StageRemoveCordon); !ok {
		t.Fatalf("the run did not cordon the node, so it is not the changed cluster this test is about: %+v", j.Entries)
	}

	_, err = f.openAs(t, "run-2", verbIdentity(operation.KindNodeRemove, "10.0.3.10"))
	if err == nil {
		t.Fatal("the journal of a remove that cordoned its node was replaced by a remove of another node")
	}
	if !strings.Contains(err.Error(), "records a different") || !strings.Contains(err.Error(), "machine") {
		t.Errorf("want the engine's identity refusal naming the machine, got %v", err)
	}
}

// The other two node verbs alike: `node add` refused by its pre-flight checks
// and `node replace` refused for the old machine's local volumes have each
// completed read-only stages and changed nothing, so neither binds the next run
// of its verb on the cluster to the machine it refused.
func TestAnAddOrAReplaceRefusedBeforeItChangedAnythingDoesNotBindTheNextOne(t *testing.T) {
	t.Run("add refused by its pre-flight checks", func(t *testing.T) {
		f := newFixture(t, serverHost(), agentHost())
		f.agent.on("/etc/os-release", ok("ID=ubuntu\nVERSION_ID=\"22.04\"\nPRETTY_NAME=\"Ubuntu 22.04.3 LTS\"\n"))
		first, err := f.openAs(t, "run-1", verbIdentity(operation.KindNodeAdd, testAgentAddr))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.runAdd(&Add{Session: first, Opts: AddOptions{Agent: testAgentAddr}}); err == nil {
			t.Fatal("an unsupported Ubuntu release was accepted")
		}
		refusedAt(t, f, StageAddPreflight)
		if _, err := f.openAs(t, "run-2", verbIdentity(operation.KindNodeAdd, "10.0.3.10")); err != nil {
			t.Errorf("an add refused by its pre-flight checks bound the next add of another machine: %v", err)
		}
	})
	t.Run("replace refused for the old machine's volumes", func(t *testing.T) {
		f := replaceFixture(t, true, true)
		f.strands(t, testAgentNode, strandedClaim{Namespace: "db", Workload: "pg", PVCs: []string{"data-pg-0"}})
		first, err := f.openAs(t, "run-1", verbIdentity(operation.KindNodeReplace, "h-agt"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.runReplace(NewReplace(first, ReplaceOptions{Node: "h-agt", With: testAgentAddr})); err == nil {
			t.Fatal("a reachable machine holding local volumes was replaced")
		}
		refusedAt(t, f, StageReplaceVolumes)
		if _, err := f.openAs(t, "run-2", verbIdentity(operation.KindNodeReplace, "10.0.3.10")); err != nil {
			t.Errorf("a replace refused for its volumes bound the next replace of another machine: %v", err)
		}
	})
}
