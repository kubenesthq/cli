package install

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/recoverykit"
)

// The control plane records WHAT a kit is: when it was written, its immutable
// artifact id, and a fingerprint of each key it carries. Never a key. This
// test drives the real call against a server that reads the body field by
// field, so a mistyped JSON tag — the backend's request model is
// `extra="forbid"`, so a typo is a 422 at install time — cannot pass here.
func TestKitRecordCarriesFingerprintsAndNeverAKey(t *testing.T) {
	const (
		instanceID = "inst-1"
		orgID      = "org-1"
		clusterID  = "cluster-1"
		repoPw     = "repo-password-that-must-not-be-sent"
		joinToken  = "K10aaa::server:bbb-must-not-be-sent"
	)
	type received struct {
		path string
		body map[string]any
		raw  string
	}
	var got received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the record body: %v", err)
		}
		got.raw = string(raw)
		if err := json.Unmarshal([]byte(got.raw), &got.body); err != nil {
			t.Errorf("the record body is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"artifact_id":"x"}`))
	}))
	defer srv.Close()

	client, err := api.New(srv.URL, api.WithToken("cli-token"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{API: client, orgID: orgID}

	kit, err := recoverykit.New(
		recoverykit.Binding{Kind: recoverykit.KindCluster, InstanceID: instanceID, OrganisationID: orgID, ClusterID: clusterID},
		"20260925T100000Z-abcdef0123456789",
		recoverykit.Location{Endpoint: "http://minio.internal:9000", Bucket: "kubenest-backups", Region: "main", Prefix: "prod-1"},
		"velero-default-kopia",
		map[string]string{recoverykit.KeyVeleroRepoPassword: repoPw, recoverykit.KeyK3sJoinToken: joinToken},
		time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.SealTo(mustFleetRecipient(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.recordKit(context.Background(), recoverykit.KindCluster, kit.Binding, kit); err != nil {
		t.Fatalf("recording the cluster kit: %v", err)
	}

	// The route: a cluster kit under its cluster.
	if want := "/api/v1/orgs/" + orgID + "/clusters/" + clusterID + "/recovery-kits"; got.path != want {
		t.Errorf("the cluster kit went to %q, want %q", got.path, want)
	}
	// The body: every field the control plane's model requires, under the name
	// it requires it in.
	for _, field := range []string{"artifact_id", "fingerprints", "written_at", "instance_id", "cluster_id", "velero_repository_id", "s3_location"} {
		if _, ok := got.body[field]; !ok {
			t.Errorf("the record body has no %q: %v", field, got.body)
		}
	}
	if got.body["artifact_id"] != kit.ArtifactID {
		t.Errorf("artifact_id = %v, want %q", got.body["artifact_id"], kit.ArtifactID)
	}
	if got.body["velero_repository_id"] != "velero-default-kopia" {
		t.Errorf("velero_repository_id = %v", got.body["velero_repository_id"])
	}
	fingerprints, ok := got.body["fingerprints"].(map[string]any)
	if !ok {
		t.Fatalf("fingerprints is not a map: %v", got.body["fingerprints"])
	}
	for _, name := range []string{recoverykit.KeyVeleroRepoPassword, recoverykit.KeyK3sJoinToken} {
		if fingerprints[name] != kit.Fingerprints[name] {
			t.Errorf("fingerprints[%q] = %v, want %q", name, fingerprints[name], kit.Fingerprints[name])
		}
	}
	location, ok := got.body["s3_location"].(map[string]any)
	if !ok {
		t.Fatalf("s3_location is not a map: %v", got.body["s3_location"])
	}
	for _, field := range []string{"endpoint", "bucket", "region", "prefix"} {
		if _, ok := location[field]; !ok {
			t.Errorf("s3_location has no %q: %v", field, location)
		}
	}

	// And the whole point: no key, no value, and no sealed document.
	for _, secret := range []string{repoPw, joinToken} {
		if strings.Contains(got.raw, secret) {
			t.Errorf("the record body carries the key %q:\n%s", secret, got.raw)
		}
	}
	if strings.Contains(got.raw, "sealed") {
		t.Errorf("the record body carries the sealed document:\n%s", got.raw)
	}

	// The instance's own kit is an instance resource: it posts on its own
	// route, and names the management cluster so the control plane can tell
	// which cluster is not an organisation's business.
	cpKit, err := recoverykit.New(
		recoverykit.Binding{Kind: recoverykit.KindControlPlane, InstanceID: instanceID, ClusterID: clusterID},
		"20260925T100000Z-cp00",
		kit.S3Location, "",
		map[string]string{
			recoverykit.KeyEncryptionKey:  "enc-key-must-not-be-sent",
			recoverykit.KeyAgentJWTSecret: "jwt-must-not-be-sent",
			recoverykit.KeyControlPlaneCA: "-----BEGIN CERTIFICATE-----",
		}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := cpKit.SealTo(mustFleetRecipient(t)); err != nil {
		t.Fatal(err)
	}
	got = received{}
	if err := s.recordKit(context.Background(), recoverykit.KindControlPlane, cpKit.Binding, cpKit); err != nil {
		t.Fatalf("recording the control-plane kit: %v", err)
	}
	if want := "/api/v1/orgs/" + orgID + "/recovery-kits/control-plane"; got.path != want {
		t.Errorf("the control-plane kit went to %q, want %q", got.path, want)
	}
	if got.body["cluster_id"] != clusterID {
		t.Errorf("the control-plane kit's record names cluster %v, want the management cluster %q — that is how the control plane learns which cluster is an instance resource", got.body["cluster_id"], clusterID)
	}
	for _, secret := range []string{"enc-key-must-not-be-sent", "jwt-must-not-be-sent"} {
		if strings.Contains(got.raw, secret) {
			t.Errorf("the record body carries the key %q:\n%s", secret, got.raw)
		}
	}

	// A session with no control-plane client refuses rather than silently
	// skipping: an unrecorded kit is one nothing will offer to export.
	blind := &Session{}
	err = blind.recordKit(context.Background(), recoverykit.KindCluster, kit.Binding, kit)
	if err == nil {
		t.Fatal("recording a kit with no control-plane client must fail, not be skipped")
	}
	if !strings.Contains(err.Error(), "export") {
		t.Errorf("the refusal must say what is lost: %v", err)
	}
}

// mustFleetRecipient is a throwaway fleet recipient: a generated key whose
// public half is used and whose private half is discarded, because these tests
// never need to open what they seal.
func mustFleetRecipient(t *testing.T) string {
	t.Helper()
	key, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	return key.Recipient()
}
