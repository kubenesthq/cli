package upgrade

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
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
