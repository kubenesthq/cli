package install

import (
	"context"
	"io"
	"strings"
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

// Acceptance check 4 asks whether the cluster is MANAGED, and a standalone
// cluster has nothing to be managed by. The one thing it must not do is
// quietly succeed: an install that skipped a check has not passed it.
func TestStandaloneClusterCheckDoesNotSilentlyPass(t *testing.T) {
	s := verifySession(t) // no API client, so standalone

	err := verifyClusterReportsIn(context.Background(), s)
	if err == nil {
		t.Fatal("acceptance check 4 reported a pass on a cluster with nothing to report to")
	}
	if !strings.Contains(err.Error(), "kn-sf17") {
		t.Errorf("the refusal does not name the bead that lands the replacement: %v", err)
	}
}

// The agent's row in the record check used to be derived from the minted
// credentials, so every install without them — every standalone one — dropped
// the agent from the comparison and still reported a pass. A check that
// quietly stops checking is worse than one that fails.
func TestTheRecordCheckCoversTheAgentWithoutCredentials(t *testing.T) {
	s := verifySession(t)
	if s.Creds != nil {
		t.Fatal("this test is about the no-credentials case")
	}

	components := chartComponents(s)
	got, ok := components["kubenest-agent"]
	if !ok {
		t.Fatal("the record check does not cover kubenest-agent when no credentials were minted")
	}
	// It must be the HelmChart RESOURCE name, because that is what
	// installedChartVersions keys by — matching on anything else would
	// report that nothing on the cluster installs the agent.
	if got != agent.ReleaseName() {
		t.Errorf("the agent is matched on %q, but the HelmChart resource is named %q", got, agent.ReleaseName())
	}
}

// And with credentials it must still be the same row: the two modes compare
// the same object, or the record check means different things in each.
func TestTheRecordCheckNamesTheSameAgentObjectInBothModes(t *testing.T) {
	without := chartComponents(verifySession(t))["kubenest-agent"]

	s := verifySession(t)
	s.Creds = agentCredentialsForTest()
	with := chartComponents(s)["kubenest-agent"]

	if without != with {
		t.Errorf("the record check matches %q standalone and %q registered", without, with)
	}
}

// agentCredentialsForTest is a minted-credentials value carrying only the
// non-secret fields this test reads. api.Secret refuses to be written down at
// all, so there is nothing here that could become a credential by accident.
func agentCredentialsForTest() *api.AgentCredentials {
	return &api.AgentCredentials{
		ClusterID: "cluster-1",
		Operator:  api.OperatorInstallInfo{Namespace: "kubenest-system", ChartRef: "oci://example.invalid/chart:1"},
	}
}
