package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestKitLocationsForOtherClustersReadsEveryRecordedPeerAndFailsClosed(t *testing.T) {
	denied := false
	locationReads := 0
	client, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/orgs/org-1/recovery-kits/locations":
			locationReads++
			if denied {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"cluster_id": "own", "s3_location": nil},
				{"cluster_id": "peer", "s3_location": map[string]string{"endpoint": "store.example", "bucket": "shared", "prefix": "prod-2"}},
				{"cluster_id": "peer", "s3_location": map[string]string{"endpoint": "store.example", "bucket": "shared", "prefix": "old-prod-2"}},
				{"cluster_id": "deleted-peer", "s3_location": map[string]string{"endpoint": "store.example", "bucket": "shared", "prefix": "retired-prod-3"}},
			}})
		case strings.Contains(r.URL.Path, "/clusters/") && strings.HasSuffix(r.URL.Path, "/recovery-kits"):
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	locations, err := client.KitLocationsForOtherClusters(context.Background(), "org-1", "own")
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 3 || locations[0].Prefix != "prod-2" || locations[1].Prefix != "old-prod-2" || locations[2].Prefix != "retired-prod-3" {
		t.Errorf("peer kit locations = %+v, want current, older, and deleted-cluster prefixes", locations)
	}
	if locationReads != 1 {
		t.Errorf("org kit-location reads = %d, want one", locationReads)
	}
	denied = true
	if _, err := client.KitLocationsForOtherClusters(context.Background(), "org-1", "own"); err == nil {
		t.Error("an unreadable org kit-location list must block the scope check")
	}
	if locationReads != 2 {
		t.Errorf("org kit-location reads after denial = %d, want two", locationReads)
	}
}

func TestKitLocationsForOtherClustersRejectsAnIncompleteFleetAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   any
	}{
		{"missing data", http.StatusOK, map[string]any{}},
		{"null location", http.StatusOK, map[string]any{"data": []map[string]any{{"cluster_id": "peer", "s3_location": nil}}}},
		{"incomplete location", http.StatusOK, map[string]any{"data": []map[string]any{{"cluster_id": "peer", "s3_location": map[string]string{"endpoint": "store.example", "bucket": "shared"}}}}},
		{"missing cluster id", http.StatusOK, map[string]any{"data": []map[string]any{{"s3_location": map[string]string{"endpoint": "store.example", "bucket": "shared", "prefix": "peer"}}}}},
		{"unauthorized", http.StatusUnauthorized, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locationReads := 0
			client, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/orgs/org-1/recovery-kits/locations":
					locationReads++
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(tc.body)
				case strings.Contains(r.URL.Path, "/clusters/") && strings.HasSuffix(r.URL.Path, "/recovery-kits"):
					w.WriteHeader(http.StatusUnauthorized)
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			if _, err := client.KitLocationsForOtherClusters(context.Background(), "org-1", "own"); err == nil {
				t.Error("an incomplete or denied location response must block the scope check")
			}
			if locationReads != 1 {
				t.Errorf("org kit-location reads = %d, want one", locationReads)
			}
		})
	}
}
