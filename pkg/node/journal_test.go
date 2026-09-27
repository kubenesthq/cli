package node

import (
	"context"
	"os"
	"strings"
	"testing"

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
