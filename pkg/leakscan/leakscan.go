// Package leakscan answers one question for tests: can this secret be
// recovered from that command string?
//
// It exists because "the secret is not in the command" was asserted three
// times in this repo by `strings.Contains(cmd, secret)`, and was false all
// three times. k3s.WriteManifest base64-encoded whole documents into the
// command string, so the plaintext could never appear there and the check
// could never fail — while the agent JWT, the per-cluster GitOps deploy key
// and the object store's secret access key really were in `ps auxww` on the
// target host, one `base64 -d` from anyone with a shell (kn-40rd).
//
// A test that cannot observe the failure it is named for is worse than no
// test, because it gets cited as evidence the failure cannot happen. So this
// decodes before it looks, and it is itself tested — see leakscan_test.go —
// against the exact command shape that shipped the defect.
//
// WHAT IT CATCHES: the plaintext; the whole needle base64-encoded; and a
// needle recoverable from any single contiguous base64 run in the command,
// in any of the four standard alphabets.
//
// WHAT IT DOES NOT CATCH, stated because the previous version of this comment
// claimed otherwise and was wrong: base64 wrapped across lines, base64 split
// into shell-concatenated chunks, hex, and anything compressed before
// encoding. Those all escape it, and leakscan_test.go asserts that they do,
// so the limit is recorded rather than assumed. This is a regression net for
// the shapes this codebase has actually produced, not a proof of absence.
package leakscan

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// base64Runs matches runs long enough to be a payload rather than a path
// segment or a flag.
var base64Runs = regexp.MustCompile(`[A-Za-z0-9+/_-]{20,}={0,2}`)

// RecoverableFrom reports whether needle can be recovered from command:
// verbatim, base64-encoded whole, or one decode away inside any base64-looking
// run the command contains.
func RecoverableFrom(command, needle string) bool {
	if Carries(command, needle) {
		return true
	}
	if strings.Contains(command, base64.StdEncoding.EncodeToString([]byte(needle))) {
		return true
	}
	for _, run := range base64Runs.FindAllString(command, -1) {
		for _, enc := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding,
			base64.URLEncoding, base64.RawURLEncoding,
		} {
			if decoded, err := enc.DecodeString(run); err == nil && Carries(string(decoded), needle) {
				return true
			}
		}
	}
	return false
}

// Carries reports whether every line of needle appears in text.
//
// A multiline secret is matched line by line rather than as one string,
// because YAML renders a PEM as an indented block scalar: the deploy key is
// present in the values document byte for byte, and yet strings.Contains for
// the key never matches, because every line but the first gained two spaces.
// Requiring the contiguous form would hand these tests back the blindness
// they exist to remove — a leaked key that happens to be indented is still a
// leaked key.
func Carries(text, needle string) bool {
	for _, line := range strings.Split(needle, "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(text, line) {
			return false
		}
	}
	return true
}
