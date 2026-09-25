package cmd

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/stages"
)

// A COMPLETED UPGRADE MUST LEAVE NO JOURNAL BEHIND (hardware, 2026-09-25).
//
// The journal is what lets a re-run skip completed stages, which is right for a
// resume of the SAME attempt and wrong for the next upgrade, which has work to
// do. Measured: after the control plane was put back on the previous candidate,
// the next run found the completed entries and printed "Upgraded the control
// plane to 1.1 in 14s" having skipped the fence, the migration, the chart and
// the unfence — and then reported success over a control plane it had not
// touched.
//
// THE ASSERTION IS A FILE. A test that only checked that some call exists would
// pass against a function that calls Remove on the wrong path, or that removes
// it for a FAILED run as well — which would be worse than leaving it.
func TestASucceededControlPlaneUpgradeLeavesNoJournalForTheNextOneToSkipInto(t *testing.T) {
	isolateHome(t)

	identity := stages.Identity{
		Kind:    controlplane.UpgradeKind,
		Cluster: "prod-1",
		Fields:  map[string]string{"from bundle": "1.0", "to bundle": "1.1"},
	}
	path, err := controlplane.JournalPath("prod-1")
	if err != nil {
		t.Fatal(err)
	}

	// A journal in exactly the state a skipped run reads: the stages of a
	// completed upgrade, by name.
	journal, err := stages.OpenJournal(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{controlplane.StageFence, controlplane.StageMigration, controlplane.StageChart, controlplane.StageUnfence} {
		if err := journal.Append(stages.Entry{Stage: stage, Status: stages.StatusCompleted, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the journal was not written, so this test would pass vacuously: %v", err)
	}

	// THE COMPLETED RUN REMOVES IT.
	var out bytes.Buffer
	if err := finishControlPlaneJournal(&out, journal, nil); err != nil {
		t.Fatalf("finishing a completed upgrade: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the completed upgrade left its journal at %s, so the next upgrade skips the stages it names and reports success having changed nothing (stat error: %v)",
			path, err)
	}

	// AND A RUN THAT DID NOT COMPLETE KEEPS IT. That journal is where the
	// attempt stopped; the identical command is meant to continue it.
	failed, err := stages.OpenJournal(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Append(stages.Entry{Stage: controlplane.StageMigration, Status: stages.StatusFailed, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := failed.Save(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := finishControlPlaneJournal(&out, failed, errors.New("the migration Job failed")); err != nil {
		t.Fatalf("finishing a failed upgrade: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a failed run's journal was removed, so the next run cannot continue from where it stopped: %v", err)
	}
	// It is still the journal it was: the stage the failure names survives.
	reopened, err := stages.ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Completed(controlplane.StageMigration); ok {
		t.Error("the failed stage reads as completed in the journal that was kept")
	}
}
