// Package version carries build-time version information, stamped by the
// release workflow via -ldflags. A dev build reports "dev".
package version

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns the human-readable version line.
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
