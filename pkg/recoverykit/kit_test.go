package recoverykit

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/operation"
)

var testNow = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

// Sealer is the second of the two interfaces pkg/operation declares for the
// off-cluster record copy, and it is satisfied here rather than in
// pkg/operation: a later install seals to the fleet recipient without ever
// holding a private key, which is the whole point of the recipient being the
// only half that is kept. (*s3.Client satisfies ObjectWriter, asserted in
// pkg/s3.)
var _ operation.Sealer = Sealer{}

func clusterBinding(cluster string) Binding {
	return Binding{Kind: KindCluster, InstanceID: "inst-1", OrganisationID: "org-1", ClusterID: cluster}
}

func controlPlaneBinding() Binding {
	return Binding{Kind: KindControlPlane, InstanceID: "inst-1"}
}

func testLocation() Location {
	return Location{Endpoint: "http://minio.internal:9000", Bucket: "kubenest-backups", Region: "main", Prefix: "prod-1"}
}

func clusterSecrets(repoPassword, joinToken string) map[string]string {
	return map[string]string{
		KeyVeleroRepoPassword: repoPassword,
		KeyK3sJoinToken:       joinToken,
	}
}

func controlPlaneSecrets(enc, jwt, ca string) map[string]string {
	return map[string]string{
		KeyEncryptionKey:  enc,
		KeyAgentJWTSecret: jwt,
		KeyControlPlaneCA: ca,
	}
}

func mustSealedClusterKit(t *testing.T, fleet *FleetKey, cluster string) *Kit {
	t.Helper()
	kit, err := New(clusterBinding(cluster), "20260925T100000Z-abcdef0123456789", testLocation(), "velero-default-kopia",
		clusterSecrets("repo-pw-1", "K10aaa::server:bbb"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.SealTo(fleet.Recipient()); err != nil {
		t.Fatal(err)
	}
	return kit
}

// Every kit written by an install opens with the fleet key, and the same kit
// does not open with any other key. The negative half is the one that matters:
// a "sealed" kit that opened with anything would be a kit in name only.
func TestKitOpensWithFleetKeyAndNotWithout(t *testing.T) {
	fleet, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	other, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	kit := mustSealedClusterKit(t, fleet, "cluster-1")

	secrets, err := kit.Secrets(fleet.SecretKeyString())
	if err != nil {
		t.Fatalf("the kit must open with the fleet key that sealed it: %v", err)
	}
	if secrets[KeyVeleroRepoPassword] != "repo-pw-1" || secrets[KeyK3sJoinToken] != "K10aaa::server:bbb" {
		t.Errorf("the kit opened to the wrong contents: %v", secrets)
	}
	if err := kit.FingerprintsMatch(secrets); err != nil {
		t.Errorf("the kit's own fingerprints must match the keys it carries: %v", err)
	}

	// The planted negative: a different fleet key must not open it, and the
	// failure must be the decryption one, so a check can report it as such
	// rather than as a damaged upload or an incomplete set.
	if _, err := kit.Secrets(other.SecretKeyString()); err == nil {
		t.Fatal("a kit sealed to one fleet key opened with another: the sealing is not doing anything")
	} else if !errors.Is(err, ErrDoesNotDecrypt) {
		t.Errorf("a wrong key must be reported as does-not-decrypt, got %v", err)
	}

	// A garbage key is refused before it is ever handed to the cipher.
	if _, err := kit.Secrets("not-a-key"); err == nil {
		t.Error("a non-key input must be refused")
	}
}

// A later install holds only the public recipient. It must be able to write a
// kit that the fleet key opens, and it must be able to prove its upload is
// intact by digest — without claiming to have decrypted anything, and without
// ever being asked for the private key.
func TestLaterInstallVerifiesByDigestWithoutThePrivateKey(t *testing.T) {
	fleet, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}

	// This is all a later install has: the recipient. It is also exactly what
	// pkg/operation's Sealer is handed for the off-cluster record copy.
	later := Sealer{Recipient: fleet.Recipient()}
	record, err := later.Seal([]byte(`{"operation_id":"op-1"}`))
	if err != nil {
		t.Fatalf("a later install must be able to seal with the recipient alone: %v", err)
	}
	if _, err := Open(fleet.SecretKeyString(), record); err != nil {
		t.Fatalf("what a later install sealed must open with the fleet key: %v", err)
	}

	kit, err := New(clusterBinding("cluster-1"), "20260925T100000Z-1111", testLocation(), "velero-default-kopia",
		clusterSecrets("repo-pw-2", "K10ccc::server:ddd"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.SealTo(later.Recipient); err != nil {
		t.Fatalf("the kit must seal to the recipient alone: %v", err)
	}

	doc, err := kit.Document()
	if err != nil {
		t.Fatal(err)
	}
	local := Digest(doc)

	// The upload: the very same bytes land in the bucket, and the fetched
	// copy digests the same. Nothing here needs a key.
	fetched := append([]byte(nil), doc...)
	if got := Digest(fetched); got != local {
		t.Fatalf("a byte-identical upload digested %q locally and %q fetched", local, got)
	}

	// The positive control for the digest above: the object really is the
	// sealed kit, and the fleet key opens it. Without this, the comparison
	// would pass just as well on a Sealer that emitted nothing.
	back, err := Load(fetched)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := back.Secrets(fleet.SecretKeyString())
	if err != nil {
		t.Fatalf("what a later install sealed must open with the fleet key: %v", err)
	}
	if secrets[KeyVeleroRepoPassword] != "repo-pw-2" {
		t.Errorf("opened %v", secrets)
	}
	if strings.Contains(string(doc), "repo-pw-2") || strings.Contains(string(doc), "K10ccc::server:ddd") {
		t.Errorf("a sealed kit document carries its secrets in plaintext:\n%s", doc)
	}

	// A damaged upload must NOT digest the same, so the check is not vacuous.
	damaged := append([]byte(nil), doc...)
	damaged[len(damaged)/2] ^= 0x01
	if Digest(damaged) == local {
		t.Fatal("a mutated upload digested the same as the original")
	}

	// The later install cannot open what it wrote. There is no method through
	// which it could claim to have: Sealer has none, and the recipient is not
	// an identity.
	if _, err := Open(later.Recipient, doc); err == nil {
		t.Error("a public recipient must not open a kit")
	}
}

// A cluster kit carries the cluster's Velero repository password and join
// token and nothing that opens the control plane. The refusal is structural:
// the kit cannot be built carrying control-plane material in the first place,
// and what it does carry never appears in plaintext in the document.
func TestClusterKitCarriesNoControlPlaneKey(t *testing.T) {
	fleet, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}

	// Planted negative: a cluster kit asked to carry the control plane's key
	// material is refused, naming the key.
	for _, name := range ControlPlaneSecrets {
		secrets := clusterSecrets("repo-pw", "K10aaa::server:bbb")
		secrets[name] = "cp-secret-value-must-not-leave"
		if _, err := New(clusterBinding("cluster-1"), "art-1", testLocation(), "", secrets, testNow); err == nil {
			t.Errorf("a cluster kit carrying %q must be refused", name)
		} else if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal must name %q, got %v", name, err)
		}
	}
	// And it must carry the two things a recovery cannot proceed without.
	for _, name := range ClusterSecrets {
		secrets := clusterSecrets("repo-pw", "K10aaa::server:bbb")
		delete(secrets, name)
		if _, err := New(clusterBinding("cluster-1"), "art-1", testLocation(), "", secrets, testNow); err == nil {
			t.Errorf("a cluster kit with no %q must be refused", name)
		}
	}

	kit := mustSealedClusterKit(t, fleet, "cluster-1")
	doc, err := kit.Document()
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	for _, name := range ControlPlaneSecrets {
		if strings.Contains(text, name) {
			t.Errorf("a cluster kit's document mentions the control-plane key %q:\n%s", name, text)
		}
	}
	// The positive control: it does fingerprint what it carries, so the checks
	// above are not passing because the header is empty.
	for _, name := range ClusterSecrets {
		if kit.Fingerprints[name] == "" {
			t.Errorf("the kit records no fingerprint for %q", name)
		}
	}

	// The other direction: a control-plane kit must not carry cluster material
	// either, so the two scopes cannot be collapsed into one.
	cp := controlPlaneSecrets("enc-1", "jwt-1", "-----BEGIN CERTIFICATE-----")
	cp[KeyK3sJoinToken] = "K10aaa::server:bbb"
	if _, err := New(controlPlaneBinding(), "art-cp", testLocation(), "", cp, testNow); err == nil {
		t.Error("a control-plane kit carrying a cluster join token must be refused")
	}
	cpOnly := controlPlaneSecrets("enc-1", "jwt-1", "-----BEGIN CERTIFICATE-----")
	good, err := New(controlPlaneBinding(), "art-cp", testLocation(), "", cpOnly, testNow)
	if err != nil {
		t.Fatalf("a control-plane kit with its three keys must be accepted: %v", err)
	}
	if err := good.SealTo(fleet.Recipient()); err != nil {
		t.Fatal(err)
	}
	cpDoc, err := good.Document()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cpDoc), KeyEncryptionKey) {
		t.Errorf("the control-plane kit does not name the keys it carries:\n%s", cpDoc)
	}
	for _, secret := range []string{"enc-1", "jwt-1"} {
		if strings.Contains(string(cpDoc), secret) {
			t.Errorf("the control-plane kit carries %q in plaintext", secret)
		}
	}
}

// A fingerprint is stable (the same key fingerprints the same across installs)
// and is not the key: it does not contain it, is not equal to it, and is
// separated by key name so one value cannot stand in for another.
func TestFingerprintsAreStableAndNotTheKeys(t *testing.T) {
	const name, value = KeyVeleroRepoPassword, "repo-password-that-must-not-appear"

	first, second := Fingerprint(name, value), Fingerprint(name, value)
	if first != second {
		t.Fatalf("the same key fingerprinted %q then %q", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Errorf("fingerprint = %q, want sha256: and 64 hex characters", first)
	}
	if strings.Contains(first, value) || first == value {
		t.Errorf("the fingerprint contains the key it is of: %q", first)
	}
	if sameValueAnotherName := Fingerprint(KeyK3sJoinToken, value); sameValueAnotherName == first {
		t.Error("one value under two key names fingerprinted the same, so a fingerprint does not say which key it is of")
	}
	if different := Fingerprint(name, value+"x"); different == first {
		t.Error("two different values fingerprinted the same")
	}

	// And the kit records fingerprints of what it carries — never the values.
	fleet, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	kit := mustSealedClusterKit(t, fleet, "cluster-1")
	if kit.Fingerprints[KeyVeleroRepoPassword] != Fingerprint(KeyVeleroRepoPassword, "repo-pw-1") {
		t.Errorf("the kit's fingerprint of its repository password is not the fingerprint of that password: %v", kit.Fingerprints)
	}
	if kit.Fingerprints[KeyK3sJoinToken] != Fingerprint(KeyK3sJoinToken, "K10aaa::server:bbb") {
		t.Errorf("the kit's fingerprint of its join token is not the fingerprint of that token: %v", kit.Fingerprints)
	}
}

// A kit or recovery set belonging to another cluster is refused, and the
// refusal names the instance, organisation and cluster it does belong to —
// "wrong cluster" without the name is the actionable half missing.
func TestRefusesKitFromAnotherCluster(t *testing.T) {
	fleet, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	foreign := mustSealedClusterKit(t, fleet, "cluster-9")
	foreign.Binding.InstanceID = "inst-2"
	foreign.Binding.OrganisationID = "org-7"

	err = foreign.Verify(clusterBinding("cluster-1"))
	if err == nil {
		t.Fatal("a kit from another cluster must be refused")
	}
	if !errors.Is(err, ErrForeign) {
		t.Errorf("the refusal must be distinguishable from an unreadable artifact, got %v", err)
	}
	for _, want := range []string{"inst-2", "org-7", "cluster-9", "inst-1", "cluster-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %v", want, err)
		}
	}
	if err := foreign.Verify(foreign.Binding); err != nil {
		t.Errorf("the kit must verify against its own binding: %v", err)
	}

	// A control-plane kit is not a cluster kit, even with matching ids.
	cp, err := New(controlPlaneBinding(), "art-cp", testLocation(), "", controlPlaneSecrets("enc", "jwt", "ca"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Verify(clusterBinding("cluster-1")); err == nil {
		t.Error("a control-plane kit must be refused where a cluster kit is expected")
	}

	// And a recovery set from another cluster is refused the same way.
	doc, err := foreign.Document()
	if err != nil {
		t.Fatal(err)
	}
	set, err := NewSet(foreign, map[string]string{"bundle": "1.2"}, Digest(doc), testNow)
	if err != nil {
		t.Fatal(err)
	}
	set.Complete = true
	if err := set.Verify(clusterBinding("cluster-1")); err == nil {
		t.Fatal("a recovery set from another cluster must be refused")
	} else if !errors.Is(err, ErrForeign) || !strings.Contains(err.Error(), "cluster-9") {
		t.Errorf("the set's refusal must wrap ErrForeign and name the cluster it belongs to: %v", err)
	}
	if err := set.Verify(foreign.Binding); err != nil {
		t.Errorf("the set must verify against its own cluster: %v", err)
	}
}

// A backup that did not complete is never recorded in a set and never offered
// as the latest a recovery may start from: a partial or failed upload is not
// something to restore.
func TestIncompleteBackupIsNeverEligible(t *testing.T) {
	fleet, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	kit := mustSealedClusterKit(t, fleet, "cluster-1")
	doc, err := kit.Document()
	if err != nil {
		t.Fatal(err)
	}
	set, err := NewSet(kit, map[string]string{"bundle": "1.2"}, Digest(doc), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.WithBackup(Backup{Name: "daily-x", Status: "PartiallyFailed", CompletedAt: testNow}); err == nil {
		t.Error("a partially failed backup must not be recorded")
	}
	good, err := set.WithBackup(Backup{Name: "daily-good", Status: "Completed", CompletedAt: testNow.Add(-time.Hour), Coverage: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.WithBackup(Backup{Name: "daily-bad", Status: "Failed", CompletedAt: testNow}); err == nil {
		t.Error("a failed backup must not be recorded")
	}
	latest, ok := good.Latest()
	if !ok || latest.Name != "daily-good" {
		t.Errorf("latest = %+v (found=%v), want daily-good", latest, ok)
	}

	complete := *good
	complete.Complete = true
	if !complete.Eligible() {
		t.Error("a completed set with a kit checksum must be eligible")
	}
	incomplete := *good
	incomplete.Complete = false
	if incomplete.Eligible() {
		t.Error("a set whose upload never completed must not be eligible as latest")
	}
	noChecksum := complete
	noChecksum.Checksums = nil
	if noChecksum.Eligible() {
		t.Error("a set with no kit checksum cannot prove an upload, so it must not be eligible")
	}
}

// A new kit never changes what an existing set means: the set names the kit it
// was built from by artifact id and checksum, and building another kit leaves
// it untouched.
func TestNewKitDoesNotChangeAnExistingSet(t *testing.T) {
	fleet, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	first := mustSealedClusterKit(t, fleet, "cluster-1")
	firstDoc, err := first.Document()
	if err != nil {
		t.Fatal(err)
	}
	set, err := NewSet(first, map[string]string{"bundle": "1.2"}, Digest(firstDoc), testNow)
	if err != nil {
		t.Fatal(err)
	}
	before, err := set.Document()
	if err != nil {
		t.Fatal(err)
	}

	second, err := New(clusterBinding("cluster-1"), "20260926T100000Z-2222", testLocation(), "velero-default-kopia",
		clusterSecrets("repo-pw-rotated", "K10eee::server:fff"), testNow.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := second.SealTo(fleet.Recipient()); err != nil {
		t.Fatal(err)
	}
	secondDoc, err := second.Document()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSet(second, map[string]string{"bundle": "1.2"}, Digest(secondDoc), testNow.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	after, err := set.Document()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("writing a new kit changed an existing set:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if SetKey("prod-1", "cluster-1", KindCluster, first.ArtifactID) == SetKey("prod-1", "cluster-1", KindCluster, second.ArtifactID) {
		t.Error("both kits address the same object, so the second would overwrite the first's set")
	}
	// The set must not be wearable as a different kit's set: its checksum is
	// the old kit's digest and must still be, after the new kit was written.
	if set.Checksums["kit"] != Digest(firstDoc) {
		t.Errorf("the set's kit checksum changed: %v", set.Checksums)
	}
	var parsed map[string]any
	if err := json.Unmarshal(after, &parsed); err != nil {
		t.Fatalf("the set document is not JSON: %v", err)
	}
	if parsed["artifact_id"] != first.ArtifactID {
		t.Errorf("the set now names %v, not the kit it was built from (%s)", parsed["artifact_id"], first.ArtifactID)
	}
}

// A kit's document is the plaintext header plus the sealed body, and the
// object keys put kits and sets under the scope the caller owns — the layout
// `backup set-target` protects when it probes a credential's reach.
func TestDocumentAndObjectKeys(t *testing.T) {
	fleet, err := GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	kit := mustSealedClusterKit(t, fleet, "cluster-1")
	doc, err := kit.Document()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Load(doc)
	if err != nil {
		t.Fatal(err)
	}
	if back.Binding != kit.Binding || back.ArtifactID != kit.ArtifactID || back.VeleroRepositoryID != "velero-default-kopia" {
		t.Errorf("round trip lost the header: %+v", back)
	}
	if back.S3Location != testLocation() {
		t.Errorf("round trip lost the S3 location: %+v", back.S3Location)
	}

	if got, want := KitKey("prod-1", "cluster-1", KindCluster, "art-1"), "prod-1/recovery-kits/cluster-1/cluster-art-1.json"; got != want {
		t.Errorf("KitKey = %q, want %q", got, want)
	}
	if got, want := SetKey("/prod-1/", "cluster-1", KindCluster, "art-1"), "prod-1/recovery-sets/cluster-1/cluster-art-1.json"; got != want {
		t.Errorf("SetKey = %q, want %q", got, want)
	}
	if got, want := SetKey(ControlPlaneScope, "cluster-1", KindControlPlane, "art-2"), "control-plane/recovery-sets/cluster-1/control-plane-art-2.json"; got != want {
		t.Errorf("the control plane's own set must live under %q, got %q", want, got)
	}
}
