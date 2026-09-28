package recovery

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
)

// The measured bucket, and the question each kind of set is asked.
//
// kn-t48-all-host-recovery-s11-prao.2, found on hardware 2026-09-28: an
// all-in-one recovery and `kubenest recovery-kit check --kind control-plane`
// both refused a bucket that was exactly what a real 1.1 customer has — a
// control-plane set that names no Velero backup (nothing writes one there), a
// CLUSTER set for the management cluster that names its workload backup, and two
// sealed checkpoints. The recovery was refused before it changed anything, on a
// rule that could never be satisfied.

// bucket is a throwaway S3-compatible store for these tests.
type bucket struct {
	objects map[string][]byte
	puts    int
}

func (b *bucket) Get(_ context.Context, key string) ([]byte, error) {
	if body, ok := b.objects[key]; ok {
		return body, nil
	}
	return nil, s3.ErrNotFound
}

func (b *bucket) List(_ context.Context, prefix string) ([]string, bool, error) {
	var keys []string
	for key := range b.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, false, nil
}

const (
	testScope    = "demo-a"
	testInstance = "9c11f0aa-0000-7000-8000-000000000001"
	testOrg      = "0f3d0000-0000-7000-8000-000000000002"
	testMgmt     = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c30"
	testArtifact = "20260927T193827Z-aaaaaaaa"
)

// published is one kit and its set, sealed to the fleet key exactly as the
// install writes them.
type published struct {
	fleet *recoverykit.FleetKey
	store *bucket
}

func newPublished(t *testing.T) *published {
	t.Helper()
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	return &published{fleet: fleet, store: &bucket{objects: map[string][]byte{}}}
}

func (p *published) write(t *testing.T, bind recoverykit.Binding, artifact string, secrets, versions map[string]string, repoID string, complete bool, backups ...recoverykit.Backup) *recoverykit.Set {
	t.Helper()
	now := time.Date(2026, 9, 27, 19, 38, 27, 0, time.UTC)
	kit, err := recoverykit.New(bind, artifact, recoverykit.Location{
		Endpoint: "minio.example.test", Bucket: "kubenest-demo", Region: "main", Prefix: testScope,
	}, repoID, secrets, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.SealTo(p.fleet.Recipient()); err != nil {
		t.Fatal(err)
	}
	doc, err := kit.Document()
	if err != nil {
		t.Fatal(err)
	}
	set, err := recoverykit.NewSet(kit, versions, recoverykit.Digest(doc), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range backups {
		set, err = set.WithBackup(b)
		if err != nil {
			t.Fatal(err)
		}
	}
	set.Complete = complete
	if !complete {
		delete(set.Checksums, "kit")
	}
	setDoc, err := set.Document()
	if err != nil {
		t.Fatal(err)
	}
	p.store.objects[recoverykit.KitKey(testScope, bind.ClusterID, bind.Kind, artifact)] = doc
	p.store.objects[recoverykit.SetKey(testScope, bind.ClusterID, bind.Kind, artifact)] = setDoc
	loaded, err := recoverykit.LoadSet(setDoc)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// controlPlaneBinding is the instance's own kit binding: instance and the
// management cluster, and NO organisation — a control-plane kit is an instance
// resource.
func controlPlaneBinding() recoverykit.Binding {
	return recoverykit.Binding{Kind: recoverykit.KindControlPlane, InstanceID: testInstance, ClusterID: testMgmt}
}

func managementClusterBinding() recoverykit.Binding {
	return recoverykit.Binding{Kind: recoverykit.KindCluster, InstanceID: testInstance, OrganisationID: testOrg, ClusterID: testMgmt}
}

var (
	cpSecrets = map[string]string{
		recoverykit.KeyEncryptionKey:  "enc",
		recoverykit.KeyAgentJWTSecret: "jwt",
		recoverykit.KeyControlPlaneCA: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n-----BEGIN EC PRIVATE KEY-----\ny\n-----END EC PRIVATE KEY-----\n",
	}
	clusterSecrets = map[string]string{
		recoverykit.KeyVeleroRepoPassword: "repo-password",
		recoverykit.KeyK3sJoinToken:       "join-token",
	}
)

// TestAControlPlaneSetNeedsNoWorkloadBackup is the measured shape, end to end
// through the two questions the recovery asks: the instance's set is complete
// and about this instance with no Velero backup in it, and the MANAGEMENT
// cluster's own set is where the workload backup is.
func TestAControlPlaneSetNeedsNoWorkloadBackup(t *testing.T) {
	p := newPublished(t)
	// The control-plane set as every install writes it: a kit, versions, and
	// no backup.
	p.write(t, controlPlaneBinding(), testArtifact, cpSecrets, map[string]string{"bundle": "1.1"}, "", true)
	// The management cluster's OWN set, with the workload backup a real bucket
	// holds (the lab's `manual-20260927-235954`).
	p.write(t, managementClusterBinding(), testArtifact, clusterSecrets, map[string]string{"bundle": "1.1"},
		"velero-default-kopia", true, recoverykit.Backup{
			Name:        "manual-20260927-235954",
			CompletedAt: time.Date(2026, 9, 27, 23, 59, 54, 0, time.UTC),
			Coverage:    []string{"e2e-demo-web"},
			Status:      "Completed",
		})

	ctx := context.Background()
	cp, err := SelectControlPlane(ctx, p.store, testScope, testInstance, "")
	if err != nil {
		t.Fatalf("the instance's own recovery set was refused: %v", err)
	}
	cpCheck, err := Verify(ctx, p.store, cp, p.fleet.SecretKeyString())
	if err != nil {
		t.Fatal(err)
	}
	if !cpCheck.AllGood() {
		t.Fatalf("the measured bucket's control-plane set did not check out: %s", strings.Join(cpCheck.Failed(), "; "))
	}
	if cpCheck.Set.Detail == "" || !cpCheck.Set.OK {
		t.Fatalf("the fourth answer is not a yes: %+v", cpCheck.Set)
	}
	// The answer has to say what the recovery actually starts from, or an
	// operator reading it cannot tell why a set with no backup in it is fine.
	if !strings.Contains(cpCheck.Set.Detail, "CHECKPOINT") {
		t.Fatalf("the answer does not name the checkpoint a control-plane recovery starts from: %s", cpCheck.Set.Detail)
	}

	workload, err := SelectManagementCluster(ctx, p.store, testScope, testInstance, testMgmt, "")
	if err != nil {
		t.Fatalf("the management cluster's own recovery set was refused: %v", err)
	}
	if workload.Backup.Name != "manual-20260927-235954" {
		t.Fatalf("the management cluster's backup is %q", workload.Backup.Name)
	}
	if len(workload.Backup.Coverage) != 1 || workload.Backup.Coverage[0] != "e2e-demo-web" {
		t.Fatalf("the namespaces to restore are %v, so the restore stage would have nothing to iterate", workload.Backup.Coverage)
	}
	if workload.Set.VeleroRepositoryID != "velero-default-kopia" {
		t.Fatalf("the repository identity the recovery must open is %q", workload.Set.VeleroRepositoryID)
	}
}

// TestTheFourthQuestionIsTheSameInBothKindsAndOneImplementation: the rule the
// recovery applies is the rule `kubenest recovery-kit check` reports, because
// they call this. A check that answered differently would be the document an
// operator reads to decide whether to run the recovery.
func TestTheFourthQuestionIsTheSameInBothKindsAndOneImplementation(t *testing.T) {
	p := newPublished(t)
	emptyClusterSet := p.write(t, managementClusterBinding(), testArtifact, clusterSecrets, nil, "velero-default-kopia", true)
	cpSet := p.write(t, controlPlaneBinding(), testArtifact, cpSecrets, nil, "", true)

	// A CLUSTER set with no completed backup is still refused, with the
	// sentence that names what is missing: this is today's rule, and the fix
	// for the control plane must not leak into it.
	clusterAnswer := SetQuestion{
		Kind: recoverykit.KindCluster, Set: emptyClusterSet,
		SetKey:     recoverykit.SetKey(testScope, testMgmt, recoverykit.KindCluster, testArtifact),
		ArtifactID: testArtifact, Expected: managementClusterBinding(),
	}.Answer()
	if clusterAnswer.OK {
		t.Fatal("a cluster recovery set with no completed backup was accepted: that is the state before any backup has been taken, and a recovery from it restores nothing")
	}
	if !strings.Contains(clusterAnswer.Detail, "no completed backup") {
		t.Fatalf("the refusal does not name what is missing: %s", clusterAnswer.Detail)
	}

	// The SAME set contents, as a control-plane kit, are complete: the
	// question is the kind's, not the backup's.
	cpAnswer := SetQuestion{
		Kind: recoverykit.KindControlPlane, Set: cpSet,
		SetKey:     recoverykit.SetKey(testScope, testMgmt, recoverykit.KindControlPlane, testArtifact),
		ArtifactID: testArtifact, Expected: controlPlaneBinding(),
	}.Answer()
	if !cpAnswer.OK {
		t.Fatalf("a control-plane set with no Velero backup was refused: %s", cpAnswer.Detail)
	}

	// An upload that never completed is refused whatever the kind.
	partial := p.write(t, controlPlaneBinding(), "20260927T193828Z-bbbbbbbb", cpSecrets, nil, "", false)
	partialAnswer := SetQuestion{
		Kind: recoverykit.KindControlPlane, Set: partial,
		SetKey:     recoverykit.SetKey(testScope, testMgmt, recoverykit.KindControlPlane, "20260927T193828Z-bbbbbbbb"),
		ArtifactID: "20260927T193828Z-bbbbbbbb", Expected: controlPlaneBinding(),
	}.Answer()
	if partialAnswer.OK {
		t.Fatal("a partial upload was accepted: it is never something to recover from, whatever kind it is")
	}
}

// TestABucketWithNoWorkloadBackupIsStillRefused: the management cluster's set
// exists but names no completed backup, so the half of an all-in-one host that
// holds data cannot come back. Refused, naming what is missing — rather than
// recovering the database and silently leaving the workloads behind.
func TestABucketWithNoWorkloadBackupIsStillRefused(t *testing.T) {
	p := newPublished(t)
	p.write(t, controlPlaneBinding(), testArtifact, cpSecrets, nil, "", true)
	p.write(t, managementClusterBinding(), testArtifact, clusterSecrets, nil, "velero-default-kopia", true)

	_, err := SelectManagementCluster(context.Background(), p.store, testScope, testInstance, testMgmt, "")
	if err == nil {
		t.Fatal("a management cluster with no workload backup was accepted, so a control-plane recovery would rebuild the database and drop its own workloads")
	}
	for _, want := range []string{"no usable workload backup", testMgmt, "backup now"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q, so the operator cannot tell what to take: %v", want, err)
		}
	}
}

// TestTheCheckpointIsTheControlPlaneStartingPoint: the bucket's checkpoints are
// found and checked by the recovery itself, which is why the set needs none.
func TestTheCheckpointIsTheControlPlaneStartingPoint(t *testing.T) {
	p := newPublished(t)
	sealed, err := recoverykit.SealTo(p.fleet.Recipient(), []byte("the dump"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"key":                         "control-plane/2026-09-27T003149Z-manual",
		"at":                          "2026-09-27T00:31:49Z",
		"control_plane_version":       "1.1",
		"management_cluster_id":       testMgmt,
		"includes_security_change_at": "2026-09-26T23:00:00Z",
		"envelope": map[string]any{
			"sha256":     digestOf(sealed),
			"size_bytes": len(sealed),
		},
		"dump": map[string]any{"postgres_major": 17, "postgres_image": "bitnami/postgresql@sha256:cccc"},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dir := "control-plane/2026-09-27T003149Z-manual"
	p.store.objects[dir+"/manifest.json"] = body
	p.store.objects[dir+"/control-plane.dump.age"] = sealed

	cp, dump, err := controlplane.SelectControlPlaneCheckpoint(context.Background(), p.store.List, p.store.Get, "control-plane")
	if err != nil {
		t.Fatalf("the bucket's checkpoint was not found: %v", err)
	}
	if cp.ControlPlaneVersion != "1.1" || cp.Dump.PostgresMajor != 17 {
		t.Fatalf("the checkpoint was read as %+v", cp)
	}
	// The selection hands back the SEALED dump, which is what the restore
	// stage opens with the fleet key: a plaintext database never leaves the
	// process that loaded it.
	plaintext, err := recoverykit.Open(p.fleet.SecretKeyString(), dump)
	if err != nil {
		t.Fatalf("the selected dump does not open with the fleet key: %v", err)
	}
	if string(plaintext) != "the dump" {
		t.Fatalf("the dump the recovery would load is %q", string(plaintext))
	}
}

// digestOf mirrors the manifest's digest field, which is what the bucket's
// checkpoint manifests carry (sha256:…).
func digestOf(doc []byte) string { return recoverykit.Digest(doc) }
