package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The fleet health reads (T2.9). These pin the CONTRACT the backend routes
// serve, from the backend's own field names rather than from the structs above:
// building the fixture out of our own types would let a wrong json tag agree
// with itself and fail only on a live control plane. The bodies below are
// kubenest-backend/app/schemas/cluster_health.py.

const fleetHealthBody = `{
  "org_id": "0192f0c4-0000-7000-8000-00000000a001",
  "total": 1,
  "counts": {"ok": 0, "warning": 0, "critical": 1, "unknown": 0, "unsupported": 0, "not_applicable": 0},
  "exit_code": 2,
  "clusters": [{
    "cluster_id": "0192f0c4-0000-7000-8000-00000000c001",
    "cluster_name": "staging",
    "status": "critical",
    "exit_code": 2,
    "problems": [{
      "check": "backup",
      "status": "critical",
      "reason_code": "NO_BACKUP_TARGET",
      "message": "no backup target configured",
      "detail": {"paused_projects": 2},
      "evidence": {"measured_at": "2026-09-27T09:58:00+00:00", "received_at": "2026-09-27T09:58:05+00:00", "source": "operator", "check_set_version": "2"}
    }],
    "last_reported_at": "2026-09-27T09:58:05+00:00",
    "bundle_version": "1.1",
    "ha_tier": "single-server",
    "no_control_plane_redundancy": true
  }],
  "sweeper": {"alive": true, "age_seconds": 12}
}`

const clusterHealthBody = `{
  "cluster_id": "0192f0c4-0000-7000-8000-00000000c001",
  "cluster_name": "staging",
  "status": "warning",
  "exit_code": 1,
  "checks": [{
    "check": "os_patching",
    "status": "warning",
    "reason_code": "REBOOT_PENDING",
    "message": "1 node is waiting for a reboot",
    "detail": {"pending_reboot_nodes": 1},
    "evidence": {"measured_at": "2026-09-27T09:57:00+00:00", "received_at": "2026-09-27T09:58:05+00:00", "source": "operator", "check_set_version": "2"}
  }],
  "reported_at": "2026-09-27T09:58:00+00:00",
  "received_at": "2026-09-27T09:58:05+00:00",
  "report_interval_seconds": 60,
  "bundle_version": "1.1",
  "thresholds_provisional": false
}`

func TestFleetHealthReadsTheOrgsFleet(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if auth := r.Header.Get("Authorization"); auth != "Bearer knp_tok" {
			t.Errorf("Authorization = %q, want the stored token", auth)
		}
		w.Write([]byte(fleetHealthBody))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	fleet, err := c.FleetHealth(context.Background(), "0192f0c4-0000-7000-8000-00000000a001")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/orgs/0192f0c4-0000-7000-8000-00000000a001/fleet-health"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if fleet.ExitCode != 2 || fleet.Total != 1 || fleet.Counts["critical"] != 1 {
		t.Errorf("fleet = %+v, want the org's single critical cluster", fleet)
	}
	if len(fleet.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(fleet.Clusters))
	}
	row := fleet.Clusters[0]
	if row.ClusterName != "staging" || row.Status != "critical" || row.ExitCode != 2 {
		t.Errorf("row = %+v", row)
	}
	if row.LastReportedAt == nil || row.BundleVersion != "1.1" || row.HATier != "single-server" {
		t.Errorf("row = %+v, want the record's own facts", row)
	}
	if len(row.Problems) != 1 {
		t.Fatalf("problems = %d, want the one non-ok check", len(row.Problems))
	}
	problem := row.Problems[0]
	if problem.Check != "backup" || problem.ReasonCode != "NO_BACKUP_TARGET" {
		t.Errorf("problem = %+v", problem)
	}
	if problem.Detail["paused_projects"] != float64(2) {
		t.Errorf("problem detail = %+v, want the reported count", problem.Detail)
	}
	if problem.Evidence["source"] != "operator" || problem.Evidence["check_set_version"] != "2" {
		t.Errorf("problem evidence = %+v, want the envelope", problem.Evidence)
	}
}

// A cluster that has never reported has no receipt time, and the difference
// between "never" and "a while ago" is the whole reason the field is a pointer.
func TestFleetHealthKeepsANeverReportedClusterDistinct(t *testing.T) {
	body := `{"org_id": "o", "total": 1, "counts": {"unknown": 1}, "exit_code": 3,
	  "clusters": [{"cluster_id": "c", "cluster_name": "new", "status": "unknown", "exit_code": 3,
	    "problems": [], "last_reported_at": null, "bundle_version": null, "ha_tier": null,
	    "no_control_plane_redundancy": false}], "sweeper": {}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	fleet, err := c.FleetHealth(context.Background(), "o")
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Clusters[0].LastReportedAt != nil {
		t.Errorf("last_reported_at = %v, want nil for a cluster that never reported", fleet.Clusters[0].LastReportedAt)
	}
}

func TestClusterHealthDetailReadsEveryCheck(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(clusterHealthBody))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	detail, err := c.ClusterHealthDetail(context.Background(), "0192f0c4-0000-7000-8000-00000000c001")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/clusters/0192f0c4-0000-7000-8000-00000000c001/health"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if detail.Status != "warning" || detail.ExitCode != 1 {
		t.Errorf("detail = %+v", detail)
	}
	if detail.ReportedAt == nil || detail.ReceivedAt == nil {
		t.Error("the two clocks were not decoded")
	}
	if detail.ReportIntervalSeconds == nil || *detail.ReportIntervalSeconds != 60 {
		t.Errorf("report_interval_seconds = %v", detail.ReportIntervalSeconds)
	}
	if detail.ThresholdsProvisional == nil || *detail.ThresholdsProvisional {
		t.Errorf("thresholds_provisional = %v, want false", detail.ThresholdsProvisional)
	}
	if len(detail.Checks) != 1 || detail.Checks[0].Check != "os_patching" {
		t.Fatalf("checks = %+v", detail.Checks)
	}
	if detail.Checks[0].Detail["pending_reboot_nodes"] != float64(1) {
		t.Errorf("detail = %+v", detail.Checks[0].Detail)
	}
}

// A refusal from either route must reach the operator with the reason the
// control plane gave, and a 401 must name the recovery rather than the status.
func TestHealthReadRefusalsCarryTheirReason(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
		callIt func(c *Client) error
	}{
		{
			name:   "fleet, forbidden",
			status: http.StatusForbidden,
			body:   `{"detail": "this token cannot read that organisation"}`,
			want:   "this token cannot read that organisation",
			callIt: func(c *Client) error { _, err := c.FleetHealth(context.Background(), "o"); return err },
		},
		{
			name:   "detail, not found",
			status: http.StatusNotFound,
			body:   `{"detail": "no such cluster"}`,
			want:   "no such cluster",
			callIt: func(c *Client) error { _, err := c.ClusterHealthDetail(context.Background(), "c"); return err },
		},
		{
			name:   "detail, unauthorized",
			status: http.StatusUnauthorized,
			body:   `{"detail": "expired"}`,
			want:   "kubenest login",
			callIt: func(c *Client) error { _, err := c.ClusterHealthDetail(context.Background(), "c"); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c, _ := New(srv.URL, WithToken("stale"))
			err := tc.callIt(c)
			if err == nil {
				t.Fatal("a refusal was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not carry %q", err.Error(), tc.want)
			}
		})
	}
}
