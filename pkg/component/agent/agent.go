// Package agent is stage 10: the KubeNest agent — which IS the operator
// (decision G, 2026-08-20). The bundle pins the kubenest-operator-2 chart
// under the name kubenest-agent, and this package installs that chart with
// the identity minted in stage 2.
//
// CREDENTIAL HANDLING, which is the whole reason this package is small:
// the agent JWT and the per-cluster GitOps deploy key reach the cluster as
// chart values and by no other route. The values document travels to the
// server node over stdin (k3s.WriteManifest), never as an argument to a
// command — a command line is the argv of the shell sshd spawns, readable in
// `ps auxww` by any local user on the target host — never a log line, and
// never journalled. The values file it lands in is chmod 600 on the server
// node, because the k3s auto-deploy directory is world-readable by default
// and a JWT sitting at 0644 on a customer's box is a finding.
//
// "Never an argument to a command" was stated here, and tested for, while it
// was false: the write path base64-encoded the whole document into the
// command string, so a test scanning for the plaintext could not see it
// (kn-40rd). The tests now decode before they look, and one of them checks
// the scanner itself against the defective command shape — a leak assertion
// that cannot fail is the thing that let this stand.
//
// The chart's identity Secret is rendered from clusterID + jwtSecret
// (kn-z6e4): a helm-only install is self-sufficient and nothing has to reach
// into the cluster afterwards.
package agent

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// manifestName is the file the HelmChart resource is written to.
const manifestName = "kubenest-agent"

// releaseName is the Helm release, and it is SHORT for a reason that is not
// style. The chart names its metrics service
// <release>-kubenest-operator-2-controller-manager-metrics-service, and
// Kubernetes refuses any object name over 63 characters. "kubenest-agent"
// produces 69 and the install fails at the moment helm creates that service —
// observed on a real two-node cluster. "operator" produces 62, lands inside
// the limit, and is what production has always used (AGENTS.md section 5).
const releaseName = "operator"

// ChartName is the chart as Helm names it, which is also the prefix of the
// helm.sh/chart label every object the chart renders carries —
// "helm.sh/chart: kubenest-operator-2-<chart version>", from the chart's
// templates/_helpers.tpl ("kubenest-operator.labels"). The agent upgrade
// reads that label to tell whether helm has applied a new chart version, so
// the name lives here rather than being spelled out at each reader.
const ChartName = "kubenest-operator-2"

// DeploymentName is the operator's Deployment: the release name, the chart
// name and the suffix the chart gives its controller manager. Exported
// because the installer, the upgrade and the acceptance checks all need to
// name the same object.
const DeploymentName = releaseName + "-" + ChartName + "-controller-manager"

// ReleaseName is the Helm release the agent is installed as. Callers that
// need to find it on a cluster read it from here rather than repeating it.
func ReleaseName() string { return releaseName }

// platformCAConfigMap is the ConfigMap the install creates when it hands the
// operator the platform's certificate authority. It lives in the operator's
// own namespace so the chart's volume reference needs no cross-namespace
// lookup.
const platformCAConfigMap = "kubenest-platform-ca"

// platformCAMountPath is where that ConfigMap is mounted. SSL_CERT_DIR points
// at it, and the Go runtime loads every *.crt-style file in every directory
// named there — which is why the file inside the ConfigMap is called ca.crt.
const platformCAMountPath = "/etc/kubenest/platform-ca"

// ValuesOptions carries what an install knows and the mint cannot.
//
// BackendURLOverride replaces the hub URL the mint returned. The management
// cluster's operator talks to the control plane running inside that same
// cluster, where the control plane is reachable by its in-cluster service name
// and not by the public hub URL the mint hands every other cluster.
//
// ControlPlaneCA is the PEM of the authority the control plane's serving
// certificate chains to. When it is set the rendered install mounts it and
// points the operator's runtime at it, so the operator can verify an endpoint
// whose certificate no public root signs.
type ValuesOptions struct {
	BackendURLOverride string
	ControlPlaneCA     []byte
}

// Values renders the chart values for one cluster's agent.
//
// Three bootstrap dependencies of the operator chart are decided here, and
// each is a "two of the same thing" hazard if left at its default on a
// platform cluster:
//
//	cert-manager  DISABLED. Stage 6 installed the platform's pinned
//	              cert-manager; the chart's bootstrap copy is a different
//	              version, and two cert-managers fight over the same CRDs.
//	gitea         DISABLED when the control plane minted a repository
//	              credential — an in-cluster Git server would be a second
//	              source of truth beside the per-cluster repo the broker
//	              issued. Left at the chart default when no repo credential
//	              was issued, which is the chart's zero-external-dependency
//	              fallback.
//	argo-cd       left at the chart default: it is the operator's own
//	              deploy engine, not a platform component.
//
// The fourth decision is what the disabled Gitea is replaced WITH: when the
// mint carries a repo_credential, this renders the per-cluster GitOps
// credential into kubenest.bootstrapController — gitRepoURL, gitRepoBranch,
// and gitSSHPrivateKey, the write deploy key on that cluster's own repo
// (kn-rnyl phases B/C). Disabling Gitea WITHOUT rendering this is the
// kn-rnyl.2 defect: the installed operator ends up with neither a Git server
// nor the external repo. The private key rides the same protected path as
// the agent JWT: chart values, written to a 0600 file, never a command
// argument, never the journal.
//
// The fifth is whether to pin the GitOps host's SSH key: whenever the mint
// carries known_hosts (kn-rnyl.1), this renders it into gitSSHKnownHosts AND
// into argo-cd.configs.ssh.extraHosts, so go-git verifies the host and ArgoCD
// is taught the same key. That pairing is safe on every chart this package
// accepts, which is why the version is no longer an input here: Chart refuses
// anything below MinChartOwningApplications, 2.6.5, and 2.6.0 was the first
// whose operator writes the host into argocd-ssh-known-hosts-cm instead of
// only dropping insecureIgnoreHostKey. A pin below 2.6.0 was an outage rather
// than weaker hardening — ArgoCD refused a repository it had no entry for —
// and 2.6.0 is unreachable now.
//
// The last two values do not come from the mint at all: ValuesOptions carries
// what only the caller knows, which is how the management cluster's operator
// finds the control plane that runs beside it and how it verifies that control
// plane's certificate.
func Values(creds *api.AgentCredentials, opts ValuesOptions) (string, error) {
	if creds == nil {
		return "", fmt.Errorf("the agent needs the credentials minted in stage 2")
	}
	if creds.ClusterID == "" {
		return "", fmt.Errorf("the minted credentials carry no cluster id: the agent's identity Secret cannot be rendered without one, and an operator with an empty cluster id has every heartbeat rejected (kn-z6e4)")
	}
	if creds.AgentJWT.Token.IsZero() {
		return "", fmt.Errorf("the minted credentials carry no agent JWT")
	}
	if creds.AgentJWT.HubURL == "" {
		return "", fmt.Errorf("the minted credentials carry no hub URL: the agent would dial the chart's default, which is not this control plane")
	}
	if creds.RepoCredential != nil {
		// A half-rendered repo credential installs an operator that can
		// neither push nor sync; refuse at render time instead.
		if creds.RepoCredential.RepoURL == "" {
			return "", fmt.Errorf("the minted repo credential carries no repo_url: the operator would push to nothing — re-mint with POST /clusters/{id}/agent-credentials")
		}
		if creds.RepoCredential.PrivateKey.IsZero() {
			return "", fmt.Errorf("the minted repo credential carries no private_key: a gitSSHPrivateKey of '' is a startup error for the operator — re-mint with POST /clusters/{id}/agent-credentials")
		}
		if creds.RepoCredential.Branch == "" {
			return "", fmt.Errorf("the minted repo credential carries no branch: the operator would clone the empty default and never find the cluster's desired state — re-mint with POST /clusters/{id}/agent-credentials")
		}
	}

	values := map[string]any{
		"kubenest": map[string]any{
			"clusterID": creds.ClusterID,
			// Reveal at the point of use, and only here: this value is
			// written to a 0600 file on the server node and read by the
			// in-cluster helm controller.
			"jwtSecret":  creds.AgentJWT.Token.Reveal(),
			"backendURL": creds.AgentJWT.HubURL,
			// The operator owns workload Applications on every cluster
			// (kn-cjqw OPTION A, 2026-09-10). The control plane never creates
			// one, so the mint's creates_workload_applications is deliberately
			// not read here: a receiver that was told it owns nothing would
			// leave every workload Application owned by nobody.
			"workloadApplications": map[string]any{"enabled": true},
		},
		"bootstrap": map[string]any{
			"certManager": map[string]any{"enabled": false},
		},
	}
	if opts.BackendURLOverride != "" {
		// The management cluster's own operator dials the control plane that
		// runs inside it, whose in-cluster service name is not the public hub
		// URL the mint returned.
		values["kubenest"].(map[string]any)["backendURL"] = opts.BackendURLOverride
	}
	if len(opts.ControlPlaneCA) > 0 {
		values["extraVolumes"] = []any{map[string]any{
			"name":      platformCAConfigMap,
			"configMap": map[string]any{"name": platformCAConfigMap},
		}}
		values["extraVolumeMounts"] = []any{map[string]any{
			"name":      platformCAConfigMap,
			"mountPath": platformCAMountPath,
			"readOnly":  true,
		}}
		// SSL_CERT_DIR and NOT SSL_CERT_FILE: the runtime loads the
		// certificates found in every directory listed here in ADDITION to the
		// system roots, while SSL_CERT_FILE would replace them and leave the
		// operator unable to verify anything the platform CA has not signed.
		values["extraEnv"] = []any{map[string]any{
			"name":  "SSL_CERT_DIR",
			"value": "/etc/ssl/certs:" + platformCAMountPath,
		}}
	}
	if creds.RepoCredential != nil {
		values["bootstrap"].(map[string]any)["gitea"] = map[string]any{"enabled": false}
		// The per-cluster GitOps repo the broker issued (kn-rnyl). The key is
		// revealed at the point of use and only here, like the JWT: it lands
		// in the 0600 values file the in-cluster helm controller reads, and
		// the chart mounts it as a file — never the operator's environment.
		// gitToken, the legacy backend-wide writer, is deliberately left at
		// its empty default: the deploy key wins when both are present, and
		// rendering the token is how the kn-rnyl leak happened in the first
		// place.
		bootstrapController := map[string]any{
			"gitRepoURL":       creds.RepoCredential.RepoURL,
			"gitRepoBranch":    creds.RepoCredential.Branch,
			"gitSSHPrivateKey": creds.RepoCredential.PrivateKey.Reveal(),
		}
		// known_hosts is the server's PUBLIC key. It is not revealed from a
		// Secret because it was never one, and it is what stops an attacker
		// on the path to Gitea from serving forged desired state (kn-rnyl.1).
		if creds.RepoCredential.KnownHosts != "" {
			bootstrapController["gitSSHKnownHosts"] = creds.RepoCredential.KnownHosts
			// The Argo CD subchart owns argocd-ssh-known-hosts-cm. Give Helm
			// the same public pin it gives the operator, rather than making the
			// operator an out-of-band writer of that Helm-managed field. Apart
			// from avoiding an SSA conflict on credential rotation, this means
			// ArgoCD starts strict on the first reconciliation.
			values["argo-cd"] = map[string]any{
				"configs": map[string]any{
					"ssh": map[string]any{"extraHosts": creds.RepoCredential.KnownHosts},
				},
			}
		}
		values["kubenest"].(map[string]any)["bootstrapController"] = bootstrapController
	}

	out, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// MinChartOwningApplications is the first kubenest-agent chart whose operator
// creates the cluster's workload Argo CD Applications itself.
//
// The control plane never creates them (kn-cjqw OPTION A, 2026-09-10):
// Values renders kubenest.workloadApplications.enabled true on EVERY install
// and the mint's creates_workload_applications is deliberately not read.
// The operator's half of that bargain first shipped in chart 2.6.5: op3
// 0ba157a taught the operator to own the workload's ArgoCD Application, made
// to survive `helm upgrade --reuse-values` in 5c965a8, and 2.6.5 (op3 f767d97)
// is the first RELEASE carrying both — 5c965a8 did not bump the chart version.
//
// Below it the value is accepted and silently discarded, because no chart in
// this line ships a values.schema.json: helm has no schema to reject
// kubenest.workloadApplications against, the install goes green, and NOTHING
// creates that cluster's workload Applications. That end state is the defect;
// CheckChart is the refusal that prevents it.
const MinChartOwningApplications = "2.6.5"

// CheckChart refuses a kubenest-agent pin below MinChartOwningApplications.
//
// It lives here rather than at the install call site because BOTH ends need
// it and they are far apart: preflight runs it before a single byte reaches a
// host (pkg/preflight), and Chart runs it before rendering, so a caller that
// skipped preflight still cannot install a cluster whose applications would be
// owned by nobody. An unreadable version is refused too — a gate that defaults
// an unparseable pin to "new enough" fails open, which is the shape of defect
// this whole check exists to stop.
func CheckChart(version string) error {
	cmp, err := manifest.CompareVersions(version, MinChartOwningApplications)
	if err != nil {
		return fmt.Errorf("cannot tell whether kubenest-agent %s creates the cluster's workload Argo CD Applications: %w", version, err)
	}
	if cmp < 0 {
		return fmt.Errorf(
			"bundle pins kubenest-agent %s, whose operator does not create the cluster's workload Argo CD Applications, and the control plane never creates them, so this cluster would install and then deploy nothing. %s is the first agent chart that creates them. Install this cluster from a bundle that pins kubenest-agent %s or later",
			version, MinChartOwningApplications, MinChartOwningApplications)
	}
	return nil
}

// Chart renders the agent's HelmChart resource at the bundle's pin. The chart
// reference comes from the MINT (operator.chart_ref), not from a constant:
// the control plane composes it from the manifest's sources section, and a
// hardcoded registry is how kn-z6e4 shipped a chart_ref that did not exist.
func Chart(bundle *manifest.Manifest, creds *api.AgentCredentials, opts ValuesOptions) (k3s.HelmChart, error) {
	version, err := bundle.Core.Version("kubenest-agent")
	if err != nil {
		return k3s.HelmChart{}, err
	}
	// Before anything else: an accepted pin is one whose operator creates the
	// cluster's workload Applications, and every later decision in this
	// function — including the host-key pin — is safe only on such a chart.
	if err := CheckChart(version); err != nil {
		return k3s.HelmChart{}, err
	}
	if creds == nil || creds.Operator.ChartRef == "" {
		return k3s.HelmChart{}, fmt.Errorf("the minted credentials carry no operator chart reference")
	}
	namespace := creds.Operator.Namespace
	if namespace == "" {
		return k3s.HelmChart{}, fmt.Errorf("the minted credentials carry no operator namespace")
	}
	values, err := Values(creds, opts)
	if err != nil {
		return k3s.HelmChart{}, err
	}

	// The bundle pin is authoritative for the version; the ref says only
	// where the chart lives.
	ref := chartRepository(creds.Operator.ChartRef)

	return k3s.HelmChart{
		// The HelmChart resource name IS the Helm release name, which is why
		// this is the short one and not manifestName.
		Name:            releaseName,
		Chart:           ref,
		Version:         version,
		TargetNamespace: namespace,
		ValuesYAML:      values,
	}, nil
}

// chartRepository strips any tag from an OCI reference, leaving only where
// the chart lives.
//
// ONE PIN, ONE PLACE. The bundle manifest's core section decides the version;
// the mint's chart_ref decides the registry and repository. The control plane
// composes that ref with a tag for its own convenience, and that tag reflects
// whichever bundle the control plane last looked at — not necessarily the one
// being installed. Installing bundle 0.9 with a ref the control plane tagged
// 2.3.4 produced:
//
//	Error: INSTALLATION FAILED: chart reference and version mismatch:
//	2.2.0 is not 2.3.4
//
// An earlier version of this function stripped the tag only when it already
// matched the bundle pin, on the reasoning that an unexpected tag should be
// reported rather than silently rewritten. It was reported, loudly and
// legibly — and it also made installing any bundle other than the newest
// impossible. The version was never the ref's to carry.
//
// A registry port is not a tag: only a colon in the segment after the last
// slash is one.
func chartRepository(ref string) string {
	slash := strings.LastIndex(ref, "/")
	tail := ref[slash+1:]
	colon := strings.LastIndex(tail, ":")
	if colon < 0 {
		return ref
	}
	return ref[:slash+1+colon]
}

// Install places the agent and waits for it to be Ready.
//
// Every cluster — the management cluster included — reaches this with
// credentials minted by a control plane, because every cluster registers
// through the same API path (decision D17, 2026-09-24). The management
// cluster's difference is in opts, not in the path taken.
func Install(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, creds *api.AgentCredentials, opts ValuesOptions, rep converge.Reporter) error {
	chart, err := Chart(bundle, creds, opts)
	if err != nil {
		return err
	}
	return installChart(ctx, r, bundle, chart, opts.ControlPlaneCA, rep)
}

// installChart writes the agent's manifest, restricts it, and waits for it to
// be Ready.
//
// The 0600 is not conditional on there being a secret in the file today: the
// manifest directory is world-readable, and every values document this package
// renders names the cluster's identity or its credentials.
//
// controlPlaneCA, when set, is written as a manifest of its own BEFORE the
// chart's, so the ConfigMap the operator's Pod mounts is on the way to the
// cluster first. The agent's own manifest stays a single HelmChart document,
// which is what pkg/component/agent's rotation reads and patches.
func installChart(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, chart k3s.HelmChart, controlPlaneCA []byte, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	if len(controlPlaneCA) > 0 {
		configMap, err := platformCAManifest(chart.TargetNamespace, controlPlaneCA)
		if err != nil {
			return err
		}
		if err := k3s.WriteManifest(ctx, r, platformCAConfigMap, configMap); err != nil {
			return err
		}
	}
	doc, err := chart.Manifest()
	if err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, manifestName, doc); err != nil {
		return err
	}
	// The values carry the agent JWT and the auto-deploy directory is
	// world-readable by default.
	if err := restrict(ctx, r, k3s.ManifestDir+"/"+manifestName+".yaml"); err != nil {
		return err
	}

	res, err := converge.Wait(ctx,
		component.ConditionProbe(r, "deployment/"+DeploymentName, chart.TargetNamespace, "Available"),
		converge.Options{Name: "kubenest-agent", Deadline: deadline, Reporter: rep})
	if err != nil {
		return err
	}
	return res.Err()
}

// platformCAManifest renders the ConfigMap the operator mounts when the
// install hands it the platform's certificate authority.
//
// The namespace is the OPERATOR's, taken from the mint rather than fixed here,
// because a ConfigMap can only be mounted from the namespace the pod runs in.
func platformCAManifest(namespace string, ca []byte) ([]byte, error) {
	if namespace == "" {
		return nil, fmt.Errorf("the platform CA ConfigMap needs the operator's namespace")
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      platformCAConfigMap,
			"namespace": namespace,
			"labels":    map[string]string{"app.kubernetes.io/managed-by": "kubenest-cli"},
		},
		"data": map[string]string{"ca.crt": string(ca)},
	})
}

func restrict(ctx context.Context, r k3s.Runner, path string) error {
	res, err := r.Run(ctx, "sudo -n chmod 600 "+path)
	if err != nil {
		return fmt.Errorf("restricting %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restricting %s: exit %d: %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
