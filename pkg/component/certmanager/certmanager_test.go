package certmanager

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/manifest"
)

func TestChartThreadsTheManifestPin(t *testing.T) {
	m, err := manifest.Parse([]byte(`
bundle: "1.0"
core:
  cert-manager: v1.21.1
limits:
  timeouts:
    component-ready: 10m
`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Chart(m)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "v1.21.1" {
		t.Errorf("version = %q, want the manifest pin v1.21.1", c.Version)
	}
	if c.Repo != "https://charts.jetstack.io" || c.Chart != "cert-manager" {
		t.Errorf("chart source = %s/%s", c.Repo, c.Chart)
	}
}

func TestChartWithoutPinIsAnError(t *testing.T) {
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\ncore:\n  k3s: v1\nlimits:\n  timeouts:\n    component-ready: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Chart(m)
	if err == nil || !strings.Contains(err.Error(), "core.cert-manager") {
		t.Errorf("missing pin must error naming core.cert-manager, got %v", err)
	}
}

// CRDs must ship with the chart and the Gateway API integration must be on —
// the platform Gateway's TLS depends on both.
func TestValuesEnableCRDsAndGatewayAPI(t *testing.T) {
	var v struct {
		CRDs struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"crds"`
		Config struct {
			GatewayAPI struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"gatewayAPI"`
		} `yaml:"config"`
	}
	if err := yaml.Unmarshal([]byte(Values), &v); err != nil {
		t.Fatalf("Values is not valid YAML: %v", err)
	}
	if !v.CRDs.Enabled {
		t.Error("crds.enabled must be true")
	}
	if !v.Config.GatewayAPI.Enabled {
		t.Error("config.gatewayAPI.enabled must be true — the Gateway's cert-manager annotation depends on it")
	}
}
