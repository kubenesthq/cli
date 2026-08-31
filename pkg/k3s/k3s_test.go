package k3s

import (
	"context"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/sshx"
)

// fakeRunner scripts command → result. Unknown commands fail the test, so
// every remote call a function makes is visible in its test.
type fakeRunner struct {
	t       *testing.T
	replies map[string]sshx.Result
	ran     []string
}

func (f *fakeRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	f.ran = append(f.ran, command)
	res, ok := f.replies[command]
	if !ok {
		f.t.Fatalf("unscripted command: %q", command)
	}
	return res, nil
}

// This fake scripts kubectl calls, which never stream. A streamed call here
// means the code under test changed shape and the script no longer describes
// it, so say so rather than answering an empty success.
func (f *fakeRunner) RunInput(_ context.Context, command string, _ io.Reader) (sshx.Result, error) {
	f.t.Fatalf("unexpected streamed command: %q", command)
	return sshx.Result{}, nil
}

// The manifest content reaches the host over stdin and appears nowhere in
// the command string. This is the property kn-40rd is about: manifests carry
// secrets (the agent's values document holds the agent JWT and the GitOps
// deploy key), and a command string is the argv of the shell sshd spawns —
// world-readable in `ps auxww` on the target host.
//
// The command is asserted to contain NO base64 run of any length, rather
// than merely not containing the plaintext. The defect this replaces was
// invisible to a plaintext scan precisely because it base64-encoded first,
// so "the content is not in the command" is not a strong enough assertion to
// state here — "the command carries no payload at all" is.
func TestWriteManifestSendsContentOverStdinAndNeverOnTheCommandLine(t *testing.T) {
	content := []byte("kind: StorageClass\nmetadata:\n  name: x\n")
	var captured string
	r := &capturingRunner{onRun: func(cmd string) sshx.Result {
		captured = cmd
		return sshx.Result{}
	}}

	if err := WriteManifest(context.Background(), r, "kubenest-storageclass", content); err != nil {
		t.Fatal(err)
	}
	// Exact equality, not a substring check. The command is fully determined
	// by the manifest path, so stating it exactly is the assertion that no
	// payload in ANY encoding can be hiding in it — a scan for the plaintext,
	// or even for base64-looking runs, is the kind of check the old form
	// slipped past.
	target := ManifestDir + "/kubenest-storageclass.yaml"
	want := "sudo -n install -m 0600 /dev/stdin " + target + ".tmp" +
		" && sudo -n mv -f " + target + ".tmp " + target +
		" || { sudo -n rm -f " + target + ".tmp; false; }"
	if captured != want {
		t.Errorf("command =\n  %q\nwant\n  %q", captured, want)
	}
	if string(r.stdin) != string(content) {
		t.Errorf("stdin payload = %q, want %q", r.stdin, content)
	}
}

type capturingRunner struct {
	onRun func(cmd string) sshx.Result
	// stdin holds what the last streamed call sent, so a test can assert on
	// the payload separately from the command that carried it.
	stdin []byte
}

func (c *capturingRunner) Run(_ context.Context, cmd string) (sshx.Result, error) {
	return c.onRun(cmd), nil
}

func (c *capturingRunner) RunInput(_ context.Context, cmd string, stdin io.Reader) (sshx.Result, error) {
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return sshx.Result{}, err
		}
		c.stdin = b
	}
	return c.onRun(cmd), nil
}

func TestWriteManifestRejectsShellySuspectNames(t *testing.T) {
	err := WriteManifest(context.Background(), &capturingRunner{onRun: func(string) sshx.Result {
		t.Fatal("must not run anything for a bad name")
		return sshx.Result{}
	}}, "Bad;Name", nil)
	if err == nil {
		t.Fatal("bad manifest name accepted")
	}
}

func TestHelmChartManifestPinsWhatTheBundleSays(t *testing.T) {
	raw, err := HelmChart{
		Name:            "openebs-lvm-localpv",
		Repo:            "https://openebs.github.io/lvm-localpv",
		Chart:           "lvm-localpv",
		Version:         "1.10.0",
		TargetNamespace: "openebs",
		ValuesYAML:      "analytics:\n  enabled: false\n",
	}.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec struct {
			Repo            string `yaml:"repo"`
			Chart           string `yaml:"chart"`
			Version         string `yaml:"version"`
			TargetNamespace string `yaml:"targetNamespace"`
			CreateNamespace bool   `yaml:"createNamespace"`
			ValuesContent   string `yaml:"valuesContent"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.APIVersion != "helm.cattle.io/v1" || doc.Kind != "HelmChart" {
		t.Errorf("rendered %s/%s, want helm.cattle.io/v1 HelmChart", doc.APIVersion, doc.Kind)
	}
	if doc.Metadata.Namespace != "kube-system" {
		t.Errorf("CR namespace %q, want kube-system (where the helm-controller watches)", doc.Metadata.Namespace)
	}
	if doc.Spec.Version != "1.10.0" || doc.Spec.Chart != "lvm-localpv" {
		t.Errorf("spec pins %s@%s, want lvm-localpv@1.10.0", doc.Spec.Chart, doc.Spec.Version)
	}
	if doc.Spec.TargetNamespace != "openebs" || !doc.Spec.CreateNamespace {
		t.Errorf("target %q createNamespace=%v, want openebs true", doc.Spec.TargetNamespace, doc.Spec.CreateNamespace)
	}
	if !strings.Contains(doc.Spec.ValuesContent, "enabled: false") {
		t.Errorf("valuesContent %q lost the values", doc.Spec.ValuesContent)
	}
}

// A version is required: pins come from the bundle manifest, and a chart CR
// without one would install "latest", which the bundle model forbids.
func TestHelmChartWithoutVersionIsAnError(t *testing.T) {
	_, err := HelmChart{Name: "x", Repo: "r", Chart: "c", TargetNamespace: "ns"}.Manifest()
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("versionless HelmChart error = %v, want one naming the version rule", err)
	}
}

const podsCmd = "sudo -n k3s kubectl get pods -n openebs -o json"

func TestCheckPodsReadyReportsTheStuckPodWithItsReason(t *testing.T) {
	pending := `{"items":[{"metadata":{"name":"lvm-node-abc"},"status":{
		"phase":"Pending",
		"conditions":[{"type":"PodScheduled","status":"False","message":"0/1 nodes are available: 1 Insufficient memory."}]}}]}`
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		podsCmd: {Stdout: pending},
	}}
	done, state, err := CheckPodsReady(context.Background(), r, "openebs")
	if err != nil || done {
		t.Fatalf("done=%v err=%v, want converging", done, err)
	}
	if !strings.Contains(state.Object, "lvm-node-abc") {
		t.Errorf("state %q does not name the pod", state)
	}
	if !strings.Contains(state.Detail, "Insufficient memory") {
		t.Errorf("state %q does not carry the scheduler's fix-shaped message", state)
	}
}

func TestCheckPodsReadyTreatsCrashLoopAsObservationNotVerdict(t *testing.T) {
	crash := `{"items":[{"metadata":{"name":"lvm-controller-0"},"status":{
		"phase":"Running",
		"conditions":[{"type":"Ready","status":"False"}],
		"containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 20s"}}}]}}]}`
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{podsCmd: {Stdout: crash}}}
	done, state, err := CheckPodsReady(context.Background(), r, "openebs")
	if err != nil {
		t.Fatalf("CrashLoopBackOff produced an error verdict: %v — transient states are observations", err)
	}
	if done {
		t.Fatal("CrashLoopBackOff counted as ready")
	}
	if state.Status != "CrashLoopBackOff" {
		t.Errorf("status %q, want the waiting reason CrashLoopBackOff", state.Status)
	}
}

func TestCheckPodsReadyPassesWhenAllReadyAndIgnoresSucceeded(t *testing.T) {
	ok := `{"items":[
		{"metadata":{"name":"helm-install-x"},"status":{"phase":"Succeeded"}},
		{"metadata":{"name":"lvm-controller-0"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{podsCmd: {Stdout: ok}}}
	done, _, err := CheckPodsReady(context.Background(), r, "openebs")
	if err != nil || !done {
		t.Fatalf("done=%v err=%v, want done with a Succeeded job ignored", done, err)
	}
}

func TestCheckPodsReadyWithNoPodsIsConvergingNotPassing(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{podsCmd: {Stdout: `{"items":[]}`}}}
	done, state, err := CheckPodsReady(context.Background(), r, "openebs")
	if err != nil || done {
		t.Fatalf("an empty namespace must converge, not pass (done=%v err=%v)", done, err)
	}
	if !strings.Contains(state.Status, "no pods") {
		t.Errorf("state %q does not say the namespace is empty", state)
	}
}

func TestKubectlErrorCarriesStderr(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		"sudo -n k3s kubectl get nothing": {ExitCode: 1, Stderr: "error: the server doesn't have a resource type \"nothing\"\nmore"},
	}}
	_, err := Kubectl(context.Background(), r, "get nothing")
	if err == nil || !strings.Contains(err.Error(), "resource type") {
		t.Fatalf("err = %v, want stderr's first line in it", err)
	}
}
