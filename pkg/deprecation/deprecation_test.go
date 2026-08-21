package deprecation_test

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/deprecation"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

func bundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`
bundle: "1.1"
core:
  k3s: v1.36.3+k3s1
limits:
  timeouts: { component-ready: 10m }
upgrade:
  deprecation-scanner:
    tool: pluto
    version: v5.24.3
    dataset: v5.24.3
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// CAPTURED FROM THE PINNED BINARY, not transcribed from documentation.
// pluto v5.24.3 run against a real cluster and real manifests on 2026-08-21;
// the field names are nested under `api` and hyphenated, which an earlier
// invented fixture got wrong in every particular. A fixture for an adopted
// tool is only worth what its provenance is worth.
const plutoFindings = `{"items":[
 {"name":"api-gateway","filePath":"/tmp/x.yaml","namespace":"payments","api":{"version":"networking.k8s.io/v1beta1","kind":"Ingress","deprecated-in":"v1.19.0","removed-in":"v1.22.0","replacement-api":"networking.k8s.io/v1","replacement-available-in":"v1.19.0","component":"k8s"},"deprecated":true,"removed":true,"replacementAvailable":true},
 {"name":"worker","filePath":"/tmp/x.yaml","namespace":"payments","api":{"version":"autoscaling/v2beta2","kind":"HorizontalPodAutoscaler","deprecated-in":"v1.23.0","removed-in":"v1.26.0","replacement-api":"autoscaling/v2","replacement-available-in":"v1.23.0","component":"k8s"},"deprecated":true,"removed":true,"replacementAvailable":true},
 {"name":"nightly-report","filePath":"/tmp/x.yaml","namespace":"internal","api":{"version":"batch/v1beta1","kind":"CronJob","deprecated-in":"v1.21.0","removed-in":"v1.25.0","replacement-api":"batch/v1","replacement-available-in":"v1.21.0","component":"k8s"},"deprecated":true,"removed":false,"replacementAvailable":true}
],"target-versions":{"cert-manager":"v1.5.3","istio":"v1.11.0","k8s":"v1.36.3"}}`

// What pluto ACTUALLY emits for a cluster with nothing to report: no items
// key at all. Treating that as an unparseable shape — as this package first
// did — refuses every healthy cluster, which is the opposite of the failure
// the fail-closed rule exists to prevent.
const plutoClean = `{"target-versions":{"cert-manager":"v1.5.3","istio":"v1.11.0","k8s":"v1.36.3"}}`

func runner(t *testing.T, scanOutput string, overrides map[string]sshx.Result) *componenttest.FakeRunner {
	t.Helper()
	return &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		for match, res := range overrides {
			if strings.Contains(cmd, match) {
				return res, nil
			}
		}
		switch {
		case strings.Contains(cmd, "test -x"):
			return sshx.Result{Stdout: "present\n"}, nil
		case strings.Contains(cmd, "detect-all-in-cluster"):
			return sshx.Result{Stdout: scanOutput}, nil
		case strings.Contains(cmd, "uname -m"):
			return sshx.Result{Stdout: "x86_64\n"}, nil
		}
		return sshx.Result{}, nil
	}}
}

// THE gate: a workload using a removed API blocks the upgrade, and the
// refusal names the resource, its namespace, the API it uses and what it
// should become. An upgrade that cleanly upgrades the cluster and takes the
// customer's product down has actively harmed them.
func TestRemovedAPIsBlockAndNameTheResource(t *testing.T) {
	report, err := deprecation.Scan(context.Background(), runner(t, plutoFindings, nil), bundle(t), nil, "v1.36.3+k3s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Blocking) != 2 {
		t.Fatalf("blocking on %d findings, want 2: %+v", len(report.Blocking), report.Blocking)
	}
	if len(report.Warnings) != 1 {
		t.Errorf("deprecated-but-not-removed must warn, not block: %+v", report.Warnings)
	}

	err = report.Err()
	if err == nil {
		t.Fatal("blocking findings must produce a refusal")
	}
	for _, want := range []string{
		"BLOCKED", "payments", "Ingress", "api-gateway",
		"networking.k8s.io/v1beta1", "networking.k8s.io/v1",
		"HorizontalPodAutoscaler", "autoscaling/v2",
		"Nothing has been changed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q:\n%s", want, err)
		}
	}
}

// A clean cluster passes.
func TestNoFindingsPasses(t *testing.T) {
	report, err := deprecation.Scan(context.Background(), runner(t, plutoClean, nil), bundle(t), nil, "v1.36.3+k3s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := report.Err(); err != nil {
		t.Errorf("a clean cluster must pass: %v", err)
	}
}

// THE failure mode the pinning exists to prevent: a scan that cannot run must
// FAIL, never quietly report a clean cluster. These are the ways it can fail.
func TestTheScanFailsClosed(t *testing.T) {
	cases := map[string]map[string]sshx.Result{
		"the scanner will not run": {
			"detect-all-in-cluster": {ExitCode: 1, Stderr: "error: unable to reach the API server"},
		},
		"the scanner emits nothing": {
			"detect-all-in-cluster": {Stdout: ""},
		},
		"the scanner emits something else": {
			"detect-all-in-cluster": {Stdout: `{"unexpected":"shape"}`},
		},
		"the scan targeted a different Kubernetes version": {
			"detect-all-in-cluster": {Stdout: `{"target-versions":{"k8s":"v1.29.0"}}`},
		},
		"the scanner emits unparsable output": {
			"detect-all-in-cluster": {Stdout: "panic: runtime error"},
		},
		"the scanner cannot be installed": {
			"test -x":   {Stdout: "absent\n"},
			"mktemp -d": {ExitCode: 1, Stderr: "curl: (6) Could not resolve host"},
		},
	}
	for name, overrides := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := deprecation.Scan(context.Background(), runner(t, plutoFindings, overrides), bundle(t), nil, "v1.36.3+k3s1")
			if err == nil {
				t.Fatal("a scan that could not run must fail the gate, not report a clean cluster")
			}
			if !strings.Contains(err.Error(), "refused") {
				t.Errorf("the failure must say the upgrade is refused:\n%v", err)
			}
		})
	}
}

// A binary that is not the pinned version is refused rather than trusted: the
// dataset behind the scan is the pin, and a mismatch means the confidence is
// not the confidence the bundle promised.
func TestAScannerThatIsNotThePinnedVersionIsRefused(t *testing.T) {
	overrides := map[string]sshx.Result{
		"test -x":          {Stdout: "absent\n"},
		"pluto-v5.24.3 ve": {Stdout: "Version:5.19.0\n"},
	}
	_, err := deprecation.Scan(context.Background(), runner(t, plutoFindings, overrides), bundle(t), nil, "v1.36.3+k3s1")
	if err == nil || !strings.Contains(err.Error(), "pins") {
		t.Fatalf("want a refusal naming the pin mismatch, got %v", err)
	}
}

// Override is per-resource and only per-resource. A blanket --force is how a
// customer takes their own product down and then calls us.
func TestAcknowledgementIsPerResource(t *testing.T) {
	report, err := deprecation.Scan(context.Background(), runner(t, plutoFindings, nil), bundle(t),
		[]string{"payments/Ingress/api-gateway"}, "v1.36.3+k3s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Acknowledged) != 1 {
		t.Errorf("acknowledged %d, want 1", len(report.Acknowledged))
	}
	if len(report.Blocking) != 1 {
		t.Fatalf("still blocking on %d, want the one that was NOT acknowledged", len(report.Blocking))
	}
	if report.Blocking[0].Name != "worker" {
		t.Errorf("still blocking on %q, want worker", report.Blocking[0].Name)
	}
	if err := report.Err(); err == nil {
		t.Error("an unacknowledged finding must still block")
	}

	// And the refusal tells the operator exactly what to type for each one,
	// which is what makes per-resource acknowledgement usable and auditable.
	if !strings.Contains(report.Err().Error(), "--acknowledge payments/HorizontalPodAutoscaler/worker") {
		t.Errorf("the refusal must name the acknowledgement for each finding:\n%v", report.Err())
	}
}

// The scan targets the Kubernetes version the BUNDLE moves to, not the one
// the cluster is on.
func TestTheScanTargetsTheKubernetesVersionBeingMovedTo(t *testing.T) {
	fake := runner(t, plutoClean, nil)
	if _, err := deprecation.Scan(context.Background(), fake, bundle(t), nil, "v1.36.3+k3s1"); err != nil {
		t.Fatal(err)
	}
	var scan string
	for _, c := range fake.Commands() {
		if strings.Contains(c, "detect-all-in-cluster") {
			scan = c
		}
	}
	if !strings.Contains(scan, "--target-versions k8s='v1.36.3'") {
		t.Errorf("the scan must target the bundle's k3s pin without its suffix:\n  %s", scan)
	}
}

func TestKubernetesVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v1.35.7+k3s1": "v1.35.7",
		"1.36.3+k3s2":  "v1.36.3",
		"v1.36.3":      "v1.36.3",
		"":             "",
	} {
		if got := deprecation.KubernetesVersion(in); got != want {
			t.Errorf("KubernetesVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
