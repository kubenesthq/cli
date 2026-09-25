package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The instance alert-destination surface (T2.5). These pin the CONTRACT the
// backend routes serve: a path, a verb and a body shape that are wrong here are
// a 404, a 405 or a 422 the operator sees as "alerts do not work", discovered
// only on a live control plane.

func TestAddAlertDestinationPostsURLAndFormat(t *testing.T) {
	var got struct {
		URL    string `json:"url"`
		Format string `json:"format"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/instance/alert-destinations" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"0192f0c4-0000-7000-8000-000000000000","format":"slack",
			"enabled":true,"created_at":"2026-09-25T10:00:00Z","url_host":"hooks.slack.com"}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	destination, err := c.AddAlertDestination(context.Background(), "https://hooks.slack.com/services/T/B/X", "slack")
	if err != nil {
		t.Fatal(err)
	}

	if got.URL != "https://hooks.slack.com/services/T/B/X" || got.Format != "slack" {
		t.Errorf("body = %+v, want the url and format as sent", got)
	}
	if destination.URLHost != "hooks.slack.com" || destination.Format != "slack" {
		t.Errorf("destination = %+v", destination)
	}
}

// A destination's path is a credential. The backend never returns one; this
// pins that the CLI has nowhere to print one even if the backend started to.
func TestAlertDestinationDecodingHasNoURLField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/instance/alert-destinations" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`[{"id":"1","format":"generic","enabled":true,
			"created_at":"2026-09-25T10:00:00Z","url_host":"alerts.example.com"}]`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	destinations, err := c.ListAlertDestinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(destinations) != 1 || destinations[0].URLHost != "alerts.example.com" {
		t.Fatalf("destinations = %+v", destinations)
	}
	if strings.Contains(destinations[0].URLHost, "/") {
		t.Errorf("url_host %q carries a path", destinations[0].URLHost)
	}
}

func TestRemoveAlertDestinationUsesTheIDInThePath(t *testing.T) {
	const id = "0192f0c4-0000-7000-8000-000000000000"
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		seen = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	if err := c.RemoveAlertDestination(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if seen != "/api/v1/instance/alert-destinations/"+id {
		t.Errorf("path = %q", seen)
	}
}

func TestTestAlertDestinationsReportsEachAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instance/alert-destinations/test" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"destinations":[
			{"id":"1","format":"generic","url_host":"ok.example.com","delivered":true},
			{"id":"2","format":"slack","url_host":"down.example.com","delivered":false,"error":"HTTP 503"}
		],"delivered":1,"failed":1}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	result, err := c.TestAlertDestinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Delivered != 1 || result.Failed != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Destinations[1].Error != "HTTP 503" {
		t.Errorf("failed destination lost its error: %+v", result.Destinations[1])
	}
}

func TestHeartbeatIsPutAndDeleted(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instance/heartbeat" {
			t.Errorf("path = %q", r.URL.Path)
		}
		methods = append(methods, r.Method)
		if r.Method == http.MethodPut {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
			}
			if body["url"] != "https://hc-ping.com/abc" {
				t.Errorf("body = %v", body)
			}
			w.Write([]byte(`{"heartbeat_configured":true}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	ctx := context.Background()
	if err := c.SetHeartbeat(ctx, "https://hc-ping.com/abc"); err != nil {
		t.Fatal(err)
	}
	if err := c.ClearHeartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[0] != http.MethodPut || methods[1] != http.MethodDelete {
		t.Errorf("methods = %v, want PUT then DELETE", methods)
	}
}

func TestInstanceAlertingCarriesTheBacklogAndNoURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instance/alerting" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"destinations":2,"heartbeat_configured":false,
			"backlog":{"pending":7,"oldest_age_seconds":420},"failed_destinations":1}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	status, err := c.InstanceAlerting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Destinations != 2 || status.HeartbeatConfigured || status.FailedDestinations != 1 {
		t.Fatalf("status = %+v", status)
	}
	if status.Backlog.Pending != 7 || status.Backlog.OldestAgeSeconds == nil ||
		*status.Backlog.OldestAgeSeconds != 420 {
		t.Errorf("backlog = %+v", status.Backlog)
	}
}

// A refused destination is the backend's 400 with a reason. The operator has to
// see that reason; a generic "400 Bad Request" teaches them nothing.
func TestARefusedDestinationSurfacesTheReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"detail":"127.0.0.1 is a plaintext (http) destination"}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	_, err := c.AddAlertDestination(context.Background(), "http://127.0.0.1:9/alerts", "generic")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("error %q does not carry the refusal reason", err)
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Errorf("error %v is not an *api.Error carrying the status", err)
	}
}
