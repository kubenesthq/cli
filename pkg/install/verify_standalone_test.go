package install

import (
	"context"
	"io"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
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
//
// Until kn-sf17 this returned an error saying the replacement did not exist
// yet. It exists now — verifyAgentReconciles — so the check runs for real, and
// what this holds is that it still cannot pass by doing nothing.
func TestStandaloneClusterCheckDoesNotSilentlyPass(t *testing.T) {
	s := verifySession(t) // no API client, so standalone; and no nodes

	err := verifyClusterReportsIn(context.Background(), s)
	if err == nil {
		t.Fatal("acceptance check 4 reported a pass on a cluster with nothing to report to")
	}
}

// The probe is the whole check, so it is tested directly rather than through
// converge.Wait, whose deadline is ten minutes.
//
// The case that matters is the middle one. An agent whose pod is Available but
// whose controllers are not reconciling is exactly what unmanaged mode risks:
// readiness no longer reflects a hub, so nothing else in the install would
// notice. A probe that treated "the CR exists" as success would pass on it.
func TestTheReconcileProbeDistinguishesRunningFromReconciling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project string
		wantOK  bool
		wantIn  string
	}{
		{
			name:    "reconciled",
			project: `{"status":{"phase":"Ready"}}`,
			wantOK:  true,
		},
		{
			name:    "accepted but never looked at",
			project: `{"status":{}}`,
			wantOK:  false,
			wantIn:  "controllers have not reconciled",
		},
		{
			name:    "reconciled and failed",
			project: `{"status":{"phase":"Failed"}}`,
			wantOK:  false,
			wantIn:  "Failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
				if strings.Contains(command, "get project") {
					return sshx.Result{Stdout: tc.project}, nil
				}
				return sshx.Result{Stdout: "Running"}, nil
			}}
			ok, state, err := reconciledProjectProbe(r)(context.Background())
			if err != nil {
				t.Fatalf("probe errored: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("probe returned %v, want %v (state: %+v)", ok, tc.wantOK, state)
			}
			if tc.wantIn != "" && !strings.Contains(state.Status, tc.wantIn) {
				t.Errorf("the state does not say what went wrong: %+v", state)
			}
		})
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
		Operator:  api.OperatorInstallInfo{Namespace: "kubenest-system", ChartRef: "oci://example.invalid/chart:1", CreatesWorkloadApplications: true},
	}
}
