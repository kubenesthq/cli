package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The tests below are the CLI's half of the mirrored operation record (T2.3):
// the wire shape the backend route accepts, and the two refusals it would
// otherwise have to make for us.

const testClusterID = "8f14e45f-ceea-4a1e-9a0f-2b3c4d5e6f70"

func testState() ClusterOperationState {
	at := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	return ClusterOperationState{
		OperationID:         "a1b2c3d4e5f60718",
		RequestDigest:       strings.Repeat("b", 64),
		Terminal:            false,
		State:               ClusterOperationRunning,
		Stage:               "apply-plan",
		ResumeCommand:       "kubenest platform upgrade --resume a1b2c3d4e5f60718",
		ExecutorHeartbeatAt: &at,
		Revision:            "100",
	}
}

// TestPutClusterOperationSendsExactlyTheContractsFields: the route forbids a
// field it does not know (`extra="forbid"`), and it is right to — this record
// is mirrored to a control plane that never sees the request, so a field added
// carelessly (a command, a token) must be refused there rather than stored.
// The client must therefore send the contract's fields and nothing else.
func TestPutClusterOperationSendsExactlyTheContractsFields(t *testing.T) {
	var (
		method  string
		path    string
		auth    string
		ctype   string
		sent    map[string]any
		rawBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		auth = r.Header.Get("Authorization")
		rawBody, _ = io.ReadAll(r.Body)
		json.Unmarshal(rawBody, &sent)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"operation_id":"a1b2c3d4e5f60718"}`)
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithToken("knp_report"))
	if err != nil {
		t.Fatal(err)
	}
	state := testState()
	if err := c.PutClusterOperation(context.Background(), testClusterID, state); err != nil {
		t.Fatalf("mirroring the record: %v", err)
	}

	if method != http.MethodPut {
		t.Errorf("the mirror went out as %s, want PUT", method)
	}
	if want := "/api/v1/clusters/" + testClusterID + "/operation"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if auth != "Bearer knp_report" {
		t.Errorf("Authorization = %q: the mirror is an authenticated write with the CLI's install:report token", auth)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ctype)
	}

	want := []string{"executor_heartbeat_at", "operation_id", "request_digest", "resume_command", "revision", "stage", "state", "terminal"}
	if len(sent) != len(want) {
		t.Fatalf("the mirror carried %d field(s) %v, want %d %v", len(sent), keysOf(sent), len(want), want)
	}
	for _, field := range want {
		if _, ok := sent[field]; !ok {
			t.Errorf("the mirror does not carry %q: %s", field, rawBody)
		}
	}
	if sent["operation_id"] != state.OperationID || sent["state"] != ClusterOperationRunning || sent["terminal"] != false {
		t.Errorf("the mirror's identity or state is wrong: %s", rawBody)
	}
	if sent["revision"] != "100" || sent["stage"] != "apply-plan" {
		t.Errorf("the mirror does not carry where the operation got to: %s", rawBody)
	}
	// The heartbeat is the one field a staleness reading is taken from, so its
	// shape matters: RFC3339, in UTC.
	if got, _ := sent["executor_heartbeat_at"].(string); got != "2026-09-25T10:00:00Z" {
		t.Errorf("executor_heartbeat_at = %q, want the heartbeat in RFC3339 UTC", got)
	}
}

// TestPutClusterOperationRefusesAStateThatDisagreesWithTerminal: the route
// refuses a record whose state and terminal flag disagree, because a mirror
// holding a different derivation would be a record lying about its own state.
// The CLI refuses to send one, so the failure is the caller's to fix and not a
// 422 it cannot read.
func TestPutClusterOperationRefusesAStateThatDisagreesWithTerminal(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithToken("knp_report"))
	if err != nil {
		t.Fatal(err)
	}

	lying := testState()
	lying.Terminal = true // state still says "running"
	if err := c.PutClusterOperation(context.Background(), testClusterID, lying); err == nil {
		t.Fatal("a record claiming to be both terminal and running was mirrored")
	}
	if requests != 0 {
		t.Fatalf("the refusal still made %d request(s) to the control plane", requests)
	}

	// A record with no revision cannot be placed: the mirror only moves
	// forward, and the route compares the revision it carries.
	noRevision := testState()
	noRevision.Revision = ""
	if err := c.PutClusterOperation(context.Background(), testClusterID, noRevision); err == nil {
		t.Fatal("a record with no revision was mirrored")
	}
	// Terminal really does travel with the terminal state.
	done := testState()
	done.Terminal, done.State, done.ResumeCommand = true, ClusterOperationTerminal, ""
	if err := c.PutClusterOperation(context.Background(), testClusterID, done); err != nil {
		t.Fatalf("mirroring a terminal record: %v", err)
	}
	if requests != 1 {
		t.Fatalf("the control plane saw %d request(s), want the one terminal write", requests)
	}
}

// TestGetClusterOperationReadsNoRecordAsNoRecord: a cluster that has never run
// a disruptive operation has no record, and the route's 404 says exactly that.
// It is a normal state, not an error about the control plane — `check_upgrade`
// reads the same absence as "no upgrade performed".
func TestGetClusterOperationReadsNoRecordAsNoRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("the read went out as %s, want GET", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"detail": "No operation record for cluster prod-1. The cluster has not run a disruptive operation since the control plane last saw it."}`)
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithToken("knp_read"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetClusterOperation(context.Background(), testClusterID)
	if err != nil {
		t.Fatalf("a cluster with no record is not an error: %v", err)
	}
	if got != nil {
		t.Fatalf("a cluster with no record returned %+v", got)
	}
}

// TestGetClusterOperationRoundTripsTheStoredRecord: what the console and
// `check_upgrade` read back is the record the CLI mirrored, including the two
// fields an interrupted operation is reported from.
func TestGetClusterOperationRoundTripsTheStoredRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"cluster_id": "`+testClusterID+`",
			"operation_id": "a1b2c3d4e5f60718",
			"request_digest": "`+testState().RequestDigest+`",
			"state": "running",
			"stage": "kubernetes",
			"resume_command": "kubenest platform upgrade --resume a1b2c3d4e5f60718",
			"executor_heartbeat_at": "2026-09-25T10:00:00Z",
			"terminal": false,
			"revision": "140",
			"updated_at": "2026-09-25T10:00:05Z"
		}`)
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithToken("knp_read"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetClusterOperation(context.Background(), testClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("the stored record was read as no record")
	}
	if got.OperationID != "a1b2c3d4e5f60718" || got.Stage != "kubernetes" || got.Revision != "140" || got.Terminal {
		t.Fatalf("the record read back is %+v", got)
	}
	if got.ResumeCommand != "kubenest platform upgrade --resume a1b2c3d4e5f60718" {
		t.Fatalf("the resume command read back is %q", got.ResumeCommand)
	}
	if got.ExecutorHeartbeatAt == nil || !got.ExecutorHeartbeatAt.Equal(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("the heartbeat read back is %v", got.ExecutorHeartbeatAt)
	}
	// A read is not a write: the control plane's own columns are read into the
	// read type, which is not what a put sends.
	if got.ClusterID != testClusterID || got.UpdatedAt == nil {
		t.Fatalf("the control plane's own columns are missing: %+v", got)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
