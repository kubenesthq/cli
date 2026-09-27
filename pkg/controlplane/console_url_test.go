package controlplane

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The backend hands `kubenest login` the console URL to approve a device code
// at, and it takes that URL from the first of its CORS origins. On hardware
// (2026-09-27, lab cp) the chart set no origins, so the backend fell back to
// its development default and every login was sent to http://localhost:3000.
// The first origin must be the host the Gateway actually publishes the console
// at, which is also the origin the console's browser calls come from.
func TestTheBackendsConsoleURLIsWhereTheGatewayServesTheConsole(t *testing.T) {
	chartRoot := siblingChartRoot(t)
	values := fenceTestValues + "backend:\n  admin:\n    password: p\ngatewayCA:\n  certificate: c\n  privateKey: k\ncheckpoint:\n  enabled: false\n"
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("helm", "template", ReleaseName, chartRoot, "-f", file).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	var origins, consoleHost string
	decoder := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name   string            `yaml:"name"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				Hostnames []string `yaml:"hostnames"`
				Template  struct {
					Spec struct {
						Containers []struct {
							Env []struct {
								Name  string `yaml:"name"`
								Value string `yaml:"value"`
							} `yaml:"env"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := decoder.Decode(&doc); err != nil {
			break
		}
		switch {
		case doc.Kind == "Deployment" && doc.Metadata.Name == backendService:
			for _, c := range doc.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					if e.Name == "CORS_ORIGINS" {
						origins = e.Value
					}
				}
			}
		case doc.Kind == "HTTPRoute" && doc.Metadata.Labels["app.kubernetes.io/component"] == "ui":
			if len(doc.Spec.Hostnames) > 0 {
				consoleHost = doc.Spec.Hostnames[0]
			}
		}
	}
	if consoleHost == "" {
		t.Fatal("the rendered chart publishes no console route, so there is nothing to compare with")
	}
	if origins == "" {
		t.Fatalf("the chart sets no CORS_ORIGINS, so the backend falls back to its development default and `kubenest login` sends the operator to http://localhost:3000/cli-authorize instead of https://%s", consoleHost)
	}
	first, _, _ := strings.Cut(origins, ",")
	if want := "https://" + consoleHost; strings.TrimSpace(first) != want {
		t.Errorf("the backend's first CORS origin is %q, want %q: `kubenest login` would send the operator to %s/cli-authorize instead of the console",
			first, want, first)
	}
}
