// Package timewindow is kured's own maintenance-window code, vendored so that
// the CLI's window and the backend's validator can be tested against the
// implementation that actually decides when a cluster reboots (plan 7.4 item
// 7, bead kn-t34-window-semantics-kured-s-2rlu).
//
// PROVENANCE — the two files beside this one are UNMODIFIED upstream bytes.
//
//	module      github.com/kubereboot/kured
//	tag         1.23.0
//	commit      ef3c90b2bd58c0571e6eae7b893f0de3709cc98f (refs/tags/1.23.0)
//	files       pkg/timewindow/timewindow.go   sha256 08ee7e139c79d3500a06221d1fd227363e63fd295baffa812a8c0e293f892fb1
//	            pkg/timewindow/days.go         sha256 ab2392a00f574b888cc679a409ee644cd27e8f54a73dc9d3d027b7474ad24fa9
//	licence     Apache-2.0, the LICENSE at the root of that repository; the two
//	            upstream files carry no per-file header, and none was added —
//	            adding one would change the bytes the SHA-256 above pins.
//
// The tag is spelled 1.23.0 because that is how upstream tags it: this module
// has no `v`-prefixed tags at all (`git ls-remote --tags` lists refs/tags/1.23.0
// and no refs/tags/v1.23.0), and the module proxy answers `unknown revision
// v1.23.0`. kured_oracle_test.go::TestVendoredOracleMatchesUpstreamTag hashes
// these two files against the digests above, so the oracle cannot drift from
// kured 1.23.0 unnoticed.
//
// WHY VENDORED RATHER THAN REQUIRED. `go get github.com/kubereboot/kured@1.23.0`
// rewrites this module's `go 1.25.0` to `go 1.26.4` (kured's go.mod declares
// 1.26.4), and this repository's CI installs Go from go.mod
// (`go-version-file: go.mod`), so requiring the module moves the toolchain of
// every build and every CI image. There is no `v1.23.0` tag to ask for either.
// kured_oracle_test.go records the measurement at the top of the file.
//
// THESE FILES MOVE ONLY WITH THE BUNDLE'S KURED PIN. When the bundle moves to a
// new kured release, copy that release's pkg/timewindow/{timewindow.go,days.go}
// verbatim, update the provenance above and the digests in the test — and only
// then re-read this file's callers, because the window's semantics are the
// contract the CLI and the backend are held to.
//
// The package name is upstream's, not this directory's: the directory is named
// for what it is to this repository (the oracle), the package keeps the name and
// the bytes it has upstream. Import it as
//
//	timewindow "kubenest.io/cli/internal/kuredoracle"
package timewindow
