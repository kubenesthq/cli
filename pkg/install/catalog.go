package install

import (
	"context"

	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/preflight"
)

// EmbeddedCatalog is preflight's view of the bundles built into this binary.
//
// A --control-plane install checks its request against this catalog rather
// than the control plane's: the control plane is what the same run is
// installing, so there is nothing to ask until stage 9 has finished. The
// CHECK is unchanged — a request for a tier or a profile the bundle does not
// offer is refused before anything is written to a machine — and a registered
// install checks the same request against the control plane's catalog, which
// is the same shipped set. Where the offer is read from moves; what it must
// contain does not.
type EmbeddedCatalog struct{}

// ListBundles returns the offered bundles from the embedded catalog.
func (EmbeddedCatalog) ListBundles(context.Context) ([]preflight.BundleEntry, error) {
	entries, err := bundles.Catalog()
	if err != nil {
		return nil, err
	}
	out := make([]preflight.BundleEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, preflight.BundleEntry{Version: e.Version, HATiers: e.HATiers, Profiles: e.Profiles})
	}
	return out, nil
}
