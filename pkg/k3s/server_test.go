package k3s_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/leakscan"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

func bundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\ncore:\n  k3s: v1.35.7+k3s1\nlimits:\n  timeouts:\n    node-ready: 2s\n"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// tokenFilePath is where the join credential lives on a node.
const tokenFilePath = "/etc/rancher/kubenest-join-token"

const readyNode = `{"items":[{"metadata":{"name":"node-1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`

// The flags are not style. --cluster-init is decision A (embedded etcd on
// every tier), --disable traefik is D9, --disable local-storage is the one
// that keeps k3s's local-path from becoming a second default StorageClass
// beside kubenest-local — which makes PVC binding order-dependent and was
// observed for real on the lab host — and --secrets-encryption is what keeps
// a datastore snapshot in the customer's bucket from carrying every Secret in
// plaintext (its own positive test is TestServerFlagsEnableSecretsEncryption).
func TestServerInstallUsesTheCanonicalFlagSet(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		switch {
		case strings.Contains(cmd, "command -v k3s"):
			return sshx.Result{}, nil // not installed
		case strings.Contains(cmd, "get nodes"):
			return sshx.Result{Stdout: readyNode}, nil
		}
		return sshx.Result{}, nil
	}}
	if err := k3s.InstallServer(context.Background(), fake, bundle(t), k3s.ServerOptions{}, nil); err != nil {
		t.Fatal(err)
	}

	var install string
	for _, c := range fake.Commands() {
		if strings.Contains(c, "get.k3s.io") {
			install = c
		}
	}
	if install == "" {
		t.Fatal("no k3s install command was run")
	}
	for _, want := range []string{
		"INSTALL_K3S_VERSION='v1.35.7+k3s1'", // pinned by the bundle, never latest
		"server",
		"--cluster-init",
		"--disable traefik",
		"--disable local-storage",
	} {
		if !strings.Contains(install, want) {
			t.Errorf("the k3s install command is missing %q:\n  %s", want, install)
		}
	}
}

// Secrets encryption is what keeps a datastore snapshot in the customer's
// bucket from being a copy of every Kubernetes Secret in plaintext — the
// bucket credentials among them. The set is asserted through ServerFlags(),
// the accessor, so the test tracks the actual flag set rather than a literal
// copied beside it.
func TestServerFlagsEnableSecretsEncryption(t *testing.T) {
	var enabled bool
	for _, f := range k3s.ServerFlags() {
		if f == "--secrets-encryption" {
			enabled = true
		}
	}
	if !enabled {
		t.Fatalf("ServerFlags() must carry --secrets-encryption, got %v", k3s.ServerFlags())
	}

	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		switch {
		case strings.Contains(cmd, "command -v k3s"):
			return sshx.Result{}, nil // not installed
		case strings.Contains(cmd, "get nodes"):
			return sshx.Result{Stdout: readyNode}, nil
		}
		return sshx.Result{}, nil
	}}
	if err := k3s.InstallServer(context.Background(), fake, bundle(t), k3s.ServerOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	var install string
	for _, c := range fake.Commands() {
		if strings.Contains(c, "get.k3s.io") {
			install = c
		}
	}
	if install == "" {
		t.Fatal("no k3s install command was run")
	}
	if !strings.Contains(install, "--secrets-encryption") {
		t.Errorf("the first server's install must encrypt Secrets at rest:\n  %s", install)
	}
}

// A resumed install finds k3s already there at the pinned version and leaves
// it alone rather than reinstalling.
func TestServerInstallIsIdempotentAtThePinnedVersion(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		switch {
		case strings.Contains(cmd, "command -v k3s"):
			return sshx.Result{Stdout: "k3s version v1.35.7+k3s1 (0123abcd)\n"}, nil
		case strings.Contains(cmd, "get nodes"):
			return sshx.Result{Stdout: readyNode}, nil
		}
		return sshx.Result{}, nil
	}}
	if err := k3s.InstallServer(context.Background(), fake, bundle(t), k3s.ServerOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.Commands() {
		if strings.Contains(c, "get.k3s.io") {
			t.Fatalf("re-ran the k3s installer on a node already at the pinned version: %s", c)
		}
	}
}

// A different k3s version is an upgrade, and an install must not do one
// silently.
func TestServerInstallRefusesADifferentVersion(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "command -v k3s") {
			return sshx.Result{Stdout: "k3s version v1.36.3+k3s1 (0123abcd)\n"}, nil
		}
		return sshx.Result{}, nil
	}}
	err := k3s.InstallServer(context.Background(), fake, bundle(t), k3s.ServerOptions{}, nil)
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"v1.36.3+k3s1", "v1.35.7+k3s1", "upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name both versions and the right command, missing %q: %v", want, err)
		}
	}
}

// The cluster token is a credential: it reaches the target as a root-only
// file, never as a command-line argument, because command lines are visible
// in the target host's process list.
func TestJoinTokenNeverAppearsOnACommandLine(t *testing.T) {
	const token = "K10secret::server:tokenvalue"
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		switch {
		case strings.Contains(cmd, "command -v k3s"):
			return sshx.Result{}, nil
		case strings.Contains(cmd, "get nodes"):
			return sshx.Result{Stdout: readyNode}, nil
		}
		return sshx.Result{}, nil
	}}
	err := k3s.InstallServer(context.Background(), fake, bundle(t),
		k3s.ServerOptions{JoinURL: "https://10.0.1.10:6443", Token: token}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// RecoverableFrom, not strings.Contains. The verbatim check this replaces
	// passed for months while writeTokenFile base64-encoded the token onto the
	// command line under a comment asserting the value "never appears as a
	// command argument" — the plaintext genuinely was absent, and the token was
	// one `base64 -d` away for anyone with a shell on the host (kn-40rd).
	for _, c := range fake.Commands() {
		if leakscan.RecoverableFrom(c, token) {
			t.Fatalf("the cluster token is recoverable from a command line: %s", c)
		}
		if strings.Contains(c, "get.k3s.io") && !strings.Contains(c, "--token-file") {
			t.Errorf("a joining server must use --token-file: %s", c)
		}
	}

	// Positive control: the token must actually have been delivered, over
	// stdin. Without this the check above passes just as well if the token is
	// never sent at all, which is a green earned by doing nothing.
	var streamed bool
	for _, in := range fake.Inputs() {
		if string(in) == token {
			streamed = true
		}
	}
	if !streamed {
		t.Error("the token reached the host in no stdin payload; the leak check above proves nothing")
	}
	// And it must NOT be removed. k3s bakes --token-file into the systemd
	// unit, so the file is read on every start of the service, not only at
	// join. Removing it leaves a node that works until its first restart and
	// then hangs forever waiting for a file that no longer exists — observed
	// on a real cluster during an upgrade, and it would happen on any reboot.
	for _, c := range fake.Commands() {
		if strings.Contains(c, "rm -f") && strings.Contains(c, tokenFilePath) {
			t.Errorf("the token file must persist: k3s reads it on every restart, not only at join:\n  %s", c)
		}
	}
	var staged bool
	for _, c := range fake.Commands() {
		if strings.Contains(c, "install -m 0600") && strings.Contains(c, tokenFilePath) {
			staged = true
		}
	}
	if !staged {
		t.Error("the token must be staged as a root-only 0600 file")
	}
}

// The first server is the cluster's token source: it must mint one, stage it
// root-only, and pass --token-file, exactly like a joining node. k3s writes
// its own node-token only once it has started, so a first server with no
// token staged leaves the rest of the cluster nothing to authenticate
// against until that node is up — and because k3s bakes --token-file into
// the systemd unit, the file is read at every start, not only at first boot.
func TestFirstServerGetsTheGeneratedTokenFile(t *testing.T) {
	tokenShape := regexp.MustCompile(`^K10[0-9a-f]{40}::server:[0-9a-f]{40}$`)

	fresh := func() *componenttest.FakeRunner {
		return &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
			switch {
			case strings.Contains(cmd, "command -v k3s"):
				return sshx.Result{}, nil // not installed
			case strings.Contains(cmd, "get nodes"):
				return sshx.Result{Stdout: readyNode}, nil
			}
			return sshx.Result{}, nil
		}}
	}

	fake := fresh()
	if err := k3s.InstallServer(context.Background(), fake, bundle(t), k3s.ServerOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	var install string
	for _, c := range fake.Commands() {
		if strings.Contains(c, "get.k3s.io") {
			install = c
		}
	}
	if install == "" {
		t.Fatal("no k3s install command was run")
	}
	for _, want := range []string{"--token-file", "--secrets-encryption"} {
		if !strings.Contains(install, want) {
			t.Errorf("the first server's install is missing %q:\n  %s", want, install)
		}
	}
	if strings.Contains(install, "--server ") {
		t.Errorf("the first server initialises the cluster, it does not join one:\n  %s", install)
	}

	// The token must actually have been delivered, over stdin, to a root-only
	// file. Without a non-empty payload the leak check below proves nothing:
	// it passes just as well if no token is sent at all.
	var staged []byte
	for _, e := range fake.Executions() {
		if strings.Contains(e.Command, "install -m 0600") && strings.Contains(e.Command, tokenFilePath) {
			staged = e.Stdin
		}
	}
	if len(staged) == 0 {
		t.Fatal("the first server's token was never streamed to a root-only file")
	}
	token := string(staged)
	if !tokenShape.MatchString(token) {
		t.Errorf("the generated token is not k3s-shaped (want K10<40 hex>::server:<40 hex>): %q", token)
	}
	// And it must never appear in a command string: command lines are visible
	// in the target host's process list, and an encoding is not a concealment
	// (the kn-40rd shape).
	for _, c := range fake.Commands() {
		if strings.Contains(c, token) || leakscan.RecoverableFrom(c, token) {
			t.Fatalf("the generated cluster token is recoverable from a command line: %s", c)
		}
	}

	// And it must persist, exactly as on a joiner: k3s bakes --token-file into
	// the systemd unit, so the file is read at every start of the service. A
	// node whose token file was cleaned up works until its first restart and
	// then hangs forever waiting for it.
	for _, c := range fake.Commands() {
		if strings.Contains(c, "rm ") && strings.Contains(c, tokenFilePath) {
			t.Errorf("the first server's token file must persist: k3s reads it on every restart:\n  %s", c)
		}
	}

	// A caller that already minted the install plan's one token gets THAT
	// value on the first server too, not a second generated one — otherwise
	// the join token the plan derived for the other servers does not match.
	const explicit = "K10aaa::server:bbb"
	fake2 := fresh()
	if err := k3s.InstallServer(context.Background(), fake2, bundle(t), k3s.ServerOptions{Token: explicit}, nil); err != nil {
		t.Fatal(err)
	}
	var streamed bool
	for _, in := range fake2.Inputs() {
		if string(in) == explicit {
			streamed = true
		}
	}
	if !streamed {
		t.Error("an explicit token must be staged verbatim; a fresh one must not be generated over it")
	}

	// Two mints must differ. A token is a credential and it is minted on
	// operator laptops with no shared state: a repeatable sequence would let
	// one cluster's token predict another's.
	first, err := k3s.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := k3s.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("two generated cluster tokens are identical")
	}
	if !tokenShape.MatchString(first) || !tokenShape.MatchString(second) {
		t.Errorf("generated tokens are not k3s-shaped: %q, %q", first, second)
	}
}

func TestAgentJoinsWithServerAndToken(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		return sshx.Result{}, nil
	}}
	if err := k3s.InstallAgent(context.Background(), fake, bundle(t), "https://10.0.1.10:6443", "K10token", nil); err != nil {
		t.Fatal(err)
	}
	var install string
	for _, c := range fake.Commands() {
		if strings.Contains(c, "get.k3s.io") {
			install = c
		}
	}
	for _, want := range []string{"sh -s - agent", "--server https://10.0.1.10:6443", "--token-file"} {
		if !strings.Contains(install, want) {
			t.Errorf("the agent install is missing %q:\n  %s", want, install)
		}
	}
	if strings.Contains(install, "--cluster-init") {
		t.Errorf("an agent is not a server: %s", install)
	}
}

// An agent is not a server: secrets encryption is an etcd-member flag, set
// by the servers that own the datastore, so it has no place in an agent's
// install command. A standalone agent node has no datastore to encrypt.
func TestAgentFlagsCarryNoSecretsEncryption(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		return sshx.Result{}, nil
	}}
	if err := k3s.InstallAgent(context.Background(), fake, bundle(t), "https://10.0.1.10:6443", "K10token", nil); err != nil {
		t.Fatal(err)
	}
	var install string
	for _, c := range fake.Commands() {
		if strings.Contains(c, "get.k3s.io") {
			install = c
		}
	}
	if install == "" {
		t.Fatal("no k3s agent install command was run")
	}
	if strings.Contains(install, "--secrets-encryption") {
		t.Errorf("an agent is not a server: --secrets-encryption is cluster-wide from the etcd members and must not be in the agent install:\n  %s", install)
	}
}

// Every acceptance check is a convergence check: a node that is not Ready yet
// is an observation, and only the deadline fails it.
func TestNodesReadyWaitsRatherThanSampling(t *testing.T) {
	var calls int
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if !strings.Contains(cmd, "get nodes") {
			return sshx.Result{}, nil
		}
		calls++
		if calls < 2 {
			// The API server is not up yet — transient, not a verdict.
			return sshx.Result{ExitCode: 1, Stderr: "The connection to the server localhost:8080 was refused"}, nil
		}
		return sshx.Result{Stdout: readyNode}, nil
	}}
	m := bundle(t)
	if err := k3s.WaitNodesReady(context.Background(), fake, m, 1, nil); err != nil {
		t.Fatalf("a briefly unreachable API server must not fail the wait: %v", err)
	}
	if calls < 2 {
		t.Errorf("the check sampled once (%d calls) instead of converging", calls)
	}
}

func TestNodesReadyFailsWithTheStuckNodeNamed(t *testing.T) {
	const notReady = `{"items":[{"metadata":{"name":"node-2"},"status":{"conditions":[{"type":"Ready","status":"False","reason":"KubeletNotReady","message":"container runtime network not ready: cni plugin not initialized"}]}}]}`
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "get nodes") {
			return sshx.Result{Stdout: notReady}, nil
		}
		return sshx.Result{}, nil
	}}
	err := k3s.WaitNodesReady(context.Background(), fake, bundle(t), 1, nil)
	if err == nil {
		t.Fatal("want a failure after the deadline")
	}
	for _, want := range []string{"node-2", "KubeletNotReady", "cni plugin not initialized"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must name the stuck node and the reason, missing %q: %v", want, err)
		}
	}
}

// The inventory records a host's Node UID, and it has to come from the cluster
// and match on the ADDRESS (kn-t50): the installer knows a machine by the
// address the operator gave it, the Node object knows itself by its hostname
// and its internal IP, and a host whose UID comes back empty is one a later
// node operation misreads as "no such node".
func TestNodeUIDsAreReadByEveryAddressTheClusterKnows(t *testing.T) {
	const nodes = `{"items":[
	  {"metadata":{"name":"cp-1","uid":"uid-cp-1"},
	   "status":{"addresses":[{"type":"Hostname","address":"cp-1"},{"type":"InternalIP","address":"10.0.1.10"}]}},
	  {"metadata":{"name":"worker-1","uid":"uid-worker-1"},
	   "status":{"addresses":[{"type":"InternalIP","address":"10.0.1.20"}]}},
	  {"metadata":{"name":"no-uid-yet"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.1.30"}]}}
	]}`
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "get nodes -o json") {
			return sshx.Result{Stdout: nodes}, nil
		}
		return sshx.Result{ExitCode: 127}, nil
	}}

	uids, err := k3s.NodeUIDsByAddress(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	for address, want := range map[string]string{
		"cp-1":      "uid-cp-1",
		"10.0.1.10": "uid-cp-1",
		"10.0.1.20": "uid-worker-1",
	} {
		if uids[address] != want {
			t.Errorf("UID for %s = %q, want %q", address, uids[address], want)
		}
	}
	// A Node object with no UID contributes nothing rather than an empty
	// string that reads as a recorded UID.
	if uid, ok := uids["10.0.1.30"]; ok {
		t.Errorf("a node with no UID answered %q for its address, want no answer", uid)
	}
}
