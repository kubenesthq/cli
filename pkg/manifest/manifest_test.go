package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadParsesTimeouts(t *testing.T) {
	m, err := Load("testdata/platform-test.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Bundle != "1.0" {
		t.Errorf("bundle = %q, want 1.0", m.Bundle)
	}

	want := map[string]time.Duration{
		"node-ready":      5 * time.Minute,
		"component-ready": 10 * time.Minute,
		"install-total":   30 * time.Minute,
		"restore-drill":   2 * time.Hour,
	}
	for key, dur := range want {
		got, err := m.Limits.Timeouts.For(key)
		if err != nil {
			t.Errorf("For(%s): %v", key, err)
			continue
		}
		if got != dur {
			t.Errorf("timeouts.%s = %s, want %s", key, got, dur)
		}
	}
}

// A missing timeout is an error, never a default: the manifest is the record
// of what the platform does, and a constant in code would silently unrecord it.
func TestMissingTimeoutKeyIsAnErrorNotADefault(t *testing.T) {
	m, err := Load("testdata/platform-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Limits.Timeouts.For("no-such-wait")
	if err == nil {
		t.Fatal("a missing limits.timeouts key must be an error")
	}
	if !strings.Contains(err.Error(), "no-such-wait") || !strings.Contains(err.Error(), "manifest") {
		t.Errorf("error %q should name the key and point at the manifest", err)
	}
}

func TestParseRejectsBrokenManifests(t *testing.T) {
	cases := map[string]string{
		"no bundle version": "limits:\n  timeouts:\n    node-ready: 5m\n",
		"no timeouts":       "bundle: \"1.0\"\n",
		"bad duration":      "bundle: \"1.0\"\nlimits:\n  timeouts:\n    node-ready: five-minutes\n",
		"negative duration": "bundle: \"1.0\"\nlimits:\n  timeouts:\n    node-ready: -5m\n",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// writeManifest writes a minimal manifest with one timeout value.
func writeManifest(t *testing.T, componentReady string) string {
	t.Helper()
	doc := "bundle: \"1.0\"\nlimits:\n  timeouts:\n    component-ready: " + componentReady + "\n"
	path := filepath.Join(t.TempDir(), "bundle.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Changing the manifest changes the deadline the code sees — nothing else in
// the code decides it. (The behavioral half — the changed deadline flipping a
// check's verdict — is asserted in pkg/converge's integration test.)
func TestChangingTheManifestChangesTheDeadline(t *testing.T) {
	short, err := Load(writeManifest(t, "50ms"))
	if err != nil {
		t.Fatal(err)
	}
	long, err := Load(writeManifest(t, "10s"))
	if err != nil {
		t.Fatal(err)
	}

	d1, _ := short.Limits.Timeouts.For("component-ready")
	d2, _ := long.Limits.Timeouts.For("component-ready")
	if d1 != 50*time.Millisecond || d2 != 10*time.Second {
		t.Errorf("deadlines = %s and %s, want 50ms and 10s straight from the files", d1, d2)
	}
}

func TestCoreVersionReadsThePin(t *testing.T) {
	m, err := Load("testdata/platform-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.Core.Version("openebs-lvm-localpv")
	if err != nil {
		t.Fatal(err)
	}
	if v != "1.10.0" {
		t.Errorf("core.openebs-lvm-localpv = %q, want 1.10.0", v)
	}
}

// A missing component pin is an error, never a default — the same rule as
// timeouts, for the same reason.
func TestMissingCorePinIsAnErrorNotADefault(t *testing.T) {
	m, err := Load("testdata/platform-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Core.Version("velero")
	if err == nil {
		t.Fatal("Version(velero) = nil error for an unpinned component")
	}
	if !strings.Contains(err.Error(), "core.velero") {
		t.Errorf("error %q does not name the missing manifest key core.velero", err)
	}
}

func TestBackupSectionParses(t *testing.T) {
	m, err := Load("testdata/platform-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := m.Backup.Plugin()
	if err != nil {
		t.Fatal(err)
	}
	if p.Provider != "aws" || p.Version != "v1.14.2" {
		t.Errorf("plugin = %+v, want aws v1.14.2", p)
	}
	w, err := m.Backup.Defaults.Workload()
	if err != nil {
		t.Fatal(err)
	}
	if w.Interval.Duration() != 24*time.Hour || w.Keep != 14 {
		t.Errorf("workload-backup = %s × %d, want 24h × 14", w.Interval.Duration(), w.Keep)
	}
	d, err := m.Backup.Defaults.Datastore()
	if err != nil {
		t.Fatal(err)
	}
	if d.Interval.Duration() != time.Hour || d.Keep != 24 {
		t.Errorf("datastore-snapshot = %s × %d, want 1h × 24", d.Interval.Duration(), d.Keep)
	}
	// Days are an interval unit the schema allows and ParseDuration does not.
	r, err := m.Backup.Defaults.Drill()
	if err != nil {
		t.Fatal(err)
	}
	if r.Interval.Duration() != 7*24*time.Hour {
		t.Errorf("restore-drill interval = %s, want 168h", r.Interval.Duration())
	}
}

// A manifest without the backup section still parses (older fixtures, other
// installers), but ACCESSING the plugin or the defaults is an error — the
// same missing-is-an-error rule as pins and timeouts.
func TestMissingBackupSectionIsAnErrorOnAccessNotParse(t *testing.T) {
	m, err := Parse([]byte("bundle: \"1.0\"\nlimits:\n  timeouts:\n    component-ready: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Backup.Plugin(); err == nil || !strings.Contains(err.Error(), "object-store-plugin") {
		t.Errorf("Plugin() on a manifest without backup: err = %v, want one naming backup.object-store-plugin", err)
	}
	if _, err := m.Backup.Defaults.Workload(); err == nil || !strings.Contains(err.Error(), "workload-backup") {
		t.Errorf("Workload() on a manifest without backup: err = %v, want one naming backup.defaults.workload-backup", err)
	}
	if _, err := m.Backup.Defaults.Datastore(); err == nil || !strings.Contains(err.Error(), "datastore-snapshot") {
		t.Errorf("Datastore() on a manifest without backup: err = %v, want one naming backup.defaults.datastore-snapshot", err)
	}
	if _, err := m.Backup.Defaults.Drill(); err == nil || !strings.Contains(err.Error(), "restore-drill") {
		t.Errorf("Drill() on a manifest without backup: err = %v, want one naming backup.defaults.restore-drill", err)
	}
}

func TestIntervalRejectsWhatTheSchemaRejects(t *testing.T) {
	for _, bad := range []string{"day", "1w", "-1h", "0h", "1.5h", "h", ""} {
		doc := "bundle: \"1.0\"\nlimits:\n  timeouts:\n    component-ready: 10m\nbackup:\n  defaults:\n    workload-backup: { interval: \"" + bad + "\", keep: 1 }\n"
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("interval %q parsed without error", bad)
		}
	}
}

// The REAL authored manifest in kubenest-contracts must parse with core pins
// intact — the manifest is the contract artifact, and this is the file the
// installer will actually read. Skipped when the contracts checkout is not
// beside this repo.
func TestRealPlatformManifestCarriesCorePins(t *testing.T) {
	real := filepath.Join("..", "..", "..", "kubenest-contracts", "bundles", "platform-1.0.yaml")
	if _, err := os.Stat(real); err != nil {
		t.Skipf("contracts checkout not present: %v", err)
	}
	m, err := Load(real)
	if err != nil {
		t.Fatal(err)
	}
	v, err := m.Core.Version("openebs-lvm-localpv")
	if err != nil {
		t.Fatal(err)
	}
	if v == "" {
		t.Error("core.openebs-lvm-localpv is empty in the real manifest")
	}
	// The backup seam (contracts v1.15.0): plugin pin, workload defaults and
	// the backup deadline must all be present in the shipped manifest.
	if _, err := m.Backup.Plugin(); err != nil {
		t.Errorf("real manifest: %v", err)
	}
	if _, err := m.Backup.Defaults.Workload(); err != nil {
		t.Errorf("real manifest: %v", err)
	}
	if _, err := m.Limits.Timeouts.For("backup"); err != nil {
		t.Errorf("real manifest: %v", err)
	}
}
