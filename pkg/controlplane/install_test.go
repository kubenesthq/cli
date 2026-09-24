package controlplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// installBundle is the manifest the install path reads its deadline from: a
// missing timeout is an error, never a default.
func installBundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`
bundle: "1.0"
limits:
  timeouts:
    component-ready: 1m
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// deploymentProbeCmd is the command component.CheckCondition runs for a
// Deployment in this namespace.
func deploymentProbeCmd(name string) string {
	return "sudo -n k3s kubectl get deployment/" + name + " -o json -n " + Namespace
}

// The chart reaches the cluster inside the HelmChart resource itself
// (spec.chartContent), which is the whole reason this package exists: no
// registry, no chart repository, no network. It also has to carry the values
// document as written, and to wait for all four workloads rather than the
// first one that answers.
func TestInstallAppliesTheEmbeddedChartAndWaitsForTheControlPlane(t *testing.T) {
	ctx := context.Background()
	settings := Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"}
	sec := Secrets{JWTSecret: "jwt", EncryptionKey: "enc", PostgresPassword: "pg", AdminPassword: "adm"}
	values, err := Values(settings, sec)
	if err != nil {
		t.Fatal(err)
	}

	available := sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Available","status":"True"}]}}`}
	answers := map[string]sshx.Result{
		deploymentProbeCmd(ReleaseName + "-backend"): available,
		deploymentProbeCmd(ReleaseName + "-hub"):     available,
		deploymentProbeCmd(ReleaseName + "-ui"):      available,
		"sudo -n k3s kubectl get statefulset " + postgresStatefulSet + " -n " + Namespace + " -o json": {
			Stdout: `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`,
		},
	}
	r := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if strings.HasPrefix(command, "sudo -n install -m 0600 ") {
			return sshx.Result{}, nil
		}
		res, ok := answers[command]
		if !ok {
			t.Fatalf("unscripted command: %q", command)
		}
		return res, nil
	}}

	var events []converge.Event
	rep := converge.ReporterFunc(func(e converge.Event) { events = append(events, e) })

	if err := Install(ctx, r, values, installBundle(t), rep); err != nil {
		t.Fatal(err)
	}

	inputs := r.Inputs()
	if len(inputs) != 1 {
		t.Fatalf("streamed %d documents to the host, want the one HelmChart", len(inputs))
	}
	var doc map[string]any
	if err := yaml.Unmarshal(inputs[0], &doc); err != nil {
		t.Fatalf("the applied manifest is not valid YAML: %v", err)
	}
	if doc["apiVersion"] != "helm.cattle.io/v1" || doc["kind"] != "HelmChart" {
		t.Errorf("applied %v/%v, want helm.cattle.io/v1 HelmChart", doc["apiVersion"], doc["kind"])
	}
	metadata := section(t, doc, "metadata")
	if metadata["name"] != ReleaseName || metadata["namespace"] != "kube-system" {
		t.Errorf("applied to %v/%v, want kube-system/%s", metadata["namespace"], metadata["name"], ReleaseName)
	}
	spec := section(t, doc, "spec")
	want := []string{"chartContent", "createNamespace", "targetNamespace", "valuesContent"}
	if got := sortedKeys(spec); !equalStrings(got, want) {
		t.Errorf("spec keys = %v, want exactly %v: chartContent excludes repo, chart and version", got, want)
	}
	if spec["targetNamespace"] != Namespace {
		t.Errorf("targetNamespace = %v, want %s", spec["targetNamespace"], Namespace)
	}
	if spec["chartContent"] != base64.StdEncoding.EncodeToString(ChartArchive()) {
		t.Error("chartContent is not the embedded chart archive: the install would fetch a chart from somewhere instead")
	}
	if spec["valuesContent"] != values {
		t.Errorf("valuesContent = %v, want the values document verbatim", spec["valuesContent"])
	}

	// The chart has to be READY, not merely applied.
	if len(events) == 0 {
		t.Fatal("no progress was reported")
	}
	last := events[len(events)-1]
	if last.Outcome != converge.Pass || last.Check != "kubenest-control-plane-ready" {
		t.Errorf("last event = %s %s, want a pass of kubenest-control-plane-ready", last.Check, last.Outcome)
	}
}

// The CLI reaches the backend through a tunnel it opens from the node, so
// what it needs is the Service's ClusterIP and its port, and nothing less
// specific than that will do.
func TestBackendAddrIsTheServiceClusterIPAndPort(t *testing.T) {
	ctx := context.Background()
	cmd := "sudo -n k3s kubectl get service " + backendService + " -n " + Namespace + " -o jsonpath='{.spec.clusterIP}'"

	addr, err := BackendAddr(ctx, fakeRunner(t, map[string]sshx.Result{cmd: {Stdout: "10.43.0.10\n"}}))
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.43.0.10:8000" {
		t.Errorf("BackendAddr = %q, want 10.43.0.10:8000", addr)
	}

	if _, err := BackendAddr(ctx, fakeRunner(t, map[string]sshx.Result{cmd: {Stdout: "\n"}})); err == nil {
		t.Error("a Service with no ClusterIP must be an error, not an empty address")
	}
}

// The platform CA is what lets every later CLI command verify a control plane
// whose certificate no public trust anchor signed.
func TestPlatformCAReadsTheCAAndFallsBackToTheTLSKey(t *testing.T) {
	ctx := context.Background()
	ca := testCA(t)
	cmd := "sudo -n k3s kubectl get secret " + caSecretName + " -n " + caNamespace + " -o json"
	stored := func(keys map[string]string) sshx.Result {
		encoded := map[string]string{}
		for key, value := range keys {
			encoded[key] = base64.StdEncoding.EncodeToString([]byte(value))
		}
		body, err := json.Marshal(map[string]any{"data": encoded})
		if err != nil {
			t.Fatal(err)
		}
		return sshx.Result{Stdout: string(body)}
	}

	for _, tc := range []struct {
		name string
		keys map[string]string
		want string
	}{
		{"ca.crt", map[string]string{caCertKey: string(ca)}, string(ca)},
		{"ca.crt empty, tls.crt holds the CA", map[string]string{caCertKey: "", caTLSKey: string(ca)}, string(ca)},
		{"ca.crt absent, tls.crt holds the CA", map[string]string{caTLSKey: string(ca)}, string(ca)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlatformCA(ctx, fakeRunner(t, map[string]sshx.Result{cmd: stored(tc.keys)}))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("PlatformCA = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("not a certificate", func(t *testing.T) {
		_, err := PlatformCA(ctx, fakeRunner(t, map[string]sshx.Result{
			cmd: stored(map[string]string{caCertKey: "ssh-rsa AAAA not a certificate"}),
		}))
		if err == nil || !strings.Contains(err.Error(), "x509") {
			t.Fatalf("error = %v, want one saying the value is not an x509 certificate", err)
		}
	})
}

// testCA is a self-signed CA certificate in PEM, which is what cert-manager
// stores in the platform CA Secret.
func testCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kubenest-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
