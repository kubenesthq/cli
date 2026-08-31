package leakscan

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// A PEM shaped like the per-cluster GitOps deploy key: multiline, and rendered
// into YAML as an indented block scalar.
const testKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAAB
AAAAMwAAAAtzc2gtZWQyNTUxOQAAACBmYWtlZmFrZWZha2VmYWtl
-----END OPENSSH PRIVATE KEY-----`

const testJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.ZmFrZS1hZ2VudC1qd3QtcGF5bG9hZA.c2ln"

// The command shape that shipped the defect: the whole values document
// base64-encoded onto the command line. This is the case the scanner exists
// for, and if it ever stops matching, every leak assertion in this repo goes
// vacuous at once.
func TestCatchesTheShapeThatShippedTheDefect(t *testing.T) {
	values := "agentJWT: " + testJWT + "\ngitSSHPrivateKey: |-\n"
	for _, line := range strings.Split(testKey, "\n") {
		values += "                    " + line + "\n"
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(values))
	cmd := fmt.Sprintf("printf '%%s' %s | base64 -d | sudo -n tee /var/lib/rancher/k3s/server/manifests/kubenest-agent.yaml >/dev/null", encoded)

	if !RecoverableFrom(cmd, testJWT) {
		t.Error("the agent JWT is not recoverable from the pre-fix command shape; the scanner is blind")
	}
	if !RecoverableFrom(cmd, testKey) {
		t.Error("the deploy key is not recoverable from the pre-fix command shape; the scanner is blind")
	}
}

// Negative control: an ordinary command carrying no payload must not match,
// or the scanner would report a leak everywhere and mean nothing.
func TestDoesNotMatchAnOrdinaryCommand(t *testing.T) {
	for _, cmd := range []string{
		"sudo -n chmod 600 /var/lib/rancher/k3s/server/manifests/kubenest-agent.yaml",
		"sudo -n install -m 0600 /dev/stdin /etc/rancher/kubenest-join-token",
		"sudo -n k3s kubectl apply -f -",
		"sudo -n k3s kubectl get deployment/kubenest-agent -n kubenest-system -o json",
	} {
		if RecoverableFrom(cmd, testJWT) || RecoverableFrom(cmd, testKey) {
			t.Errorf("false positive on a payload-free command: %q", cmd)
		}
	}
}

// Carries must see a PEM through YAML block-scalar indentation. Requiring a
// contiguous match is exactly the blindness this package removes.
func TestCarriesSeesThroughBlockScalarIndentation(t *testing.T) {
	var indented strings.Builder
	indented.WriteString("gitSSHPrivateKey: |-\n")
	for _, line := range strings.Split(testKey, "\n") {
		indented.WriteString("      " + line + "\n")
	}
	if strings.Contains(indented.String(), testKey) {
		t.Fatal("the fixture is not actually indented; this test proves nothing")
	}
	if !Carries(indented.String(), testKey) {
		t.Error("Carries missed an indented PEM — the contiguous-match blindness is back")
	}
}

// The limits, asserted rather than assumed.
//
// The package comment used to claim that "any future write path that encodes,
// wraps, or chunks a secret onto a command line trips it". That was false. An
// unfalsifiable claim about a falsification tool is the same defect one level
// up, so the true shape of the limit is pinned here: if someone later makes
// the scanner catch these, this test fails and the comment gets corrected
// with it.
func TestKnownBlindSpotsAreStillBlindSpots(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte(testJWT))

	var wrapped strings.Builder
	for i := 0; i < len(enc); i += 40 {
		end := i + 40
		if end > len(enc) {
			end = len(enc)
		}
		wrapped.WriteString(enc[i:end] + "\n")
	}

	var chunked strings.Builder
	for i := 0; i < len(enc); i += 40 {
		end := i + 40
		if end > len(enc) {
			end = len(enc)
		}
		chunked.WriteString("'" + enc[i:end] + "' ")
	}

	for _, c := range []struct {
		name string
		cmd  string
	}{
		{"base64 wrapped across lines", "printf '%s' " + wrapped.String() + " | base64 -d"},
		{"base64 in shell-concatenated chunks", "printf '%s' " + chunked.String() + " | base64 -d"},
		{"hex", "printf '%s' " + hex.EncodeToString([]byte(testJWT)) + " | xxd -r -p"},
	} {
		if RecoverableFrom(c.cmd, testJWT) {
			t.Errorf("%s is now caught — good, but the package comment still lists it as a blind spot; update the comment", c.name)
		}
	}
}
