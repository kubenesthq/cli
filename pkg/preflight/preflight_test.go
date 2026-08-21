package preflight_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/sshx"
)

// A host that meets the specification: Ubuntu 24.04, passwordless sudo, no
// Kubernetes, kubenest-vg with free extents, and the sizing a "8 GB / 80 GB"
// machine actually reports (measured on a real Hetzner host, kn-bkwa).
func healthyHost(overrides map[string]sshx.Result) func(string) (sshx.Result, error) {
	return func(cmd string) (sshx.Result, error) {
		for match, res := range overrides {
			if strings.Contains(cmd, match) {
				return res, nil
			}
		}
		switch {
		case strings.Contains(cmd, "/etc/os-release"):
			return sshx.Result{Stdout: "ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"}, nil
		case strings.Contains(cmd, "sudo -n true"):
			return sshx.Result{}, nil
		case strings.Contains(cmd, "command -v"):
			return sshx.Result{Stdout: "\n"}, nil
		case strings.Contains(cmd, "MemTotal"):
			// 7.57 GiB of RAM, 74.77 GiB free — an "8 GB / 80 GB" host.
			return sshx.Result{Stdout: "cpu=4\nmemkb=7936000\ndiskbytes=80284000000\n"}, nil
		case strings.Contains(cmd, "vgs"):
			return sshx.Result{Stdout: "  53687091200\n"}, nil
		case strings.Contains(cmd, "curl"):
			return sshx.Result{Stdout: "https://ghcr.io/v2/ 401\nhttps://charts.jetstack.io/index.yaml 200\n"}, nil
		}
		return sshx.Result{}, nil
	}
}

type fakeCatalog struct {
	entries []preflight.BundleEntry
	err     error
}

func (f fakeCatalog) ListBundles(context.Context) ([]preflight.BundleEntry, error) {
	return f.entries, f.err
}

func testManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`
bundle: "1.0"
os:
  supported: [ubuntu-24.04]
ha-tiers: [single-server, ha]
limits:
  resources:
    floor: { cpu: 2, memory: 3.7Gi, disk: 36Gi }
    recommended: { cpu: 4, memory: 7.4Gi, disk: 92Gi }
    upgrade-headroom: { disk: 10Gi }
  timeouts:
    node-ready: 5m
    install-total: 30m
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func baseOptions(t *testing.T, respond func(string) (sshx.Result, error)) preflight.Options {
	t.Helper()
	return preflight.Options{
		Bundle:        testManifest(t),
		BundleVersion: "1.0",
		HATier:        "single-server",
		Nodes: []preflight.Node{{
			Address: "10.0.1.10", Role: "server",
			Runner: &componenttest.FakeRunner{Respond: respond},
		}},
		Egress: []preflight.EgressTarget{
			{Name: "container registry", URL: "https://ghcr.io/v2/"},
			{Name: "cert-manager charts", URL: "https://charts.jetstack.io/index.yaml"},
		},
		Catalog: fakeCatalog{entries: []preflight.BundleEntry{
			{Version: "1.0", HATiers: []string{"single-server", "ha"}, Profiles: []string{"observability", "secrets"}},
		}},
	}
}

func outcomeOf(rep preflight.Report, check string) (preflight.Result, bool) {
	for _, r := range rep.Results {
		if r.Check == check {
			return r, true
		}
	}
	return preflight.Result{}, false
}

func TestHealthyHostPassesEveryCheck(t *testing.T) {
	rep, err := preflight.Run(context.Background(), baseOptions(t, healthyHost(nil)))
	if err != nil {
		t.Fatalf("a correctly-specified host must pass:\n%v", err)
	}
	// All eleven checks from install.mdx's table must be present in the
	// report — a check that silently does not run is worse than one that
	// fails, because the operator believes it passed.
	wantChecks := []string{
		preflight.CheckControlPlane, preflight.CheckSSH, preflight.CheckOS,
		preflight.CheckPrivilege, preflight.CheckExistingK8s, preflight.CheckVolumeGroup,
		preflight.CheckPorts, preflight.CheckEgress, preflight.CheckResources,
		preflight.CheckNodeCount, preflight.CheckBundle,
	}
	for _, want := range wantChecks {
		if _, ok := outcomeOf(rep, want); !ok {
			t.Errorf("check %q did not run", want)
		}
	}
	if got := len(rep.Results); got != len(wantChecks) {
		t.Errorf("ran %d checks on a single-node install, want %d: %v", got, len(wantChecks), rep.Results)
	}
}

// The kn-bkwa defect, as a test: a host sold as 8 GB reports 7.57 GiB and a
// host sold as 4 GB reports 3.7 GiB. Neither may be refused.
func TestBinaryUnitsDoNotRefuseACorrectlySizedHost(t *testing.T) {
	// A "4 GB / 40 GB" machine as the kernel sees it.
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"MemTotal": {Stdout: "cpu=2\nmemkb=3993600\ndiskbytes=40802189312\n"},
	}))
	rep, err := preflight.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("a host sold as 4 GB / 40 GB must not be refused:\n%v", err)
	}
	res, _ := outcomeOf(rep, preflight.CheckResources)
	if res.Outcome != preflight.Warn {
		t.Errorf("a floor-but-not-recommended host must WARN, not %s: %s", res.Outcome, res.Detail)
	}
	if !strings.Contains(res.Fix, "will proceed") {
		t.Errorf("the warning must say the install proceeds: %q", res.Fix)
	}
}

func TestUndersizedHostIsRefusedWithTheMeasurement(t *testing.T) {
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"MemTotal": {Stdout: "cpu=1\nmemkb=1998848\ndiskbytes=20000000000\n"},
	}))
	rep, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("a 2 GB host must be refused")
	}
	res, _ := outcomeOf(rep, preflight.CheckResources)
	if res.Outcome != preflight.Fail {
		t.Fatalf("want fail, got %s", res.Outcome)
	}
	for _, want := range []string{"floor", "vCPU", "RAM"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("the refusal must show the measurement and the floor, got %q", res.Detail)
		}
	}
}

func TestNonUbuntuIsRefused(t *testing.T) {
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"/etc/os-release": {Stdout: "ID=debian\nVERSION_ID=\"12\"\nPRETTY_NAME=\"Debian GNU/Linux 12\"\n"},
	}))
	_, err := preflight.Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "Debian") {
		t.Fatalf("want a refusal naming what the node runs, got %v", err)
	}
}

func TestExistingKubernetesIsRefused(t *testing.T) {
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"command -v": {Stdout: "k3s\ncontainerd\n"},
	}))
	_, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("a host with an existing cluster must be refused")
	}
	if !strings.Contains(err.Error(), "k3s") || !strings.Contains(err.Error(), "new clusters only") {
		t.Errorf("the refusal must name what it found and why:\n%v", err)
	}
}

// Resume always re-runs preflight, by which time WE have installed k3s. The
// check that forbids adopting someone else's cluster must not refuse our own.
func TestResumeIsNotRefusedByItsOwnK3s(t *testing.T) {
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"command -v": {Stdout: "k3s\ncontainerd\n"},
	}))
	opts.Nodes[0].ExistingK3sIsOurs = true
	rep, err := preflight.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("a resumed install must pass its own k3s:\n%v", err)
	}
	res, _ := outcomeOf(rep, preflight.CheckExistingK8s)
	if !strings.Contains(res.Detail, "resuming") {
		t.Errorf("the pass must say why it passed: %q", res.Detail)
	}
}

func TestNoPasswordlessSudoIsRefusedWithTheRemediation(t *testing.T) {
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"sudo -n true": {ExitCode: 1, Stderr: "sudo: a password is required"},
	}))
	rep, _ := preflight.Run(context.Background(), opts)
	res, _ := outcomeOf(rep, preflight.CheckPrivilege)
	if res.Outcome != preflight.Fail {
		t.Fatalf("want fail, got %s", res.Outcome)
	}
	if !strings.Contains(res.Fix, "passwordless sudo") {
		t.Errorf("the refusal must carry the exact remediation, got %q", res.Fix)
	}
}

func TestBlockedEgressIsRefused(t *testing.T) {
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"curl": {Stdout: "https://ghcr.io/v2/ 000\nhttps://charts.jetstack.io/index.yaml 200\n"},
	}))
	rep, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("a node with no egress to the registry must be refused")
	}
	res, _ := outcomeOf(rep, preflight.CheckEgress)
	if !strings.Contains(res.Detail, "container registry") {
		t.Errorf("the refusal must name which target was unreachable, got %q", res.Detail)
	}
}

// A 401 from a registry is egress working, not egress failing.
func TestAuthenticationChallengeCountsAsReachable(t *testing.T) {
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"curl": {Stdout: "https://ghcr.io/v2/ 401\nhttps://charts.jetstack.io/index.yaml 403\n"},
	}))
	rep, err := preflight.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("an HTTP status means the node reached the host: %v", err)
	}
	res, _ := outcomeOf(rep, preflight.CheckEgress)
	if res.Outcome != preflight.Pass {
		t.Errorf("want pass, got %s: %s", res.Outcome, res.Detail)
	}
}

func TestUnreachableControlPlaneIsRefusedBeforeAnythingElse(t *testing.T) {
	opts := baseOptions(t, healthyHost(nil))
	opts.Catalog = fakeCatalog{err: errors.New("dial tcp: connection refused")}
	rep, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("install requires a control plane and must refuse without one")
	}
	res, _ := outcomeOf(rep, preflight.CheckControlPlane)
	if !strings.Contains(res.Fix, "kubenest login") {
		t.Errorf("the refusal must name the fix, got %q", res.Fix)
	}
}

func TestUnknownBundleAndTierAreRefused(t *testing.T) {
	t.Run("unknown version", func(t *testing.T) {
		opts := baseOptions(t, healthyHost(nil))
		opts.BundleVersion = "9.9"
		_, err := preflight.Run(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "does not offer bundle") {
			t.Fatalf("want a refusal naming the offered versions, got %v", err)
		}
	})
	t.Run("tier the bundle does not offer", func(t *testing.T) {
		opts := baseOptions(t, healthyHost(nil))
		opts.HATier = "ha"
		opts.Nodes = append(opts.Nodes,
			preflight.Node{Address: "10.0.1.11", Role: "server", Runner: &componenttest.FakeRunner{Respond: healthyHost(nil)}},
			preflight.Node{Address: "10.0.1.12", Role: "server", Runner: &componenttest.FakeRunner{Respond: healthyHost(nil)}})
		opts.Catalog = fakeCatalog{entries: []preflight.BundleEntry{{Version: "1.0", HATiers: []string{"single-server"}}}}
		_, err := preflight.Run(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "does not offer the \"ha\" tier") {
			t.Fatalf("want a refusal, got %v", err)
		}
	})
	t.Run("unknown profile", func(t *testing.T) {
		opts := baseOptions(t, healthyHost(nil))
		opts.Profiles = []string{"obervability"}
		_, err := preflight.Run(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "profile named") {
			t.Fatalf("an unknown profile must be rejected, not ignored, got %v", err)
		}
	})
}

func TestHATierNeedsThreeServers(t *testing.T) {
	opts := baseOptions(t, healthyHost(nil))
	opts.HATier = "ha"
	_, err := preflight.Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "three control-plane nodes") {
		t.Fatalf("want a node-count refusal, got %v", err)
	}
}

// Every failing check is reported, not just the first: one re-run should fix
// everything the operator can see.
func TestEveryFailureIsReportedNotJustTheFirst(t *testing.T) {
	opts := baseOptions(t, healthyHost(map[string]sshx.Result{
		"sudo -n true":    {ExitCode: 1, Stderr: "sudo: a password is required"},
		"/etc/os-release": {Stdout: "ID=debian\nVERSION_ID=\"12\"\n"},
		"MemTotal":        {Stdout: "cpu=1\nmemkb=1998848\ndiskbytes=20000000000\n"},
	}))
	rep, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if got := len(rep.Failures()); got < 3 {
		t.Errorf("reported %d failures, want every one of them: %v", got, rep.Failures())
	}
	for _, want := range []string{"Operating system", "Privilege", "Host resources"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the aggregate refusal is missing %q:\n%v", want, err)
		}
	}
}

// An unreachable node fails once, with the fix — not eight times with the
// same connection error.
func TestUnreachableNodeFailsOnceWithTheFix(t *testing.T) {
	opts := baseOptions(t, healthyHost(nil))
	opts.Nodes = append(opts.Nodes, preflight.Node{
		Address: "10.0.1.11", Role: "agent",
		DialErr: errors.New("ssh: handshake failed: unable to authenticate"),
	})
	rep, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("want a refusal")
	}
	var forNode []preflight.Result
	for _, r := range rep.Failures() {
		if r.Node == "10.0.1.11" {
			forNode = append(forNode, r)
		}
	}
	if len(forNode) != 1 {
		t.Fatalf("an unreachable node produced %d failures, want 1: %v", len(forNode), forNode)
	}
	if !strings.Contains(forNode[0].Fix, "--ssh-key") {
		t.Errorf("the failure must name the fix, got %q", forNode[0].Fix)
	}
}
