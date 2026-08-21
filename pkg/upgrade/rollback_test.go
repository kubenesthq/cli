package upgrade

import (
	"strings"
	"testing"

	"kubenest.io/cli/pkg/stages"
)

func journalWith(t *testing.T, stageNames ...string) *stages.Journal {
	t.Helper()
	j, err := stages.OpenJournal(t.TempDir()+"/upgrade.json",
		stages.Identity{Kind: Kind, Cluster: "prod-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range stageNames {
		if err := j.Append(stages.Entry{Stage: name, Status: stages.StatusStarted}); err != nil {
			t.Fatal(err)
		}
	}
	return j
}

// THE decision the ordering buys: a failure before the kubernetes stage costs
// seconds, and one after it costs a datastore restore. Everything else about
// rollback follows from which side of that line the failure fell on.
func TestTheMechanismFollowsFromWhereItFailed(t *testing.T) {
	from := parseManifest(t, "bundle: \"1.0\"\ncore: {traefik: 41.2.0, cert-manager: v1.21.1}\nlimits: {timeouts: {node-ready: 5m}}\n")
	to := parseManifest(t, "bundle: \"1.1\"\ncore: {traefik: 42.0.0, cert-manager: v1.22.0}\nlimits: {timeouts: {node-ready: 5m}}\n")
	rec := record{FromBundle: "1.0", ToBundle: "1.1", Snapshot: "pre-upgrade-1-1"}

	t.Run("stopped before anything moved", func(t *testing.T) {
		plan := PlanRollback(journalWith(t, StagePreflight, StageBackup), from, to, rec)
		if plan.Mechanism != MechanismNothing {
			t.Errorf("mechanism = %s, want nothing to undo", plan.Mechanism)
		}
	})

	t.Run("components moved, kubernetes never started", func(t *testing.T) {
		plan := PlanRollback(journalWith(t, StagePreflight, StageBackup, StageComponents), from, to, rec)
		if plan.Mechanism != MechanismComponents {
			t.Fatalf("mechanism = %s, want a component revert", plan.Mechanism)
		}
		report := plan.String()
		for _, want := range []string{"traefik 42.0.0 → 41.2.0", "no data implications", "seconds"} {
			if !strings.Contains(report, want) {
				t.Errorf("the plan does not mention %q:\n%s", want, report)
			}
		}
	})

	t.Run("kubernetes started", func(t *testing.T) {
		plan := PlanRollback(journalWith(t, StagePreflight, StageBackup, StageComponents, StageKubernetes), from, to, rec)
		if plan.Mechanism != MechanismRestore {
			t.Fatalf("mechanism = %s, want a datastore restore", plan.Mechanism)
		}
		report := plan.String()
		// The report must say the three things that surprise people.
		for _, want := range []string{
			"does not downgrade",
			"service interruption",
			"PERSISTENT VOLUMES ARE NOT TOUCHED",
			"PersistentVolumeClaim created during the upgrade window",
			"pre-upgrade-1-1",
		} {
			if !strings.Contains(report, want) {
				t.Errorf("the plan does not mention %q:\n%s", want, report)
			}
		}
	})
}

// A restore with no snapshot is refused rather than attempted: there is
// nothing to go back to, and saying so is more useful than failing partway.
func TestRestoringWithoutASnapshotIsRefused(t *testing.T) {
	err := RestoreSnapshot(nil, nil, "")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "nothing to restore to") {
		t.Errorf("the refusal must say why: %v", err)
	}
}
