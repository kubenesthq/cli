package agent_test

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

const agentJWT = "eyJhbGciOiJIUzI1NiJ9.AGENT_TOKEN_VALUE.signature"

func bundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\ncore:\n  kubenest-agent: 2.2.0\nlimits:\n  timeouts:\n    component-ready: 2s\n"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func creds(withRepo bool) *api.AgentCredentials {
	c := &api.AgentCredentials{
		ClusterID: "019d52e1-ba17-7e70-94a0-8a33a48b7fcb",
		AgentJWT: api.AgentJWT{
			Token:        api.NewSecret(agentJWT),
			HubURL:       "wss://hub.example.test/ws/operator",
			TokenVersion: 2,
		},
		Operator: api.OperatorInstallInfo{
			Namespace: "kubenest-system",
			ChartRef:  "oci://ghcr.io/kubenesthq/charts/kubenest-operator-2:2.2.0",
		},
	}
	if withRepo {
		c.RepoCredential = &api.RepoCredential{
			PrivateKey: api.NewSecret("-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----"),
			RepoURL:    "git@gitea.example.test:kubenest/prod-1.git",
			Branch:     "main",
		}
	}
	return c
}

// The chart reference comes from the mint and the VERSION from the bundle —
// one pin, one place. A hardcoded registry is how kn-z6e4 shipped a chart_ref
// that did not exist.
func TestChartUsesTheMintedRefAndTheBundlePin(t *testing.T) {
	chart, err := agent.Chart(bundle(t), creds(false))
	if err != nil {
		t.Fatal(err)
	}
	if chart.Chart != "oci://ghcr.io/kubenesthq/charts/kubenest-operator-2" {
		t.Errorf("chart ref is %q — the version must not be carried twice", chart.Chart)
	}
	if chart.Version != "2.2.0" {
		t.Errorf("version is %q, want the bundle pin", chart.Version)
	}
	if chart.Repo != "" {
		t.Errorf("an oci:// chart carries its own registry, got repo %q", chart.Repo)
	}
	if chart.Name != "operator" {
		t.Errorf("release name is %q — the chart's metrics service is <release>-kubenest-operator-2-controller-manager-metrics-service, and Kubernetes refuses names over 63 characters", chart.Name)
	}
	if len(chart.Name+"-kubenest-operator-2-controller-manager-metrics-service") > 63 {
		t.Errorf("the metrics service name would be %d characters, over Kubernetes' 63-character limit",
			len(chart.Name+"-kubenest-operator-2-controller-manager-metrics-service"))
	}
	if chart.TargetNamespace != "kubenest-system" {
		t.Errorf("namespace is %q, want the minted one", chart.TargetNamespace)
	}
	doc, err := chart.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "repo:") {
		t.Errorf("helm-controller rejects a HelmChart that sets both repo and an oci chart:\n%s", doc)
	}
}

// Two of the same thing is the hazard the platform exists to remove: the
// chart's bootstrap cert-manager is a different version from the one stage 6
// installed, and both claim the same CRDs.
func TestBootstrapCertManagerIsDisabled(t *testing.T) {
	values, err := agent.Values(creds(false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(values, "certManager") || !strings.Contains(values, "enabled: false") {
		t.Errorf("the chart's bootstrap cert-manager must be disabled — stage 6 already installed the platform's:\n%s", values)
	}
}

// The credential API calls this field hub_url because it points at the hub.
// The established chart key is kubenest.backendURL, which becomes the
// KUBENEST_BACKEND_URL environment variable. The removed backend-generated
// Helm command once copied the API name into a nonexistent kubenest.hubURL
// key; Helm ignored it and the operator silently dialled the chart default.
func TestHubURLMapsToTheChartBackendURLKey(t *testing.T) {
	values, err := agent.Values(creds(false))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Kubenest map[string]any `yaml:"kubenest"`
	}
	if err := yaml.Unmarshal([]byte(values), &document); err != nil {
		t.Fatal(err)
	}
	if got := document.Kubenest["backendURL"]; got != "wss://hub.example.test/ws/operator" {
		t.Errorf("kubenest.backendURL = %v, want the minted agent_jwt.hub_url", got)
	}
	if _, exists := document.Kubenest["hubURL"]; exists {
		t.Error("kubenest.hubURL does not exist in the operator chart; use backendURL")
	}
}

// When the control plane issued a per-cluster repository credential, the
// GitOps repo is that one; an in-cluster Gitea would be a second source of
// truth. With no repo credential the chart's own fallback is left alone.
func TestGiteaFollowsTheMintedRepoCredential(t *testing.T) {
	withRepo, err := agent.Values(creds(true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withRepo, "gitea") {
		t.Errorf("a minted repo credential must disable the in-cluster Gitea:\n%s", withRepo)
	}
	withoutRepo, err := agent.Values(creds(false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(withoutRepo, "gitea") {
		t.Errorf("with no repo credential the chart's own fallback is left at its default:\n%s", withoutRepo)
	}
}

// THE rule for this package: the JWT reaches the cluster as chart values and
// by no other route. Never a command argument — command lines are visible in
// the target host's process list — and the file it lands in is 0600, because
// the k3s auto-deploy directory is world-readable by default.
func TestTheAgentJWTNeverReachesACommandLineAndItsFileIsPrivate(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "get deployment") || strings.Contains(cmd, "kubectl get") {
			return sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Available","status":"True"}]}}`}, nil
		}
		return sshx.Result{}, nil
	}}
	if err := agent.Install(context.Background(), fake, bundle(t), creds(true), nil); err != nil {
		t.Fatal(err)
	}

	var chmodded bool
	for _, cmd := range fake.Commands() {
		if strings.Contains(cmd, agentJWT) {
			t.Fatalf("the agent JWT appeared verbatim in a command:\n%s", cmd)
		}
		if strings.Contains(cmd, "AGENT_TOKEN_VALUE") {
			t.Fatalf("the agent JWT leaked into a command:\n%s", cmd)
		}
		if strings.Contains(cmd, "chmod 600") && strings.Contains(cmd, "kubenest-agent.yaml") {
			chmodded = true
		}
	}
	if !chmodded {
		t.Error("the values file carries the agent JWT and must be chmod 600 on the server node")
	}
}

// An empty cluster id is the kn-z6e4 defect: the pod starts, authenticates,
// and has every heartbeat rejected. Refuse it at render time.
func TestMissingIdentityIsRefused(t *testing.T) {
	cases := map[string]func(*api.AgentCredentials){
		"no cluster id": func(c *api.AgentCredentials) { c.ClusterID = "" },
		"no jwt":        func(c *api.AgentCredentials) { c.AgentJWT.Token = api.Secret{} },
		"no hub url":    func(c *api.AgentCredentials) { c.AgentJWT.HubURL = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := creds(false)
			mutate(c)
			if _, err := agent.Values(c); err == nil {
				t.Fatal("want a refusal")
			}
		})
	}
	if _, err := agent.Values(nil); err == nil {
		t.Fatal("want a refusal with no credentials at all")
	}
}

// The bundle pin decides the version; the mint's ref decides only where the
// chart lives. A control plane that tags the ref with a different bundle's
// version must not be able to break an install — it did, with
// "chart reference and version mismatch: 2.2.0 is not 2.3.4", on a real
// cluster installing an older bundle.
func TestTheChartRefNeverCarriesTheVersion(t *testing.T) {
	cases := map[string]string{
		"oci://ghcr.io/kubenesthq/charts/kubenest-operator-2:2.2.0": "oci://ghcr.io/kubenesthq/charts/kubenest-operator-2",
		"oci://ghcr.io/kubenesthq/charts/kubenest-operator-2:2.3.4": "oci://ghcr.io/kubenesthq/charts/kubenest-operator-2",
		"oci://ghcr.io/kubenesthq/charts/kubenest-operator-2":       "oci://ghcr.io/kubenesthq/charts/kubenest-operator-2",
		// A registry port is not a tag.
		"oci://registry.internal:5000/kubenest/operator":       "oci://registry.internal:5000/kubenest/operator",
		"oci://registry.internal:5000/kubenest/operator:9.9.9": "oci://registry.internal:5000/kubenest/operator",
	}
	for ref, want := range cases {
		c := creds(false)
		c.Operator.ChartRef = ref
		chart, err := agent.Chart(bundle(t), c)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if chart.Chart != want {
			t.Errorf("Chart ref %q became %q, want %q", ref, chart.Chart, want)
		}
		if chart.Version != "2.2.0" {
			t.Errorf("%s: version is %q, want the bundle pin", ref, chart.Version)
		}
	}
}
