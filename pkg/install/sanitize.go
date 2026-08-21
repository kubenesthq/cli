package install

import (
	"regexp"
	"strings"
)

// DetailLimit is the contract's cap on an install-journal entry's detail
// (install_journal_entry.json, maxLength 2000). Longer text is truncated
// rather than dropped: a truncated fix still names the object.
const DetailLimit = 2000

// The shapes that must never reach a journal entry or a stage event. The
// contract says detail is "user-safe result or remediation detail. Secrets
// and raw command output must never be placed here" — this is that rule as
// code rather than as a promise, because the text being sanitized comes from
// component installers and from remote shells, and neither is under the
// engine's control.
var (
	// PEM private keys, in whole. The per-cluster deploy key is exactly this
	// shape and it passes through this process.
	pemKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// JWTs — the agent credential.
	jwt = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)
	// The CLI's own control-plane token.
	cliToken = regexp.MustCompile(`knp_[A-Za-z0-9_\-]{8,}`)
	// Long mixed-case base64-ish runs: a blob someone shell-echoed. Lowercase
	// hex digests and UUIDs deliberately do not match — they are useful in a
	// failure message and are not secrets.
	blob = regexp.MustCompile(`[A-Za-z0-9+/]{60,}={0,2}`)
	// Bearer headers, which show up whenever a curl is echoed back.
	bearer = regexp.MustCompile(`(?i)(authorization:\s*bearer|bearer)\s+[A-Za-z0-9._\-]{8,}`)
)

// Sanitize makes text safe to journal and to publish.
//
// It strips credential-shaped runs and truncates to the contract's limit. It
// deliberately does NOT strip anything else: the convergence state is the
// part that names the fix — "pod openebs-lvm-node-x in openebs is
// CrashLoopBackOff — volume group kubenest-vg not found" — and sanitizing it
// into "install failed" would defeat the whole failure path.
func Sanitize(s string) string {
	s = pemKey.ReplaceAllString(s, "[redacted private key]")
	s = jwt.ReplaceAllString(s, "[redacted token]")
	s = cliToken.ReplaceAllString(s, "[redacted token]")
	s = bearer.ReplaceAllString(s, "[redacted credential]")
	s = blob.ReplaceAllStringFunc(s, func(m string) string {
		if hasUpper(m) && hasLower(m) {
			return "[redacted]"
		}
		return m // lowercase hex digest, or an uppercase constant — not a secret
	})
	s = strings.TrimSpace(s)
	if len(s) > DetailLimit {
		const note = "… (truncated)"
		s = s[:DetailLimit-len(note)] + note
	}
	return s
}

func hasUpper(s string) bool {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

func hasLower(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			return true
		}
	}
	return false
}
