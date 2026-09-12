package agent

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A manifest shaped like the one an install writes: the agent JWT sits beside a
// GitOps deploy key, a workload-Applications declaration and an argo-cd block,
// which is exactly what a re-render would lose.
const installedManifest = `apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: operator
  namespace: kube-system
spec:
  chart: oci://ghcr.io/kubenesthq/charts/kubenest-operator-2
  createNamespace: true
  targetNamespace: kubenest-system
  version: 2.6.16
  valuesContent: |
    argo-cd:
      configs:
        ssh:
          extraHosts: |
            gitea.example ssh-ed25519 AAAA-public-host-key
    bootstrap:
      certManager:
        enabled: false
      gitea:
        enabled: false
    kubenest:
      backendURL: ws://hub.example:8001/ws/operator
      bootstrapController:
        gitRepoBranch: main
        gitRepoURL: ssh://git@gitea.example/kubenest/cluster.git
        gitSSHPrivateKey: |
          -----BEGIN OPENSSH PRIVATE KEY-----
          not-a-real-key
          -----END OPENSSH PRIVATE KEY-----
      clusterID: 019d52e1-ba17-7e70-94a0-8a33a48b7fcb
      jwtSecret: the-old-token
      workloadApplications:
        enabled: false
`

func valuesOf(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := yaml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("parsing patched manifest: %v", err)
	}
	spec, ok := parsed["spec"].(map[string]any)
	if !ok {
		t.Fatal("patched manifest has no spec")
	}
	raw, ok := spec["valuesContent"].(string)
	if !ok {
		t.Fatal("patched manifest has no spec.valuesContent")
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatalf("parsing patched valuesContent: %v", err)
	}
	return values
}

func TestReplaceJWTSecretReplacesOnlyTheToken(t *testing.T) {
	patched, namespace, err := ReplaceJWTSecret([]byte(installedManifest), "the-new-token")
	if err != nil {
		t.Fatalf("ReplaceJWTSecret: %v", err)
	}
	if namespace != "kubenest-system" {
		t.Errorf("namespace = %q, want kubenest-system (read from the manifest, not assumed)", namespace)
	}

	before := valuesOf(t, []byte(installedManifest))
	after := valuesOf(t, patched)

	kubenest, _ := after["kubenest"].(map[string]any)
	if kubenest["jwtSecret"] != "the-new-token" {
		t.Errorf("jwtSecret = %v, want the-new-token", kubenest["jwtSecret"])
	}

	// THE POINT OF THE WHOLE FUNCTION. Everything a re-render would have to
	// reconstruct from a response that does not carry it must survive verbatim.
	beforeKubenest, _ := before["kubenest"].(map[string]any)
	beforeKubenest["jwtSecret"] = "the-new-token"
	if diff := compare(before, after); diff != "" {
		t.Errorf("patch changed something other than the token: %s", diff)
	}
}

func TestReplaceJWTSecretDoesNotLeakTheOldToken(t *testing.T) {
	patched, _, err := ReplaceJWTSecret([]byte(installedManifest), "the-new-token")
	if err != nil {
		t.Fatalf("ReplaceJWTSecret: %v", err)
	}
	if strings.Contains(string(patched), "the-old-token") {
		t.Error("the replaced token is still present in the written manifest")
	}
}

func TestReplaceJWTSecretRefuses(t *testing.T) {
	unmanaged := strings.Replace(installedManifest,
		"      jwtSecret: the-old-token\n", "", 1)
	noValues := `apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: operator
spec:
  chart: oci://example/chart
  targetNamespace: kubenest-system
  version: 2.6.16
`
	noNamespace := strings.Replace(installedManifest, "  targetNamespace: kubenest-system\n", "", 1)

	cases := []struct {
		name, manifest, token, want string
	}{
		{"an empty token", installedManifest, "", "empty agent JWT"},
		{"a different kind", strings.Replace(installedManifest, "kind: HelmChart", "kind: ConfigMap", 1), "t", "not HelmChart"},
		{"no valuesContent", noValues, "t", "no spec.valuesContent"},
		{"no jwtSecret to replace", unmanaged, "t", "sets no kubenest.jwtSecret"},
		{"no targetNamespace", noNamespace, "t", "no spec.targetNamespace"},
		{"not YAML at all", "\x00\x01 not yaml: [", "t", "parsing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ReplaceJWTSecret([]byte(tc.manifest), tc.token)
			if err == nil {
				t.Fatalf("accepted %s; a patch that guesses here writes a value nobody reads", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the cause %q", err, tc.want)
			}
		})
	}
}

// compare reports the first structural difference, so a failure names what moved.
func compare(a, b any) string {
	left, _ := yaml.Marshal(a)
	right, _ := yaml.Marshal(b)
	if string(left) == string(right) {
		return ""
	}
	return "\n--- before (with the new token substituted)\n" + string(left) +
		"\n--- after\n" + string(right)
}
