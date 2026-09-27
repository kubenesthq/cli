package upgrade

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
)

// corednsUpgradeSession is an upgrade of `nodes` control-plane nodes from one
// pinned k3s version to another, with the bundle timeouts a real bundle
// carries — the Kubernetes stage takes its per-node deadline from the manifest
// and the cluster-DNS step waits on component-ready.
func corednsUpgradeSession(t *testing.T, from, to string, nodes int, runner k3s.Runner) *Session {
	t.Helper()
	pins := func(version string) string {
		return "bundle: \"1.2\"\ncore:\n  k3s: " + version +
			"\nlimits:\n  timeouts:\n    upgrade-per-node: 30m\n    component-ready: 10m\n"
	}
	s := &Session{
		ID:   "run-1",
		From: parseManifest(t, pins(from)),
		To:   parseManifest(t, pins(to)),
		Out:  io.Discard,
	}
	for i := 0; i < nodes; i++ {
		s.Nodes = append(s.Nodes, Node{Address: fmt.Sprintf("10.0.1.%d", 10+i), Server: true, Runner: runner})
	}
	return s
}

// runUpgradePlannedStage finds the named stage in the plan the upgrade would
// actually run and runs it: the test fails if the step is not wired in.
func runUpgradePlannedStage(t *testing.T, s *Session, name string) error {
	t.Helper()
	for _, stage := range Plan(s) {
		if stage.Name == name {
			return stage.Run(context.Background())
		}
	}
	t.Fatalf("the plan has no %q stage", name)
	return nil
}

// k3sNodesJSON is `kubectl get nodes -o json` for n control-plane nodes on
// `version`, Ready and uncordoned.
func k3sNodesJSON(version string, n int) string {
	items := make([]string, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, fmt.Sprintf(
			`{"metadata":{"name":"kubenest-lab-w3-%d","labels":{"node-role.kubernetes.io/control-plane":""}},`+
				`"spec":{"unschedulable":false},`+
				`"status":{"nodeInfo":{"kubeletVersion":%q},"conditions":[{"type":"Ready","status":"True"}]}}`,
			i+1, version))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// corednsSpreadJSON is the CoreDNS pods once the second replica is up on
// another node.
const corednsSpreadJSON = `{"items":[
  {"metadata":{"name":"coredns-aaaa"},"spec":{"nodeName":"kubenest-lab-w3-1"},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
  {"metadata":{"name":"coredns-bbbb"},"spec":{"nodeName":"kubenest-lab-w3-2"},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}
]}`

// An upgrade that moves Kubernetes must end with the CoreDNS replica count
// re-asserted, and re-asserted AFTER the plan that moves the nodes.
//
// The k3s stage is the one operation that re-applies every packaged manifest —
// each server rewrites them at start-up and the deploy controller's first pass
// applies them with the checksum comparison off — so whatever a k3s release
// does to the CoreDNS Deployment, the upgrade that moved it puts back. Without
// this, a cluster could be upgraded onto a k3s release whose manifest declares
// spec.replicas and silently return to DNS on one node, which is the defect at
// kn-t43.
func TestAnUpgradeReassertsCoreDNSAfterTheKubernetesStage(t *testing.T) {
	const from, to = "v1.35.7+k3s1", "v1.35.8+k3s1"
	runner := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		switch {
		case strings.Contains(cmd, k3s.ManifestDir):
			return sshx.Result{}, nil
		case strings.Contains(cmd, "get nodes -o json"):
			return sshx.Result{Stdout: k3sNodesJSON(to, 3)}, nil
		case strings.Contains(cmd, "get pods -n kube-system -l k8s-app=kube-dns -o json"):
			return sshx.Result{Stdout: corednsSpreadJSON}, nil
		case strings.Contains(cmd, "patch deployment coredns"):
			return sshx.Result{}, nil
		}
		t.Fatalf("unscripted command: %q", cmd)
		return sshx.Result{}, nil
	}}

	s := corednsUpgradeSession(t, from, to, 3, runner)
	if err := runUpgradePlannedStage(t, s, StageKubernetes); err != nil {
		t.Fatalf("%s: %v", StageKubernetes, err)
	}

	execs := runner.Executions()
	planAt, patchAt := -1, -1
	for i, e := range execs {
		if strings.Contains(e.Command, "kubenest-k3s-server") {
			planAt = i
		}
		if strings.Contains(e.Command, "patch deployment coredns") {
			patchAt = i
		}
	}
	if planAt < 0 {
		t.Fatalf("the Kubernetes stage wrote no plan, so nothing in this test moved a node: %v", runner.Commands())
	}
	if patchAt < 0 {
		t.Fatalf("Kubernetes moved and the upgrade never re-asserted the CoreDNS replica count, so a k3s release that declared spec.replicas would leave this cluster with DNS on one node. Commands: %v", runner.Commands())
	}
	if patchAt < planAt {
		t.Errorf("the CoreDNS patch ran at position %d, before the k3s plan at %d: the re-assert has to come after the last thing that could revert it", patchAt, planAt)
	}
	if !strings.Contains(string(execs[patchAt].Stdin), `"replicas":2`) {
		t.Errorf("the re-asserted patch does not ask for two replicas: %s", execs[patchAt].Stdin)
	}
}

// It runs even when the Kubernetes pin does not move: the stage is where an
// upgrade asserts what it moved, and the cluster's own record can have gone
// stale in the meantime — a node removed, a replica scaled back by hand.
func TestAnUpgradeReassertsCoreDNSEvenWhenKubernetesDoesNotMove(t *testing.T) {
	const pin = "v1.35.8+k3s1"
	runner := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		switch {
		case strings.Contains(cmd, "get pods -n kube-system -l k8s-app=kube-dns -o json"):
			return sshx.Result{Stdout: corednsSpreadJSON}, nil
		case strings.Contains(cmd, "patch deployment coredns"):
			return sshx.Result{}, nil
		}
		t.Fatalf("unscripted command: %q", cmd)
		return sshx.Result{}, nil
	}}

	s := corednsUpgradeSession(t, pin, pin, 3, runner)
	if err := runUpgradePlannedStage(t, s, StageKubernetes); err != nil {
		t.Fatalf("%s: %v", StageKubernetes, err)
	}
	if cmds := runner.Commands(); len(cmds) != 2 {
		t.Fatalf("an unchanged Kubernetes pin produced %v; want exactly the CoreDNS patch and the pod list", cmds)
	}
}

// The boundary of the rule, as at install: one node keeps one replica, because
// a second one has no other node to be scheduled on.
func TestASingleNodeUpgradeLeavesCoreDNSAlone(t *testing.T) {
	const pin = "v1.35.8+k3s1"
	runner := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		t.Fatalf("a one-node upgrade must not touch cluster DNS, and it ran %q", cmd)
		return sshx.Result{}, nil
	}}

	s := corednsUpgradeSession(t, pin, pin, 1, runner)
	if err := runUpgradePlannedStage(t, s, StageKubernetes); err != nil {
		t.Fatalf("%s: %v", StageKubernetes, err)
	}
	if cmds := runner.Commands(); len(cmds) != 0 {
		t.Errorf("a one-node upgrade ran %v; k3s's single replica is already what that cluster can schedule", cmds)
	}
}
