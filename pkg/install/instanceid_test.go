package install

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/sshx"
)

// The instance id every recovery kit and every recovery set is bound to has to
// reach the operator, because the recovery scenarios assume the machine that
// ran the install is gone: `platform install --recover` refuses without it and
// tells the operator to read it off the machine that installed the control
// plane. Hardware found on 2026-09-28 that nothing ever printed it, so a fresh
// laptop could not learn the id its fleet's artifacts are bound to.
//
// These tests drive the real control-plane stage against a fake host rather
// than a printer helper, so what they assert is what the STAGE prints to the
// operator's own output — the same output the fleet recovery key goes to.

// instanceRunner is the fake host the control-plane stage runs against: a
// cluster whose instance identity may or may not exist yet.
//
// It answers the identity Secret and nothing else. The install Secret, the
// chart and the readiness probes are refused, because what is under test is
// what the stage printed on the way to the identity, and a fake that rendered
// a whole control plane would be a test of the fake.
type instanceRunner struct {
	t *testing.T
	// stored is the instance Secret this cluster serves back once it has one:
	// the keys the stage created, as the API server would store them.
	stored map[string]string
	// creates counts the instance identities the stage asked for, so a re-run
	// can be shown to mint nothing.
	creates int
}

func (r *instanceRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	if strings.Contains(command, controlplane.InstanceName) {
		if r.stored == nil {
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "` + controlplane.InstanceName + `" not found`}, nil
		}
		return sshx.Result{Stdout: r.instanceJSON()}, nil
	}
	// Past the identity: this host has no install Secret, and the run stops
	// there rather than rendering a control plane nothing here asserts about.
	return sshx.Result{ExitCode: 1, Stderr: "this fake host has no install Secret"}, nil
}

func (r *instanceRunner) RunInput(_ context.Context, command string, in io.Reader) (sshx.Result, error) {
	body, err := io.ReadAll(in)
	if err != nil {
		r.t.Fatalf("reading the document the stage sent: %v", err)
	}
	if !bytes.Contains(body, []byte(controlplane.InstanceName)) {
		return sshx.Result{ExitCode: 1, Stderr: "this fake host has no install Secret"}, nil
	}
	var doc struct {
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		r.t.Fatalf("the instance manifest is not the YAML this fake reads back: %v\n%s", err, body)
	}
	r.creates++
	r.stored = doc.StringData
	return sshx.Result{}, nil
}

// instanceJSON is the Secret as `kubectl get ... -o json` returns it: the
// stringData the stage created, base64-encoded into data.
func (r *instanceRunner) instanceJSON() string {
	data := make(map[string]string, len(r.stored))
	for key, value := range r.stored {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	body, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		r.t.Fatalf("encoding the instance Secret: %v", err)
	}
	return string(body)
}

// cpIdentitySession is a --control-plane session whose only node is the fake
// host, with the operator's output captured exactly where the command puts it
// (pkg/cmd platform_run.go hands the session the console it writes to).
func cpIdentitySession(t *testing.T, runner *instanceRunner) (*Session, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	s := &Session{
		ID: "run-1",
		Opts: Options{
			Bundle: "1.2", Name: "cp-1", Servers: []string{"10.0.1.10"}, HATier: "single-server",
			ControlPlaneInstall: true, Domain: "kn.example.test", AdminEmail: "admin@kn.example.test",
		},
		Out:   &out,
		Nodes: []Node{{Address: "10.0.1.10", Role: RoleServer, Runner: runner}},
	}
	return s, &out
}

// printedFleetKey is the one line of the output that carries the key, so the
// test reads the key the operator would read rather than the key the stage
// happens to hold.
func printedFleetKey(t *testing.T, printed string) string {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "AGE-SECRET-KEY-1") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("the output carries no fleet recovery key line:\n%s", printed)
	return ""
}

// The first install mints the fleet recovery key and prints it once — and it
// has to hand the operator the instance id in the same run, on the same
// output. An operator who writes down the key but not the id has a kit that
// opens and a recovery that cannot be started.
//
// The planted negative: the id must be in the operator's OWN output, printed
// by the same stage, not only in a log, journal or event nobody reads — so the
// assertion is made against the session's Out, which is the same sink the key
// is asserted on.
func TestTheControlPlaneInstallPrintsTheInstanceIDWithTheKey(t *testing.T) {
	runner := &instanceRunner{t: t}
	s, out := cpIdentitySession(t, runner)

	// This run stops at the install Secret the fake does not have. The
	// assertions are about what the stage printed on the way there, and the
	// failure must not be mistaken for the stage having completed.
	if err := stageControlPlane(context.Background(), s); err == nil {
		t.Fatal("this fake host has no install Secret, so the stage cannot have reached its end")
	}
	if runner.creates != 1 {
		t.Fatalf("the first install created %d instance identities, want exactly 1: this is the run that mints it", runner.creates)
	}
	printed := out.String()
	id, recipient := runner.stored["instance-id"], runner.stored["fleet-recipient"]
	if id == "" || recipient == "" {
		t.Fatalf("the fake host recorded no complete identity: id %q, recipient %q", id, recipient)
	}

	if !strings.Contains(printed, id) {
		t.Errorf("the install never printed the instance id %q, so a laptop that does not have this one's config cannot learn the id its kits and recovery sets are bound to:\n%s", id, printed)
	}
	// The key's one-and-only-copy guarantee is unchanged: printed once, and
	// the key printed is the one whose public half this run recorded.
	if got := strings.Count(printed, "AGE-SECRET-KEY-1"); got != 1 {
		t.Fatalf("the fleet recovery key appears %d times, want exactly 1:\n%s", got, printed)
	}
	key, err := recoverykit.ParseFleetKey(printedFleetKey(t, printed))
	if err != nil {
		t.Fatalf("the printed key does not parse as a fleet recovery key: %v", err)
	}
	if key.Recipient() != recipient {
		t.Errorf("this run recorded recipient %q and printed a key whose public half is %q: a kit sealed to one could not be opened by the other", recipient, key.Recipient())
	}
}

// A RE-RUN prints the instance id and nothing else. The fleet key has been in
// the operator's hands since the first install and this process never sees it,
// but the id is what a recovery is asked for and the first run's output may be
// long gone — so an operator who lost it learns it from the machine that runs
// the install.
//
// The planted negative: no key material on this path. There is no key to print
// here, and an install that found the identity already recorded must never
// print one.
func TestAControlPlaneReRunPrintsTheInstanceIDAndNoKey(t *testing.T) {
	const id = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c35"
	runner := &instanceRunner{t: t, stored: map[string]string{
		"instance-id":     id,
		"fleet-recipient": mustFleetRecipient(t),
	}}
	s, out := cpIdentitySession(t, runner)

	if err := stageControlPlane(context.Background(), s); err == nil {
		t.Fatal("this fake host has no install Secret, so the stage cannot have reached its end")
	}
	if runner.creates != 0 {
		t.Errorf("the re-run created %d instance identities, want none: the id is minted once and is never replaced", runner.creates)
	}
	printed := out.String()

	if !strings.Contains(printed, id) {
		t.Errorf("the re-run never printed the instance id %q, so an operator who lost the first install's output has no way to learn it from the machine that runs the install:\n%s", id, printed)
	}
	if strings.Contains(printed, "AGE-SECRET-KEY-1") {
		t.Errorf("the re-run printed secret key material, and the fleet key's only copy is the one the operator wrote down at the first install:\n%s", printed)
	}
}
