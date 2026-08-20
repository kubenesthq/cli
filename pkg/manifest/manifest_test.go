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
}
