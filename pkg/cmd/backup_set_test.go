package cmd

import (
	"testing"
	"time"

	"kubenest.io/cli/pkg/recovery"
	"kubenest.io/cli/pkg/recoverykit"
)

// A backup `backup now` records in the recovery set must be one a recovery
// restores from. Lab s6, 2026-09-28: the set named the backup with no
// coverage, so the recovery chose it, skipped every workload namespace, and
// reported success with nothing restored.
func TestABackupRecordedWithItsCoverageIsOneARecoveryRestores(t *testing.T) {
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	kit, doc := sealedKit(t, fleet, kitCluster)
	baseline, err := recoverykit.NewSet(kit, map[string]string{"bundle": "1.2"}, recoverykit.Digest(doc), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	set, err := withRecordedBackup(baseline, "manual-1", []string{"app", "shop"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !set.Complete {
		t.Error("the set is not marked complete after a backup was recorded in it")
	}
	chosen, skip, err := recovery.ChooseBackup(set, "")
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Name != "manual-1" || skip != "" {
		t.Fatalf("a recovery from this set chose %q and would skip its restore (%q): the backup was recorded without the coverage that makes it restorable", chosen.Name, skip)
	}
	if len(chosen.Coverage) != 2 {
		t.Errorf("the recorded backup covers %v, want app and shop", chosen.Coverage)
	}
}
