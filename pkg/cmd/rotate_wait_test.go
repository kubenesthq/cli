package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
)

// clusterServer answers GET /api/v1/clusters/<id> with a fixed status and
// heartbeat, and counts the polls so a test can tell "returned on the first
// look" from "waited".
func clusterServer(t *testing.T, status string, heartbeat *time.Time) (*api.Client, *int) {
	t.Helper()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls++
		hb := "null"
		if heartbeat != nil {
			hb = fmt.Sprintf("%q", heartbeat.UTC().Format(time.RFC3339Nano))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"c1","name":"c1","status":%q,"org_id":"o1","last_heartbeat":%s}`, status, hb)
	}))
	t.Cleanup(srv.Close)
	c, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return c, &polls
}

// A CLUSTER STILL SHOWING THE HEARTBEAT IT HAD BEFORE THE ROTATION IS NOT
// RECONNECTED, AND THIS IS THE TEST THE COMMAND WAS MISSING (kn-tlmv).
//
// Nothing in the backend moves Cluster.status when a rotation drops the hub
// connection — the health sweeper decides that from a clock — so the row keeps
// reading "connected" from the pre-rotation heartbeat. A wait keyed on the
// status alone therefore returns on its first poll and reports success over a
// cluster that may never come back.
//
// MUTATION THAT MUST FAIL THIS TEST: drop the heartbeatAdvanced call from
// waitConnected's success condition. That is the pre-fix behaviour and this
// test then passes instantly instead of timing out.
func TestWaitConnectedRefusesAStaleHeartbeat(t *testing.T) {
	before := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	client, polls := clusterServer(t, "connected", &before)

	err := waitConnected(context.Background(), client, "c1", takenBaseline(&before), 100*time.Millisecond)
	if err == nil {
		t.Fatal("waitConnected accepted a cluster whose heartbeat had not moved since before the rotation")
	}
	if !strings.Contains(err.Error(), "same heartbeat as before the rotation") {
		t.Fatalf("the failure must say WHICH condition is outstanding, got: %v", err)
	}
	if *polls == 0 {
		t.Fatal("the control plane was never asked")
	}
}

// A heartbeat later than the one held before the rotation is the proof the new
// token authenticated: the old token is below the revocation floor and cannot
// produce one.
func TestWaitConnectedAcceptsAnAdvancedHeartbeat(t *testing.T) {
	before := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	after := before.Add(30 * time.Second)
	client, _ := clusterServer(t, "connected", &after)

	if err := waitConnected(context.Background(), client, "c1", takenBaseline(&before), 2*time.Second); err != nil {
		t.Fatalf("waitConnected rejected a genuine reconnection: %v", err)
	}
}

// Connected with no recorded heartbeat is not an advance. Absence of a
// timestamp is not evidence of a report.
func TestWaitConnectedRefusesAMissingHeartbeat(t *testing.T) {
	before := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	client, _ := clusterServer(t, "connected", nil)

	err := waitConnected(context.Background(), client, "c1", takenBaseline(&before), 100*time.Millisecond)
	if err == nil {
		t.Fatal("waitConnected accepted a cluster with no recorded heartbeat")
	}
	if !strings.Contains(err.Error(), "no heartbeat") {
		t.Fatalf("the failure must name the missing heartbeat, got: %v", err)
	}
}

// A cluster that has never reported has no baseline, so the first heartbeat it
// produces is an advance. This is the correct reading rather than a special
// case, and it is here so nobody "fixes" it into a refusal.
func TestWaitConnectedAcceptsAFirstEverHeartbeat(t *testing.T) {
	first := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	client, _ := clusterServer(t, "connected", &first)

	if err := waitConnected(context.Background(), client, "c1", takenBaseline(nil), 2*time.Second); err != nil {
		t.Fatalf("waitConnected rejected a cluster's first-ever heartbeat: %v", err)
	}
}

// The status half must still refuse on its own, so the new condition has not
// replaced the old one.
func TestWaitConnectedStillRefusesADisconnectedStatus(t *testing.T) {
	before := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	after := before.Add(30 * time.Second)
	client, _ := clusterServer(t, "disconnected", &after)

	err := waitConnected(context.Background(), client, "c1", takenBaseline(&before), 100*time.Millisecond)
	if err == nil {
		t.Fatal("waitConnected accepted a disconnected cluster")
	}
	if !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("the failure must name the status, got: %v", err)
	}
}

// AND THE CALL SITE IS PINNED TOO, WHICH THE FIVE TESTS ABOVE DO NOT DO.
//
// They pass `since` explicitly, so they pin waitConnected while leaving
// runRotate free to stop reading the baseline. Measured: with a bare
// *time.Time, deleting that read reintroduced the defect in full and all five
// still passed. The zero-value baseline now refuses, so the same deletion
// produces a loud internal error instead of a silent success.
func TestWaitConnectedRefusesAnUntakenBaseline(t *testing.T) {
	after := time.Date(2026, 9, 12, 10, 0, 30, 0, time.UTC)
	client, polls := clusterServer(t, "connected", &after)

	err := waitConnected(context.Background(), client, "c1", heartbeatBaseline{}, 2*time.Second)
	if err == nil {
		t.Fatal("waitConnected reported success without a pre-rotation baseline")
	}
	if !strings.Contains(err.Error(), "no pre-rotation heartbeat baseline") {
		t.Fatalf("the refusal must name the missing baseline, got: %v", err)
	}
	if *polls != 0 {
		t.Fatalf("it must refuse before asking the control plane, polled %d times", *polls)
	}
}
