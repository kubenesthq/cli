package backup

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// fakeRunner records every command and answers via Respond. pkg/component's
// componenttest fake is not imported on purpose — that package is another
// bead's in-flight work and this one must build without it.
type fakeRunner struct {
	mu       sync.Mutex
	commands []string
	// Respond maps a command to its result; nil means success, empty output.
	Respond func(command string) (sshx.Result, error)
}

func (f *fakeRunner) Run(ctx context.Context, command string) (sshx.Result, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()
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
		}},
		Backup: manifest.Backup{
			ObjectStorePlugin: manifest.ObjectStorePlugin{Provider: "aws", Version: "v1.14.2"},
			Defaults: manifest.BackupDefaults{
				WorkloadBackup: manifest.BackupSchedule{Interval: manifest.Interval(24 * time.Hour), Keep: 14},
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
