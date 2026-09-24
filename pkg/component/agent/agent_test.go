package agent_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/leakscan"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

const agentJWT = "eyJhbGciOiJIUzI1NiJ9.AGENT_TOKEN_VALUE.signature"

// repoKey is the per-cluster write deploy key the mint returns. It is
// multiline on purpose: a PEM reaches the chart values as a block and must
// survive the YAML round trip byte-for-byte, or the operator mounts a broken
// key and every push fails at ssh.
const repoKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----"

// bundle pins kubenest-agent 2.6.16 — the pin platform-1.0 carries. Every
// test here renders against a chart at or above agent.MinChartOwningApplications
// because agent.Chart refuses anything below it: below 2.6.5 the operator does
// not create the cluster's workload Argo CD Applications and nothing else does
// either (chartgate_test.go has the refused pins and the measured reason).
func bundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	return bundleWithAgent(t, chartOwningApplications)
}

// chartOwningApplications is the pin bundle carries. The chart version is no
// longer an input to Values: the host-key pin renders whenever the mint
// carries known_hosts, which is safe on every chart Chart accepts.
const chartOwningApplications = "2.6.16"

// knownHosts is a host-key pin as the mint returns it: OpenSSH known_hosts
// lines for the host in repo_url. Public key material, not a secret.
const knownHosts = "[gitea.example.test]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExampleExampleExampleExamp"

func bundleWithAgent(t *testing.T, version string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\ncore:\n  kubenest-agent: " + version + "\nlimits:\n  timeouts:\n    component-ready: 2s\n"))
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
			// The control plane always answers false now (kn-cjqw OPTION A).
			// Option A has no receiver that creates workload Applications, so
			// this is what a mint looks like.
			CreatesWorkloadApplications: false,
		},
	}
	if withRepo {
		c.RepoCredential = &api.RepoCredential{
			PrivateKey: api.NewSecret(repoKey),
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
	chart, err := agent.Chart(bundle(t), creds(false), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if chart.Chart != "oci://ghcr.io/kubenesthq/charts/kubenest-operator-2" {
		t.Errorf("chart ref is %q — the version must not be carried twice", chart.Chart)
	}
	if chart.Version != chartOwningApplications {
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
	values, err := agent.Values(creds(false), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(values, "certManager") || !strings.Contains(values, "enabled: false") {
		t.Errorf("the chart's bootstrap cert-manager must be disabled — stage 6 already installed the platform's:\n%s", values)
	}
}

// The operator owns workload Applications on EVERY cluster (kn-cjqw OPTION A,
// 2026-09-10): the control plane no longer creates them at all, and it answers
// this field false. A receiver that read the field would install an operator
// that deploys nothing, so the value reaches no decision here — including the
// one a legacy control plane could still send.
func TestWorkloadApplicationsAreEnabledWhateverTheMintDeclares(t *testing.T) {
	for _, declared := range []bool{false, true} {
		c := creds(false)
		c.Operator.CreatesWorkloadApplications = declared
		values, err := agent.Values(c, agent.ValuesOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
			t.Fatal(err)
		}
		k := doc["kubenest"].(map[string]any)
		w := k["workloadApplications"].(map[string]any)
		if w["enabled"] != true {
			t.Errorf("a mint declaring creates_workload_applications=%v produced operator gate %v, want true", declared, w["enabled"])
		}
	}
}

// The management cluster's operator dials the control plane that runs inside
// that same cluster. The mint knows only the public hub URL, so the caller
// overrides it with the in-cluster service address; every other cluster keeps
// what the mint returned.
func TestBackendURLOverrideReplacesTheMintedHubURL(t *testing.T) {
	override := "ws://kubenest-cp-hub.kubenest-system.svc.cluster.local:8001/ws/operator"
	values, err := agent.Values(creds(false), agent.ValuesOptions{BackendURLOverride: override})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Kubenest map[string]any `yaml:"kubenest"`
	}
	if err := yaml.Unmarshal([]byte(values), &document); err != nil {
		t.Fatal(err)
	}
	if got := document.Kubenest["backendURL"]; got != override {
		t.Errorf("kubenest.backendURL = %v, want the override %q", got, override)
	}
}

// The operator must be able to verify the control plane's certificate, which
// no public root signs. The platform CA reaches it as a mounted ConfigMap and
// an environment variable that ADDS that directory to the system roots —
// which is why the value is SSL_CERT_DIR with /etc/ssl/certs still listed
// first, and not SSL_CERT_FILE, which would replace them.
func TestControlPlaneCARendersTheConfigMapAndTheTrustOptions(t *testing.T) {
	ca := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	values, err := agent.Values(creds(false), agent.ValuesOptions{ControlPlaneCA: ca})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		ExtraVolumes      []map[string]any `yaml:"extraVolumes"`
		ExtraVolumeMounts []map[string]any `yaml:"extraVolumeMounts"`
		ExtraEnv          []map[string]any `yaml:"extraEnv"`
	}
	if err := yaml.Unmarshal([]byte(values), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.ExtraVolumes) != 1 {
		t.Fatalf("extraVolumes = %v, want exactly the platform CA ConfigMap", document.ExtraVolumes)
	}
	volume := document.ExtraVolumes[0]
	if volume["name"] != "kubenest-platform-ca" {
		t.Errorf("volume name = %v", volume["name"])
	}
	if configMap, ok := volume["configMap"].(map[string]any); !ok || configMap["name"] != "kubenest-platform-ca" {
		t.Errorf("volume is not the kubenest-platform-ca ConfigMap: %v", volume)
	}
	if len(document.ExtraVolumeMounts) != 1 {
		t.Fatalf("extraVolumeMounts = %v, want exactly the platform CA mount", document.ExtraVolumeMounts)
	}
	mount := document.ExtraVolumeMounts[0]
	if mount["name"] != "kubenest-platform-ca" || mount["mountPath"] != "/etc/kubenest/platform-ca" || mount["readOnly"] != true {
		t.Errorf("mount = %v, want the platform CA mounted read-only at /etc/kubenest/platform-ca", mount)
	}
	if len(document.ExtraEnv) != 1 {
		t.Fatalf("extraEnv = %v, want exactly SSL_CERT_DIR", document.ExtraEnv)
	}
	env := document.ExtraEnv[0]
	if env["name"] != "SSL_CERT_DIR" {
		t.Errorf("environment variable = %v, want SSL_CERT_DIR (SSL_CERT_FILE would discard the system roots)", env["name"])
	}
	dirs, _ := env["value"].(string)
	if !strings.Contains(dirs, "/etc/ssl/certs") {
		t.Errorf("SSL_CERT_DIR = %q, which drops the system roots: a platform CA is one authority, not the whole trust store", dirs)
	}
	if !strings.Contains(dirs, "/etc/kubenest/platform-ca") {
		t.Errorf("SSL_CERT_DIR = %q, which does not include the mounted platform CA", dirs)
	}
}

// No CA means no mount, no volume and no environment variable: a cluster whose
// control plane has a publicly trusted certificate must not be handed
// directories that do not exist.
func TestNoControlPlaneCARendersNoTrustOptions(t *testing.T) {
	values, err := agent.Values(creds(false), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal([]byte(values), &document); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"extraVolumes", "extraVolumeMounts", "extraEnv"} {
		if _, present := document[key]; present {
			t.Errorf("%s rendered with no platform CA:\n%s", key, values)
		}
	}
	if strings.Contains(values, "kubenest-platform-ca") || strings.Contains(values, "SSL_CERT_DIR") {
		t.Errorf("the platform CA values rendered with no CA to trust:\n%s", values)
	}
}

// The ConfigMap is written ahead of the chart's own manifest, so the object
// the operator's Pod mounts is on the way to the cluster first — and the
// agent's manifest stays a single HelmChart document, which is the shape the
// JWT-rotation path parses and patches.
func TestInstallWritesThePlatformCAManifestBeforeTheChart(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "get deployment") || strings.Contains(cmd, "kubectl get") {
			return sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Available","status":"True"}]}}`}, nil
		}
		return sshx.Result{}, nil
	}}
	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	if err := agent.Install(context.Background(), fake, bundle(t), creds(false),
		agent.ValuesOptions{ControlPlaneCA: []byte(ca)}, nil); err != nil {
		t.Fatal(err)
	}

	var configMap, chart []byte
	configMapAt, chartAt := -1, -1
	for i, e := range fake.Executions() {
		switch {
		case strings.Contains(string(e.Stdin), "kind: ConfigMap"):
			configMap, configMapAt = e.Stdin, i
		case strings.Contains(string(e.Stdin), "kind: HelmChart"):
			chart, chartAt = e.Stdin, i
		}
	}
	if configMapAt < 0 {
		t.Fatal("the platform CA ConfigMap did not reach the cluster")
	}
	if chartAt < 0 {
		t.Fatal("the HelmChart did not reach the cluster")
	}
	if configMapAt > chartAt {
		t.Error("the ConfigMap was written after the HelmChart: the operator's Pod would mount an object that is not there yet")
	}
	text := string(configMap)
	if !strings.Contains(text, "ca.crt") || !strings.Contains(text, "BEGIN CERTIFICATE") || !strings.Contains(text, "MIIB") {
		t.Errorf("the ConfigMap does not carry the CA under the ca.crt key:\n%s", text)
	}
	if !strings.Contains(text, "namespace: kubenest-system") {
		t.Errorf("the ConfigMap is not in the operator's namespace:\n%s", text)
	}
	if !strings.Contains(text, "name: kubenest-platform-ca") {
		t.Errorf("the ConfigMap is not the one the values reference:\n%s", text)
	}
	if strings.Contains(string(chart), "kind: ConfigMap") {
		t.Errorf("the agent manifest must stay a single HelmChart document:\n%s", chart)
	}
}

// The credential API calls this field hub_url because it points at the hub.
// The established chart key is kubenest.backendURL, which becomes the
// KUBENEST_BACKEND_URL environment variable. The removed backend-generated
// Helm command once copied the API name into a nonexistent kubenest.hubURL
// key; Helm ignored it and the operator silently dialled the chart default.
func TestHubURLMapsToTheChartBackendURLKey(t *testing.T) {
	values, err := agent.Values(creds(false), agent.ValuesOptions{})
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
	withRepo, err := agent.Values(creds(true), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withRepo, "gitea") {
		t.Errorf("a minted repo credential must disable the in-cluster Gitea:\n%s", withRepo)
	}
	withoutRepo, err := agent.Values(creds(false), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(withoutRepo, "gitea") {
		t.Errorf("with no repo credential the chart's own fallback is left at its default:\n%s", withoutRepo)
	}
}

// recoverableFrom and carries delegate to pkg/leakscan, which owns the
// scanner and its own tests. They were defined here first, and then pkg/backup
// and pkg/k3s turned out to need exactly the same check — the object store
// credentials and the cluster join token were leaking by the same mechanism.
// Copying a subtle scanner into three packages is how it drifts, and a drifted
// leak scanner fails open.
//
// Note what leakscan does NOT catch: base64 wrapped across lines, base64 in
// shell-concatenated chunks, hex, and anything compressed before encoding.
// The comment that used to live here claimed "any future write path that
// encodes, wraps, or chunks a secret onto a command line trips it", which was
// false — an unfalsifiable claim about a falsification tool. leakscan's own
// TestKnownBlindSpotsAreStillBlindSpots pins the real limit.
func recoverableFrom(command, needle string) bool {
	return leakscan.RecoverableFrom(command, needle)
}

func carries(text, needle string) bool { return leakscan.Carries(text, needle) }

// The scanner above is the only thing standing between these tests and the
// vacuity they had before, so it is itself tested — against the exact command
// shape that shipped the defect. Without this, a scanner that silently
// matched nothing would make every assertion below pass forever, which is the
// failure mode being corrected, reintroduced one level up.
func TestTheLeakScannerCatchesTheShapeThatShippedTheDefect(t *testing.T) {
	values, err := agent.Values(creds(true), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// k3s.WriteManifest as it stood before kn-40rd.
	defective := "printf '%s' " + base64.StdEncoding.EncodeToString([]byte(values)) +
		" | base64 -d | sudo -n tee /var/lib/rancher/k3s/server/manifests/kubenest-agent.yaml >/dev/null"

	if strings.Contains(defective, agentJWT) {
		t.Fatal("the fixture is not the defect: the JWT is plaintext in it, which the old check would have caught")
	}
	for name, needle := range map[string]string{"agent JWT": agentJWT, "repo deploy key": repoKey} {
		if !recoverableFrom(defective, needle) {
			t.Errorf("the scanner does not catch the %s in the command shape that shipped the defect — every leak assertion below is vacuous", name)
		}
	}
	if recoverableFrom("sudo -n chmod 600 /var/lib/rancher/k3s/server/manifests/kubenest-agent.yaml >/dev/null", agentJWT) {
		t.Error("the scanner matches a command carrying no payload — it would fail on any install")
	}
}

// THE rule for this package: the JWT reaches the cluster as chart values over
// stdin and by no other route. Never a command argument, in any encoding —
// command lines are visible in the target host's process list — and the file
// it lands in is 0600, because the k3s auto-deploy directory is
// world-readable by default.
func TestTheAgentJWTNeverReachesACommandLineAndItsFileIsPrivate(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "get deployment") || strings.Contains(cmd, "kubectl get") {
			return sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Available","status":"True"}]}}`}, nil
		}
		return sshx.Result{}, nil
	}}
	if err := agent.Install(context.Background(), fake, bundle(t), creds(true), agent.ValuesOptions{}, nil); err != nil {
		t.Fatal(err)
	}

	var chmodded bool
	for _, cmd := range fake.Commands() {
		if recoverableFrom(cmd, agentJWT) || recoverableFrom(cmd, "AGENT_TOKEN_VALUE") {
			t.Fatalf("the agent JWT is recoverable from a command line:\n%s", cmd)
		}
		if strings.Contains(cmd, "chmod 600") && strings.Contains(cmd, "kubenest-agent.yaml") {
			chmodded = true
		}
	}
	if !chmodded {
		t.Error("the values file carries the agent JWT and must be chmod 600 on the server node")
	}

	// Positive control for THIS run: the JWT must actually have been written,
	// over stdin. Without it the loop above would also pass on an install
	// that rendered no credential at all — a green earned by doing nothing.
	var delivered bool
	for _, in := range fake.Inputs() {
		if carries(string(in), agentJWT) {
			delivered = true
		}
	}
	if !delivered {
		t.Error("the agent JWT reached the cluster by no route at all — it must travel to the auto-deploy dir over stdin")
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
			if _, err := agent.Values(c, agent.ValuesOptions{}); err == nil {
				t.Fatal("want a refusal")
			}
		})
	}
	if _, err := agent.Values(nil, agent.ValuesOptions{}); err == nil {
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
		chart, err := agent.Chart(bundle(t), c, agent.ValuesOptions{})
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if chart.Chart != want {
			t.Errorf("Chart ref %q became %q, want %q", ref, chart.Chart, want)
		}
		if chart.Version != chartOwningApplications {
			t.Errorf("%s: version is %q, want the bundle pin", ref, chart.Version)
		}
	}
}

// kn-rnyl.2: when the mint carries a repo_credential, the agent must hand the
// operator the per-cluster repo it just disabled Gitea in favour of — URL,
// branch and write deploy key — through the chart's bootstrapController keys.
// Disabling Gitea without this leaves the operator with NEITHER a Git server
// nor the external repo.
func TestRepoCredentialRendersIntoTheBootstrapControllerKeys(t *testing.T) {
	values, err := agent.Values(creds(true), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Kubenest struct {
			BootstrapController map[string]any `yaml:"bootstrapController"`
		} `yaml:"kubenest"`
		Bootstrap struct {
			Gitea map[string]any `yaml:"gitea"`
		} `yaml:"bootstrap"`
	}
	if err := yaml.Unmarshal([]byte(values), &document); err != nil {
		t.Fatal(err)
	}
	bc := document.Kubenest.BootstrapController
	if bc == nil {
		t.Fatalf("kubenest.bootstrapController is absent — the minted repo credential was dropped:\n%s", values)
	}
	if got := bc["gitRepoURL"]; got != "git@gitea.example.test:kubenest/prod-1.git" {
		t.Errorf("gitRepoURL = %v, want the minted repo_url", got)
	}
	if got := bc["gitRepoBranch"]; got != "main" {
		t.Errorf("gitRepoBranch = %v, want the minted branch", got)
	}
	if got := bc["gitSSHPrivateKey"]; got != repoKey {
		t.Errorf("gitSSHPrivateKey did not survive the YAML round trip byte-for-byte;\n got %q\nwant %q", got, repoKey)
	}
	if _, leaked := bc["gitToken"]; leaked {
		t.Error("gitToken is the legacy backend-wide writer and must stay at its empty default")
	}
	if document.Bootstrap.Gitea == nil || document.Bootstrap.Gitea["enabled"] != false {
		t.Errorf("a minted repo credential must disable the in-cluster Gitea:\n%s", values)
	}
}

// No repo credential means the control plane has no GitOps configured: the
// chart's own Gitea fallback stands, and no external credential values may
// render — a gitRepoURL of "" with Gitea disabled reproduces kn-rnyl.2.
func TestNoRepoCredentialRendersNoBootstrapController(t *testing.T) {
	values, err := agent.Values(creds(false), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Kubenest struct {
			BootstrapController map[string]any `yaml:"bootstrapController"`
		} `yaml:"kubenest"`
	}
	if err := yaml.Unmarshal([]byte(values), &document); err != nil {
		t.Fatal(err)
	}
	if document.Kubenest.BootstrapController != nil {
		t.Errorf("with no repo credential, bootstrapController must stay at the chart defaults:\n%s", values)
	}
	if strings.Contains(values, "gitSSHPrivateKey") || strings.Contains(values, "gitRepoURL") {
		t.Errorf("external credential keys rendered without a repo credential:\n%s", values)
	}
}

// THE rule for this package, extended to the deploy key: it reaches the
// cluster as chart values and by no other route. Never a command argument —
// command lines are visible in the target host's process list — and never the
// journal: api.Secret refuses to marshal, and the install Record keeps only
// the non-secret repo_url (pkg/install/plan.go).
func TestTheRepoKeyNeverReachesACommandLineNorAJournal(t *testing.T) {
	fake := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "get deployment") || strings.Contains(cmd, "kubectl get") {
			return sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Available","status":"True"}]}}`}, nil
		}
		return sshx.Result{}, nil
	}}
	if err := agent.Install(context.Background(), fake, bundle(t), creds(true), agent.ValuesOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range fake.Commands() {
		if recoverableFrom(cmd, repoKey) || recoverableFrom(cmd, "OPENSSH PRIVATE KEY") {
			t.Fatalf("the repo private key is recoverable from a command line:\n%s", cmd)
		}
	}

	// Positive control: the key must have reached the cluster over stdin, or
	// the scan above proves nothing about a document that was never rendered.
	var delivered bool
	for _, in := range fake.Inputs() {
		if carries(string(in), repoKey) {
			delivered = true
		}
	}
	if !delivered {
		t.Error("the repo deploy key reached the cluster by no route at all — it must travel to the auto-deploy dir over stdin")
	}

	// The journal path: anything a journal could marshal must refuse to carry
	// the key. AgentCredentials contains RepoCredential.PrivateKey (api.Secret),
	// so encoding it is an error, not a leak.
	if _, err := json.Marshal(creds(true)); err == nil {
		t.Fatal("marshaling the minted credentials succeeded — the private key could reach a journal")
	}
	var leaked strings.Builder
	enc := json.NewEncoder(&leaked)
	_ = enc.Encode(creds(true)) // error by design; check nothing escaped anyway
	if strings.Contains(leaked.String(), "OPENSSH PRIVATE KEY") {
		t.Fatalf("the private key leaked into encoded output:\n%s", leaked.String())
	}
}

// A half-rendered repo credential installs an operator that can neither push
// nor sync; the mint is the only producer, so refuse at render time and name
// the fix.
func TestAMalformedRepoCredentialIsRefused(t *testing.T) {
	cases := map[string]func(*api.AgentCredentials){
		"no repo_url":    func(c *api.AgentCredentials) { c.RepoCredential.RepoURL = "" },
		"no private_key": func(c *api.AgentCredentials) { c.RepoCredential.PrivateKey = api.Secret{} },
		"no branch":      func(c *api.AgentCredentials) { c.RepoCredential.Branch = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := creds(true)
			mutate(c)
			if _, err := agent.Values(c, agent.ValuesOptions{}); err == nil {
				t.Fatal("want a refusal for a repo credential missing its " + name)
			}
		})
	}
}

// kn-rnyl.1: the deploy key proves the CLIENT to Gitea. Nothing proved Gitea
// to the cluster, so an attacker on the path could serve forged desired state
// to the operator and to ArgoCD. When the mint carries a host-key pin it must
// reach the chart on BOTH sides — go-git verifies against gitSSHKnownHosts and
// Helm teaches ArgoCD the same key through argo-cd.configs.ssh.extraHosts.
//
// This renders unconditionally now, with no chart version to consult: every
// chart agent.Chart accepts is >= 2.6.5, and 2.6.0 was the first whose operator
// both stops setting insecureIgnoreHostKey and writes the host into
// argocd-ssh-known-hosts-cm. Below 2.6.0 the pin was an outage rather than
// weaker hardening (ArgoCD refuses a repository it has no entry for), and that
// range is refused before Values runs.
func TestHostKeyPinRendersOnEveryAcceptedChart(t *testing.T) {
	c := creds(true)
	c.RepoCredential.KnownHosts = knownHosts

	values, err := agent.Values(c, agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bc := bootstrapControllerOf(t, values)
	if got := bc["gitSSHKnownHosts"]; got != knownHosts {
		t.Errorf("gitSSHKnownHosts = %v, want the minted known_hosts verbatim", got)
	}
	if got, present := argoSSHExtraHostsOf(t, values); !present || got != knownHosts {
		t.Errorf("argo-cd.configs.ssh.extraHosts = %q, %v; want the minted known_hosts verbatim and present", got, present)
	}
}

// A mint from a control plane that could not establish the host key returns
// none, and an empty pin must not be rendered: gitSSHKnownHosts: "" writes an
// empty known_hosts key into the bootstrap Secret, and the operator reads an
// empty file as "no pin" only because it checks — an empty file handed to
// go-git's parser fails every clone with "no valid known_hosts".
func TestNoHostKeyPinRendersNoValue(t *testing.T) {
	values, err := agent.Values(creds(true), agent.ValuesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := bootstrapControllerOf(t, values)["gitSSHKnownHosts"]; present {
		t.Errorf("an absent pin must leave the chart default alone:\n%s", values)
	}
	if _, present := argoSSHExtraHostsOf(t, values); present {
		t.Errorf("an absent pin must not render argo-cd.configs.ssh.extraHosts:\n%s", values)
	}
}

// argoSSHExtraHostsOf returns the same public pin Helm passes to the Argo CD
// subchart. It must travel with gitSSHKnownHosts: otherwise the operator is an
// out-of-band writer of a Helm-managed ConfigMap and a later credential
// rotation makes Helm 4 reject its upgrade with an SSA ownership conflict.
func argoSSHExtraHostsOf(t *testing.T, values string) (string, bool) {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal([]byte(values), &document); err != nil {
		t.Fatal(err)
	}
	argo, ok := document["argo-cd"].(map[string]any)
	if !ok {
		return "", false
	}
	configs, ok := argo["configs"].(map[string]any)
	if !ok {
		return "", false
	}
	ssh, ok := configs["ssh"].(map[string]any)
	if !ok {
		return "", false
	}
	extraHosts, ok := ssh["extraHosts"].(string)
	return extraHosts, ok
}

func bootstrapControllerOf(t *testing.T, values string) map[string]any {
	t.Helper()
	var document struct {
		Kubenest struct {
			BootstrapController map[string]any `yaml:"bootstrapController"`
		} `yaml:"kubenest"`
	}
	if err := yaml.Unmarshal([]byte(values), &document); err != nil {
		t.Fatal(err)
	}
	if document.Kubenest.BootstrapController == nil {
		t.Fatalf("kubenest.bootstrapController is absent:\n%s", values)
	}
	return document.Kubenest.BootstrapController
}
