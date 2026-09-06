package bundles

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The catalog is only as good as the fact that it parses. A manifest that
// ships broken would fail at install time on a customer's host, having
// already been shipped.
func TestEveryEmbeddedManifestParses(t *testing.T) {
	versions := Versions()
	if len(versions) == 0 {
		t.Fatal("this binary carries no bundle manifests: standalone install has no version pins at all")
	}
	for _, v := range versions {
		m, err := Manifest(v)
		if err != nil {
			t.Fatalf("bundle %s: %v", v, err)
		}
		// The filename and the document must agree, or `--bundle 1.0` would
		// install whatever 0.9 pins while every message said 1.0.
		if m.Bundle != v {
			t.Errorf("manifests/platform-%s.yaml declares bundle %q", v, m.Bundle)
		}
		if len(m.HATiers) == 0 {
			t.Errorf("bundle %s offers no HA tiers, so preflight would refuse every install", v)
		}
	}
}

// Every deadline and every pin the install path reads must be present, in
// every embedded bundle. A missing key is an error at the stage that needed
// it — which on a standalone install is a customer's host, mid-run.
func TestEmbeddedManifestsCarryWhatInstallReads(t *testing.T) {
	// The component keys the install plan can act on, and the timeout keys
	// its stages ask for. Both are named here rather than imported to keep
	// pkg/bundles free of a dependency on pkg/install.
	components := []string{
		"k3s", "traefik", "gateway-api", "cert-manager", "openebs-lvm-localpv",
		"velero", "system-upgrade-controller", "kured", "kubenest-agent",
	}
	timeouts := []string{"install-total", "component-ready"}

	for _, v := range Versions() {
		m, err := Manifest(v)
		if err != nil {
			t.Fatalf("bundle %s: %v", v, err)
		}
		for _, c := range components {
			if _, err := m.Core.Version(c); err != nil {
				t.Errorf("bundle %s: %v", v, err)
			}
		}
		for _, key := range timeouts {
			if _, err := m.Limits.Timeouts.For(key); err != nil {
				t.Errorf("bundle %s: %v", v, err)
			}
		}
	}
}

func TestUnknownVersionNamesWhatIsCarried(t *testing.T) {
	_, err := Manifest("99.9")
	if err == nil {
		t.Fatal("an unknown bundle version must be refused, not installed")
	}
	for _, v := range Versions() {
		if !contains(err.Error(), v) {
			t.Errorf("the refusal does not name carried version %s: %v", v, err)
		}
	}
}

func TestCatalogMatchesTheManifests(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) != len(Versions()) {
		t.Fatalf("catalog has %d entries for %d embedded manifests", len(catalog), len(Versions()))
	}
	for _, e := range catalog {
		m, err := Manifest(e.Version)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.HATiers) != len(m.HATiers) {
			t.Errorf("bundle %s: catalog offers %v, the manifest offers %v", e.Version, e.HATiers, m.HATiers)
		}
	}
}

// DRIFT. kubenest-contracts owns these documents; the copies here are a
// convenience so the binary can install without a control plane. If the two
// ever disagree, a standalone install and a registered install of the SAME
// bundle version would install different things — which is the one thing the
// bundle is for.
//
// Skips when the umbrella workspace is not checked out beside this repo
// (which is the case in this repo's own CI), because a check that cannot run
// must say so rather than pass.
func TestEmbeddedManifestsMatchContracts(t *testing.T) {
	root := filepath.Join("..", "..", "..", "kubenest-contracts", "bundles")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("kubenest-contracts is not checked out beside this repo, so drift cannot be checked here: %v", err)
	}
	for _, v := range Versions() {
		name := prefix + v + suffix
		want, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("bundle %s is embedded here but absent from kubenest-contracts: %v", v, err)
			continue
		}
		got, err := Raw(v)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("manifests/%s differs from kubenest-contracts/bundles/%s. "+
				"contracts is the original: copy it here rather than editing this copy", name, name)
		}
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// SECURITY FLOOR (kn-b49n). Traefik chart 41.4.0 ships Traefik v3.7.12, the
// lowest version with no open Traefik advisory. The pin this replaced, 41.2.0,
// shipped v3.7.10, against which seven were open — among them
// GHSA-5w68-77r2-r64c, a CRITICAL complete authentication bypass in the
// digestAuth middleware. That one is reachable on a platform cluster rather
// than theoretical: the chart installs middlewares.traefik.io and pkg/component
// /traefik runs Traefik with --providers.kubernetescrd, so any project
// namespace can create the Middleware that triggers it.
//
// The floor applies to the CURRENT bundle only. Older bundles keep the pins
// that were current when they were cut — that is what platform-0.9.yaml's
// header says and why it says do not install 0.9 — so this walks the newest
// version rather than every version.
//
// Deliberately a static floor and not a live advisory query: a unit test that
// reaches the network fails for the wrong reasons. It catches the regression
// that matters, someone pinning traefik back to fix an unrelated problem.
// Noticing NEW advisories above the floor is kn-eewa's advisory feed;
// asserting the version actually running on a cluster is kn-twe's matrix.
func TestCurrentBundleTraefikPinIsAtOrAboveTheSecurityFloor(t *testing.T) {
	versions := Versions()
	if len(versions) == 0 {
		t.Fatal("this binary carries no bundle manifests")
	}
	current := versions[len(versions)-1]
	m, err := Manifest(current)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := m.Core.Version("traefik")
	if err != nil {
		t.Fatal(err)
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(pin, "%d.%d.%d", &major, &minor, &patch); err != nil {
		t.Fatalf("bundle %s pins traefik to %q, which is not a chart version: %v", current, pin, err)
	}
	const floorMajor, floorMinor, floorPatch = 41, 4, 0
	below := major < floorMajor ||
		(major == floorMajor && minor < floorMinor) ||
		(major == floorMajor && minor == floorMinor && patch < floorPatch)
	if below {
		t.Errorf("bundle %s pins traefik chart %s, below the %d.%d.%d security floor "+
			"(Traefik v3.7.12) — see kn-b49n", current, pin, floorMajor, floorMinor, floorPatch)
	}
}
