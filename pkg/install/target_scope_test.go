package install

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
)

func TestInstallLoadsTheRecordedPeerPrefixBeforeConfiguringBackup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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
	session := &Session{API: client, Jnl: &Journal{ClusterID: "own"}, orgID: "org"}
	target := backup.Target{Endpoint: "store.example", Bucket: "shared", Prefix: "prod-1"}
	peers, err := stageKnownSiblingPrefixes(context.Background(), session, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0] != "prod-2" {
		t.Errorf("install peer probes = %v, want the recorded real sibling", peers)
	}
	recovery := &Session{Opts: Options{Recovery: &RecoveryOptions{}}, Jnl: &Journal{ClusterID: "own"}}
	peers, err = stageKnownSiblingPrefixes(context.Background(), recovery, target)
	if err != nil || len(peers) != 0 {
		t.Errorf("a recovery must not need the unavailable control plane (peers %v, error %v)", peers, err)
	}
}
