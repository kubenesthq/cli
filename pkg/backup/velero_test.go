package backup

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/leakscan"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// fakeRunner records every command and answers via Respond. pkg/component's
// componenttest fake is not imported on purpose — that package is another
// bead's in-flight work and this one must build without it.
type fakeRunner struct {
	mu       sync.Mutex
	commands []string
	inputs   [][]byte
	// Respond maps a command to its result; nil means success, empty output.
	Respond func(command string) (sshx.Result, error)
	// RespondInput, when set, answers a RunInput from the command AND the
	// document it streamed. A read-back of what a create just wrote — the
	// repository password, which the get can only answer after the create has
	// run — cannot be answered from the command alone.
	RespondInput func(command string, stdin []byte) (sshx.Result, error)
}

func (f *fakeRunner) Run(ctx context.Context, command string) (sshx.Result, error) {
	return f.record(command, nil)
}

// RunInput records the streamed manifest alongside its command. Velero's
// values document carries the backup target's object-store credentials, so
// the same rule as the agent applies here: content over stdin, never in the
// command string (kn-40rd).
func (f *fakeRunner) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	var payload []byte
	if stdin != nil {
		var err error
		payload, err = io.ReadAll(stdin)
		if err != nil {
			return sshx.Result{}, err
		}
	}
	return f.record(command, payload)
}

func (f *fakeRunner) record(command string, stdin []byte) (sshx.Result, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	// Appended unconditionally, nil included, so inputs stays index-aligned
	// with commands. A stdin slice that skipped the plain Run calls would
	// silently pair a payload with the wrong command.
	f.inputs = append(f.inputs, stdin)
	f.mu.Unlock()
	if f.RespondInput != nil && stdin != nil {
		return f.RespondInput(command, stdin)
	}
	if f.Respond == nil {
		return sshx.Result{}, nil
	}
	return f.Respond(command)
}

func (f *fakeRunner) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

// Inputs returns every stdin payload streamed so far, in order, skipping the
// plain Run calls that streamed nothing.
func (f *fakeRunner) Inputs() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]byte
	for _, in := range f.inputs {
		if in != nil {
			out = append(out, append([]byte(nil), in...))
		}
	}
	return out
}

// appliedDoc is one `kubectl apply` and the document it streamed. At is the
// index in Commands, so a test can still assert ordering.
type appliedDoc struct {
	At  int
	Doc string
}

// Applied returns every applied document, read from STDIN rather than from
// the command. The documents carry the object store's access key and secret
// key, so they no longer appear in the command at all (kn-40rd) — a test that
// still parsed the command would find nothing and pass vacuously.
func (f *fakeRunner) Applied() []appliedDoc {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []appliedDoc
	for i, c := range f.commands {
		if strings.Contains(c, "kubectl apply") {
			out = append(out, appliedDoc{At: i, Doc: string(f.inputs[i])})
		}
	}
	return out
}

// testManifest builds an in-memory bundle with everything backup consumes.
// Timeouts are short so converge failures settle in test time, not manifest
// time — the values still flow through the manifest, per the invariant.
func testManifest() *manifest.Manifest {
	return &manifest.Manifest{
		Bundle: "1.0",
		Core:   manifest.Components{"velero": "12.1.0"},
		Limits: manifest.Limits{Timeouts: manifest.Timeouts{
			"component-ready": 300 * time.Millisecond,
			"backup":          300 * time.Millisecond,
			"restore-drill":   300 * time.Millisecond,
			"node-ready":      300 * time.Millisecond,
		}},
		Backup: manifest.Backup{
			ObjectStorePlugin: manifest.ObjectStorePlugin{Provider: "aws", Version: "v1.14.2"},
			Defaults: manifest.BackupDefaults{
				DatastoreSnapshot: manifest.BackupSchedule{Interval: manifest.Interval(time.Hour), Keep: 24},
				WorkloadBackup:    manifest.BackupSchedule{Interval: manifest.Interval(24 * time.Hour), Keep: 14},
				RestoreDrill:      manifest.DrillSchedule{Interval: manifest.Interval(7 * 24 * time.Hour)},
			},
		},
	}
}

func TestChartPinsComeFromTheManifest(t *testing.T) {
	chart, err := Chart(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if chart.Version != "12.1.0" {
		t.Errorf("chart version = %q, want the manifest's 12.1.0", chart.Version)
	}
	if chart.Repo != "https://vmware-tanzu.github.io/helm-charts" {
		t.Errorf("chart repo = %q — the chart still releases from vmware-tanzu; velero-io hosts no chart index", chart.Repo)
	}
	if !strings.Contains(chart.ValuesYAML, "velero/velero-plugin-for-aws:v1.14.2") {
		t.Errorf("values do not carry the pinned plugin image:\n%s", chart.ValuesYAML)
	}
	// The manifest the HelmChart CR renders to must be valid YAML with the
	// values inline.
	doc, err := chart.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("rendered HelmChart is not YAML: %v", err)
	}
}

func TestChartInstallsUnconfigured(t *testing.T) {
	chart, err := Chart(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(chart.ValuesYAML), &values); err != nil {
		t.Fatalf("values are not YAML: %v", err)
	}
	// No target at install: no BSL, no VSL, no credentials Secret. The node
	// agent IS on — file-system backup is the only path that gets Local PV
	// LVM volume data off the node.
	if values["backupsEnabled"] != false || values["snapshotsEnabled"] != false {
		t.Errorf("install must not create storage/snapshot locations: %v", chart.ValuesYAML)
	}
	creds, _ := values["credentials"].(map[string]any)
	if creds["useSecret"] != false {
		t.Errorf("credentials.useSecret must be false at install:\n%s", chart.ValuesYAML)
	}
	if values["deployNodeAgent"] != true {
		t.Errorf("deployNodeAgent must be true:\n%s", chart.ValuesYAML)
	}
	conf, _ := values["configuration"].(map[string]any)
	if conf["defaultVolumesToFsBackup"] != true {
		t.Errorf("configuration.defaultVolumesToFsBackup must be true:\n%s", chart.ValuesYAML)
	}
}

// A manifest missing the velero pin or the plugin pin is an error — the
// bundle decides both; nothing here defaults.
func TestChartRefusesUnpinnedManifests(t *testing.T) {
	m := testManifest()
	m.Core = manifest.Components{}
	if _, err := Chart(m); err == nil || !strings.Contains(err.Error(), "core.velero") {
		t.Errorf("missing velero pin: err = %v", err)
	}

	m = testManifest()
	m.Backup.ObjectStorePlugin = manifest.ObjectStorePlugin{}
	if _, err := Chart(m); err == nil || !strings.Contains(err.Error(), "object-store-plugin") {
		t.Errorf("missing plugin pin: err = %v", err)
	}

	m = testManifest()
	m.Backup.ObjectStorePlugin.Provider = "gcp"
	if _, err := Chart(m); err == nil || !strings.Contains(err.Error(), "aws") {
		t.Errorf("unknown provider must be rejected, not ignored: err = %v", err)
	}
}

func TestUnconfiguredReadsTheStorageLocations(t *testing.T) {
	ctx := context.Background()
	r := &fakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		return sshx.Result{Stdout: ""}, nil
	}}
	un, err := Unconfigured(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !un {
		t.Error("no BackupStorageLocation must report unconfigured")
	}

	r = &fakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		return sshx.Result{Stdout: "default"}, nil
	}}
	un, err = Unconfigured(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if un {
		t.Error("an existing BackupStorageLocation must not report unconfigured")
	}
}

// chartObject is the slice of a rendered chart document this test reads.
// yaml.v3 ignores every field that is not here.
type chartObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules   []chartRule `yaml:"rules"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"subjects"`
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string           `yaml:"serviceAccountName"`
				Containers         []chartContainer `yaml:"containers"`
				InitContainers     []chartContainer `yaml:"initContainers"`
				Volumes            []chartVolume    `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type chartRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// chartContainer is the part of a container a credential could travel in: env
// and volume mounts, in either direction.
type chartContainer struct {
	Name         string          `yaml:"name"`
	Env          []chartEnv      `yaml:"env"`
	VolumeMounts []chartMountRef `yaml:"volumeMounts"`
}

type chartEnv struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom *struct {
		SecretKeyRef *struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

type chartMountRef struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}

type chartVolume struct {
	Name   string `yaml:"name"`
	Secret *struct {
		SecretName string `yaml:"secretName"`
	} `yaml:"secret"`
}

// chartRuleAllows reports whether one Role rule permits verb on resource in
// apiGroup. The pinned chart expresses its server rule as `*` on `*`, which is
// what the wildcard branch is for: the assertion is about the permission
// existing, not about how the chart spells it.
func chartRuleAllows(rule chartRule, apiGroup, resource, verb string) bool {
	has := func(list []string, want string) bool {
		for _, v := range list {
			if v == want || v == "*" {
				return true
			}
		}
		return false
	}
	return has(rule.APIGroups, apiGroup) && has(rule.Resources, resource) && has(rule.Verbs, verb)
}

// carriesRepoPassword reports whether one string names any part of the
// repository password's identity: the Secret it lives in, the key inside it,
// kopia's word for it, or the vendored default the whole change exists to stop
// using. Case-insensitively — an env var's name is whatever its author spelled,
// and the credential it would carry is the same one.
func carriesRepoPassword(s string) bool {
	low := strings.ToLower(s)
	for _, marker := range []string{
		strings.ToLower(RepositorySecretName),
		RepositoryPasswordKey,
		"repo-password",
		"repo_password",
		"static-passw0rd",
	} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// renderVeleroChart renders the vendored pinned chart with the values Chart
// produces, and parses every document of the result. It runs the helm binary
// rather than reading the chart's templates: the question is what the chart
// does with these values, and only helm can answer it. A render that fails is
// a failure, never a skip — only a missing binary skips.
func renderVeleroChart(t *testing.T, chart k3s.HelmChart) (string, []chartObject) {
	t.Helper()
	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("the helm binary is not on PATH: this assertion renders the pinned chart (testdata/velero-12.1.0.tgz) with it, and a render that did not happen is not evidence")
	}
	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, []byte(chart.ValuesYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	// The release name and namespace are the ones Install will use, so the
	// rendered object names are the ones the cluster will see.
	out, err := exec.Command(helmBin, "template", chart.Name, "testdata/velero-12.1.0.tgz",
		"--namespace", Namespace, "-f", values).Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		t.Fatalf("rendering the vendored pinned chart failed: %v\n%s", err, stderr)
	}
	rendered := string(out)
	var objs []chartObject
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var o chartObject
		if err := dec.Decode(&o); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("the rendered chart is not a YAML document stream: %v", err)
		}
		if o.Kind != "" {
			objs = append(objs, o)
		}
	}
	return rendered, objs
}

// TestChartCarriesThePerClusterRepositoryPassword tests the mechanism that
// keeps a cluster's volume backups off Velero's vendored password: the CLI
// installs velero-repo-credentials, the chart's RBAC is what lets the server
// read it through the API, and nothing else — chart object, env var or mounted
// Secret — carries a second copy.
func TestChartCarriesThePerClusterRepositoryPassword(t *testing.T) {
	ctx := context.Background()
	chart, err := Chart(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	rendered, objs := renderVeleroChart(t, chart)

	// The chart must not render the Secret. Velero's own
	// EnsureCommonRepositoryKey creates velero-repo-credentials holding
	// static-passw0rd when it is absent, so a chart object of that name would
	// either race the CLI's create or hand the repository straight back to the
	// password printed in Velero's source.
	for _, o := range objs {
		if o.Metadata.Name == RepositorySecretName {
			t.Errorf("the chart renders %s %s/%s: the repository-password Secret must be the CLI's object, so that nothing clobbers it and nothing defaults it",
				o.Kind, o.Metadata.Namespace, o.Metadata.Name)
		}
	}
	for _, marker := range []string{RepositorySecretName, RepositoryPasswordKey, "static-passw0rd"} {
		if strings.Contains(rendered, marker) {
			t.Errorf("the rendered chart mentions %q: no part of the repository password may come from the chart", marker)
		}
	}

	// Velero reads the password through the Kubernetes API, so what the chart
	// must supply is the RBAC to do it: a Role in its own namespace permitting
	// get/list on secrets, bound to the ServiceAccount the server runs as.
	var server *chartObject
	for i := range objs {
		if objs[i].Kind == "Deployment" && objs[i].Metadata.Namespace == Namespace {
			server = &objs[i]
		}
	}
	if server == nil {
		t.Fatalf("the chart renders no Deployment in %s, and the server is what reads the password", Namespace)
	}
	sa := server.Spec.Template.Spec.ServiceAccountName
	if sa == "" {
		t.Errorf("the server Deployment runs as no ServiceAccount: without one no RoleBinding lets it read %s/%s", Namespace, RepositorySecretName)
	}
	serviceAccounts := map[string]bool{}
	for _, o := range objs {
		if o.Kind == "ServiceAccount" && o.Metadata.Namespace == Namespace {
			serviceAccounts[o.Metadata.Name] = true
		}
	}
	if !serviceAccounts[sa] {
		t.Errorf("the server Deployment runs as ServiceAccount %q, which the chart does not render in %s", sa, Namespace)
	}
	secretReaders := map[string]bool{}
	for _, o := range objs {
		if o.Kind != "Role" || o.Metadata.Namespace != Namespace {
			continue
		}
		for _, rule := range o.Rules {
			if chartRuleAllows(rule, "", "secrets", "get") && chartRuleAllows(rule, "", "secrets", "list") {
				secretReaders[o.Metadata.Name] = true
			}
		}
	}
	if len(secretReaders) == 0 {
		t.Errorf("no Role in %s grants get and list on secrets, so the server cannot read %s/%s through the API", Namespace, Namespace, RepositorySecretName)
	}
	bound := false
	for _, o := range objs {
		if o.Kind != "RoleBinding" || o.Metadata.Namespace != Namespace || o.RoleRef.Kind != "Role" || !secretReaders[o.RoleRef.Name] {
			continue
		}
		for _, s := range o.Subjects {
			if s.Kind == "ServiceAccount" && s.Name == sa {
				bound = true
			}
		}
	}
	if !bound {
		t.Errorf("no RoleBinding in %s binds a secrets-reading Role to the server's ServiceAccount %q", Namespace, sa)
	}

	// And nothing else may carry the password into the server: an env var or a
	// mounted Secret would be a second copy that a rotation never updates.
	pod := server.Spec.Template.Spec
	containers := append(append([]chartContainer{}, pod.InitContainers...), pod.Containers...)
	for _, c := range containers {
		for _, e := range c.Env {
			from := ""
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				from = e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
			}
			if carriesRepoPassword(e.Name) || carriesRepoPassword(e.Value) || carriesRepoPassword(from) {
				t.Errorf("container %q env %q carries the repository password: the server reads %s/%s through the API",
					c.Name, e.Name, Namespace, RepositorySecretName)
			}
		}
		for _, m := range c.VolumeMounts {
			if carriesRepoPassword(m.Name) || carriesRepoPassword(m.MountPath) {
				t.Errorf("container %q mounts %q at %q, which names the repository password: it must be read through the API, never mounted",
					c.Name, m.Name, m.MountPath)
			}
		}
	}
	for _, v := range pod.Volumes {
		if carriesRepoPassword(v.Name) || (v.Secret != nil && carriesRepoPassword(v.Secret.SecretName)) {
			t.Errorf("the server pod has a volume %q naming the repository password: a mounted Secret is a copy the server never rotates", v.Name)
		}
	}

	// The document the CLI installs: Velero matches the name and the namespace
	// exactly, and stringData keeps the password out of the document as base64
	// noise.
	doc, err := RepositoryPasswordSecret("pw-xyz")
	if err != nil {
		t.Fatal(err)
	}
	var sec struct {
		Kind     string `yaml:"kind"`
		Type     string `yaml:"type"`
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		StringData map[string]string `yaml:"stringData"`
		Data       map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(doc, &sec); err != nil {
		t.Fatalf("the repository-password Secret is not YAML: %v", err)
	}
	if sec.Kind != "Secret" || sec.Type != "Opaque" {
		t.Errorf("kind/type = %s/%s, want Secret/Opaque", sec.Kind, sec.Type)
	}
	if sec.Metadata.Name != RepositorySecretName || sec.Metadata.Namespace != Namespace {
		t.Errorf("the Secret is %s/%s, want %s/%s", sec.Metadata.Namespace, sec.Metadata.Name, Namespace, RepositorySecretName)
	}
	if sec.StringData[RepositoryPasswordKey] != "pw-xyz" {
		t.Errorf("stringData[%q] = %q, want pw-xyz", RepositoryPasswordKey, sec.StringData[RepositoryPasswordKey])
	}
	if _, ok := sec.Data[RepositoryPasswordKey]; ok {
		t.Error("the password must travel in stringData, not in data: the API server does the encoding")
	}

	// A cluster that already has a repository: the stored password comes back
	// and nothing is written.
	storedJSON := `{"data":{"` + RepositoryPasswordKey + `":"` + base64.StdEncoding.EncodeToString([]byte("stored-pw")) + `"}}`
	r := &fakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if !strings.Contains(cmd, "get secret "+RepositorySecretName) || !strings.Contains(cmd, "-o json") {
			t.Errorf("reading the repository password must ask the API for the Secret as JSON: %s", cmd)
			return sshx.Result{ExitCode: 1}, nil
		}
		return sshx.Result{Stdout: storedJSON}, nil
	}}
	pw, created, err := EnsureRepositoryPassword(ctx, r)
	if err != nil {
		t.Fatalf("an existing repository password must be returned, not replaced: %v", err)
	}
	if pw != "stored-pw" || created {
		t.Errorf("EnsureRepositoryPassword = (%q, %v), want (stored-pw, false)", pw, created)
	}
	for _, c := range r.Commands() {
		if strings.Contains(c, "kubectl create") || strings.Contains(c, "kubectl apply") {
			t.Errorf("a stored password must never be written over: %s", c)
		}
	}

	// A Secret that is there and unusable is not an absent one: regenerating
	// would encrypt every later backup under a password the running server
	// cannot read the existing repository with.
	for _, body := range []string{"not json", `{"data":{}}`, `{"data":{"` + RepositoryPasswordKey + `":""}}`} {
		r := &fakeRunner{Respond: func(cmd string) (sshx.Result, error) {
			return sshx.Result{Stdout: body}, nil
		}}
		if _, _, err := EnsureRepositoryPassword(ctx, r); err == nil {
			t.Errorf("an unusable stored Secret (%s) must be an error, not a regeneration", body)
		}
		for _, c := range r.Commands() {
			if strings.Contains(c, "create") {
				t.Errorf("an unusable stored Secret (%s) must not be overwritten: %s", body, c)
			}
		}
	}

	// The first call: absent Secret, generate, create, read back. The get
	// answers NotFound until the create lands, so this is exactly the
	// first-install path.
	var generated string
	var createdDoc []byte
	namespaceApplied := false
	r = &fakeRunner{
		Respond: func(cmd string) (sshx.Result, error) {
			if generated == "" {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "` + RepositorySecretName + `" not found`}, nil
			}
			return sshx.Result{Stdout: `{"data":{"` + RepositoryPasswordKey + `":"` + base64.StdEncoding.EncodeToString([]byte(generated)) + `"}}`}, nil
		},
		RespondInput: func(cmd string, stdin []byte) (sshx.Result, error) {
			// The API refuses a Secret in a namespace that does not exist yet,
			// which is the state before the Velero chart runs (found on real
			// hardware 2026-09-25).
			if strings.Contains(string(stdin), "kind: Namespace") {
				namespaceApplied = true
				return sshx.Result{}, nil
			}
			if !namespaceApplied {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): error when creating "STDIN": namespaces "` + Namespace + `" not found`}, nil
			}
			createdDoc = append([]byte(nil), stdin...)
			var doc struct {
				StringData map[string]string `yaml:"stringData"`
			}
			if err := yaml.Unmarshal(stdin, &doc); err != nil {
				t.Fatalf("the created Secret is not YAML: %v", err)
			}
			generated = doc.StringData[RepositoryPasswordKey]
			return sshx.Result{}, nil
		},
	}
	pw, created, err = EnsureRepositoryPassword(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("the first call must report that it generated the password")
	}
	if generated == "" {
		t.Error("no kubectl create streamed a Secret: the repository password would never reach the cluster")
	}
	if pw != generated {
		t.Errorf("EnsureRepositoryPassword returned %q, but the cluster stores %q", pw, generated)
	}
	if len(generated) != 32 || strings.Trim(generated, "0123456789abcdef") != "" {
		t.Errorf("the generated password %q is not 32 hex characters", generated)
	}
	if generated != "" && !strings.Contains(string(createdDoc), generated) {
		t.Error("the generated password must reach the cluster over STDIN, inside the document")
	}
	creates := 0
	for _, c := range r.Commands() {
		if strings.Contains(c, "kubectl create") {
			creates++
			if !strings.Contains(c, "create -f -") {
				t.Errorf("the Secret must be created from stdin, never from a command string: %s", c)
			}
		}
		if strings.Contains(c, "kubectl apply") && !namespaceApplied {
			t.Errorf("creation must be kubectl create, never apply: %s", c)
		}
		if generated != "" && leakscan.RecoverableFrom(c, generated) {
			t.Errorf("the repository password is recoverable from a command line: %s", c)
		}
	}
	if creates != 1 {
		t.Errorf("want exactly one kubectl create, got %d: %v", creates, r.Commands())
	}

	// The read on its own: absent is an error, and it writes nothing.
	r = &fakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "` + RepositorySecretName + `" not found`}, nil
	}}
	if _, err := RepositoryPassword(ctx, r); err == nil {
		t.Error("an absent repository-password Secret is an error: the caller must install one, never invent one over a live repository")
	}
	for _, c := range r.Commands() {
		if strings.Contains(c, "create") || strings.Contains(c, "apply") {
			t.Errorf("RepositoryPassword is a read: %s", c)
		}
	}

	// Install's ordering is the point of the whole change: the Secret must be
	// created before the HelmChart is written, or a server pod that starts
	// first finds the Secret absent and creates the repository under the
	// vendored password instead.
	var installed bool
	ir := &fakeRunner{
		Respond: func(cmd string) (sshx.Result, error) {
			if !installed {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "` + RepositorySecretName + `" not found`}, nil
			}
			return sshx.Result{Stdout: `{"data":{"` + RepositoryPasswordKey + `":"` + base64.StdEncoding.EncodeToString([]byte("install-pw")) + `"}}`}, nil
		},
		RespondInput: func(cmd string, stdin []byte) (sshx.Result, error) {
			if strings.Contains(cmd, "kubectl create -f -") {
				installed = true
			}
			return sshx.Result{}, nil
		},
	}
	// The converge wait cannot be satisfied by a fake with no pods, so the
	// returned error is expected here; the ordering below is not.
	_ = Install(ctx, ir, testManifest(), nil)
	createAt, manifestAt := -1, -1
	for i, c := range ir.Commands() {
		if strings.Contains(c, "kubectl create -f -") {
			createAt = i
		}
		if strings.Contains(c, k3s.ManifestDir) {
			manifestAt = i
		}
	}
	if createAt < 0 || manifestAt < 0 || createAt > manifestAt {
		t.Errorf("Install must create %s/%s before writing the Velero manifest: create at %d, manifest write at %d, commands %v",
			Namespace, RepositorySecretName, createAt, manifestAt, ir.Commands())
	}

	// And a password that cannot be established fails the stage before the
	// chart lands, rather than installing a Velero that will default its own.
	failing := &fakeRunner{
		Respond: func(cmd string) (sshx.Result, error) {
			return sshx.Result{ExitCode: 1, Stderr: "Error from server (NotFound)"}, nil
		},
		RespondInput: func(cmd string, stdin []byte) (sshx.Result, error) {
			return sshx.Result{ExitCode: 1, Stderr: "Error from server (AlreadyExists)"}, nil
		},
	}
	if err = Install(ctx, failing, testManifest(), nil); err == nil {
		t.Error("Install must fail when the repository password cannot be established")
	}
	for _, c := range failing.Commands() {
		if strings.Contains(c, k3s.ManifestDir) {
			t.Errorf("the Velero manifest must not be written when the repository password could not be established: %s", c)
		}
	}
}
