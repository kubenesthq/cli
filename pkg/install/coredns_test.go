package install_test

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// coreDNSInstallSession is an install of `nodes` server nodes (the `ha` tier
// shape) with one scripted runner, carrying the bundle timeouts every real
// bundle carries: the cluster-DNS step waits on component-ready, and a missing
// timeout is an error rather than a default (the CLI's third invariant).
func coreDNSInstallSession(t *testing.T, nodes int, runner *componenttest.FakeRunner) *install.Session {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.1\"\nlimits:\n  timeouts:\n    install-total: 30m\n    component-ready: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	servers := make([]string, 0, nodes)
	for i := 0; i < nodes; i++ {
		servers = append(servers, fmt.Sprintf("10.0.1.%d", 10+i))
	}
	opts := install.Options{Bundle: "1.1", Name: "prod-1", Servers: servers, HATier: "ha"}
	j, err := install.OpenJournal(filepath.Join(t.TempDir(), "journal.json"), opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	s := &install.Session{ID: "run-dns", Opts: opts, Bundle: m, Jnl: j, Emit: &recorder{}, Out: io.Discard}
	for _, addr := range servers {
		s.Nodes = append(s.Nodes, install.Node{Address: addr, Role: install.RoleServer, Runner: runner})
	}
	return s
}

// runPlannedStage finds the named stage in the plan the installer would
// actually run, and runs it. Driving the plan rather than calling a stage
// function is the point: it fails if the step is not wired into the install.
func runPlannedStage(t *testing.T, s *install.Session, name string) error {
	t.Helper()
	for _, stage := range install.Plan(s) {
		if stage.Name == name {
			return stage.Run(context.Background())
		}
	}
	t.Fatalf("the plan has no %q stage, so nothing in the install does what that stage is for", name)
	return nil
}

// corednsSpreadJSON is what kubectl returns for the CoreDNS pods once the
// second replica is up on a different node.
const corednsSpreadJSON = `{"items":[
  {"metadata":{"name":"coredns-aaaa"},"spec":{"nodeName":"kubenest-lab-w3-1"},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
  {"metadata":{"name":"coredns-bbbb"},"spec":{"nodeName":"kubenest-lab-w3-2"},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}
]}`

// A cluster of three nodes must leave the install with CoreDNS on two of them.
//
// One replica is k3s's default and it is the defect (kn-t43): the node hosting
// it stopping takes every in-cluster name lookup with it until Kubernetes
// evicts the pod, which is long enough for a Velero restore to fail on its
// backup target and for the next backup to report the location Unavailable.
func TestAMultiNodeInstallPutsCoreDNSOnMoreThanOneNode(t *testing.T) {
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

	s := coreDNSInstallSession(t, 3, runner)
	if err := runPlannedStage(t, s, install.StageK3sAgents); err != nil {
		t.Fatalf("%s: %v", install.StageK3sAgents, err)
	}

	var patch *componenttest.Execution
	for i, e := range runner.Executions() {
		if strings.Contains(e.Command, "patch deployment coredns") {
			patch = &runner.Executions()[i]
		}
	}
	if patch == nil {
		t.Fatalf("a three-node install never set the CoreDNS replica count, so the cluster keeps k3s's single replica and loses DNS with whichever node hosts it. Commands run: %v", runner.Commands())
	}
	if !strings.Contains(string(patch.Stdin), `"replicas":2`) {
		t.Errorf("the patch does not ask for two replicas: %s", patch.Stdin)
	}
	if !strings.Contains(string(patch.Stdin), `"topologyKey":"kubernetes.io/hostname"`) {
		t.Errorf("the patch sets no one-pod-per-node spread, so both replicas may land on the node that is lost: %s", patch.Stdin)
	}
}

// The boundary of the rule: a single-node cluster keeps k3s's one replica.
//
// There is no other node for a second one to be scheduled on — k3s's own
// packaged manifest spreads these pods one per hostname with DoNotSchedule — so
// patching there would only leave a Pending pod behind. This is the planted
// negative for the rule: it fails if the step is ever made unconditional.
func TestASingleNodeInstallLeavesCoreDNSAlone(t *testing.T) {
	runner := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		t.Fatalf("a one-node install must not touch cluster DNS, and it ran %q", cmd)
		return sshx.Result{}, nil
	}}

	s := coreDNSInstallSession(t, 1, runner)
	if err := runPlannedStage(t, s, install.StageK3sAgents); err != nil {
		t.Fatalf("%s: %v", install.StageK3sAgents, err)
	}
	if cmds := runner.Commands(); len(cmds) != 0 {
		t.Errorf("a one-node install ran %v; k3s's single replica is already what that cluster can schedule", cmds)
	}
}
