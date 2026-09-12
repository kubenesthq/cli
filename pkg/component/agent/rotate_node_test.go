package agent_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
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

// TestDeliverJWTPatchesTheManifestOnARealNode is kn-tlmv clause B: the delivery
// step, proven against a real k3s server node rather than a fixture.
//
// GATED ON AN EXPLICIT NODE because it needs a cluster. Unset, it skips, so
// `go test ./...` is unaffected. Set KUBENEST_TEST_K3S_NODE to a k3d server
// container that carries the agent manifest.
//
// WHAT IT ASSERTS IS THE RECEIVER, NOT THE CALL: the manifest ON THE NODE
// afterwards carries the new secret, every other line of it is byte-identical,
// and its mode is 0600.
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

	// EVERYTHING ELSE MUST BE UNCHANGED. This is the property that separates a
	// patch from a re-render, and a re-render would drop the repo credential and
	// flip the workload-Applications declaration (kn-tlmv, kn-zod2).
	beforeLines := strings.Split(strings.TrimSpace(before.Stdout), "\n")
	afterLines := strings.Split(strings.TrimSpace(after.Stdout), "\n")
	if len(beforeLines) != len(afterLines) {
		t.Fatalf("line count changed: %d -> %d; the patch rewrote more than the secret",
			len(beforeLines), len(afterLines))
	}
	changed := 0
	for i := range beforeLines {
		if beforeLines[i] != afterLines[i] {
			changed++
			if !strings.Contains(afterLines[i], "jwtSecret") {
				t.Fatalf("line %d changed and it is not the jwtSecret line: %q -> %q",
					i+1, beforeLines[i], afterLines[i])
			}
		}
	}
	if changed != 1 {
		t.Fatalf("expected exactly one changed line, got %d", changed)
	}

	mode, err := r.Run(ctx, "stat -c %a "+agent.ManifestPath)
	if err != nil || mode.ExitCode != 0 {
		t.Fatalf("stat: exit %d err %v", mode.ExitCode, err)
	}
	if got := strings.TrimSpace(mode.Stdout); got != "600" {
		t.Fatalf("the manifest carries a live credential and its mode is %q, not 600", got)
	}
	t.Logf("manifest patched in place, one line changed, mode %s", strings.TrimSpace(mode.Stdout))
}
