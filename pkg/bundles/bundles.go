// Package bundles is the bundle catalog this binary carries.
//
// WHY THE MANIFESTS ARE EMBEDDED. `kubenest platform install` reads every
// version pin and every deadline out of the bundle manifest (pkg/manifest,
// and the CLI's third invariant: a missing timeout is an error, never a
// default). Until kn-l827 the only source of that document was the control
// plane — so an installer with nothing to log in to had no versions, no
// deadlines, and could not run at all.
//
// Nothing is hosted by us (decision 2026-08-26), so the pins have to travel
// with the binary that installs them. That is also the stronger form of the
// product's own claim: "we tell you exactly what we install, and pin every
// version of it" reads better when the pinned binary carries its own pins
// than when it has to phone somewhere to find out.
//
// THE COPIES ARE NOT THE ORIGINAL. kubenest-contracts/bundles/ owns the
// manifest schema and its releases (kn-boj); kubenest-backend serves a
// byte-identical copy. The files under manifests/ here are a third copy, and
// bundles_test.go checks them against the contracts originals whenever the
// umbrella workspace is checked out beside this repo. A bundle version is an
// immutable release — when 1.1 ships, contracts changes first and these
// copies follow, in that order.
package bundles

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"kubenest.io/cli/pkg/manifest"
)

//go:embed manifests/*.yaml
var files embed.FS

const (
	dir    = "manifests"
	prefix = "platform-"
	suffix = ".yaml"
)

// Manifest returns the parsed manifest for a bundle version.
//
// An unknown version names the versions this binary DOES carry, because the
// most likely cause is a version that exists in a newer release of the CLI
// than the one the operator is holding.
func Manifest(version string) (*manifest.Manifest, error) {
	raw, err := Raw(version)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the bundle manifest for %s built into this CLI is not valid: %w", version, err)
	}
	return m, nil
}

// Raw returns the manifest bytes for a bundle version.
func Raw(version string) ([]byte, error) {
	if version == "" {
		return nil, fmt.Errorf("no bundle version given: this CLI carries %s", strings.Join(Versions(), ", "))
	}
	raw, err := files.ReadFile(path.Join(dir, prefix+version+suffix))
	if err != nil {
		return nil, fmt.Errorf("this CLI does not carry bundle %s: it carries %s. "+
			"Install one of those, or upgrade the CLI to a release that ships the bundle you want",
			version, strings.Join(Versions(), ", "))
	}
	return raw, nil
}

// Versions is every bundle version built into this binary, in the catalog's
// PUBLISHED ORDER, oldest first.
//
// NUMERIC, NOT LEXICOGRAPHIC: "1.10" is newer than "1.9" and sorting the names
// as strings puts it first. The order is load-bearing — `Catalog` offers bundles
// oldest-first and the bundle-path gate measures adjacency in this sequence
// (kn-mtpf) — so there is exactly one ordering rule and it is this one.
func Versions() []string {
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		// Unreachable: the directory is embedded at build time.
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix))
	}
	sort.Slice(out, func(i, j int) bool {
		cmp, err := manifest.CompareBundleVersions(out[i], out[j])
		return err == nil && cmp < 0
	})
	return out
}

// Release is one entry of the catalog's published sequence: a bundle version and
// whether that release exists only to carry a patched component.
type Release struct {
	Version string
	// SecurityOnly marks a release that does not consume the two-release
	// support window: the bundle-path gate skips it when it measures how far
	// ahead a target bundle is, so 1.0 -> 1.2 is one hop when 1.1 was
	// security-only (decision K, kn-mtpf).
	SecurityOnly bool
}

// Sequence is the catalog's published sequence, oldest first, with each release's
// security-only marker.
//
// THE BUNDLE-PATH GATE WALKS THIS AND NOTHING ELSE. "How far ahead is this
// target" is a question about the catalog's order; comparing version strings
// answers a different question — "1.10" sorts before "1.9" — and an adjacency
// check built on that answer refuses a legal hop and allows an illegal one.
func Sequence() ([]Release, error) {
	out := make([]Release, 0, len(Versions()))
	for _, v := range Versions() {
		m, err := Manifest(v)
		if err != nil {
			return nil, err
		}
		out = append(out, Release{Version: m.Bundle, SecurityOnly: m.SecurityOnly})
	}
	return out, nil
}

// Entry is one offered bundle, in the shape preflight checks a request
// against: which version, which tiers, which profiles, and whether it is still
// offered as a NEW install.
type Entry struct {
	Version  string
	HATiers  []string
	Profiles []string
	// UpgradeOnly marks a bundle that is no longer offered as a NEW install
	// (F19): preflight refuses an install of one and names the installable
	// bundle. Absent in the manifest is false.
	UpgradeOnly bool
}

// Catalog is every bundle this binary carries, parsed, in published order.
//
// It stands in for the control plane's bundle list when there is no control
// plane to ask yet — the --control-plane install — and it is deliberately the
// SAME check: a request for a tier or a profile the bundle does not offer is
// refused at preflight wherever the list came from, rather than discovered at
// the stage that would have installed it.
func Catalog() ([]Entry, error) {
	var out []Entry
	for _, v := range Versions() {
		m, err := Manifest(v)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{
			Version:     m.Bundle,
			HATiers:     m.HATiers,
			Profiles:    m.Profiles.Names(),
			UpgradeOnly: m.UpgradeOnly,
		})
	}
	return out, nil
}
