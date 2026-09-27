package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shipped parses one of the bundle manifests this binary carries.
//
// The window under test is the RELEASE's declaration, not a fixture's: a value
// the catalog does not actually ship must not be able to pass here, and the
// inline alternative would be a second copy of a manifest that already exists
// three times.
func shipped(t *testing.T, version string) *Manifest {
	t.Helper()
	path := filepath.Join("..", "bundles", "manifests", "platform-"+version+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the shipped bundle %s: %v", version, err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("the shipped bundle %s does not parse: %v", version, err)
	}
	return m
}

func TestTheDeclaredCompatibilityWindowParses(t *testing.T) {
	m := shipped(t, "1.2")
	if !m.Compatibility.Declared() {
		t.Fatal("bundle 1.2 carries a compatibility block and must read as declaring a window")
	}
	if m.Compatibility.ControlPlane.MinContract != 3 {
		t.Errorf("min-contract read as %d, want 3 — era 3 is where the control plane stops holding a "+
			"tenant credential the operator owns", m.Compatibility.ControlPlane.MinContract)
	}
	// An absent maximum is "no upper bound", not zero: a zero would read as a
	// window no control plane can be inside.
	if m.Compatibility.ControlPlane.MaxContract != nil {
		t.Errorf("an absent max-contract must read as nil, got %v", *m.Compatibility.ControlPlane.MaxContract)
	}
	if !m.Compatibility.ControlPlane.Supports(3) {
		t.Error("era 3 is at the floor and must be supported")
	}
	if !m.Compatibility.ControlPlane.Supports(4) {
		t.Error("an absent maximum is no bound, so a later era must be supported")
	}
	if m.Compatibility.ControlPlane.Supports(2) {
		t.Error("era 2 is below the floor and must not be supported")
	}
	if got := m.Compatibility.Agents["3"].Accepts; len(got) != 2 || got[0] != "1.1" || got[1] != "1.2" {
		t.Errorf("era 3's accepted bundles read as %v", got)
	}
}

func TestAReleasedManifestWithoutTheBlockDeclaresNoWindow(t *testing.T) {
	// 0.9, 1.0 and 1.1 are released documents with no block. A reader that
	// refused on that silence would refuse the released catalog on a question it
	// was never asked (kn-opj8).
	m := shipped(t, "1.1")
	if m.Compatibility.Declared() {
		t.Error("a released manifest with no compatibility block declares no window")
	}
	if err := m.Compatibility.CheckAgent(3, "1.0"); err != nil {
		t.Errorf("an undeclared window must not refuse anything: %v", err)
	}
}

func TestAnInWindowAgentIsAccepted(t *testing.T) {
	m := shipped(t, "1.2")
	for _, agent := range []string{"1.1", "1.2"} {
		if err := m.Compatibility.CheckAgent(3, agent); err != nil {
			t.Errorf("bundle %s is inside era 3's window and must be accepted: %v", agent, err)
		}
	}
	// The same bundle at the era that owns it, which is the other row.
	if err := m.Compatibility.CheckAgent(2, "1.0"); err != nil {
		t.Errorf("era 2 accepts 1.0 and must not refuse it: %v", err)
	}
}

func TestAnOutOfWindowAgentIsRefusedWithTheBundleToUpgradeTo(t *testing.T) {
	m := shipped(t, "1.2")
	err := m.Compatibility.CheckAgent(3, "1.0")
	if err == nil {
		t.Fatal("bundle 1.0 is outside era 3's window and must be refused")
	}
	refusal, ok := err.(*AgentWindowRefusal)
	if !ok {
		t.Fatalf("the refusal must be actionable rather than a bare string: %T", err)
	}
	// The STEP, not the jump: bundles are stepped through one at a time
	// (decision K), so naming 1.2 would hand back a hop the path gate refuses.
	if refusal.UpgradeTo != "1.1" {
		t.Errorf("refusal names %q to upgrade to, want 1.1", refusal.UpgradeTo)
	}
	for _, want := range []string{"1.0", "1.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name bundle %s: %v", want, err)
		}
	}
}

func TestAnAgentNewerThanTheWindowNamesTheControlPlaneUpgrade(t *testing.T) {
	m := shipped(t, "1.2")
	err := m.Compatibility.CheckAgent(3, "1.3")
	if err == nil {
		t.Fatal("an agent from a bundle this era does not declare must be refused")
	}
	refusal, ok := err.(*AgentWindowRefusal)
	if !ok {
		t.Fatalf("the refusal must be actionable rather than a bare string: %T", err)
	}
	if refusal.UpgradeTo != "" {
		t.Errorf("there is no bundle to upgrade TO when the cluster is the newer side, got %q", refusal.UpgradeTo)
	}
	if !strings.Contains(err.Error(), "control plane") {
		t.Errorf("the refusal must name the control-plane upgrade as the fix: %v", err)
	}
}

func TestAnUndeclaredEraIsNotARefusal(t *testing.T) {
	m := shipped(t, "1.2")
	accepted, declared := m.Compatibility.Accepts(9, "1.0")
	if accepted || declared {
		t.Fatalf("era 9 has no row: accepted=%v declared=%v", accepted, declared)
	}
	if err := m.Compatibility.CheckAgent(9, "1.0"); err != nil {
		t.Errorf("a row that does not exist is not a refusal: %v", err)
	}
}

func TestBundleVersionsCompareNumerically(t *testing.T) {
	// "1.10" is newer than "1.9" and a string comparison says otherwise. This is
	// the trap the window's ordering exists to avoid.
	cmp, err := CompareBundleVersions("1.9", "1.10")
	if err != nil {
		t.Fatal(err)
	}
	if cmp != -1 {
		t.Errorf("1.9 vs 1.10 compared %d, want -1", cmp)
	}
	if got := NextAcceptedBundle([]string{"1.9", "1.10", "1.11"}, "1.8"); got != "1.9" {
		t.Errorf("the next accepted bundle from 1.8 is 1.9, got %q", got)
	}
	if _, err := CompareBundleVersions("1.35.8", "1.2"); err == nil {
		t.Error("a three-part version is not a bundle version and must be an error, not a silent comparison")
	}
}
