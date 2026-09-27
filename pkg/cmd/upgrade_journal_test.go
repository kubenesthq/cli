package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/upgrade"
)

// upgradeJournalPath is where this cluster's workload-upgrade journal lives,
// under the isolated HOME.
func upgradeJournalPath(t *testing.T) string {
	t.Helper()
	path, err := upgrade.JournalPath(testCluster)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// seedUpgradeJournal lays down a journal file the way a previous process left
// it, without opening it through the engine (which would check the identity).
func seedUpgradeJournal(t *testing.T, path string, journal stages.Journal) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the journal was not written, so this test would pass vacuously: %v", err)
	}
}

// upgradeFlagsFor is one workload upgrade request: the cluster, the bundle, and
// the nodes named explicitly (this laptop has no install journal for it in the
// test's HOME).
func upgradeFlagsFor(servers ...string) UpgradeFlags {
	return UpgradeFlags{Cluster: testCluster, To: "1.1", Servers: servers}
}

// A FINISHED workload upgrade must not bind the cluster's next one. Its journal
// is a leftover, and reading it as a resume of a different request is what made
// 1.1 → 1.2 after 1.0 → 1.1 refuse to run from the same laptop
// (kn-t73-upgrade-host-step-apt-jp0a.1). The journal is replaced, and said in
// one line.
func TestAFinishedUpgradeJournalDoesNotRefuseTheNextUpgrade(t *testing.T) {
	isolateHome(t)
	client := rebootControlPlane(t, []api.HostRecord{serverHost()},
		windowRecord([]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, "00:00", "23:59", "UTC"))
	path := upgradeJournalPath(t)

	// A journal from an earlier, FINISHED upgrade of this cluster, to a
	// different transition: every stage's last word is `completed`.
	seedUpgradeJournal(t, path, stages.Journal{
		Identity: stages.Identity{
			Kind: upgrade.Kind, Cluster: testCluster,
			Fields: map[string]string{"from bundle": "1.0", "to bundle": "1.1", "servers": "10.0.3.7", "agents": ""},
		},
		Entries: []stages.Entry{
			{Stage: "preflight", Status: stages.StatusCompleted},
			{Stage: "host-policy", Status: stages.StatusCompleted},
			{Stage: "kubernetes", Status: stages.StatusCompleted},
		},
	})

	var out bytes.Buffer
	session, err := buildUpgradeSessionWith(context.Background(), &out, upgradeFlagsFor("10.0.3.7"), client)
	if err != nil {
		t.Fatalf("a journal from a finished upgrade must not refuse the next upgrade: %v", err)
	}
	if !strings.Contains(out.String(), "is from a finished upgrade") {
		t.Errorf("replacing a finished journal must be said in one line:\n%s", out.String())
	}
	// The replaced journal is a NEW one: none of the finished run's entries
	// survive to be skipped.
	if len(session.Jnl.Entries) != 0 {
		t.Errorf("the replaced journal still carries the finished upgrade's entries: %+v", session.Jnl.Entries)
	}
}

// A journal with a stage UNFINISHED is the record of an attempt that stopped:
// the identical command still resumes it, and a different request is still
// refused rather than run over it.
func TestAnUnfinishedUpgradeJournalStillResumesTheIdenticalCommand(t *testing.T) {
	isolateHome(t)
	client := rebootControlPlane(t, []api.HostRecord{serverHost()},
		windowRecord([]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, "00:00", "23:59", "UTC"))
	flags := upgradeFlagsFor("10.0.3.7")

	// The identity the run computes, before any journal exists: nothing is
	// written until a stage runs.
	var out bytes.Buffer
	first, err := buildUpgradeSessionWith(context.Background(), &out, flags, client)
	if err != nil {
		t.Fatal(err)
	}
	path := upgradeJournalPath(t)
	seedUpgradeJournal(t, path, stages.Journal{
		Identity: first.Jnl.Identity,
		Entries: []stages.Entry{
			{Stage: "preflight", Status: stages.StatusCompleted},
			{Stage: "kubernetes", Status: stages.StatusFailed},
		},
	})

	// The identical command resumes it: the journal it stopped in comes back,
	// with the stages it completed still completed.
	var resumedOut bytes.Buffer
	resumed, err := buildUpgradeSessionWith(context.Background(), &resumedOut, flags, client)
	if err != nil {
		t.Fatalf("the identical command must still resume an unfinished upgrade: %v", err)
	}
	if len(resumed.Jnl.Entries) != 2 {
		t.Errorf("the identical command did not pick up the journal it stopped in: %+v", resumed.Jnl.Entries)
	}
	if _, ok := resumed.Jnl.Completed("preflight"); !ok {
		t.Error("the resume lost the stage an earlier run completed, so it will run it again")
	}
	if strings.Contains(resumedOut.String(), "is from a finished") {
		t.Errorf("an unfinished journal is not replaced, so there is nothing to say:\n%s", resumedOut.String())
	}

	// A request that is NOT the identical one is refused, and the journal is
	// left where the attempt stopped.
	other := flags
	other.Servers = []string{"10.0.3.8"}
	if _, err := buildUpgradeSessionWith(context.Background(), &bytes.Buffer{}, other, client); err == nil {
		t.Fatal("a half-finished journal must still bind its identity")
	} else if !strings.Contains(err.Error(), "records a different") {
		t.Fatalf("want the engine's identity refusal, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the refused run touched the journal it must not resume: %v", err)
	}
}

// A workload upgrade that SUCCEEDED leaves no journal behind, and one that did
// not keeps it. The tail is exercised with no operation record at all, which is
// the common case: a plain run takes none until a stage needs one.
//
// THE ASSERTION IS A FILE (the shape controlplane_journal_test.go uses): a test
// that only checked that some call exists would pass against a function that
// removes the journal for a FAILED run as well, which is worse than leaving it.
func TestASucceededUpgradeLeavesNoJournalForTheNextOneToSkipInto(t *testing.T) {
	isolateHome(t)
	path := upgradeJournalPath(t)
	identity := upgrade.Options{Cluster: testCluster, To: "1.2", Servers: []string{"10.0.3.7"}}.Identity("1.1")

	// The journal of an upgrade that got all the way through.
	journal, err := stages.OpenJournal(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"preflight", "host-policy", "kubernetes"} {
		if err := journal.Append(stages.Entry{Stage: stage, Status: stages.StatusCompleted}); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Save(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	finishUpgradeRun(context.Background(), &out, nil, nil, journal, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the succeeded upgrade left its journal at %s, so the next upgrade skips the stages it names and reports success having changed nothing (stat error: %v)",
			path, err)
	}

	// A run that FAILED keeps it — the journal is where the attempt stopped —
	// and a run that was PAUSED does too: a pause is not an ending.
	failed, err := stages.OpenJournal(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Append(stages.Entry{Stage: "host-policy", Status: stages.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := failed.Append(stages.Entry{Stage: "kubernetes", Status: stages.StatusFailed}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	finishUpgradeRun(context.Background(), &out, nil, nil, failed, errors.New("the kubernetes stage failed"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a failed run's journal was removed, so the next run cannot continue from where it stopped: %v", err)
	}
	out.Reset()
	finishUpgradeRun(context.Background(), &out, nil, nil, failed, stages.Paused("the maintenance window closed mid-run"))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a paused run's journal was removed, so the resume has nothing to continue from: %v", err)
	}
	// It is still the journal it was: the stage the failure names does not read
	// as completed.
	kept, err := stages.ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kept.Completed("kubernetes"); ok {
		t.Error("the failed stage reads as completed in the journal that was kept")
	}
}
