package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
)

func TestSetTargetReadsTheActualPeerPrefixFromTheControlPlane(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/orgs":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "org", "name": "org"}})
		case "/api/v1/orgs/org/clusters":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
				{"id": "own", "name": "prod-1"}, {"id": "peer", "name": "prod-2"},
			}})
		case "/api/v1/clusters/own":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "own", "name": "prod-1", "org_id": "org"})
		case "/api/v1/orgs/org/recovery-kits/locations":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"cluster_id": "own", "s3_location": map[string]string{"endpoint": "store.example", "bucket": "shared", "prefix": "prod-1"}},
				{"cluster_id": "peer", "s3_location": map[string]string{"endpoint": "store.example", "bucket": "shared", "prefix": "prod-2"}},
			}})
		case "/api/v1/orgs/org/clusters/peer/recovery-kits":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected control-plane request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := api.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	target := backup.Target{Endpoint: "store.example", Bucket: "shared", Prefix: "prod-1"}
	peers, err := setTargetSiblingPrefixes(context.Background(), client, "prod-1", target)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0] != "prod-2" {
		t.Errorf("set-target peer probes = %v, want the recorded real sibling", peers)
	}
}
