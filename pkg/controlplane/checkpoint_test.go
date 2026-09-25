package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/s3"
)

// The control plane's own recovery target (kn-t47).
//
// TWO THINGS ARE PROVEN HERE, and they are the two halves of "the checkpoints
// have a principal of their own":
//
//   - the credential the install is about to use as the checkpoint principal is
//     REFUSED when it can reach a workload cluster's prefix, and accepted when
//     it cannot. A credential that reads a cluster's backups is cluster-admin
//     on that cluster, and one key that reaches both is one leak away from the
//     fleet's recovery history.
//   - the chart's checkpoint CronJob, rendered from THIS package's values,
//     runs the BACKEND image — the one the chart already pins by digest — and
//     carries the fleet recipient. That is what makes the CronJob renderable at
//     all: it used to require a tools image digest nothing supplied, so the
//     control plane installed with no recovery path.

const testRecipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"

const testClusterPrefix = "clusters/prod-1"

func testCheckpointTarget() CheckpointTarget {
	return CheckpointTarget{
		Recipient:         testRecipient,
		Bucket:            "kubenest-backups",
		Prefix:            backup.ControlPlanePrefix,
		Region:            "main",
		Endpoint:          "http://minio.velero-e2e.svc:9000",
		AccessKeyID:       "AKCHECKPOINT",
		SecretAccessKey:   "checkpoint-secret",
		CredentialsSecret: CheckpointCredentialsSecret,
	}
}

// fakeProbe is the slice of an S3 endpoint the scope check uses. `allowed`
// decides what the credential's policy permits; everything else answers
// AccessDenied, which is what the check discriminates on.
type fakeProbe struct {
	allowed func(key string) bool
	objects map[string][]byte
}

func (f *fakeProbe) permitted(key string) error {
	if f.allowed(key) {
		return nil
	}
	return fmt.Errorf("fake s3: AccessDenied for %s: %w", key, s3.ErrAccessDenied)
}

func (f *fakeProbe) Put(_ context.Context, key string, body []byte) error {
	if err := f.permitted(key); err != nil {
		return err
	}
	f.objects[key] = body
	return nil
}

func (f *fakeProbe) Get(_ context.Context, key string) ([]byte, error) {
	if err := f.permitted(key); err != nil {
		return nil, err
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("fake s3: %w: %s", s3.ErrNotFound, key)
	}
	return body, nil
}

func (f *fakeProbe) List(_ context.Context, prefix string) ([]string, bool, error) {
	if err := f.permitted(prefix); err != nil {
		return nil, false, err
	}
	keys := []string{}
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, false, nil
}

func (f *fakeProbe) BucketEncryption(context.Context) (s3.Protection, error) { return s3.Protection{}, nil }
func (f *fakeProbe) BucketVersioning(context.Context) (s3.Versioning, error) { return s3.Versioning{}, nil }

func newFakeProbe(allowed func(key string) bool) *fakeProbe {
	return &fakeProbe{allowed: allowed, objects: map[string][]byte{}}
}

// A CORRECT POLICY: the control-plane prefix and nothing else. This is what the
// customer's policy has to look like, and the positive arm of the check.
func TestCheckpointScopeAcceptsAPrincipalScopedToTheControlPlanePrefix(t *testing.T) {
	target := testCheckpointTarget()
	probe := newFakeProbe(func(key string) bool {
		return strings.HasPrefix(key, "control-plane/")
	})
	if err := target.VerifyScope(context.Background(), probe, []string{testClusterPrefix}); err != nil {
		t.Fatalf("a credential scoped to the control-plane prefix was refused: %v", err)
	}
	if _, ok := probe.objects["control-plane/"+keyCheckpointProbe]; !ok {
		t.Error("the scope check passed without writing its probe object, so it proved nothing about writing")
	}
}

// THE PLANTED NEGATIVE. A credential that can reach a workload cluster's prefix
// must be refused AS the checkpoint principal — that is the whole reason the
// control plane's checkpoints do not simply reuse a cluster's backup
// credential, and the reason this install refuses rather than renders.
func TestCheckpointScopeRefusesACredentialThatCanReadAClusterPrefix(t *testing.T) {
	target := testCheckpointTarget()
	cases := map[string]struct {
		allowed func(key string) bool
		want    string
	}{
		"list and read a cluster's objects": {
			// What a single credential shared with the cluster looks like.
			allowed: func(key string) bool {
				return strings.HasPrefix(key, "control-plane/") || strings.HasPrefix(key, testClusterPrefix)
			},
			want: "ListObjectsV2",
		},
		"read one cluster object, but not list": {
			// A policy with an explicit object grant: the credential REACHES
			// the cluster's prefix, and an absent object there is still a
			// reach, so this is a failure of the check rather than a pass.
			allowed: func(key string) bool {
				return strings.HasPrefix(key, "control-plane/") || key == testClusterPrefix+"/"+keyCheckpointProbe
			},
			want: "GetObject",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := target.VerifyScope(context.Background(), newFakeProbe(c.allowed), []string{testClusterPrefix})
			if err == nil {
				t.Fatal("a checkpoint credential that can reach a workload cluster's prefix was accepted as the control plane's principal")
			}
			if !strings.Contains(err.Error(), testClusterPrefix) {
				t.Errorf("the refusal does not name the cluster prefix it reached: %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not name the operation that succeeded (%s): %v", c.want, err)
			}
			// The fix has to be actionable: the message says what policy to
			// apply, because the CLI cannot create storage credentials.
			if !strings.Contains(err.Error(), "control-plane/") {
				t.Errorf("the refusal does not say which prefix the credential SHOULD be scoped to: %v", err)
			}
		})
	}
}

// The refusal that says WHICH variable to set, for the credential that is the
// cluster's own.
func TestCheckpointTargetRefusesTheClustersCredentialAsItsOwnPrincipal(t *testing.T) {
	missing := testCheckpointTarget()
	missing.AccessKeyID, missing.SecretAccessKey = "", ""
	err := missing.Validate()
	if err == nil {
		t.Fatal("a checkpoint target with no principal of its own was accepted")
	}
	for _, want := range []string{EnvCheckpointAccessKeyID, EnvCheckpointSecretAccessKey} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, so the operator cannot tell which credential is missing: %v", want, err)
		}
	}
	if err := testCheckpointTarget().Validate(); err != nil {
		t.Errorf("a complete checkpoint target was refused: %v", err)
	}
}

// The Secret the chart's environment reads, with the optional keys omitted
// rather than written empty: an empty AWS_ENDPOINT_URL would send the upload at
// AWS instead of the store the customer named.
func TestCheckpointCredentialsManifestCarriesThePrincipalsKeys(t *testing.T) {
	doc, err := testCheckpointTarget().CredentialsManifest()
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("the credentials manifest is not valid YAML: %v", err)
	}
	if parsed.Metadata.Name != CheckpointCredentialsSecret || parsed.Metadata.Namespace != Namespace {
		t.Errorf("the Secret is %s/%s, want %s/%s: the chart reads the keys by that name", parsed.Metadata.Namespace, parsed.Metadata.Name, Namespace, CheckpointCredentialsSecret)
	}
	target := testCheckpointTarget()
	if parsed.StringData[keyCheckpointAccessKeyID] != target.AccessKeyID {
		t.Errorf("AWS_ACCESS_KEY_ID = %q, want the principal's", parsed.StringData[keyCheckpointAccessKeyID])
	}
	if parsed.StringData[keyCheckpointSecretAccessKey] != target.SecretAccessKey {
		t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want the principal's", parsed.StringData[keyCheckpointSecretAccessKey])
	}
	if parsed.StringData[keyCheckpointEndpoint] != target.Endpoint || parsed.StringData[keyCheckpointRegion] != target.Region {
		t.Errorf("the store's coordinates are not in the Secret: %v", parsed.StringData)
	}

	bare := testCheckpointTarget()
	bare.Endpoint, bare.Region = "", ""
	doc, err = bare.CredentialsManifest()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), keyCheckpointEndpoint) || strings.Contains(string(doc), keyCheckpointRegion) {
		t.Errorf("an unset endpoint or region was written into the Secret as an empty value:\n%s", doc)
	}
}

// The install-side half of the same fact: the values document that enables the
// CronJob carries the four values the chart requires, under the chart's own
// key names. A rename here is a chart that renders nothing and an install that
// reports the control plane as protected while no checkpoint exists.
func TestValuesCarriesTheRecoveryTargetWhenThereIsOne(t *testing.T) {
	target := testCheckpointTarget()
	out, err := Values(
		Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com", Checkpoint: &target},
		testSecrets(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("Values is not valid YAML: %v", err)
	}
	checkpoint := section(t, doc, "checkpoint")
	want := map[string]any{
		"enabled":           true,
		"recipient":         target.Recipient,
		"bucket":            target.Bucket,
		"prefix":            target.Prefix,
		"credentialsSecret": target.CredentialsSecret,
	}
	if len(checkpoint) != len(want) {
		t.Errorf("checkpoint keys = %v, want exactly %v", sortedKeys(checkpoint), want)
	}
	for key, expected := range want {
		if checkpoint[key] != expected {
			t.Errorf("checkpoint.%s = %v, want %v", key, checkpoint[key], expected)
		}
	}
	// A target the chart could not render is refused HERE, before an apply,
	// rather than as a helm failure whose message names no field.
	broken := testCheckpointTarget()
	broken.Recipient = ""
	if _, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com", Checkpoint: &broken}, testSecrets()); err == nil {
		t.Error("Values rendered a checkpoint target with no fleet recipient")
	}
}

// ------------------------------------------------------------------ the chart

// THE CHART HALF OF THE BEAD. The values THIS package renders are what
// `platform install --control-plane` hands the chart, so the test renders the
// CLI's own embedded chart (the archive the binary installs, not the source
// checkout) with them and reads the CronJob back.
func TestTheChartRendersTheCheckpointCronJobWithTheBackendImageAndTheRecipient(t *testing.T) {
	helm := requireHelm(t)
	archive, valuesFile := renderInputs(t, Settings{
		Domain:     "kn.example.com",
		AdminEmail: "admin@kn.example.com",
		Checkpoint: &CheckpointTarget{
			Recipient:         testRecipient,
			Bucket:            "kubenest-control-plane-checkpoints",
			Prefix:            backup.ControlPlanePrefix,
			Region:            "main",
			Endpoint:          "http://minio.velero-e2e.svc:9000",
			AccessKeyID:       "AKCHECKPOINT",
			SecretAccessKey:   "checkpoint-secret",
			CredentialsSecret: CheckpointCredentialsSecret,
		},
	})

	// The image the backend itself runs: the checkpoint has to be the same
	// digest, because it IS the backend image that carries pg_dump and age.
	backend := helmTemplate(t, helm, archive, valuesFile, "templates/backend-deployment.yaml")
	deployment := objectNamed(t, backend, "Deployment", ReleaseName+"-backend", true)
	backendPod := mapping(t, dig(t, deployment, "spec", "template", "spec"))
	backendContainer := containerNamed(t, backendPod, "containers", "backend")
	if backendContainer == nil {
		t.Fatal("the rendered backend Deployment has no `backend` container")
	}
	backendImage, _ := backendContainer["image"].(string)
	if backendImage == "" {
		t.Fatal("the rendered backend Deployment has no image, so there is nothing for the checkpoint to match")
	}

	objects := helmTemplate(t, helm, archive, valuesFile, "templates/checkpoint-cronjob.yaml")
	cronjob := objectNamed(t, objects, "CronJob", ReleaseName+"-checkpoint", true)
	drill := objectNamed(t, objects, "CronJob", ReleaseName+"-checkpoint-drill", true)

	checkpointPod := mapping(t, dig(t, cronjob, "spec", "jobTemplate", "spec", "template", "spec"))
	checkpoint := containerNamed(t, checkpointPod, "containers", "checkpoint")
	if checkpoint == nil {
		t.Fatal("the checkpoint CronJob has no `checkpoint` container")
	}
	if got := checkpoint["image"]; got != backendImage {
		t.Errorf("the checkpoint container runs %v, want the backend image %q: the entrypoint that dumps, seals and uploads lives in that image, and a separate tools image is the thing this bead removed", got, backendImage)
	}

	// THE RECIPIENT, and where the objects go: the four values the install
	// passes, read back out of the rendered environment.
	for _, expected := range []struct{ name, value string }{
		{"CHECKPOINT_RECIPIENT", testRecipient},
		{"CHECKPOINT_BUCKET", "kubenest-control-plane-checkpoints"},
		{"CHECKPOINT_PREFIX", backup.ControlPlanePrefix},
	} {
		variable := envOf(t, checkpoint, expected.name)
		if variable == nil {
			t.Errorf("the checkpoint container has no %s", expected.name)
			continue
		}
		if variable["value"] != expected.value {
			t.Errorf("%s = %v, want %q", expected.name, variable["value"], expected.value)
		}
	}

	// The credentials come from the Secret the install creates, named by the
	// values rather than written into the chart.
	credentials := envOf(t, checkpoint, "AWS_ACCESS_KEY_ID")
	if credentials == nil {
		t.Fatal("the checkpoint container has no AWS_ACCESS_KEY_ID")
	}
	keyRef, ok := credentials["valueFrom"].(map[string]any)
	if !ok {
		t.Fatalf("AWS_ACCESS_KEY_ID is not read from a Secret: %v", credentials)
	}
	if got := dig(t, keyRef, "secretKeyRef", "name"); got != CheckpointCredentialsSecret {
		t.Errorf("AWS_ACCESS_KEY_ID comes from Secret %v, want %v", got, CheckpointCredentialsSecret)
	}

	// THE JOB MUST BE ABLE TO PUBLISH. The entrypoint marks a checkpoint
	// eligible with a MERGE PATCH of one key in the status ConfigMap
	// (app/services/checkpoint_runner.py::_write_status). A chart that granted
	// only `update` would fail the run at its last step with a 403 — after the
	// objects were uploaded — and the control plane would stay unprotected
	// while looking healthy.
	role := objectNamed(t, objects, "Role", ReleaseName+"-checkpoint", true)
	verbs := map[string]bool{}
	for _, rule := range dig(t, role, "rules").([]any) {
		entry, _ := rule.(map[string]any)
		resources, _ := entry["resources"].([]any)
		for _, resource := range resources {
			if resource != "configmaps" {
				continue
			}
			granted, _ := entry["verbs"].([]any)
			for _, verb := range granted {
				verbs[verb.(string)] = true
			}
		}
	}
	for _, verb := range []string{"get", "create", "patch"} {
		if !verbs[verb] {
			t.Errorf("the checkpoint ServiceAccount cannot %s the status ConfigMap: %v", verb, verbs)
		}
	}

	// THE SAME IMAGE FOR THE DRILL, except the one stage that needs a server.
	drillPod := mapping(t, dig(t, drill, "spec", "jobTemplate", "spec", "template", "spec"))
	fetch := containerNamed(t, drillPod, "initContainers", "fetch")
	verify := containerNamed(t, drillPod, "containers", "verify")
	if fetch == nil || verify == nil {
		t.Fatal("the drill CronJob is missing its fetch or verify stage")
	}
	for name, container := range map[string]map[string]any{"fetch": fetch, "verify": verify} {
		if container["image"] != backendImage {
			t.Errorf("the drill's %s stage runs %v, want the backend image %q", name, container["image"], backendImage)
		}
	}
	restore := containerNamed(t, drillPod, "initContainers", "restore")
	if restore == nil {
		t.Fatal("the drill CronJob has no `restore` stage")
	}
	if restore["image"] == backendImage {
		t.Error("the drill's restore stage was pointed at the backend image: it starts a scratch Postgres, and the backend image carries a client, not a server")
	}

	// NOTHING RENDERED NAMES THE TOOLS IMAGE. The chart used to require its
	// digest and nothing supplied one, which is why the CronJob could not
	// render; a leftover reference would bring that back.
	for _, set := range [][]map[string]any{objects, backend} {
		for _, object := range set {
			body, err := yaml.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "checkpoint-tools") {
				t.Errorf("a rendered object still names the tools image:\n%s", body)
			}
		}
	}
}

// The other direction of the same values: no backup target means no checkpoint
// CronJob — and a release that still renders, because an install with no
// recovery target must not fail on a value the chart cannot render.
func TestTheChartRendersNoCheckpointCronJobWithoutARecoveryTarget(t *testing.T) {
	helm := requireHelm(t)
	archive, valuesFile := renderInputs(t, Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"})
	objects := helmTemplate(t, helm, archive, valuesFile, "")
	for _, object := range objects {
		if object["kind"] != "CronJob" {
			continue
		}
		metadata, _ := object["metadata"].(map[string]any)
		t.Errorf("an install with no recovery target rendered CronJob %v", metadata["name"])
	}
	// The rest of the control plane is there: the absence above is the
	// checkpoint's, not a chart that rendered nothing.
	if objectNamed(t, objects, "Deployment", ReleaseName+"-backend", false) == nil {
		t.Error("the whole chart rendered no backend Deployment, so the absence of a CronJob proves nothing")
	}
}

// requireHelm skips when helm is not on PATH. It is a loud skip: the check is
// the only one that renders the chart, and a machine without helm cannot run
// it. (./scripts/install-toolchain.sh installs and pins helm.)
func requireHelm(t *testing.T) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skipf("helm is not on PATH, so the chart cannot be rendered with this package's values here: %v", err)
	}
	return helm
}

// renderInputs writes the CLI's embedded chart archive and the CLI's own values
// document into a temp directory, so the test renders exactly what the binary
// installs.
func renderInputs(t *testing.T, settings Settings) (archive, valuesFile string) {
	t.Helper()
	version, err := ChartVersion()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive = filepath.Join(dir, "kubenest-"+version+".tgz")
	if err := os.WriteFile(archive, ChartArchive(), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := Values(settings, testSecrets())
	if err != nil {
		t.Fatal(err)
	}
	valuesFile = filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesFile, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive, valuesFile
}

// helmTemplate renders the embedded chart with the CLI's values. showOnly names
// one template when the test wants only its objects; an empty showOnly renders
// the whole chart, which is what proves a release still installs.
func helmTemplate(t *testing.T, helm, archive, valuesFile string, showOnly string) []map[string]any {
	t.Helper()
	command := exec.Command(helm, "template", ReleaseName, archive, "--namespace", Namespace, "-f", valuesFile)
	if showOnly != "" {
		command.Args = append(command.Args, "--show-only", showOnly)
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s failed: %v\n%s", showOnly, err, out)
	}
	var objects []map[string]any
	decoder := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("rendering %s produced unreadable YAML: %v\n%s", showOnly, err, out)
		}
		if len(doc) > 0 && doc["kind"] != nil {
			objects = append(objects, doc)
		}
	}
	if len(objects) == 0 {
		t.Fatalf("helm rendered no objects from %s, so nothing below was checked", showOnly)
	}
	return objects
}

// mapping asserts the shape of a nested value `dig` returned, so the rest of a
// test reads as a path rather than as a chain of type assertions.
func mapping(t *testing.T, value any) map[string]any {
	t.Helper()
	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected a mapping, got %T", value)
	}
	return m
}

// objectNamed finds one object by kind and name. `required` fails the test when
// it is absent; the disabled-checkpoint case asks for absence instead.
func objectNamed(t *testing.T, objects []map[string]any, kind, name string, required bool) map[string]any {
	t.Helper()
	for _, object := range objects {
		if object["kind"] != kind {
			continue
		}
		metadata, _ := object["metadata"].(map[string]any)
		if metadata["name"] == name {
			return object
		}
	}
	if required {
		t.Fatalf("no %s named %q in the rendered chart", kind, name)
	}
	return nil
}
