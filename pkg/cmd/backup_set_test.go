package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/recovery"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/sshx"
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

// recoverableBackupRunner answers the two reads `backup now` makes when it
// decides what a recovery can restore from the backup it just took: the
// backup's expected-coverage record, and the Projects in kubenest-system.
func recoverableBackupRunner(t *testing.T, record, projects string) *componenttest.FakeRunner {
	t.Helper()
	doc, err := json.Marshal(map[string]any{"data": map[string]string{"coverage.json": record}})
	if err != nil {
		t.Fatal(err)
	}
	return &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get configmap "+backup.CoverageRecordName("manual-1")):
			return sshx.Result{Stdout: string(doc)}, nil
		case strings.Contains(command, "get projects -n "+backup.ProjectCRNamespace):
			return sshx.Result{Stdout: projects}, nil
		}
		return sshx.Result{ExitCode: 1, Stderr: "unexpected: " + command}, nil
	}}
}

// The set `backup now` writes holds only the namespaces a recovery can
// restore, not every namespace the backup's expected-coverage record lists. A
// recovery restores a namespace by pausing its Project first, so the platform
// namespaces the install creates for itself (cert-manager, openebs) are not
// ones it restores, and recording them fails the recovery on the first of them
// — lab s6, 2026-09-28, stage 16 recovery-restore could not annotate Project
// kubenest-system/cert-manager.
//
// PLANTED NEGATIVES: cert-manager and openebs are covered but have no
// Project, and ghost has a Project the backup does not cover.
func TestABackupNowRecordsOnlyTheNamespacesARecoveryCanRestore(t *testing.T) {
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	kit, doc := sealedKit(t, fleet, kitCluster)
	baseline, err := recoverykit.NewSet(kit, map[string]string{"bundle": "1.2"}, recoverykit.Digest(doc), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// The coverage is derived exactly as `backup now` derives it, and then
	// recorded exactly as `backup now` records it.
	coverage, err := backup.RecoverableNamespaces(context.Background(), recoverableBackupRunner(t,
		`{"backup":"manual-1","recorded_by":"cli","namespaces":[`+
			`{"name":"cert-manager","uid":"u1","volumes":[]},`+
			`{"name":"openebs","uid":"u2","volumes":[]},`+
			`{"name":"s6-data","uid":"u3","volumes":[]},`+
			`{"name":"shop","uid":"u4","volumes":[]}]}`,
		`{"items":[{"metadata":{"name":"ghost"}},{"metadata":{"name":"s6-data"}},{"metadata":{"name":"shop"}}]}`,
	), "manual-1")
	if err != nil {
		t.Fatal(err)
	}
	set, err := withRecordedBackup(baseline, "manual-1", coverage.Namespaces, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	chosen, skip, err := recovery.ChooseBackup(set, "")
	if err != nil {
		t.Fatal(err)
	}
	if skip != "" {
		t.Fatalf("a recovery from this set would skip its restore: %q", skip)
	}
	if strings.Join(chosen.Coverage, ",") != "s6-data,shop" {
		t.Errorf("the set records %v for %s, want only the namespaces a recovery can restore", chosen.Coverage, chosen.Name)
	}
	for _, refused := range []string{"cert-manager", "openebs", "ghost"} {
		if hasNamespace(chosen.Coverage, refused) {
			t.Errorf("%s must not be recorded: a recovery that restored it would fail on it", refused)
		}
	}
}

// A recorded backup with no restorable coverage produces one of two warnings,
// and they are not interchangeable: the backup has no expected-coverage record
// at all, or it has one that names no namespace a recovery can restore — the
// platform namespaces an install builds for itself are the ones with no
// Project, and naming one is what failed lab s6's recovery on its first
// namespace. Reporting the second case as the first sends the operator looking
// for a record that is there.
func TestTheEmptyCoverageWarningNamesTheRightCause(t *testing.T) {
	var noRecord, noProjects bytes.Buffer
	warnEmptyCoverage(&noRecord, "manual-1", backup.RecoverableCoverage{})
	warnEmptyCoverage(&noProjects, "manual-1", backup.RecoverableCoverage{Recorded: true})

	if noRecord.Len() == 0 || noProjects.Len() == 0 {
		t.Fatalf("both empty-coverage causes need a warning, got %q and %q", noRecord.String(), noProjects.String())
	}
	if noRecord.String() == noProjects.String() {
		t.Error("the two causes are different facts and must not read the same")
	}
	if !strings.Contains(noRecord.String(), "no expected-coverage record") {
		t.Errorf("a backup with no record must be reported as one: %q", noRecord.String())
	}
	if strings.Contains(noProjects.String(), "no expected-coverage record") {
		t.Errorf("a record that names no restorable namespace is not a missing record: %q", noProjects.String())
	}
	if !strings.Contains(noProjects.String(), "Project") {
		t.Errorf("the record-with-no-restorable-namespace case must name the cause: %q", noProjects.String())
	}
	for _, text := range []string{noRecord.String(), noProjects.String()} {
		if !strings.Contains(text, "manual-1") {
			t.Errorf("a warning about a backup must name it: %q", text)
		}
	}
}

// hasNamespace reports whether the list names one namespace.
func hasNamespace(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
