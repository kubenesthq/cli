package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/sshx"
)

func testTarget() Target {
	return Target{
		Endpoint:        "http://minio.velero-e2e.svc:9000",
		Bucket:          "kubenest-backups",
		Region:          "main",
		AccessKeyID:     "AKTEST",
		SecretAccessKey: "sekret",
	}
}

func TestTargetValidateNamesTheMissingPiece(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Target)
		want   string
	}{
		"endpoint":    {func(t *Target) { t.Endpoint = "" }, "--endpoint"},
		"bucket":      {func(t *Target) { t.Bucket = "" }, "--bucket"},
		"region":      {func(t *Target) { t.Region = "" }, "--region"},
		"credentials": {func(t *Target) { t.SecretAccessKey = "" }, "KUBENEST_BACKUP_ACCESS_KEY_ID"},
		"newline":     {func(t *Target) { t.SecretAccessKey = "a\nb" }, "newline"},
	}
	for name, c := range cases {
		target := testTarget()
		c.mutate(&target)
		err := target.Validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, c.want)
		}
	}
	if err := testTarget().Validate(); err != nil {
		t.Errorf("valid target rejected: %v", err)
	}
}

func TestStorageLocationSpeaksS3Compatible(t *testing.T) {
	loc, err := testTarget().storageLocationManifest()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			Provider      string `yaml:"provider"`
			Default       bool   `yaml:"default"`
			ObjectStorage struct {
				Bucket string `yaml:"bucket"`
				Prefix string `yaml:"prefix"`
			} `yaml:"objectStorage"`
			Credential struct {
				Name string `yaml:"name"`
				Key  string `yaml:"key"`
			} `yaml:"credential"`
			Config map[string]string `yaml:"config"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(loc, &doc); err != nil {
		t.Fatal(err)
	}
	s := doc.Spec
	if s.Provider != "aws" || !s.Default || s.ObjectStorage.Bucket != "kubenest-backups" {
		t.Errorf("spec = %+v", s)
	}
	if s.ObjectStorage.Prefix != "workload" {
		t.Errorf("workload prefix = %q, want workload", s.ObjectStorage.Prefix)
	}
	if s.Credential.Name != TargetSecretName || s.Credential.Key != "cloud" {
		t.Errorf("credential ref = %+v, want the per-location secret reference", s.Credential)
	}
	if s.Config["s3Url"] != "http://minio.velero-e2e.svc:9000" {
		t.Errorf("s3Url = %q — a given scheme must survive", s.Config["s3Url"])
	}
	// Non-AWS stores get path-style addressing and no request checksums —
	// the aws-sdk-go-v2 defaults are rejected by several S3-compatibles.
	if s.Config["s3ForcePathStyle"] != "true" {
		t.Errorf("s3ForcePathStyle = %q, want \"true\" off AWS", s.Config["s3ForcePathStyle"])
	}
	if v, ok := s.Config["checksumAlgorithm"]; !ok || v != "" {
		t.Errorf("checksumAlgorithm = %q (present=%v), want explicitly empty off AWS", v, ok)
	}
}

func TestStorageLocationOnAWSKeepsSDKDefaults(t *testing.T) {
	target := testTarget()
	target.Endpoint = "s3.ap-south-1.amazonaws.com"
	target.Region = "ap-south-1"
	target.Prefix = "prod-1"
	loc, err := target.storageLocationManifest()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Spec struct {
			ObjectStorage map[string]string `yaml:"objectStorage"`
			Config        map[string]string `yaml:"config"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(loc, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Spec.Config["s3Url"] != "https://s3.ap-south-1.amazonaws.com" {
		t.Errorf("s3Url = %q, want https default for a bare endpoint", doc.Spec.Config["s3Url"])
	}
	if _, ok := doc.Spec.Config["s3ForcePathStyle"]; ok {
		t.Error("s3ForcePathStyle must not be forced on AWS itself")
	}
	if _, ok := doc.Spec.Config["checksumAlgorithm"]; ok {
		t.Error("checksumAlgorithm must be left to the SDK on AWS itself")
	}
	if doc.Spec.ObjectStorage["prefix"] != "prod-1/workload" {
		t.Errorf("prefix = %q, want prod-1/workload", doc.Spec.ObjectStorage["prefix"])
	}
}

func TestTargetPartitionsWorkloadAndDatastoreObjects(t *testing.T) {
	target := testTarget()
	target.Prefix = "/prod-1/"

	loc, err := target.storageLocationManifest()
	if err != nil {
		t.Fatal(err)
	}
	var location struct {
		Spec struct {
			ObjectStorage map[string]string `yaml:"objectStorage"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(loc, &location); err != nil {
		t.Fatal(err)
	}

	secret, err := target.datastoreSecretManifest(24)
	if err != nil {
		t.Fatal(err)
	}
	var datastore struct {
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal(secret, &datastore); err != nil {
		t.Fatal(err)
	}

	if got := location.Spec.ObjectStorage["prefix"]; got != "prod-1/workload" {
		t.Errorf("Velero prefix = %q, want prod-1/workload", got)
	}
	if got := datastore.StringData["etcd-s3-folder"]; got != "prod-1/datastore" {
		t.Errorf("etcd folder = %q, want prod-1/datastore", got)
	}
}

func TestSecretCarriesAnAWSCredentialsFile(t *testing.T) {
	secret, err := testTarget().secretManifest()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata   map[string]string `yaml:"metadata"`
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal(secret, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Metadata["name"] != TargetSecretName || doc.Metadata["namespace"] != Namespace {
		t.Errorf("metadata = %v", doc.Metadata)
	}
	cloud := doc.StringData["cloud"]
	if !strings.Contains(cloud, "[default]") ||
		!strings.Contains(cloud, "aws_access_key_id=AKTEST") ||
		!strings.Contains(cloud, "aws_secret_access_key=sekret") {
		t.Errorf("stringData.cloud is not a shared-credentials file:\n%s", cloud)
	}
}

// scripted answers a Configure/TakeBackup run: applies succeed, gets return
// canned JSON.
func scripted(answers map[string]string) func(string) (sshx.Result, error) {
	return func(cmd string) (sshx.Result, error) {
		for needle, out := range answers {
			if strings.Contains(cmd, needle) {
				return sshx.Result{Stdout: out}, nil
			}
		}
		return sshx.Result{}, nil
	}
}

func TestConfigureAppliesThenProvesTheTarget(t *testing.T) {
	r := &fakeRunner{Respond: scripted(map[string]string{
		"get backupstoragelocation": `{"status":{"phase":"Available"}}`,
		"get backuprepositories":    `{"items":[]}`,
		"get schedule":              "Enabled",
	})}
	if err := Configure(context.Background(), r, testManifest(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}

	// Order: credentials before the location that references them, location
	// proven before the schedule that writes to it.
	var secretAt, locationAt, scheduleAt = -1, -1, -1
	for _, a := range r.Applied() {
		switch {
		case strings.Contains(a.Doc, "kind: Secret"):
			secretAt = a.At
		case strings.Contains(a.Doc, "kind: BackupStorageLocation"):
			locationAt = a.At
		case strings.Contains(a.Doc, "kind: Schedule"):
			scheduleAt = a.At
		}
	}
	if secretAt == -1 || locationAt == -1 || scheduleAt == -1 {
		t.Fatalf("missing applies: secret=%d location=%d schedule=%d", secretAt, locationAt, scheduleAt)
	}
	if !(secretAt < locationAt && locationAt < scheduleAt) {
		t.Errorf("apply order = secret@%d location@%d schedule@%d, want secret < location < schedule", secretAt, locationAt, scheduleAt)
	}
}

// The schedule the cluster gets is the manifest's: daily at the anchor hour,
// retention keep × interval.
func TestConfigureWritesTheManifestSchedule(t *testing.T) {
	r := &fakeRunner{Respond: scripted(map[string]string{
		"get backupstoragelocation": `{"status":{"phase":"Available"}}`,
		"get backuprepositories":    `{"items":[]}`,
		"get schedule":              "Enabled",
	})}
	if err := Configure(context.Background(), r, testManifest(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Applied() {
		doc := a.Doc
		if !strings.Contains(doc, "kind: Schedule") {
			continue
		}
		var schedule struct {
			Metadata map[string]string `yaml:"metadata"`
			Spec     struct {
				Schedule string `yaml:"schedule"`
				Template struct {
					StorageLocation    string   `yaml:"storageLocation"`
					TTL                string   `yaml:"ttl"`
					ExcludedNamespaces []string `yaml:"excludedNamespaces"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &schedule); err != nil {
			t.Fatal(err)
		}
		if schedule.Metadata["name"] != "daily" {
			t.Errorf("schedule name = %q, want daily for the 24h cadence", schedule.Metadata["name"])
		}
		if schedule.Spec.Schedule != "0 2 * * *" {
			t.Errorf("cron = %q, want 0 2 * * *", schedule.Spec.Schedule)
		}
		if schedule.Spec.Template.TTL != "336h0m0s" {
			t.Errorf("ttl = %q, want 336h0m0s (14 × 24h)", schedule.Spec.Template.TTL)
		}
		if schedule.Spec.Template.StorageLocation != StorageLocationName {
			t.Errorf("storageLocation = %q", schedule.Spec.Template.StorageLocation)
		}
		if got := strings.Join(schedule.Spec.Template.ExcludedNamespaces, ","); got != "velero,kube-system,kube-public,kube-node-lease,kubenest-system" {
			t.Errorf("excludedNamespaces = %q", got)
		}
		return
	}
	t.Fatal("no Schedule applied")
}

// A target velero cannot validate fails the convergence with the store's own
// message — the part that names the fix.
func TestConfigureFailsWithTheStoresReason(t *testing.T) {
	r := &fakeRunner{Respond: scripted(map[string]string{
		"get backupstoragelocation": `{"status":{"phase":"Unavailable","message":"AccessDenied: bucket policy"}}`,
	})}
	err := Configure(context.Background(), r, testManifest(), testTarget(), nil)
	if err == nil {
		t.Fatal("an Unavailable location must fail Configure")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err = %v, want the store's message in it", err)
	}
}

func TestTakeBackupReportsATerminalFailureImmediately(t *testing.T) {
	r := &fakeRunner{Respond: scripted(map[string]string{
		"get backup":                 `{"metadata":{"uid":"uid-manual-x"},"status":{"phase":"Failed","failureReason":"bucket vanished"}}`,
		"get namespaces":             `{"items":[]}`,
		"get persistentvolumeclaims": `{"items":[]}`,
	})}
	err := TakeBackup(context.Background(), r, testManifest(), "manual-x", nil)
	if err == nil {
		t.Fatal("a Failed backup must be an error")
	}
	if !strings.Contains(err.Error(), "Failed") || !strings.Contains(err.Error(), "bucket vanished") {
		t.Errorf("err = %v, want the phase and velero's reason", err)
	}
}

func TestTakeBackupPassesOnCompleted(t *testing.T) {
	r := &fakeRunner{Respond: scripted(map[string]string{
		"get backup":                 `{"metadata":{"uid":"uid-manual-x"},"status":{"phase":"Completed"}}`,
		"get namespaces":             `{"items":[]}`,
		"get persistentvolumeclaims": `{"items":[]}`,
	})}
	if err := TakeBackup(context.Background(), r, testManifest(), "manual-x", nil); err != nil {
		t.Fatal(err)
	}
}

// coverageFixture is the cluster state the coverage record is read from: two
// workload namespaces, one of them with a claim, one claim in an excluded
// namespace, and one claim created after the capture started.
func coverageFixture() map[string]string {
	return map[string]string{
		"get backup": `{"metadata":{"uid":"uid-manual-x"},` +
			`"status":{"phase":"Completed","startTimestamp":"2026-09-25T02:00:05Z"}}`,
		"get namespaces": `{"items":[` +
			`{"metadata":{"name":"payments","uid":"ns-payments"}},` +
			`{"metadata":{"name":"storefront","uid":"ns-storefront"}},` +
			`{"metadata":{"name":"velero","uid":"ns-velero"}},` +
			`{"metadata":{"name":"kube-system","uid":"ns-kube-system"}}]}`,
		"get persistentvolumeclaims": `{"items":[` +
			`{"metadata":{"namespace":"payments","name":"data","uid":"pvc-data","creationTimestamp":"2026-09-25T01:00:00Z"}},` +
			`{"metadata":{"namespace":"velero","name":"scratch","uid":"pvc-scratch","creationTimestamp":"2026-09-25T01:00:00Z"}},` +
			`{"metadata":{"namespace":"payments","name":"late","uid":"pvc-late","creationTimestamp":"2026-09-25T02:30:00Z"}}]}`,
	}
}

// recordedCoverage runs one TakeBackup against the fixture answers and returns
// the runner it drove plus the decoded coverage.json the CLI wrote.
func recordedCoverage(t *testing.T, name string, answers map[string]string) (*fakeRunner, []appliedDoc, map[string]any) {
	t.Helper()
	r := &fakeRunner{Respond: scripted(answers)}
	if err := TakeBackup(context.Background(), r, testManifest(), name, nil); err != nil {
		t.Fatalf("TakeBackup: %v", err)
	}
	created := createdDocs(r)
	if len(created) != 1 {
		t.Fatalf("documents created = %d, want exactly the one coverage record", len(created))
	}
	var cm struct {
		Metadata struct {
			Name            string            `yaml:"name"`
			Namespace       string            `yaml:"namespace"`
			Labels          map[string]string `yaml:"labels"`
			OwnerReferences []struct {
				APIVersion string `yaml:"apiVersion"`
				Kind       string `yaml:"kind"`
				Name       string `yaml:"name"`
				UID        string `yaml:"uid"`
			} `yaml:"ownerReferences"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(created[0].Doc), &cm); err != nil {
		t.Fatalf("the coverage record is not a readable document: %v", err)
	}
	if cm.Metadata.Name != CoverageRecordName(name) || cm.Metadata.Namespace != Namespace {
		t.Errorf("record object = %s/%s, want %s/%s",
			cm.Metadata.Namespace, cm.Metadata.Name, Namespace, CoverageRecordName(name))
	}
	if cm.Metadata.Labels[coverageLabelKey] != coverageLabel ||
		cm.Metadata.Labels["app.kubernetes.io/managed-by"] != coverageManagedBy {
		t.Errorf("record labels = %v, want the KubeNest coverage record labels", cm.Metadata.Labels)
	}
	if len(cm.Metadata.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %v, want exactly the Backup that owns the record", cm.Metadata.OwnerReferences)
	}
	owner := cm.Metadata.OwnerReferences[0]
	if owner.APIVersion != "velero.io/v1" || owner.Kind != "Backup" || owner.Name != name || owner.UID != "uid-"+name {
		t.Errorf("ownerReference = %+v, want velero.io/v1 Backup %s uid-%s: Velero's TTL must collect the record with the backup",
			owner, name, name)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(cm.Data[coverageRecordKey]), &record); err != nil {
		t.Fatalf("coverage.json is not JSON: %v (%q)", err, cm.Data[coverageRecordKey])
	}
	return r, created, record
}

// The record is the expected set, written while the backup is starting: the set
// of namespaces and claims the backup must cover, with the UIDs the cluster
// gave them.
func TestTakeBackupRecordsTheExpectedSetBeforeTheBackupSettles(t *testing.T) {
	r, created, record := recordedCoverage(t, "manual-x", coverageFixture())

	if record["backup"] != "manual-x" || record["recorded_by"] != "cli" {
		t.Errorf("record backup/recorded_by = %v/%v, want manual-x/cli", record["backup"], record["recorded_by"])
	}
	if _, err := time.Parse(time.RFC3339, record["recorded_at"].(string)); err != nil {
		t.Errorf("recorded_at = %v, want RFC3339: %v", record["recorded_at"], err)
	}
	if record["capture_started_at"] != "2026-09-25T02:00:05Z" {
		t.Errorf("capture_started_at = %v, want the Backup's own status.startTimestamp", record["capture_started_at"])
	}

	namespaces, ok := record["namespaces"].([]any)
	if !ok {
		t.Fatalf("namespaces = %#v, want a list", record["namespaces"])
	}
	if len(namespaces) != 2 {
		t.Fatalf("namespaces = %#v, want payments and storefront only: velero and kube-system are excluded from the backup", namespaces)
	}
	payments := namespaces[0].(map[string]any)
	if payments["name"] != "payments" || payments["uid"] != "ns-payments" {
		t.Errorf("namespace entry = %v, want the cluster's name and UID", payments)
	}
	volumes, ok := payments["volumes"].([]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("payments volumes = %#v, want only data: velero is excluded and `late` was created after the capture started", payments["volumes"])
	}
	volume := volumes[0].(map[string]any)
	if volume["namespace"] != "payments" || volume["name"] != "data" || volume["uid"] != "pvc-data" {
		t.Errorf("volume entry = %v, want payments/data with the claim's own UID", volume)
	}
	storefront := namespaces[1].(map[string]any)
	if storefront["name"] != "storefront" || storefront["uid"] != "ns-storefront" {
		t.Errorf("namespace entry = %v, want storefront's name and UID", storefront)
	}
	if vols, ok := storefront["volumes"].([]any); !ok || len(vols) != 0 {
		t.Errorf("storefront volumes = %#v, want an empty list: a claimed namespace with nothing in it is a fact, not a silence",
			storefront["volumes"])
	}

	// Before the settle probe can see the backup, never after it: the set is
	// the one that existed when the capture started.
	commands := r.Commands()
	observedAt := -1
	for i := len(commands) - 1; i >= 0; i-- {
		if strings.Contains(commands[i], "get backup ") {
			observedAt = i
			break
		}
	}
	if observedAt < 0 {
		t.Fatal("the backup was never observed, so nothing settled")
	}
	if created[0].At > observedAt {
		t.Errorf("the coverage record was created at command %d, after the settle probe observed the backup at %d",
			created[0].At, observedAt)
	}
}

// The record is a cross-repo contract: the operator reads and writes the same
// JSON with its own struct, so the shape is pinned in kubenest-contracts and
// compared here field for field, in both directions.
func TestTheCoverageRecordMatchesTheContractsExample(t *testing.T) {
	path := filepath.Join("..", "..", "..", "kubenest-contracts", "testdata", "backup-coverage-record.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("kubenest-contracts is not checked out beside this repo, so the record's shape cannot be checked here: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var example map[string]any
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatalf("%s is not JSON: %v", path, err)
	}

	_, _, written := recordedCoverage(t, "manual-x", coverageFixture())
	assertSameShape(t, "coverage", example, written)
}

// createdDocs returns every document streamed to `kubectl create` and where in
// the command sequence it happened. The coverage record is created rather than
// applied so that a second writer fails instead of replacing the set recorded
// at the start.
func createdDocs(r *fakeRunner) []appliedDoc {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []appliedDoc
	for i, c := range r.commands {
		if strings.Contains(c, "kubectl create") {
			out = append(out, appliedDoc{At: i, Doc: string(r.inputs[i])})
		}
	}
	return out
}

// assertSameShape requires the same key set at every path of two decoded JSON
// documents, recursing through objects and comparing the first element of each
// list. It fails in both directions: a field the writer stopped emitting and a
// field the example gained without the writer are both drift.
func assertSameShape(t *testing.T, path string, want, got any) {
	t.Helper()
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: %T in the example, %T in the record", path, want, got)
			return
		}
		if keys := mismatchedKeys(w, g); keys != "" {
			t.Errorf("%s: %s", path, keys)
		}
		for key, value := range w {
			if other, ok := g[key]; ok {
				assertSameShape(t, path+"."+key, value, other)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			t.Errorf("%s: %T in the example, %T in the record", path, want, got)
			return
		}
		if len(w) == 0 || len(g) == 0 {
			return
		}
		assertSameShape(t, path+"[]", w[0], g[0])
	default:
		if (want == nil) != (got == nil) {
			t.Errorf("%s: null in one and %v in the other", path, got)
		}
	}
}

func mismatchedKeys(want, got map[string]any) string {
	var missing, extra []string
	for key := range want {
		if _, ok := got[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			extra = append(extra, key)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Sprintf("keys missing from the record %v, keys the example does not carry %v", missing, extra)
}

// testBucketName is the bucket every fake endpoint serves.
const testBucketName = "kubenest-backups"

// fakeBucket is an httptest endpoint that answers the way an S3-compatible
// store does when the credential's policy is exactly `allow` (keyed on the
// object key, or on the list prefix). It is path-style, because the endpoint
// is not AWS. It exists so the scope check can be driven through the real
// SigV4 client without a bucket, and so the test can assert which operations
// were actually attempted.
type fakeBucket struct {
	allow          func(key string) bool
	encryption     string // "" = no default encryption configured
	versioning     string // "" = never configured
	denyProtection bool   // answer the protection reads with AccessDenied

	mu      sync.Mutex
	objects map[string][]byte
	seen    []string
	srv     *httptest.Server
}

func newFakeBucket(t *testing.T, allow func(key string) bool) *fakeBucket {
	t.Helper()
	f := &fakeBucket{allow: allow, objects: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBucket) URL() string { return f.srv.URL }

func (f *fakeBucket) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()

	q := r.URL.Query()
	// The client must actually ask the protection questions; a race where it
	// didn't would make the parse tests below vacuous. These two are asked at
	// the bucket root, which is also where a list request arrives — so they are
	// matched on the query, not on the path.
	if q.Has("encryption") {
		if f.denyProtection {
			xmlError(w, http.StatusForbidden, "AccessDenied", "Access Denied")
			return
		}
		if f.encryption == "" {
			xmlError(w, http.StatusNotFound, "ServerSideEncryptionConfigurationNotFoundError", "The server side encryption configuration was not found")
			return
		}
		fmt.Fprintf(w, `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>%s</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`, f.encryption)
		return
	}
	if q.Has("versioning") {
		if f.denyProtection {
			xmlError(w, http.StatusForbidden, "AccessDenied", "Access Denied")
			return
		}
		if f.versioning == "" {
			io.WriteString(w, `<VersioningConfiguration/>`)
			return
		}
		fmt.Fprintf(w, `<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>`, f.versioning)
		return
	}

	key := strings.TrimPrefix(r.URL.Path, "/"+testBucketName+"/")
	if r.Method == http.MethodGet && q.Has("list-type") {
		prefix := q.Get("prefix")
		if !f.allow(prefix) {
			xmlError(w, http.StatusForbidden, "AccessDenied", "Access Denied")
			return
		}
		f.mu.Lock()
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		f.mu.Unlock()
		sort.Strings(keys)
		var b strings.Builder
		fmt.Fprintf(&b, `<ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>`, testBucketName, prefix, len(keys))
		for _, k := range keys {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>1</Size><ETag>&quot;e&quot;</ETag></Contents>`, k)
		}
		b.WriteString(`</ListBucketResult>`)
		io.WriteString(w, b.String())
		return
	}

	if !f.allow(key) {
		xmlError(w, http.StatusForbidden, "AccessDenied", "Access Denied")
		return
	}
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.objects[key] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		f.mu.Lock()
		_, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"e"`)
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			xmlError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.Write(body)
	default:
		xmlError(w, http.StatusBadRequest, "InvalidRequest", "unsupported method")
	}
}

func xmlError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
}

// attempts returns every request the fake saw, for a test that must prove the
// check actually asked.
func (f *fakeBucket) attempts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeBucket) sawPrefix(needle string) bool {
	for _, s := range f.attempts() {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// scopedTarget is testTarget with a cluster-level prefix and the fake
// endpoint, plus the client built from exactly those coordinates.
func scopedTarget(t *testing.T, endpoint string) backupTargetWithClient {
	t.Helper()
	target := testTarget()
	target.Prefix = "prod-1"
	target.Endpoint = endpoint
	client, err := target.S3Client()
	if err != nil {
		t.Fatal(err)
	}
	target.Client = client
	return backupTargetWithClient{Target: target}
}

// backupTargetWithClient exists so the helper above returns something with a
// name, and so a test can see which half it is holding.
type backupTargetWithClient struct{ Target }

// A credential that can list or read the control plane's prefix, or a sibling
// cluster's, is refused — with the exact operation that succeeded named. The
// planted negative is the whole point: revert the scope check and the wide
// credential is accepted, which is what makes this test not vacuous.
func TestSetTargetRefusesCredentialThatReadsAnotherPrefix(t *testing.T) {
	ctx := context.Background()

	// The well-scoped credential: allowed only inside prod-1/. It must pass,
	// and the fake must have been asked on both sides — a check that never
	// probed the foreign prefixes would pass this half for the wrong reason.
	narrow := newFakeBucket(t, func(key string) bool { return strings.HasPrefix(key, "prod-1/") })
	scoped := scopedTarget(t, narrow.URL())
	if err := scoped.VerifyScope(ctx, scoped.Client); err != nil {
		t.Fatalf("a credential scoped to prod-1/* must be accepted: %v", err)
	}
	for _, want := range []string{
		"PUT /" + testBucketName + "/prod-1/scope-check.json",
		"GET /" + testBucketName + "/prod-1/scope-check.json",
		"list-type=2&prefix=prod-1%2F",
		"prefix=control-plane%2F",
		"prefix=kubenest-scope-check-sibling%2F",
	} {
		var found bool
		for _, a := range narrow.attempts() {
			if strings.Contains(a, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("the check never attempted %q; attempts were %v", want, narrow.attempts())
		}
	}

	// The planted negative: a credential that reaches the control-plane
	// prefix must be refused, naming the operation.
	wide := newFakeBucket(t, func(string) bool { return true })
	wideTarget := scopedTarget(t, wide.URL())
	err := wideTarget.VerifyScope(ctx, wideTarget.Client)
	if err == nil {
		t.Fatal("a credential that can list the control-plane prefix must be refused")
	}
	for _, want := range []string{"ListObjectsV2", "control-plane/", "prod-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %v", want, err)
		}
	}

	// A credential that is allowed to READ outside its prefix while listing is
	// refused there is still refused: the object being absent (NoSuchKey) is
	// not the same as the store refusing the read.
	sneaky := newFakeBucket(t, func(key string) bool {
		return strings.HasPrefix(key, "prod-1/") || key == "control-plane/scope-check.json"
	})
	sneakyTarget := scopedTarget(t, sneaky.URL())
	err = sneakyTarget.VerifyScope(ctx, sneakyTarget.Client)
	if err == nil {
		t.Fatal("a credential that can read the control-plane prefix must be refused even when the object is absent")
	}
	if !strings.Contains(err.Error(), "GetObject") {
		t.Errorf("the refusal must name the read, not the listing: %v", err)
	}

	// And Configure must run the check BEFORE it touches the cluster: no
	// kubectl command may have run, or the credentials Secret would already be
	// on the host.
	r := &fakeRunner{}
	configureErr := Configure(ctx, r, testManifest(), wideTarget.Target, nil)
	if configureErr == nil {
		t.Fatal("Configure must refuse an out-of-scope credential")
	}
	if !strings.Contains(configureErr.Error(), "can read outside its own prefix") {
		t.Errorf("Configure must fail BECAUSE of the scope check, not incidentally: %v", configureErr)
	}
	if cmds := r.Commands(); len(cmds) != 0 {
		t.Errorf("Configure ran %d commands before refusing the credential: %v", len(cmds), cmds)
	}

	// A target with no cluster-level prefix cannot be scoped at all: its own
	// prefix is the bucket root and the control plane's prefix is inside it.
	unscoped := testTarget()
	unscoped.Endpoint = narrow.URL()
	client, err := unscoped.S3Client()
	if err != nil {
		t.Fatal(err)
	}
	if err := unscoped.VerifyScope(ctx, client); err == nil {
		t.Error("a target with no --prefix must be refused: no policy can grant the bucket root without granting control-plane/")
	} else if !strings.Contains(err.Error(), "--prefix") {
		t.Errorf("the refusal must name the fix: %v", err)
	}
}

// The bucket's protection is reported before an install depends on it, and it
// warns rather than blocks: an unknown storage choice must not stop an install.
func TestPreflightWarnsOnUnencryptedBucket(t *testing.T) {
	ctx := context.Background()

	unprotected := newFakeBucket(t, func(string) bool { return true })
	target := scopedTarget(t, unprotected.URL())
	warnings := target.Preflight(ctx, target.Client)
	if len(warnings) == 0 {
		t.Fatal("a bucket with no default encryption and no versioning must warn, or an install proceeds silently into the state this task exists to remove")
	}
	var encryption, versioning, both bool
	for _, w := range warnings {
		if strings.Contains(w, "server-side encryption") {
			encryption = true
		}
		if strings.Contains(w, "versioning") {
			versioning = true
		}
		if strings.Contains(w, testBucketName) {
			both = true
		}
	}
	if !encryption {
		t.Errorf("no warning names the missing default encryption: %v", warnings)
	}
	if !versioning {
		t.Errorf("no warning names versioning: %v", warnings)
	}
	if !both {
		t.Errorf("the warnings must name the bucket: %v", warnings)
	}
	// Non-vacuity: the questions were actually asked.
	if !unprotected.sawPrefix("?encryption=") || !unprotected.sawPrefix("?versioning=") {
		t.Errorf("the pre-flight never asked the bucket: %v", unprotected.attempts())
	}
	// Warn, never refuse: a caller has nothing to check but the slice.
	if err := target.Validate(); err != nil {
		t.Errorf("the target itself is valid; the warnings must not have changed that: %v", err)
	}

	protected := newFakeBucket(t, func(string) bool { return true })
	protected.encryption = "AES256"
	protected.versioning = "Enabled"
	protectedTarget := scopedTarget(t, protected.URL())
	if warnings := protectedTarget.Preflight(ctx, protectedTarget.Client); len(warnings) != 0 {
		t.Errorf("an encrypted, versioned bucket warned: %v", warnings)
	}

	// A store that refuses the protection questions is a warning, not a block:
	// an unknown Crest storage choice must not fail an install.
	denied := newFakeBucket(t, func(string) bool { return true })
	denied.denyProtection = true
	deniedTarget := scopedTarget(t, denied.URL())
	warnings = deniedTarget.Preflight(ctx, deniedTarget.Client)
	if len(warnings) != 2 {
		t.Fatalf("a store that refuses both protection reads must produce exactly two warnings, got %v", warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "could not read") {
			t.Errorf("the warning must say the state could not be determined: %v", w)
		}
	}
}
