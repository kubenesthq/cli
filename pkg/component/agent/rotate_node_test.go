package agent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/sshx"
)

// dockerRunner is a k3s.Runner that reaches a k3d node with docker exec.
//
// IT LIVES IN A TEST FILE ON PURPOSE. The shipped command reaches a server node
// over SSH, and k3d nodes have no SSH — so proving the delivery path against a
// real k3s node needs a second transport, and that transport must not become a
// production surface nobody uses. What it exercises IS production code:
// agent.DeliverJWT, ReplaceJWTSecret, k3s.WriteManifest and the 0600
// restriction all run unmodified.
type dockerRunner struct{ container string }

func (d dockerRunner) exec(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	// A shell, because the commands under test are shell strings with pipes and
	// redirections, exactly as the SSH transport delivers them.
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", d.container, "sh", "-c", command)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := sshx.Result{Stdout: out.String(), Stderr: errb.String()}
	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		res.ExitCode = exitErr.ExitCode()
		return res, nil // a non-zero exit is a RESULT, not a transport failure
	}
	return res, err
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

func (d dockerRunner) Run(ctx context.Context, command string) (sshx.Result, error) {
	return d.exec(ctx, command, nil)
}

func (d dockerRunner) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	return d.exec(ctx, command, stdin)
}

// ---------------------------------------------------------------------------
// WHAT THE PATCH DOES TO THE FILE, AND A COMPARISON THAT CAN TELL THE TRUTH
// ABOUT IT.
//
// THE ASSERTION THIS REPLACES FAILED FOR THE RIGHT REASON AND NAMED THE WRONG
// ONE (kn-cluster-rotate-token-cli-tlmv clause B). It required the manifest's
// line count to be unchanged and exactly one LINE to differ. ReplaceJWTSecret
// parses the document, changes one leaf and SERIALISES THE WHOLE THING BACK —
// twice, because spec.valuesContent is itself a YAML string — so the file
// returns with the serialiser's indentation, the serialiser's key order and no
// comments at all. On a real node the old assertion therefore failed while its
// message said a line "changed and it is not the jwtSecret line", which sends
// the next reader hunting for a corrupted field. A failure that names the wrong
// cause is the same family as a mutant that fails for a reason nobody
// introduced: it looks exactly like a real result.
//
// THE DATA SURVIVES AND THE TEXT DOES NOT, so the property worth asserting is
// about the data. onlyTheSecretMoved compares DATA-BEARING LINES,
// whitespace-stripped and order-insensitive, and IT NEVER PARSES YAML — so it
// shares no call with the writer under test, and reindentation, key reordering
// and comment loss cannot register as drift while a dropped field, an added
// field or a flipped value must.
//
// ITS LIMITS, because a comparison that hides its blind spots is the defect it
// is replacing. A line whose data content begins with "#" inside a block scalar
// is classified as a comment and ignored. A blank line inside a block scalar is
// likewise ignored. Indentation is stripped, so a field moved to a different
// nesting level with the same text reads as unchanged. Order is ignored, so a
// reordering is invisible by design. Duplicate identical lines are compared as a
// multiset, so a duplicate collapsing to one IS detected.
// ---------------------------------------------------------------------------

// manifestDrift is how two manifest TEXTS differ once the serialiser's own
// choices are discounted.
type manifestDrift struct {
	Missing         []string // data-bearing content present before, absent after
	Appeared        []string // data-bearing content present after, absent before
	CommentsDropped int
	BlanksDropped   int
}

func classify(text string) (data map[string]int, comments, blanks int) {
	data = map[string]int{}
	for _, raw := range strings.Split(strings.TrimSpace(text), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			blanks++
		case strings.HasPrefix(line, "#"):
			comments++
		default:
			data[line]++
		}
	}
	return data, comments, blanks
}

// commentCount is how many comment lines a manifest text carries.
func commentCount(text string) int {
	_, comments, _ := classify(text)
	return comments
}

func driftOf(before, after string) manifestDrift {
	beforeData, beforeComments, beforeBlanks := classify(before)
	afterData, afterComments, afterBlanks := classify(after)

	var d manifestDrift
	for line, n := range beforeData {
		if gap := n - afterData[line]; gap > 0 {
			for i := 0; i < gap; i++ {
				d.Missing = append(d.Missing, line)
			}
		}
	}
	for line, n := range afterData {
		if gap := n - beforeData[line]; gap > 0 {
			for i := 0; i < gap; i++ {
				d.Appeared = append(d.Appeared, line)
			}
		}
	}
	sort.Strings(d.Missing)
	sort.Strings(d.Appeared)
	if gap := beforeComments - afterComments; gap > 0 {
		d.CommentsDropped = gap
	}
	if gap := beforeBlanks - afterBlanks; gap > 0 {
		d.BlanksDropped = gap
	}
	return d
}

// describe renders one manifest line for a failure message WITHOUT printing its
// value. A manifest on a real node carries a live agent JWT and a GitOps deploy
// key, so a test that prints the offending line publishes a credential into test
// output — which the assertion this replaces did. What comes back is the YAML key
// when the line has one, plus a sha256 prefix of the whole stripped line AND THE
// EXTENT HASHED, because a prefix without its extent is not an identifier and two
// prefixes over different extents are as unmatchable as two redactions.
func describe(line string) string {
	key := "none (block-scalar content or list item)"
	if i := strings.Index(line, ":"); i > 0 {
		candidate := line[:i]
		if strings.IndexFunc(candidate, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' || r == '/' ||
				(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
		}) < 0 {
			key = candidate
		}
	}
	sum := sha256.Sum256([]byte(line))
	return fmt.Sprintf("key=%s present=true sha256=%x extent=%d bytes", key, sum[:4], len(line))
}

// onlyTheSecretMoved reports whether the only data-bearing change between two
// manifest texts is the kubenest.jwtSecret line. EVERY ERROR IT RETURNS NAMES
// THE PROPERTY THAT BROKE rather than the line number it happened to notice,
// because an exit code and a line number cannot tell a caught defect from an
// instrument that looked in the wrong place.
func onlyTheSecretMoved(before, after, newToken string) error {
	d := driftOf(before, after)

	for _, line := range d.Missing {
		if !strings.Contains(line, "jwtSecret") {
			return fmt.Errorf("PROPERTY BROKEN — every field other than kubenest.jwtSecret must survive "+
				"the round-trip with its data unchanged: this one did not survive: %s. "+
				"A re-render would lose the GitOps deploy key and flip the workload-Applications "+
				"declaration, which is the whole reason this is a patch rather than a re-render",
				describe(line))
		}
	}
	for _, line := range d.Appeared {
		if !strings.Contains(line, "jwtSecret") {
			return fmt.Errorf("PROPERTY BROKEN — the patch must ADD no field: this one is present after "+
				"and absent before: %s. A patch that quietly adds a key has written a value nobody "+
				"reads, which is indistinguishable from success until the cluster fails to connect",
				describe(line))
		}
	}
	if len(d.Missing) != 1 || len(d.Appeared) != 1 {
		return fmt.Errorf("PROPERTY BROKEN — exactly one data-bearing line, the jwtSecret line, may "+
			"differ: %d line(s) disappeared and %d appeared. Serialiser reindentation, key reordering "+
			"and comment loss are excluded from this comparison by construction, so a count other than "+
			"one is real drift and not formatting", len(d.Missing), len(d.Appeared))
	}
	if !strings.Contains(d.Appeared[0], newToken) {
		return fmt.Errorf("PROPERTY BROKEN — the one changed line must carry the NEW token: the line that "+
			"appeared is %s, and it does not contain the token this rotation delivered", describe(d.Appeared[0]))
	}
	return nil
}

// ---------------------------------------------------------------------------
// The pure-function half of clause B, which needs no cluster and no artifact.
// ReplaceJWTSecret is a function of bytes, so the round-trip it performs can be
// measured here rather than inferred from a node run.
// ---------------------------------------------------------------------------

// authoredManifest is shaped like a manifest an OPERATOR has edited: two-space
// indentation, the author's key order rather than alphabetical, explanatory
// comments and a blank line. It duplicates the shape of the internal
// installedManifest fixture on purpose — this file is package agent_test and
// cannot see it — and it adds the comments and blanks that fixture lacks,
// because comment and blank handling is the behaviour under measurement.
const authoredManifest = `apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: operator
  namespace: kube-system
spec:
  chart: oci://ghcr.io/kubenesthq/charts/kubenest-operator-2
  createNamespace: true
  targetNamespace: kubenest-system
  version: 2.6.16
  valuesContent: |
    # Pinned by hand after the 2.6.14 regression — do not bump without reading kn-b9bh.
    argo-cd:
      configs:
        ssh:
          extraHosts: |
            gitea.example ssh-ed25519 AAAA-public-host-key
    bootstrap:
      certManager:
        enabled: false
      gitea:
        enabled: false

    kubenest:
      backendURL: ws://hub.example:8001/ws/operator
      bootstrapController:
        gitRepoBranch: main
        gitRepoURL: ssh://git@gitea.example/kubenest/cluster.git
        gitSSHPrivateKey: |
          -----BEGIN OPENSSH PRIVATE KEY-----
          not-a-real-key
          -----END OPENSSH PRIVATE KEY-----
      clusterID: 019d52e1-ba17-7e70-94a0-8a33a48b7fcb
      jwtSecret: the-old-token
      # workloadApplications stays off on this cluster: the control plane owns them.
      workloadApplications:
        enabled: false
`

const replacementToken = "the-new-token"

// TestReplaceJWTSecretPreservesEveryOtherFieldsData is the property the old
// line-count assertion was reaching for and could not express.
func TestReplaceJWTSecretPreservesEveryOtherFieldsData(t *testing.T) {
	patched, namespace, err := agent.ReplaceJWTSecret([]byte(authoredManifest), replacementToken)
	if err != nil {
		t.Fatalf("ReplaceJWTSecret on an operator-authored manifest: %v", err)
	}
	if namespace != "kubenest-system" {
		t.Errorf("namespace = %q, want kubenest-system read from the manifest", namespace)
	}
	if err := onlyTheSecretMoved(authoredManifest, string(patched), replacementToken); err != nil {
		t.Fatal(err)
	}

	d := driftOf(authoredManifest, string(patched))
	t.Logf("the DATA survives and the FILE does not: %d comment line(s) and %d blank line(s) were "+
		"dropped, and indentation and key order are the serialiser's rather than the author's. "+
		"Compared at the level of data-bearing lines for exactly that reason.",
		d.CommentsDropped, d.BlanksDropped)
}

// TestReplaceJWTSecretDeletesOperatorComments PINS A KNOWN DEFECT rather than a
// property anybody wants. Every rotation reserialises the manifest, so any
// comment an operator wrote in it is silently deleted — the data survives, the
// operator's explanation of it does not.
//
// IF THIS TEST FAILS BECAUSE COMMENTS NOW SURVIVE, THE DEFECT IS FIXED: delete
// this test and close kn-00u7 rather than making the assertion pass again.
func TestReplaceJWTSecretDeletesOperatorComments(t *testing.T) {
	patched, _, err := agent.ReplaceJWTSecret([]byte(authoredManifest), replacementToken)
	if err != nil {
		t.Fatalf("ReplaceJWTSecret: %v", err)
	}
	beforeComments := commentCount(authoredManifest)
	afterComments := commentCount(string(patched))
	if beforeComments == 0 {
		t.Fatal("the fixture carries no comments, so this test would prove nothing")
	}
	if afterComments != 0 {
		t.Fatalf("KNOWN DEFECT NO LONGER REPRODUCES: %d of %d comment line(s) survived the patch. "+
			"If the writer now preserves comments, delete this test and close kn-00u7",
			afterComments, beforeComments)
	}
	t.Logf("%d operator comment line(s) deleted by the round-trip, which is kn-00u7", beforeComments)
}

// TestOnlyTheSecretMovedDiscriminates is the control on the comparison itself.
// A comparison used to certify a real-node run has to be shown to FAIL on the
// things it exists to catch, or it is another guard nobody mutated.
func TestOnlyTheSecretMovedDiscriminates(t *testing.T) {
	patched, _, err := agent.ReplaceJWTSecret([]byte(authoredManifest), replacementToken)
	if err != nil {
		t.Fatalf("ReplaceJWTSecret: %v", err)
	}
	real := string(patched)

	cases := []struct {
		name string
		// after is derived from the REAL patched output, so every case differs
		// from the genuine round-trip by exactly the defect it names.
		after    string
		wantFail string // substring the failure must contain, or "" for must-pass
	}{
		{
			name:     "the genuine round-trip: reindented, reordered, comments gone",
			after:    real,
			wantFail: "",
		},
		{
			name:     "a field the patch must preserve is dropped",
			after:    strings.Replace(real, "        gitRepoBranch: main\n", "", 1),
			wantFail: "must survive the round-trip with its data unchanged",
		},
		{
			name:     "a boolean elsewhere in the document is flipped",
			after:    strings.Replace(real, "                enabled: false", "                enabled: true", 1),
			wantFail: "must survive the round-trip with its data unchanged",
		},
		{
			name:     "a field nobody asked for is added",
			after:    strings.Replace(real, "      clusterID:", "      insecureSkipVerify: true\n      clusterID:", 1),
			wantFail: "must ADD no field",
		},
		{
			name:     "the secret was not replaced at all",
			after:    strings.Replace(real, replacementToken, "the-old-token", 1),
			wantFail: "exactly one data-bearing line",
		},
		{
			name:     "the secret line carries some other token",
			after:    strings.Replace(real, replacementToken, "a-third-token", 1),
			wantFail: "must carry the NEW token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// REFUSE TO SCORE AN ARM WHOSE MUTATION DID NOT HAPPEN. Every failing
			// case above is a strings.Replace against the serialiser's real output,
			// and a pattern that does not match leaves `after` equal to the genuine
			// round-trip — which then "passes" while exercising nothing. A mutant
			// that fails for a reason nobody introduced looks exactly like a caught
			// one, and this is the cheap discriminator for it.
			if tc.wantFail != "" && tc.after == real {
				t.Fatal("this arm's fixture is byte-identical to the real patched output, so its " +
					"strings.Replace pattern did not match and the arm proves nothing. Fix the " +
					"pattern against the serialiser's actual indentation rather than the author's")
			}
			err := onlyTheSecretMoved(authoredManifest, tc.after, replacementToken)
			if tc.wantFail == "" {
				if err != nil {
					t.Fatalf("the genuine round-trip was reported as drift, so this comparison "+
						"cannot certify a real-node run: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %s: a comparison that cannot fail on this cannot certify anything", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantFail) {
				t.Fatalf("the failure does not NAME the property %q; it said: %v", tc.wantFail, err)
			}
			if strings.Contains(err.Error(), "gitRepoBranch: main") ||
				strings.Contains(err.Error(), "not-a-real-key") {
				t.Fatalf("the failure printed a manifest line verbatim; on a real node that line is a "+
					"live credential. It must carry a key name, a sha256 prefix and the extent: %v", err)
			}
		})
	}
}

// TestDeliverJWTPatchesTheManifestOnARealNode is kn-tlmv clause B: the delivery
// step, proven against a real k3s server node rather than a fixture.
//
// GATED ON AN EXPLICIT NODE because it needs a cluster. Unset, it skips, so
// `go test ./...` is unaffected. Set KUBENEST_TEST_K3S_NODE to a k3d server
// container that carries the agent manifest.
//
// WHAT IT ASSERTS IS THE RECEIVER, NOT THE CALL: the manifest ON THE NODE
// afterwards carries the new secret, every other field's DATA is unchanged, and
// its mode is 0600. Not "every other line is byte-identical" — that was the old
// assertion and the round-trip makes it false.
func TestDeliverJWTPatchesTheManifestOnARealNode(t *testing.T) {
	node := os.Getenv("KUBENEST_TEST_K3S_NODE")
	if node == "" {
		t.Skip("set KUBENEST_TEST_K3S_NODE to a k3d server container carrying the agent manifest")
	}
	r := dockerRunner{container: node}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	before, err := r.Run(ctx, "cat "+agent.ManifestPath)
	if err != nil || before.ExitCode != 0 {
		t.Fatalf("the node does not carry %s (exit %d, err %v): seed it first",
			agent.ManifestPath, before.ExitCode, err)
	}
	if !strings.Contains(before.Stdout, "jwtSecret") {
		t.Fatal("the seeded manifest has no jwtSecret to replace; the test would prove nothing")
	}

	const newToken = "kn-tlmv-b-replacement-token"
	if strings.Contains(before.Stdout, newToken) {
		t.Fatal("the manifest already contains the replacement token, so a change would prove nothing")
	}

	if err := agent.DeliverJWT(ctx, r, newToken, 10*time.Minute, nil); err != nil {
		t.Fatalf("DeliverJWT against a real node: %v", err)
	}

	after, err := r.Run(ctx, "cat "+agent.ManifestPath)
	if err != nil || after.ExitCode != 0 {
		t.Fatalf("reading the manifest back: exit %d err %v", after.ExitCode, err)
	}
	if !strings.Contains(after.Stdout, newToken) {
		t.Fatal("the manifest on the node does not carry the new token")
	}

	// EVERYTHING ELSE'S DATA MUST BE UNCHANGED. This is the property that
	// separates a patch from a re-render, and a re-render would drop the repo
	// credential and flip the workload-Applications declaration (kn-tlmv,
	// kn-zod2). The comparison discounts the serialiser's indentation, key order
	// and comment loss, all three of which this patch really does change.
	if err := onlyTheSecretMoved(before.Stdout, after.Stdout, newToken); err != nil {
		t.Fatal(err)
	}

	mode, err := r.Run(ctx, "stat -c %a "+agent.ManifestPath)
	if err != nil || mode.ExitCode != 0 {
		t.Fatalf("stat: exit %d err %v", mode.ExitCode, err)
	}
	if got := strings.TrimSpace(mode.Stdout); got != "600" {
		t.Fatalf("the manifest carries a live credential and its mode is %q, not 600", got)
	}

	d := driftOf(before.Stdout, after.Stdout)
	t.Logf("the node's manifest was patched: one data-bearing line changed and it is the jwtSecret "+
		"line, mode %s. THE FILE WAS REWRITTEN THOUGH — %d comment line(s) and %d blank line(s) on "+
		"the node are gone (kn-00u7), and indentation and key order are now the serialiser's.",
		strings.TrimSpace(mode.Stdout), d.CommentsDropped, d.BlanksDropped)
}
