package gatewayapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

func bundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`
bundle: "1.0"
core:
  gateway-api: v1.6.1
limits:
  timeouts:
    component-ready: 10m
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestURLBuiltFromPin(t *testing.T) {
	want := "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml"
	if got := URL("v1.6.1"); got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

// Install end to end against a local release server and a scripted cluster:
// the pinned manifest is fetched, lands verbatim in the k3s auto-deploy dir,
// and Install converges once every standard CRD is Established.
func TestInstallFetchesPinAndConverges(t *testing.T) {
	const crdYAML = "# gateway api v1.6.1 standard channel\nkind: CustomResourceDefinition\n"
	var served string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = r.URL.Path
		w.Write([]byte(crdYAML))
	}))
	defer srv.Close()
	old := ReleaseBaseURL
	ReleaseBaseURL = srv.URL
	defer func() { ReleaseBaseURL = old }()

	r := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "get crd/") {
			return sshx.Result{Stdout: `{"status": {"conditions": [{"type": "Established", "status": "True"}]}}`}, nil
		}
		return sshx.Result{}, nil
	}}

	if err := Install(context.Background(), r, bundle(t), nil); err != nil {
		t.Fatalf("install: %v", err)
	}

	if served != "/v1.6.1/standard-install.yaml" {
		t.Errorf("fetched %q — the pin from the manifest must pick the release", served)
	}

	// The write command carries the manifest base64-encoded into the
	// auto-deploy dir; decode and compare byte-for-byte.
	var wrote bool
	for _, cmd := range r.Commands() {
		if !strings.Contains(cmd, "/var/lib/rancher/k3s/server/manifests/kubenest-gateway-api.yaml") {
			continue
		}
		wrote = true
		fields := strings.Fields(cmd)
		decoded, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil {
			t.Fatalf("write command payload is not base64: %v", err)
		}
		if string(decoded) != crdYAML {
			t.Errorf("auto-deploy content differs from the release manifest")
		}
	}
	if !wrote {
		t.Error("no write to the k3s auto-deploy directory happened")
	}

	// Every standard CRD was checked.
	joined := strings.Join(r.Commands(), "\n")
	for _, crd := range StandardCRDs {
		if !strings.Contains(joined, crd) {
			t.Errorf("CRD %s was never verified", crd)
		}
	}
}

func TestInstallWithoutPinFailsNamingTheKey(t *testing.T) {
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\ncore:\n  k3s: v1\nlimits:\n  timeouts:\n    component-ready: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = Install(context.Background(), &componenttest.FakeRunner{}, m, nil)
	if err == nil || !strings.Contains(err.Error(), "core.gateway-api") {
		t.Errorf("want an error naming core.gateway-api, got %v", err)
	}
}
