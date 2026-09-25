package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"net/url"

	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
)

const kitTestBucket = "kubenest-backups"

// readOnlyBucket answers GETs from a map the way an S3-compatible store does,
// path-style. The check is exercised through the real SigV4 client rather than
// a stub, so a test that passes proves the check reads the bucket, not that it
// called its own fake.
func readOnlyBucket(t *testing.T, objects map[string][]byte) (*s3.Client, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/"+kitTestBucket+"/")
		body, ok := objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
			return
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	client, err := s3.New(s3.Config{
		Endpoint: srv.URL, Bucket: kitTestBucket, Region: "main",
		AccessKeyID: "AKTEST", SecretAccessKey: "sekret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, srv.URL
}

// targetFlagFor is the --target the command would be given by an operator who
// knows the bucket and the cluster prefix.
func targetFlagFor(endpoint string) string {
	query := url.Values{}
	query.Set("endpoint", endpoint)
	query.Set("region", "main")
	return "s3://" + kitTestBucket + "/" + kitScope + "?" + query.Encode()
}

const (
	kitCluster    = "cluster-1"
	kitOtherOrgs  = "org-1"
	kitInstance   = "inst-1"
	kitScope      = "prod-1"
	kitArtifactID = "20260925T100000Z-abcdef0123456789"
)

func kitBindingFor(cluster string) recoverykit.Binding {
	return recoverykit.Binding{Kind: recoverykit.KindCluster, InstanceID: kitInstance, OrganisationID: kitOtherOrgs, ClusterID: cluster}
}

// TestRecoveryKitCheckRefusesAForeignSet: a kit or a recovery set that belongs
// to another cluster is refused, and the refusal names the instance,
// organisation and cluster it does belong to.
func TestRecoveryKitCheckRefusesAForeignSet(t *testing.T) {
	ctx := context.Background()
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	other, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}

	localKit, localDoc := sealedKit(t, fleet, kitCluster)
	// The bucket holds our kit, and a set that is about another cluster: the
	// shape a confused bucket or a mis-typed --cluster produces.
	foreignKit, foreignDoc := sealedKit(t, other, "cluster-9")
	foreignKit.Binding.InstanceID = "inst-2"
	foreignSet, err := recoverykit.NewSet(foreignKit, map[string]string{"bundle": "1.2"}, recoverykit.Digest(foreignDoc), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	foreignSet.Complete = true
	foreignSetDoc, err := foreignSet.Document()
	if err != nil {
		t.Fatal(err)
	}

	store, _ := readOnlyBucket(t, map[string][]byte{
		recoverykit.KitKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): localDoc,
		// The foreign set sits at OUR key: the path says this cluster, the
		// contents say another. That disagreement is exactly what must be
		// refused before anything is reported.
		recoverykit.SetKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): foreignSetDoc,
	})

	_, err = runKitCheck(ctx, kitCheck{
		Local: localDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: fleet.SecretKeyString(), Store: store,
	})
	if err == nil {
		t.Fatal("a recovery set written for another cluster must be refused")
	}
	if !errors.Is(err, recoverykit.ErrForeign) {
		t.Errorf("the refusal must be the foreign-artifact one, not a generic failure: %v", err)
	}
	for _, want := range []string{"inst-2", "cluster-9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q — where the set DOES belong: %v", want, err)
		}
	}

	// And a kit held for a different artifact than the one being checked is
	// refused: checking one kit while holding another reports about the wrong
	// thing.
	mismatched := *localKit
	mismatched.ArtifactID = "20260926T100000Z-9999"
	mismatchedDoc, err := mismatched.Document()
	if err != nil {
		t.Fatal(err)
	}
	_, err = runKitCheck(ctx, kitCheck{
		Local: mismatchedDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: fleet.SecretKeyString(), Store: store,
	})
	if err == nil {
		t.Error("a local kit for another artifact must be refused")
	}

	// A local kit from another cluster is refused on ITS OWN evidence — the
	// bucket here holds a set that belongs to this cluster, so the refusal can
	// only come from the local copy. (With the store above it could not: that
	// set is foreign too, and would refuse first.)
	_, err = runKitCheck(ctx, kitCheck{
		Local: foreignDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: other.SecretKeyString(), Store: consistentStore(t, localDoc, localKit),
	})
	if err == nil || !errors.Is(err, recoverykit.ErrForeign) {
		t.Fatalf("a local kit from another cluster must be refused: %v", err)
	}
	if !strings.Contains(err.Error(), "cluster-9") {
		t.Errorf("that refusal must name the cluster it belongs to: %v", err)
	}

	// The positive control: the same bucket with a set that belongs here is
	// not refused, so the refusals above are about the contents and not about
	// the store being unreachable.
	ourSet, err := recoverykit.NewSet(localKit, map[string]string{"bundle": "1.2"}, recoverykit.Digest(localDoc), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ourSet.Complete = true
	ourSet, err = ourSet.WithBackup(recoverykit.Backup{Name: "daily-good", Status: "Completed", CompletedAt: time.Now().UTC(), Coverage: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	ourSetDoc, err := ourSet.Document()
	if err != nil {
		t.Fatal(err)
	}
	good, _ := readOnlyBucket(t, map[string][]byte{
		recoverykit.KitKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): localDoc,
		recoverykit.SetKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): ourSetDoc,
	})
	answers, err := runKitCheck(ctx, kitCheck{
		Local: localDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: fleet.SecretKeyString(), Backup: "daily-good", Store: good,
	})
	if err != nil {
		t.Fatalf("a consistent kit and set must not be refused: %v", err)
	}
	if !answers.AllGood() {
		t.Errorf("a consistent kit and set must answer yes to all four: %+v", answers)
	}
}

// TestRecoveryKitCheckReportsEachAnswerSeparately: the four answers are
// independent, and the failing one is named. A wrong key is reported as "does
// not decrypt", never as a damaged upload or an incomplete set; a damaged
// upload is reported as such while the key is shown to be fine.
func TestRecoveryKitCheckReportsEachAnswerSeparately(t *testing.T) {
	ctx := context.Background()
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	localKit, localDoc := sealedKit(t, fleet, kitCluster)

	set, err := recoverykit.NewSet(localKit, map[string]string{"bundle": "1.2"}, recoverykit.Digest(localDoc), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	set.Complete = true
	set, err = set.WithBackup(recoverykit.Backup{Name: "daily-good", Status: "Completed", CompletedAt: time.Now().UTC(), Coverage: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	setDoc, err := set.Document()
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string][]byte{
		recoverykit.KitKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): localDoc,
		recoverykit.SetKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): setDoc,
	}

	// The wrong key. The bytes in the bucket are the bytes that were written,
	// so the upload answer is YES; the key is what fails.
	answers, err := runKitCheck(ctx, kitCheck{
		Local: localDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: wrong.SecretKeyString(), Backup: "daily-good", Store: mustBucket(t, keys),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !answers.Upload.OK {
		t.Errorf("the upload IS intact — the wrong key must not be reported as a damaged upload: %+v", answers.Upload)
	}
	if answers.Decryption.OK {
		t.Errorf("a key that does not open the kit must be reported as such: %+v", answers.Decryption)
	}
	if !strings.Contains(strings.ToLower(answers.Decryption.Detail), "does not open") {
		t.Errorf("the decryption answer must say the key does not open the kit, got %q", answers.Decryption.Detail)
	}
	if answers.AllGood() {
		t.Error("a check that could not open the kit must not read as good")
	}
	// Conflation in the other direction is the same bug: the set is complete
	// and the fingerprints were not the problem, so neither may be blamed.
	if !answers.Set.OK {
		t.Errorf("the set is complete and belongs here; a wrong key must not be reported against it: %+v", answers.Set)
	}

	// A HEADER-only change: the bucket no longer holds the bytes that were
	// written, and the key still opens the payload. Reported as a damaged
	// upload, with the key answer left standing on its own.
	headerMutated := append([]byte(nil), localDoc...)
	headerMutated = bytes.Replace(headerMutated, []byte("minio.internal"), []byte("minio.internai"), 1)
	if bytes.Equal(headerMutated, localDoc) {
		t.Fatal("the header mutation did not change the document, so this case proves nothing")
	}
	headerKeys := damagedKeysFor(keys, headerMutated)
	answers, err = runKitCheck(ctx, kitCheck{
		Local: localDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: fleet.SecretKeyString(), Backup: "daily-good", Store: mustBucket(t, headerKeys),
	})
	if err != nil {
		t.Fatal(err)
	}
	if answers.Upload.OK {
		t.Errorf("a header-only change must not read as an intact upload: %+v", answers.Upload)
	}
	if !answers.Decryption.OK {
		t.Errorf("the key DOES open this kit; a changed header must not be reported against the key: %+v", answers.Decryption)
	}
	if answers.AllGood() {
		t.Error("a changed upload must not read as good")
	}

	// A change inside the sealed payload: the bytes are not the kit AND the
	// key no longer opens what is in the bucket. Both answers are no, and the
	// decryption line names the uploaded copy rather than blaming the set.
	sealedMutated := mutateSealed(t, localDoc)
	sealedKeys := damagedKeysFor(keys, sealedMutated)
	answers, err = runKitCheck(ctx, kitCheck{
		Local: localDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: fleet.SecretKeyString(), Backup: "daily-good", Store: mustBucket(t, sealedKeys),
	})
	if err != nil {
		t.Fatal(err)
	}
	if answers.Upload.OK {
		t.Errorf("a mutated payload must not read as intact: %+v", answers.Upload)
	}
	if answers.Decryption.OK {
		t.Errorf("nothing may be reported as decrypting when the copy in the bucket is not the kit: %+v", answers.Decryption)
	}
	if !strings.Contains(answers.Decryption.Detail, "uploaded") {
		t.Errorf("the decryption answer must say WHICH copy did not open: %q", answers.Decryption.Detail)
	}
	if !answers.Set.OK {
		t.Errorf("the set is complete and belongs here; a damaged kit must not be reported against it: %+v", answers.Set)
	}

	// Garbage in the bucket: not even a kit. Reported against the upload and
	// the key, never as a set problem.
	garbage := []byte("{not a kit at all")
	garbageKeys := damagedKeysFor(keys, garbage)
	answers, err = runKitCheck(ctx, kitCheck{
		Local: localDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: fleet.SecretKeyString(), Backup: "daily-good", Store: mustBucket(t, garbageKeys),
	})
	if err != nil {
		t.Fatal(err)
	}
	if answers.Upload.OK || answers.Decryption.OK || answers.AllGood() {
		t.Errorf("an object that is not a kit must fail the upload and the key answers: %+v", answers)
	}
	if !answers.Set.OK {
		t.Errorf("an unreadable kit object must not be reported against the set: %+v", answers.Set)
	}

	// An upload that never completed: the set says so, and no answer may be
	// invented from it.
	incomplete := *set
	incomplete.Complete = false
	incompleteDoc, err := incomplete.Document()
	if err != nil {
		t.Fatal(err)
	}
	incompleteKeys := map[string][]byte{
		recoverykit.KitKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): localDoc,
		recoverykit.SetKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): incompleteDoc,
	}
	answers, err = runKitCheck(ctx, kitCheck{
		Local: localDoc, ArtifactID: kitArtifactID, Scope: kitScope, ClusterID: kitCluster,
		Kind: recoverykit.KindCluster, Expected: kitBindingFor(kitCluster),
		FleetKey: fleet.SecretKeyString(), Backup: "daily-good", Store: mustBucket(t, incompleteKeys),
	})
	if err != nil {
		t.Fatal(err)
	}
	if answers.Set.OK {
		t.Errorf("an incomplete upload must not be offered as a set to recover from: %+v", answers.Set)
	}
	if !answers.Upload.OK || !answers.Decryption.OK {
		t.Errorf("an incomplete SET must not be reported against the kit's own answers: %+v", answers)
	}

	// And the command itself reports all four, separately, through the real
	// client, with a non-zero exit when one is no.
	kitFile := filepath.Join(t.TempDir(), "kit.json")
	if err := os.WriteFile(kitFile, localDoc, 0o600); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "fleet-key.txt")
	if err := os.WriteFile(keyFile, []byte(wrong.SecretKeyString()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The command builds its own client from --target, so it is pointed at the
	// same httptest endpoint through the flag rather than through the seam.
	t.Setenv("KUBENEST_BACKUP_ACCESS_KEY_ID", "AKTEST")
	t.Setenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "sekret")

	_, endpoint := readOnlyBucket(t, keys)
	cmd := newRecoveryKitCheckCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"--kit-file", kitFile,
		"--fleet-key-file", keyFile,
		"--cluster", kitCluster,
		"--instance", kitInstance,
		"--organisation", kitOtherOrgs,
		"--backup", "daily-good",
		"--target", targetFlagFor(endpoint),
	})
	cmd.SetContext(ctx)
	if err := cmd.Execute(); err == nil {
		t.Error("a check that cannot open the kit must exit non-zero")
	}
	text := out.String()
	for _, want := range []string{"kit upload intact", "opens with the key supplied", "fingerprints match", "recovery set complete and belongs here", "Only a completed restore proves restoration"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report must name %q separately:\n%s", want, text)
		}
	}
}

// sealedKit builds one cluster kit sealed to the fleet key.
func sealedKit(t *testing.T, fleet *recoverykit.FleetKey, cluster string) (*recoverykit.Kit, []byte) {
	t.Helper()
	kit, err := recoverykit.New(kitBindingFor(cluster), kitArtifactID,
		recoverykit.Location{Endpoint: "http://minio.internal:9000", Bucket: kitTestBucket, Region: "main", Prefix: kitScope},
		"velero-default-kopia",
		map[string]string{
			recoverykit.KeyVeleroRepoPassword: "repo-pw-1",
			recoverykit.KeyK3sJoinToken:       "K10aaa::server:bbb",
		}, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.SealTo(fleet.Recipient()); err != nil {
		t.Fatal(err)
	}
	doc, err := kit.Document()
	if err != nil {
		t.Fatal(err)
	}
	return kit, doc
}

// consistentStore is a bucket holding this cluster's kit and a complete set
// that agrees with it, so a refusal found against it can only be about the
// local copy the caller supplied.
func consistentStore(t *testing.T, kitDoc []byte, kit *recoverykit.Kit) *s3.Client {
	t.Helper()
	set, err := recoverykit.NewSet(kit, map[string]string{"bundle": "1.2"}, recoverykit.Digest(kitDoc), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	set.Complete = true
	set, err = set.WithBackup(recoverykit.Backup{Name: "daily-good", Status: "Completed", CompletedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := set.Document()
	if err != nil {
		t.Fatal(err)
	}
	client, _ := readOnlyBucket(t, map[string][]byte{
		recoverykit.KitKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): kitDoc,
		recoverykit.SetKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID): doc,
	})
	return client
}

// mustBucket is readOnlyBucket for a test that only needs the client.
func mustBucket(t *testing.T, objects map[string][]byte) *s3.Client {
	t.Helper()
	client, _ := readOnlyBucket(t, objects)
	return client
}

// damagedKeysFor is the bucket with the kit object replaced.
func damagedKeysFor(base map[string][]byte, kitDoc []byte) map[string][]byte {
	out := make(map[string][]byte, len(base))
	for k, v := range base {
		out[k] = v
	}
	out[recoverykit.KitKey(kitScope, kitCluster, recoverykit.KindCluster, kitArtifactID)] = kitDoc
	return out
}

// mutateSealed flips one byte inside the sealed payload, which is the part the
// fleet key has to open.
func mutateSealed(t *testing.T, doc []byte) []byte {
	t.Helper()
	at := bytes.Index(doc, []byte(`"sealed": "`))
	if at < 0 {
		t.Fatal("the kit document has no sealed payload")
	}
	out := append([]byte(nil), doc...)
	i := at + len(`"sealed": "`) + 8
	if i >= len(out) {
		t.Fatal("the sealed payload is too short to mutate")
	}
	out[i] ^= 0x01
	return out
}
