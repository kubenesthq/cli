package backup

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

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
		"get schedule":              "Enabled",
	})}
	if err := Configure(context.Background(), r, testManifest(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}

	// Order: credentials before the location that references them, location
	// proven before the schedule that writes to it.
	var secretAt, locationAt, scheduleAt = -1, -1, -1
	for i, cmd := range r.Commands() {
		if !strings.Contains(cmd, "kubectl apply") {
			continue
		}
		doc := decodeApplied(t, cmd)
		switch {
		case strings.Contains(doc, "kind: Secret"):
			secretAt = i
		case strings.Contains(doc, "kind: BackupStorageLocation"):
			locationAt = i
		case strings.Contains(doc, "kind: Schedule"):
			scheduleAt = i
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
		"get schedule":              "Enabled",
	})}
	if err := Configure(context.Background(), r, testManifest(), testTarget(), nil); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range r.Commands() {
		if !strings.Contains(cmd, "kubectl apply") {
			continue
		}
		doc := decodeApplied(t, cmd)
		if !strings.Contains(doc, "kind: Schedule") {
			continue
		}
		var schedule struct {
			Metadata map[string]string `yaml:"metadata"`
			Spec     struct {
				Schedule string            `yaml:"schedule"`
				Template map[string]string `yaml:"template"`
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
		if schedule.Spec.Template["ttl"] != "336h0m0s" {
			t.Errorf("ttl = %q, want 336h0m0s (14 × 24h)", schedule.Spec.Template["ttl"])
		}
		if schedule.Spec.Template["storageLocation"] != StorageLocationName {
			t.Errorf("storageLocation = %q", schedule.Spec.Template["storageLocation"])
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
		"get backup": `{"status":{"phase":"Failed","failureReason":"bucket vanished"}}`,
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
		"get backup": `{"status":{"phase":"Completed"}}`,
	})}
	if err := TakeBackup(context.Background(), r, testManifest(), "manual-x", nil); err != nil {
		t.Fatal(err)
	}
}

// decodeApplied extracts the YAML document from an apply command's base64.
func decodeApplied(t *testing.T, cmd string) string {
	t.Helper()
	var encoded string
	if _, err := fmt.Sscanf(cmd, "printf '%%s' %s ", &encoded); err != nil {
		t.Fatalf("apply command shape changed: %q", cmd)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("apply payload is not base64: %v", err)
	}
	return string(raw)
}
