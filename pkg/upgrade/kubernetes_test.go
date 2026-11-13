package upgrade

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/sshx"

	"gopkg.in/yaml.v3"
)

// agentProbeCmd is the command the agent upgrade probe runs for the operator
// Deployment.
func agentProbeCmd() string {
	return "sudo -n k3s kubectl get deployment " + agent.DeploymentName + " -n kubenest-system -o json"
}

// agentDeploymentJSON is the operator Deployment as kubectl reports it: the
// helm.sh/chart label helm stamped on it, and its generation and replica
// counts. The chart is one replica (spec.replicas 1), so the counts are the
// only thing that says which ReplicaSet's pod is the one running.
func agentDeploymentJSON(chart string, generation, observed int64, replicas, updated, available int32) string {
	labels := ""
	if chart != "" {
		labels = fmt.Sprintf(`"helm.sh/chart":%q`, chart)
	}
	return fmt.Sprintf(`{"metadata":{"labels":{%s},"generation":%d},"spec":{"replicas":1},"status":{"observedGeneration":%d,"replicas":%d,"updatedReplicas":%d,"availableReplicas":%d}}`,
		labels, generation, observed, replicas, updated, available)
}

// The agent upgrade is complete only when the operator runs the new chart.
//
// The defect this replaced: the probe passed as soon as the HelmChart's
// spec.version read the target — which is what the stage itself patched — and
// the Deployment was Available. An upgrade whose only change is the agent
// (bundle 1.0 → 1.1) has nothing else to roll, so it could be recorded
// complete while the old operator was still the one running: Available stays
// True on the previous ReplicaSet during a rolling update, and before k3s's
// helm controller applies anything the old ReplicaSet is the only one there
// is. Every case below is a Deployment that is Available and must still not
// count, and the first one is the regression.
func TestAgentUpgradeWaitsForTheNewChartToRollOut(t *testing.T) {
	const from, to = "2.6.16", "2.6.17"
	newChart := agent.ChartName + "-" + to
	oldChart := agent.ChartName + "-" + from

	cases := []struct {
		name       string
		deployment string
		ready      bool
		status     string
	}{
		{"the old chart is still the one running", agentDeploymentJSON(oldChart, 2, 2, 1, 1, 1), false, "still on chart " + oldChart},
		{"the label is not there at all", agentDeploymentJSON("", 2, 2, 1, 1, 1), false, "no helm.sh/chart label"},
		{"the controller has not observed the new pod template", agentDeploymentJSON(newChart, 2, 1, 1, 1, 1), false, "not observed yet"},
		{"a pod of the previous chart is still running", agentDeploymentJSON(newChart, 2, 2, 2, 1, 1), false, "1 replica(s) of the previous chart still running"},
		{"the new pod is not available yet", agentDeploymentJSON(newChart, 2, 2, 1, 1, 0), false, "1/1 replicas updated, 0 available"},
		{"rolled out on the new chart", agentDeploymentJSON(newChart, 2, 2, 1, 1, 1), true, agent.ChartName + "-" + to},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
				if command != agentProbeCmd() {
					t.Fatalf("unscripted command: %q", command)
				}
				return sshx.Result{Stdout: tc.deployment}, nil
			}}
			ready, state, err := agentUpgradedProbe(r, to)(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if ready != tc.ready {
				t.Errorf("ready = %v (%s), want %v", ready, state, tc.ready)
			}
			if !strings.Contains(state.Status, tc.status) {
				t.Errorf("status = %q, want it to name %q", state.Status, tc.status)
			}
		})
	}
}

// A Deployment whose spec.replicas the API server has not defaulted yet
// carries none at all, and the probe has to read that as the one replica the
// chart asks for rather than as "nothing to wait for".
func TestAgentUpgradeDefaultsToASingleReplica(t *testing.T) {
	body := fmt.Sprintf(`{"metadata":{"labels":{"helm.sh/chart":%q},"generation":2},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"availableReplicas":1}}`,
		agent.ChartName+"-2.6.17")
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if command != agentProbeCmd() {
			t.Fatalf("unscripted command: %q", command)
		}
		return sshx.Result{Stdout: body}, nil
	}}
	ready, state, err := agentUpgradedProbe(r, "2.6.17")(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Errorf("ready = false (%s), want a single unset replica count to mean one", state)
	}
}

// THE AGENT UPGRADE MOVES THE CHART SOURCE, NOT ONLY THE VERSION
// (kn-t72...0xx7.1). A bundle pins its operator as
// <sources.kubenest-agent>:<core.kubenest-agent>, and the two move
// independently: bundle 1.2's chart lives in the candidate channel while every
// released bundle's lives on the stable path. A version-only patch leaves
// spec.chart on the old registry and asks it for a chart version that is not
// there, so the 1.1 -> 1.2 upgrade either fails to pull or pulls a same-named
// chart from a registry 1.2 never named.
//
// Both directions are asserted, because the risk runs both ways: a patch that
// always wrote the candidate source would hand it to clusters that did not ask
// for it.
func TestTheAgentUpgradeMovesTheChartSourceAsWellAsTheVersion(t *testing.T) {
	for _, tc := range []struct {
		bundle      string
		wantChart   string
		wantVersion string
	}{
		{"1.1", "oci://ghcr.io/kubenesthq/charts/kubenest-operator-2", "2.6.17"},
		{"1.2", "oci://ghcr.io/kubenesthq/candidate/kubenest-operator-2", "2.7.0-rc.7"},
	} {
		t.Run(tc.bundle, func(t *testing.T) {
			m, err := bundles.Manifest(tc.bundle)
			if err != nil {
				t.Fatal(err)
			}
			version, err := m.Core.Version("kubenest-agent")
			if err != nil {
				t.Fatal(err)
			}
			if version != tc.wantVersion {
				t.Fatalf("bundle %s pins the agent at %s, want %s", tc.bundle, version, tc.wantVersion)
			}
			r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
				switch {
				case command == "sudo -n cat "+agent.ManifestPath:
					return sshx.Result{Stdout: agentManifest11}, nil
				case strings.Contains(command, agent.ManifestPath+".tmp"),
					command == "sudo -n chmod 600 "+agent.ManifestPath:
					return sshx.Result{}, nil
				case command == agentProbeCmd():
					return sshx.Result{Stdout: agentDeploymentJSON(agent.ChartName+"-"+version, 2, 2, 1, 1, 1)}, nil
				default:
					t.Fatalf("unscripted command: %q", command)
					return sshx.Result{}, nil
				}
			}}

			if err := upgradeAgentChart(context.Background(), r, m, version, converge.NewTextReporter(io.Discard)); err != nil {
				t.Fatalf("upgrading the agent to bundle %s: %v", tc.bundle, err)
			}

			// THE FILE IS WHAT MOVES (kn-oymc): k3s re-applies its manifest
			// on every start, so a patched HelmChart is put back by the
			// Kubernetes stage's own restart.
			inputs := r.Inputs()
			if len(inputs) != 1 {
				t.Fatalf("the agent upgrade wrote %d manifest(s), want the agent's manifest rewritten once; it ran:\n%s", len(inputs), strings.Join(r.Commands(), "\n"))
			}
			var written struct {
				Spec struct {
					Chart         string `yaml:"chart"`
					Version       string `yaml:"version"`
					ValuesContent string `yaml:"valuesContent"`
				} `yaml:"spec"`
			}
			if err := yaml.Unmarshal(inputs[0], &written); err != nil {
				t.Fatal(err)
			}
			if written.Spec.Chart != tc.wantChart || written.Spec.Version != tc.wantVersion {
				t.Errorf("the manifest names %s:%s, want %s:%s", written.Spec.Chart, written.Spec.Version, tc.wantChart, tc.wantVersion)
			}
			if written.Spec.ValuesContent != agentValues11 {
				t.Errorf("the agent's values changed:\n%s\nwant\n%s", written.Spec.ValuesContent, agentValues11)
			}
			for _, command := range r.Commands() {
				if strings.Contains(command, "patch helmchart") {
					t.Errorf("the upgrade patched the HelmChart object, which k3s's next start reverts: %s", command)
				}
			}
		})
	}
}

// agentValues11 and agentManifest11 are the agent manifest a 1.1 install writes,
// with placeholder credentials.
const agentValues11 = "kubenest:\n  jwtSecret: placeholder-jwt\n  clusterId: 01a1a9c1-0000-0000-0000-000000000000\n"

var agentManifest11 = "apiVersion: helm.cattle.io/v1\nkind: HelmChart\nmetadata:\n  name: operator\n  namespace: kube-system\nspec:\n" +
	"  chart: oci://ghcr.io/kubenesthq/charts/kubenest-operator-2\n  version: 2.6.17\n  targetNamespace: kubenest-system\n" +
	"  failurePolicy: abort\n  valuesContent: |\n    kubenest:\n      jwtSecret: placeholder-jwt\n      clusterId: 01a1a9c1-0000-0000-0000-000000000000\n"

// A ROLLBACK PUTS THE OPERATOR BACK (kn-oymc). The agent is its own upgrade
// stage, so the components revert never touched it, and k3s re-applies the
// agent's manifest file on every start: a rollback that left the file at the
// target bundle left a cluster recorded as 1.1 running the 1.2 operator.
func TestARollbackRewritesTheAgentManifestToTheStartingBundle(t *testing.T) {
	from, err := bundles.Manifest("1.1")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := from.Core.Version("kubenest-agent")
	movedTo12 := strings.Replace(strings.Replace(agentManifest11,
		"oci://ghcr.io/kubenesthq/charts/", "oci://ghcr.io/kubenesthq/candidate/", 1), "version: 2.6.17", "version: 2.7.0-rc.7", 1)
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case command == "sudo -n cat "+agent.ManifestPath:
			return sshx.Result{Stdout: movedTo12}, nil
		case strings.Contains(command, agent.ManifestPath+".tmp"),
			command == "sudo -n chmod 600 "+agent.ManifestPath:
			return sshx.Result{}, nil
		case command == agentProbeCmd():
			return sshx.Result{Stdout: agentDeploymentJSON(agent.ChartName+"-"+want, 2, 2, 1, 1, 1)}, nil
		}
		t.Fatalf("unscripted command: %q", command)
		return sshx.Result{}, nil
	}}

	if err := revertAgent(context.Background(), r, from, converge.NewTextReporter(io.Discard), func(string, ...any) {}); err != nil {
		t.Fatalf("reverting the agent: %v", err)
	}
	inputs := r.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("the revert wrote %d manifest(s), want the agent's manifest rewritten once", len(inputs))
	}
	var written struct {
		Spec struct {
			Chart   string `yaml:"chart"`
			Version string `yaml:"version"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(inputs[0], &written); err != nil {
		t.Fatal(err)
	}
	if written.Spec.Version != want || written.Spec.Chart != "oci://ghcr.io/kubenesthq/charts/kubenest-operator-2" {
		t.Errorf("the reverted manifest names %s:%s, want bundle 1.1's stable chart at %s", written.Spec.Chart, written.Spec.Version, want)
	}
}
