package install

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// The check used to pass on ANY heartbeat, so a re-run of a cluster whose
// agent had been retired certified a cluster that stopped reporting hours
// earlier (kn-2xdk): the heartbeat it read was the one the dead agent left
// behind. Only a heartbeat STRICTLY LATER than the one the stage started with
// proves something is reporting NOW.
func TestTheHeartbeatCheckRefusesAClusterThatStoppedReporting(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	fake := &heartbeatAPI{script: []heartbeatAnswer{{heartbeat: &stale}}}

	// The deadline is deliberately already past: this exercises the check's
	// first look at a stale cluster. How many times converge.Wait runs a
	// probe before its deadline is tested in pkg/converge, not here.
	err := verifyClusterReportsIn(context.Background(), reportingSession(t, fake.serve(t), "1ns"))
	if err == nil {
		t.Fatal("a cluster whose last heartbeat predates the check passed `cluster-connected`")
	}
	if !strings.Contains(err.Error(), stale.Format(time.RFC3339)) {
		t.Errorf("the failure does not name the stale heartbeat's time %s: %v", stale.Format(time.RFC3339), err)
	}
}

// The other half: a cluster that is reporting now must still pass, and pass
// on the heartbeat that arrives after the check started.
func TestTheHeartbeatCheckPassesOnAHeartbeatNewerThanTheOneItStartedWith(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	fresh := stale.Add(time.Minute)
	fake := &heartbeatAPI{script: []heartbeatAnswer{{heartbeat: &stale}, {heartbeat: &fresh}}}

	// One second, not the bundle's real ten minutes: a check that wrongly
	// kept waiting fails here in seconds instead of hanging the suite.
	err := verifyClusterReportsIn(context.Background(), reportingSession(t, fake.serve(t), "1s"))
	if err != nil {
		t.Fatalf("a cluster reporting a heartbeat newer than the one the stage started with did not pass: %v", err)
	}
}

// A first install has no heartbeat yet, and that has always meant "wait for
// the agent's first one". It still does: nil is the baseline, so the first
// heartbeat to arrive is a later one.
func TestTheHeartbeatCheckStillWaitsForAFirstInstallThatHasNotReportedYet(t *testing.T) {
	t.Run("no heartbeat yet is not a pass", func(t *testing.T) {
		fake := &heartbeatAPI{script: []heartbeatAnswer{{}}}
		err := verifyClusterReportsIn(context.Background(), reportingSession(t, fake.serve(t), "1ns"))
		if err == nil {
			t.Fatal("a first install with no heartbeat at all passed `cluster-connected`")
		}
	})

	t.Run("the first heartbeat passes", func(t *testing.T) {
		first := time.Now().UTC().Truncate(time.Second)
		fake := &heartbeatAPI{script: []heartbeatAnswer{{}, {heartbeat: &first}}}
		err := verifyClusterReportsIn(context.Background(), reportingSession(t, fake.serve(t), "1s"))
		if err != nil {
			t.Fatalf("a first install whose agent reported its first heartbeat did not pass: %v", err)
		}
	})
}

// The baseline read is best-effort, because an unreachable control plane is
// something to converge on rather than a verdict. But a control plane that
// was unreachable when the stage started must not leave the check with NO
// baseline: "no baseline" is exactly what passes the stale heartbeat this
// bead is about. The first successful poll becomes the baseline, so a
// heartbeat that was already there when the control plane answered is not a
// pass.
func TestTheHeartbeatCheckDoesNotPassOnAStaleHeartbeatItCouldNotBaselineBefore(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	fake := &heartbeatAPI{script: []heartbeatAnswer{
		{status: http.StatusInternalServerError},
		{heartbeat: &stale},
	}}

	err := verifyClusterReportsIn(context.Background(), reportingSession(t, fake.serve(t), "1ns"))
	if err == nil {
		t.Fatal("the check passed on a heartbeat the control plane already held, because it could not read it before")
	}
	if !strings.Contains(err.Error(), stale.Format(time.RFC3339)) {
		t.Errorf("the failure does not name the stale heartbeat's time %s: %v", stale.Format(time.RFC3339), err)
	}
}

// heartbeatAnswer is one scripted answer from heartbeatAPI: either an HTTP
// status (the control plane is not answering) or the heartbeat it holds.
type heartbeatAnswer struct {
	status    int
	heartbeat *time.Time
}

// heartbeatAPI is a fake control plane that serves cluster health from a
// scripted sequence, repeating the last answer once the script runs out.
//
// It is a real HTTP server rather than a mocked client for the same reason
// recordAPI is: the check reads this JSON field over api.Client, and the
// field it reads is part of what is under test.
type heartbeatAPI struct {
	mu     sync.Mutex
	calls  int
	script []heartbeatAnswer
}

func (h *heartbeatAPI) serve(t *testing.T) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.mu.Lock()
		i := h.calls
		h.calls++
		answer := heartbeatAnswer{}
		if n := len(h.script); n > 0 {
			answer = h.script[min(i, n-1)]
		}
		h.mu.Unlock()

		if answer.status != 0 {
			w.WriteHeader(answer.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ClusterHealth{
			Name: "lab-w3", Status: "connected", LastHeartbeat: answer.heartbeat,
		})
	}))
	t.Cleanup(srv.Close)

	client, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return client
}

// reportingSession is a run that has reached the heartbeat check: a
// registered cluster id and a control plane to ask. The deadline is per test
// — a check that must refuse has to reach it, one that must pass does not.
func reportingSession(t *testing.T, client *api.Client, componentReady string) *Session {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\nlimits:\n  timeouts:\n    install-total: 30m\n    component-ready: " + componentReady + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		ID: "run-1", Bundle: m, Out: io.Discard,
		Jnl: &Journal{ClusterID: "0199a8a0-0000-7000-8000-000000000001"},
		API: client,
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
