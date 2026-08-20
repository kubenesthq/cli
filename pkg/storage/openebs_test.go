package storage

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// prefixRunner scripts replies by command prefix and records writes to the
// k3s auto-deploy dir, decoding their payloads.
type prefixRunner struct {
	t       *testing.T
	replies []prefixReply
	writes  map[string]string // manifest file name -> decoded content
}

type prefixReply struct {
	prefix string
	res    sshx.Result
}

var writeRe = regexp.MustCompile(`printf '%s' ([A-Za-z0-9+/=]+) \| base64 -d \| sudo -n tee ` + k3s.ManifestDir + `/([a-z0-9-]+)\.yaml`)

func (p *prefixRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	if m := writeRe.FindStringSubmatch(command); m != nil {
		decoded, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			p.t.Fatalf("bad payload in %q: %v", command, err)
		}
		if p.writes == nil {
			p.writes = map[string]string{}
		}
		p.writes[m[2]] = string(decoded)
		return sshx.Result{}, nil
	}
	for _, r := range p.replies {
		if strings.HasPrefix(command, r.prefix) {
			return r.res, nil
		}
	}
	p.t.Fatalf("unscripted command: %q", command)
	return sshx.Result{}, nil
}

func testManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(
		"bundle: \"1.0\"\ncore:\n  openebs-lvm-localpv: 1.10.0\nlimits:\n  timeouts:\n    component-ready: 2s\n"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The cluster-side JSON a healthy install settles into.
var healthy = []prefixReply{
	{"sudo -n k3s kubectl get pods -n openebs", sshx.Result{Stdout: `{"items":[
		{"metadata":{"name":"lvm-controller-0"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}]}`}},
	{"sudo -n k3s kubectl get csidriver", sshx.Result{Stdout: "csidriver.storage.k8s.io/local.csi.openebs.io\n"}},
	{"sudo -n k3s kubectl get csinodes", sshx.Result{Stdout: `{"items":[
		{"metadata":{"name":"node-1"},"spec":{"drivers":[{"name":"local.csi.openebs.io"}]}}]}`}},
}

func TestInstallRendersThePinnedChartAndTheDefaultStorageClass(t *testing.T) {
	r := &prefixRunner{t: t, replies: healthy}
	if err := Install(context.Background(), r, testManifest(t), nil); err != nil {
		t.Fatal(err)
	}

	chart, ok := r.writes["openebs-lvm-localpv"]
	if !ok {
		t.Fatalf("no HelmChart CR written; writes: %v", keys(r.writes))
	}
	var cr struct {
		Spec struct {
			Repo          string `yaml:"repo"`
			Chart         string `yaml:"chart"`
			Version       string `yaml:"version"`
			ValuesContent string `yaml:"valuesContent"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(chart), &cr); err != nil {
		t.Fatal(err)
	}
	if cr.Spec.Version != "1.10.0" {
		t.Errorf("chart version %q, want the manifest's 1.10.0 — pins come from the bundle", cr.Spec.Version)
	}
	if cr.Spec.Repo != ChartRepo || cr.Spec.Chart != ChartName {
		t.Errorf("chart source %s/%s, want %s/%s", cr.Spec.Repo, cr.Spec.Chart, ChartRepo, ChartName)
	}
	if !strings.Contains(cr.Spec.ValuesContent, "enabled: false") {
		t.Errorf("analytics not disabled in values: %q", cr.Spec.ValuesContent)
	}

	sc, ok := r.writes["kubenest-storageclass"]
	if !ok {
		t.Fatalf("no StorageClass written; writes: %v", keys(r.writes))
	}
	for _, want := range []string{
		"name: " + StorageClassName,
		"storageclass.kubernetes.io/is-default-class: \"true\"",
		"provisioner: " + CSIDriverName,
		"volgroup: " + VolumeGroup,
		"volumeBindingMode: WaitForFirstConsumer",
	} {
		if !strings.Contains(sc, want) {
			t.Errorf("StorageClass manifest lacks %q:\n%s", want, sc)
		}
	}
}

// A manifest without the pin refuses to install anything — no version
// defaults in code.
func TestInstallRefusesAManifestWithoutThePin(t *testing.T) {
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\nlimits:\n  timeouts:\n    component-ready: 2s\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := &prefixRunner{t: t}
	err = Install(context.Background(), r, m, nil)
	if err == nil || !strings.Contains(err.Error(), "core.openebs-lvm-localpv") {
		t.Fatalf("err = %v, want the missing-pin error", err)
	}
	if len(r.writes) != 0 {
		t.Errorf("manifests were written without a pin: %v", keys(r.writes))
	}
}

func TestInstallFailureNamesTheStuckObject(t *testing.T) {
	stuck := []prefixReply{
		{"sudo -n k3s kubectl get pods -n openebs", sshx.Result{Stdout: `{"items":[
			{"metadata":{"name":"lvm-node-xyz"},"status":{"phase":"Pending",
			 "conditions":[{"type":"PodScheduled","status":"False","message":"0/1 nodes are available: 1 Insufficient memory."}]}}]}`}},
	}
	r := &prefixRunner{t: t, replies: stuck}
	err := Install(context.Background(), r, testManifest(t), nil)
	if err == nil {
		t.Fatal("a never-ready component passed install")
	}
	for _, want := range []string{"lvm-node-xyz", "Insufficient memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure %q does not carry %q — failures must name the stuck object and the fix", err, want)
		}
	}
}

const scCmd = "sudo -n k3s kubectl get storageclass " + StorageClassName + " -o json"

func TestVerifyPassesOnTheCorrectDefaultStorageClass(t *testing.T) {
	good := `{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}},
		"provisioner":"local.csi.openebs.io","volumeBindingMode":"WaitForFirstConsumer",
		"parameters":{"volgroup":"kubenest-vg"}}`
	r := &prefixRunner{t: t, replies: []prefixReply{{scCmd, sshx.Result{Stdout: good}}}}
	if err := Verify(context.Background(), r, testManifest(t), nil); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyFailureSaysWhatIsMisconfigured(t *testing.T) {
	wrong := `{"metadata":{"annotations":{}},
		"provisioner":"kubernetes.io/no-provisioner","volumeBindingMode":"Immediate",
		"parameters":{"volgroup":"other-vg"}}`
	r := &prefixRunner{t: t, replies: []prefixReply{{scCmd, sshx.Result{Stdout: wrong}}}}
	err := Verify(context.Background(), r, testManifest(t), nil)
	if err == nil {
		t.Fatal("a misconfigured StorageClass verified")
	}
	for _, want := range []string{"provisioner", "volgroup", "WaitForFirstConsumer", "default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure %q does not name the %s problem", err, want)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
