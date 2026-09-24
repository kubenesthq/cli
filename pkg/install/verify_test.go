package install

import (
	"io"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/manifest"
)

func verifySession(t *testing.T) *Session {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\nlimits:\n  timeouts:\n    install-total: 30m\n    component-ready: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &Session{ID: "run-1", Bundle: m, Out: io.Discard}
}

// The agent's row in the record check used to be derived from the minted
// credentials, so a run without them — a resumed install whose agent was
// already installed and holding credentials from an earlier process, which is
// exactly when the register stage does not re-mint — silently dropped the agent
// from the comparison and still reported a pass. A check that quietly stops
// checking is worse than one that fails.
func TestTheRecordCheckCoversTheAgentWithOrWithoutCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds *api.AgentCredentials
	}{
		{"without credentials", nil},
		{"with credentials", agentCredentialsForTest()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := verifySession(t)
			s.Creds = tc.creds

			got, ok := chartComponents(s)["kubenest-agent"]
			if !ok {
				t.Fatal("the record check does not cover kubenest-agent")
			}
			// It must be the HelmChart RESOURCE name, because that is what
			// installedChartVersions keys by — matching on anything else would
			// report that nothing on the cluster installs the agent.
			if got != agent.ReleaseName() {
				t.Errorf("the agent is matched on %q, but the HelmChart resource is named %q", got, agent.ReleaseName())
			}
		})
	}
}

// agentCredentialsForTest is a minted-credentials value carrying only the
// non-secret fields this test reads. api.Secret refuses to be written down at
// all, so there is nothing here that could become a credential by accident.
func agentCredentialsForTest() *api.AgentCredentials {
	return &api.AgentCredentials{
		ClusterID: "cluster-1",
		Operator:  api.OperatorInstallInfo{Namespace: "kubenest-system", ChartRef: "oci://example.invalid/chart:1", CreatesWorkloadApplications: true},
	}
}
