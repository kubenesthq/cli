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

// Versions is every bundle version built into this binary, sorted.
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
	sort.Strings(out)
	return out
}

// Entry is one offered bundle, in the shape preflight checks a request
// against: which version, which tiers, which profiles.
type Entry struct {
	Version  string
	HATiers  []string
	Profiles []string
}

// Catalog is every bundle this binary carries, parsed.
//
// It is the standalone stand-in for the control plane's bundle list, and it
// is deliberately the SAME check: a request for a tier or a profile the
// bundle does not offer is refused at preflight in both modes, rather than
// discovered at the stage that would have installed it.
func Catalog() ([]Entry, error) {
	var out []Entry
	for _, v := range Versions() {
		m, err := Manifest(v)
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{
			Version:  m.Bundle,
			HATiers:  m.HATiers,
			Profiles: m.Profiles.Names(),
		})
	}
	return out, nil
}
