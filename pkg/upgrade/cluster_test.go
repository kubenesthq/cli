package upgrade

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/stages"
)

// secondLaptop points HOME at an empty directory, so every journal lookup
// (stages.JournalDir, stages.JournalPath) finds nothing — the state of the
// engineer on another laptop, or of the one whose laptop is gone.
func secondLaptop(t *testing.T, cluster string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	dir, err := stages.JournalDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("this test needs a laptop with no journals, and %s exists (err=%v)", dir, err)
	}
	path, err := stages.JournalPath("install", cluster)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("this test needs a laptop with no install journal, and %s exists (err=%v)", path, err)
	}
}

// recordServer serves the one read the resolver makes: the cluster's bundle
// record, including its host inventory.
func recordServer(t *testing.T, body map[string]any) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/bundle") {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	client, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return client
}

const inventoryClusterID = "0199a8a0-0000-7000-8000-000000000042"

func inventoryRecord(hosts []map[string]any, revision int) map[string]any {
	return map[string]any{
		"bundle_version": "1.1",
		"profiles":       []string{"observability"},
		"ha_tier":        "single-server",
		"hosts":          hosts,
		"revision":       revision,
	}
}

// A node verb on a laptop with no install journal must work: the record is the
// authority on which machines the cluster is, and this is the case that used
// to be impossible.
func TestRecordedHostsComeFromTheControlPlaneNotTheJournal(t *testing.T) {
	secondLaptop(t, "prod-1")

	client := recordServer(t, inventoryRecord([]map[string]any{
		{
			"host_id": "h-0f2c1a", "node_uid": "uid-cp-1",
			"ssh_address": "10.0.1.10", "ssh_port": 22, "ssh_user": "kubenest",
			"host_key_fingerprint":   "SHA256:0VJ0kQG3lZf8lM0oQnW4sC1b2Ck4wq5xYz6A7b8C9d0",
			"join_address":           "https://10.0.1.10:6443",
			"role":                   "server",
			"storage_device":         "/dev/disk/by-id/nvme-eui.0000000000000001",
			"volume_group_ownership": "installer-created",
			"lifecycle_state":        "active",
		},
		{
			"host_id": "h-9b7e4d", "node_uid": "uid-worker-1",
			"ssh_address": "10.0.1.20", "ssh_port": 2222, "ssh_user": "ubuntu",
			"host_key_fingerprint":   "SHA256:1WJ0kQG3lZf8lM0oQnW4sC1b2Ck4wq5xYz6A7b8C9d1",
			"join_address":           "https://10.0.1.10:6443",
			"role":                   "agent",
			"storage_device":         "",
			"volume_group_ownership": "customer-created",
			"lifecycle_state":        "active",
		},
	}, 7))

	inv, err := ResolveHosts(context.Background(), ControlPlaneRecords{Client: client, ClusterID: inventoryClusterID})
	if err != nil {
		t.Fatalf("resolving the cluster's hosts from its record: %v", err)
	}
	if len(inv.Hosts) != 2 {
		t.Fatalf("resolved %d hosts, want the 2 the record carries: %+v", len(inv.Hosts), inv.Hosts)
	}
	if inv.Revision != 7 {
		t.Errorf("resolved revision %d, want 7 — a node verb writes its change back at the revision it read", inv.Revision)
	}
	if inv.Hosts[0].HostID != "h-0f2c1a" || inv.Hosts[0].NodeUID != "uid-cp-1" {
		t.Errorf("first host = %+v, want the recorded host ID and Node UID", inv.Hosts[0])
	}
	if inv.Hosts[1].SSHAddress != "10.0.1.20" || inv.Hosts[1].SSHPort != 2222 || inv.Hosts[1].Role != "agent" {
		t.Errorf("second host = %+v, want the recorded agent at 10.0.1.20:2222", inv.Hosts[1])
	}
}

// A record with no inventory is refused. The alternative — falling back to
// --server/--agent — would act on whatever an operator typed, which is the
// one thing the inventory exists to stop.
func TestARecordWithNoHostInventoryIsRefusedRatherThanGuessed(t *testing.T) {
	secondLaptop(t, "prod-1")

	for _, hosts := range [][]map[string]any{nil, {}} {
		client := recordServer(t, inventoryRecord(hosts, 3))
		inv, err := ResolveHosts(context.Background(), ControlPlaneRecords{Client: client, ClusterID: inventoryClusterID})
		if err == nil {
			t.Fatalf("a record with hosts=%v resolved to %+v; it must be refused", hosts, inv)
		}
		if !strings.Contains(err.Error(), "no host inventory") {
			t.Errorf("the refusal does not say what is missing: %v", err)
		}
		if inv.Hosts != nil {
			t.Errorf("a refused resolution still returned hosts: %+v", inv.Hosts)
		}
	}
}

// windowServer serves the one read the window loader makes.
func windowServer(t *testing.T, body map[string]any) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/maintenance-window") {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	c, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The gate reads the window from the control plane, so the loader has three
// distinct answers and they must not collapse into two: a stored window, NO
// window (nil, which the gate refuses), and a window it cannot read or cannot
// represent (an error, which the gate also refuses, naming why).
func TestRecordsWindowDistinguishesStoredFromAbsentFromUnreadable(t *testing.T) {
	t.Run("a stored window is parsed", func(t *testing.T) {
		records := ControlPlaneRecords{Client: windowServer(t, map[string]any{
			"window": map[string]any{"days": []string{"sat"}, "start": "02:00", "end": "06:00", "timezone": "Asia/Kolkata"},
			"revision": 4, "state": "active", "applied_revision": 4,
		}), ClusterID: "c1"}

		w, err := records.Window(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if w == nil {
			t.Fatal("a stored window must come back parsed")
		}
		if w.Location == nil || w.Location.String() != "Asia/Kolkata" {
			t.Errorf("location = %v, want the stored IANA name", w.Location)
		}
		if !w.Contains(time.Date(2026, 8, 21, 21, 0, 0, 0, time.UTC)) {
			t.Error("Saturday 02:30 IST is inside the stored window")
		}
	})

	t.Run("no stored window is nil, not zero", func(t *testing.T) {
		records := ControlPlaneRecords{Client: windowServer(t, map[string]any{
			"window": nil, "revision": nil, "state": "stored",
		}), ClusterID: "c1"}

		w, err := records.Window(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if w != nil {
			t.Fatalf("window = %+v, want nil: this cluster has none stored", w)
		}
	})

	t.Run("a window this CLI cannot represent is an error", func(t *testing.T) {
		// An offset is not an IANA name: the stored window is unreadable, which
		// is not the same answer as "no window" and must not be reported as one.
		records := ControlPlaneRecords{Client: windowServer(t, map[string]any{
			"window": map[string]any{"days": []string{"sat"}, "start": "02:00", "end": "06:00", "timezone": "+05:30"},
			"revision": 4, "state": "stored",
		}), ClusterID: "c1"}

		if _, err := records.Window(context.Background()); err == nil {
			t.Fatal("a stored window this CLI cannot represent must be an error")
		} else if !strings.Contains(err.Error(), "not one this CLI can represent") {
			t.Errorf("the error must say which failure it is, got: %v", err)
		}
	})

	t.Run("a route the control plane does not serve is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		c, _ := api.New(srv.URL)
		records := ControlPlaneRecords{Client: c, ClusterID: "c1"}

		if _, err := records.Window(context.Background()); err == nil {
			t.Fatal("an unroutable read must be an error, not a cluster without a window")
		}
	})
}
