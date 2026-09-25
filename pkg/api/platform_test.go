package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The CLI's half of the maintenance-window route (kn-t31): the wire shape the
// backend serves, and the three refusals it must not swallow.
//
// The route's shape is load-bearing. GET answers the window, its revision, the
// STATE it is in and the backup warning; PUT carries the revision the caller
// READ. A client that sent no revision would be a last-writer-wins overwrite —
// two operators setting one cluster's window from two laptops is the ordinary
// case — and a client that read a missing window as "any time" is the kn-nqj
// defect itself.

// storedWindowJSON is a stored window as the control plane answers: state
// applying, revision 4, applied at 3 (so the cluster is running the previous
// window) and no backup warning.
const storedWindowJSON = `{
  "window": {"days": ["sat","sun"], "start": "02:00", "end": "06:00", "timezone": "Asia/Kolkata"},
  "revision": 4,
  "state": "applying",
  "applied_revision": 3,
  "reject_reason": null,
  "warning": null
}`

func TestMaintenanceWindowPutCarriesTheRevisionItRead(t *testing.T) {
	var (
		method  string
		path    string
		body    map[string]any
		rawBody []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		rawBody, _ = io.ReadAll(r.Body)
		json.Unmarshal(rawBody, &body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, storedWindowJSON)
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithToken("knp_tok"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := c.PutMaintenanceWindow(context.Background(), "c1", MaintenanceWindow{
		Days: []string{"sat", "sun"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata",
	}, 3)
	if err != nil {
		t.Fatal(err)
	}

	if method != http.MethodPut || path != "/api/v1/clusters/c1/maintenance-window" {
		t.Errorf("%s %s, want PUT /api/v1/clusters/c1/maintenance-window", method, path)
	}
	// The revision the caller read is what makes the write a compare-and-swap.
	// Anything other than a number here is the silent overwrite this route
	// exists to prevent.
	if got, ok := body["revision"].(float64); !ok || int(got) != 3 {
		t.Errorf("the write carried revision %v, want the revision the caller read (3): %s", body["revision"], rawBody)
	}
	for field, want := range map[string]string{"start": "02:00", "end": "06:00", "timezone": "Asia/Kolkata"} {
		if got, _ := body[field].(string); got != want {
			t.Errorf("the write carried %s=%q, want %q", field, got, want)
		}
	}
	if days, _ := body["days"].([]any); len(days) != 2 {
		t.Errorf("the write carried days=%v, want both days", body["days"])
	}
	// And the answer is the record: the state and the revision are what the
	// command surface prints, separately.
	if record.State != WindowStateApplying || record.CurrentRevision() != 4 {
		t.Errorf("record = %+v, want the applying state at revision 4", record)
	}
	if record.AppliedRevision == nil || *record.AppliedRevision != 3 {
		t.Errorf("record.AppliedRevision = %v, want 3: the cluster is running the previous window", record.AppliedRevision)
	}
}

// A stale write is refused, and the refusal tells the operator the window moved
// rather than that the request was malformed.
func TestMaintenanceWindowPutRefusesAStaleRevision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"detail": "the window has moved on: it is at revision 5, your write carried 3 — read it again and re-apply"}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	_, err := c.PutMaintenanceWindow(context.Background(), "c1", MaintenanceWindow{
		Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: "UTC",
	}, 3)
	if err == nil {
		t.Fatal("a stale revision must be refused")
	}
	if !strings.Contains(err.Error(), "someone changed the window; re-run") {
		t.Errorf("the refusal must say the window moved and what to do about it, got: %v", err)
	}
	if !strings.Contains(err.Error(), "revision 5") {
		t.Errorf("the control plane's own detail names the current revision and must survive, got: %v", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Errorf("the 409 must stay matchable with errors.As, got %v", err)
	}
}

// A cluster with no window answers a null window and a null revision, and that
// must never be read as permission.
func TestMaintenanceWindowWithoutAWindowIsNotPermission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"window": null, "revision": null, "state": "none", "applied_revision": null, "reject_reason": null, "warning": null}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	record, err := c.MaintenanceWindow(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Window != nil {
		t.Errorf("window = %+v, want nil: this cluster has no window", record.Window)
	}
	if record.State != WindowStateNone {
		t.Errorf("state = %q, want %q: a cluster with no window is not `stored` (kn-nqj.1)",
			record.State, WindowStateNone)
	}
	if record.Revision != nil {
		t.Errorf("revision = %v, want nil", *record.Revision)
	}
	// A write based on "no window" is based on revision 0: the floor the
	// backend's first write is accepted at.
	if record.CurrentRevision() != 0 {
		t.Errorf("CurrentRevision() = %d, want 0 for a cluster that has never had a window", record.CurrentRevision())
	}
}

// A control plane that does not serve the route is a FAILED READ, not a cluster
// without a window: an unroutable read that returned a nil record would be an
// unread gate arriving at the upgrade as a passed one.
func TestMaintenanceWindowMissingRouteIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"detail": "Not Found"}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	if _, err := c.MaintenanceWindow(context.Background(), "c1"); err == nil {
		t.Fatal("a 404 must be an error, not a nil window")
	} else if !strings.Contains(err.Error(), "does not serve") {
		t.Errorf("the error must say what is wrong with the control plane, got: %v", err)
	}
}

// The three states and the backup warning are read, not collapsed: `unknown` is
// the control plane saying the backup was never measured, and null is the
// control plane saying it read the evidence and found no overlap.
func TestMaintenanceWindowReadsStateAndBackupWarning(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		state   string
		warning func(*testing.T, *string)
	}{
		{
			name:    "none, no window",
			body:    `{"window": null, "revision": null, "state": "none", "applied_revision": null, "reject_reason": null, "warning": null}`,
			state:   WindowStateNone,
			warning: func(t *testing.T, w *string) {},
		},
		{
			name:  "stored, warning unknown",
			body:  `{"window": {"days": ["sat"], "start": "02:00", "end": "06:00", "timezone": "UTC"}, "revision": 1, "state": "stored", "applied_revision": null, "reject_reason": null, "warning": "unknown"}`,
			state: WindowStateStored,
			warning: func(t *testing.T, w *string) {
				if w == nil || *w != "unknown" {
					t.Errorf("warning = %v, want the literal unknown", w)
				}
			},
		},
		{
			name:  "active, overlap warning",
			body:  `{"window": {"days": ["sat"], "start": "02:30", "end": "06:00", "timezone": "UTC"}, "revision": 2, "state": "active", "applied_revision": 2, "reject_reason": null, "warning": "this window starts at 02:30 UTC, inside the nightly backup"}`,
			state: WindowStateActive,
			warning: func(t *testing.T, w *string) {
				if w == nil || !strings.Contains(*w, "inside the nightly backup") {
					t.Errorf("warning = %v, want the overlap sentence", w)
				}
			},
		},
		{
			name:    "active, no warning",
			body:    `{"window": {"days": ["sat"], "start": "04:00", "end": "06:00", "timezone": "UTC"}, "revision": 2, "state": "active", "applied_revision": 2, "reject_reason": null, "warning": null}`,
			state:   WindowStateActive,
			warning: func(t *testing.T, w *string) {},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := c.body
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
			}))
			defer srv.Close()

			client, _ := New(srv.URL)
			record, err := client.MaintenanceWindow(context.Background(), "c1")
			if err != nil {
				t.Fatal(err)
			}
			if record.State != c.state {
				t.Errorf("state = %q, want %q", record.State, c.state)
			}
			c.warning(t, record.Warning)
		})
	}
}
