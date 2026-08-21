package k3s_test

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/k3s"
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

const readyNode = `{"items":[{"metadata":{"name":"node-1"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`

// The three flags are not style. --cluster-init is decision A (embedded etcd
// on every tier), --disable traefik is D9, and --disable local-storage is the
// one that keeps k3s's local-path from becoming a second default
// StorageClass beside kubenest-local — which makes PVC binding
// order-dependent and was observed for real on the lab host.
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
	for _, c := range fake.Commands() {
		if strings.Contains(c, token) {
			t.Fatalf("the cluster token appeared verbatim in a command: %s", c)
		}
		if strings.Contains(c, "get.k3s.io") && !strings.Contains(c, "--token-file") {
			t.Errorf("a joining server must use --token-file: %s", c)
		}
	}
	var removed bool
	for _, c := range fake.Commands() {
		if strings.Contains(c, "rm -f /etc/rancher/kubenest-join-token") {
			removed = true
		}
	}
	if !removed {
		t.Error("the staged token file must be removed once k3s has its own copy")
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
