package manifest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/manifest"
)

// gib is a variable, not a constant: Go refuses to convert a non-integer
// float CONSTANT to int64, and 3.7 GiB is the number that matters here.
var gib = float64(1 << 30)

func TestParseQuantityBinaryUnits(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"3.7Gi", int64(3.7 * gib)},
		{"36Gi", 36 << 30},
		{"512Mi", 512 << 20},
		{"1Ti", 1 << 40},
	}
	for _, c := range cases {
		got, err := manifest.ParseQuantity(c.in)
		if err != nil {
			t.Fatalf("ParseQuantity(%q): %v", c.in, err)
		}
		if got.Bytes() != c.want {
			t.Errorf("ParseQuantity(%q) = %d bytes, want %d", c.in, got.Bytes(), c.want)
		}
	}
}

// The whole point of the type: a decimal unit is refused rather than silently
// converted. Accepting "4GB" and comparing it to MemTotal is the bug that
// refuses a correctly-sized customer host (kn-bkwa finding 1).
func TestParseQuantityRefusesDecimalUnits(t *testing.T) {
	for _, in := range []string{"4GB", "4G", "4000000000", "4", "-1Gi", "0Gi"} {
		if q, err := manifest.ParseQuantity(in); err == nil {
			t.Errorf("ParseQuantity(%q) = %v, want an error", in, q)
		}
	}
}

// A 4 GB machine reports 3.7 GiB. The floor must pass it.
func TestFloorPassesTheMachineTheVendorSold(t *testing.T) {
	m := loadFixture(t, `
bundle: "1.0"
limits:
  resources:
    floor: { cpu: 2, memory: 3.7Gi, disk: 36Gi }
    recommended: { cpu: 4, memory: 7.4Gi, disk: 92Gi }
  timeouts:
    node-ready: 5m
`)
	floor, err := m.Limits.Floor()
	if err != nil {
		t.Fatal(err)
	}
	// Measured on a real Hetzner host sold as 8 GB / 80 GB (kn-bkwa).
	measuredMem := int64(7.57 * gib)
	measuredDisk := int64(74.77 * gib)
	if measuredMem < floor.Memory.Bytes() {
		t.Errorf("floor %s refuses a host reporting 7.57GiB", floor.Memory)
	}
	if measuredDisk < floor.Disk.Bytes() {
		t.Errorf("floor %s refuses a host reporting 74.77GiB", floor.Disk)
	}
	// And a genuinely undersized host is refused: 4 GB nominal is 3.7 GiB,
	// so a 2 GB host reporting 1.9 GiB must be below the floor.
	if int64(1.9*gib) >= floor.Memory.Bytes() {
		t.Errorf("floor %s accepts a 2 GB host", floor.Memory)
	}
}

func TestMissingResourcesIsAnErrorNotADefault(t *testing.T) {
	m := loadFixture(t, "bundle: \"1.0\"\nlimits:\n  timeouts:\n    node-ready: 5m\n")
	if _, err := m.Limits.Floor(); err == nil {
		t.Error("a manifest with no limits.resources.floor must be an error, not a built-in default")
	}
	if _, err := m.Limits.Recommended(); err == nil {
		t.Error("a manifest with no limits.resources.recommended must be an error, not a built-in default")
	}
}

func TestOSMatrixAndTiers(t *testing.T) {
	m := loadFixture(t, `
bundle: "1.0"
os:
  supported: [ubuntu-24.04]
ha-tiers: [single-server, ha]
limits:
  timeouts: { node-ready: 5m }
`)
	if !m.OS.Supports("ubuntu", "24.04") {
		t.Error("ubuntu 24.04 must be supported")
	}
	for _, c := range [][2]string{{"ubuntu", "22.04"}, {"debian", "12"}, {"ubuntu", "26.04"}} {
		if m.OS.Supports(c[0], c[1]) {
			t.Errorf("%s %s is not in the tested matrix and must not be supported", c[0], c[1])
		}
	}
	if err := m.OffersTier("single-server"); err != nil {
		t.Error(err)
	}
	if err := m.OffersTier("stretched"); err == nil {
		t.Error("a tier the bundle does not offer must be refused, not silently downgraded")
	}
}

// An unknown profile name is REJECTED, not ignored — the wave-1 gate's rule.
func TestUnknownProfileIsRejected(t *testing.T) {
	m := loadFixture(t, `
bundle: "1.0"
limits:
  timeouts: { node-ready: 5m }
profiles:
  observability:
    provisional: true
    components: { grafana: 12.11.1 }
  ha:
    components: {}
`)
	if _, err := m.Profiles.Get("observability"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Profiles.Get("obervability") // a typo, the realistic case
	if err == nil {
		t.Fatal("an unknown profile must be rejected")
	}
	if got := err.Error(); !strings.Contains(got, "observability") {
		t.Errorf("the refusal must name what the bundle does offer, got %q", got)
	}
}

// The REAL shipped manifest must carry every threshold preflight enforces.
func TestShippedManifestCarriesPreflightThresholds(t *testing.T) {
	real := filepath.Join("..", "..", "..", "kubenest-contracts", "bundles", "platform-1.0.yaml")
	if _, err := os.Stat(real); err != nil {
		t.Skipf("contracts checkout not present: %v", err)
	}
	m, err := manifest.Load(real)
	if err != nil {
		t.Fatal(err)
	}
	floor, err := m.Limits.Floor()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Limits.Recommended(); err != nil {
		t.Fatal(err)
	}
	if floor.CPU != 2 || floor.Memory.String() != "3.7Gi" || floor.Disk.String() != "36.0Gi" {
		t.Errorf("shipped floor is cpu=%d memory=%s disk=%s, want 2 / 3.7Gi / 36Gi",
			floor.CPU, floor.Memory, floor.Disk)
	}
	if !m.OS.Supports("ubuntu", "24.04") {
		t.Error("the shipped manifest must support ubuntu 24.04 — it is the only tested OS")
	}
	for _, tier := range []string{"single-server", "ha"} {
		if err := m.OffersTier(tier); err != nil {
			t.Error(err)
		}
	}
	for _, p := range []string{"observability", "secrets", "replicated-storage", "ha"} {
		if _, err := m.Profiles.Get(p); err != nil {
			t.Error(err)
		}
	}
}

func loadFixture(t *testing.T, doc string) *manifest.Manifest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
