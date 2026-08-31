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

// DeploymentName is the operator's Deployment, derived from the release name.
// Exported because the installer, the upgrade and the acceptance checks all
// need to name the same object.
const DeploymentName = releaseName + "-kubenest-operator-2-controller-manager"

// ReleaseName is the Helm release the agent is installed as. Callers that
// need to find it on a cluster read it from here rather than repeating it.
func ReleaseName() string { return releaseName }

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
// argument, never the journal. The chart's gitSSHKnownHosts stays at its
// default — the contract does not carry a host key yet (kn-rnyl.1), so
// host-key pinning cannot honestly be rendered from here.
func Values(creds *api.AgentCredentials) (string, error) {
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
		},
		"bootstrap": map[string]any{
			"certManager": map[string]any{"enabled": false},
		},
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
		// place. gitSSHKnownHosts stays empty until the contract carries a
		// host key (kn-rnyl.1).
		values["kubenest"].(map[string]any)["bootstrapController"] = map[string]any{
			"gitRepoURL":       creds.RepoCredential.RepoURL,
			"gitRepoBranch":    creds.RepoCredential.Branch,
			"gitSSHPrivateKey": creds.RepoCredential.PrivateKey.Reveal(),
		}
	}

	out, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// minChartWithRepoCredential is the first published kubenest-operator-2 chart
// whose values carry gitSSHPrivateKey and whose deployment renders the
// GIT_SSH_* environment. Chart 2.2.0, which platform-0.9 pins, has neither.
//
// Helm does not reject unknown values — this chart ships no values.schema.json
// — so rendering gitSSHPrivateKey against 2.2.0 does not fail. It is accepted
// and discarded. The install then completes green with bootstrap.gitea
// disabled and no Git credential anywhere: neither the bundled Git server nor
// the external repo, which is precisely the state kn-rnyl.2 exists to prevent,
// reached silently.
//
// 0.9's pin is deliberate history and must not move (see the header of
// platform-0.9.yaml: back-pinning it to a chart that did not exist when 0.9
// was current "would be fiction"). So the refusal belongs here.
const minChartWithRepoCredential = "2.3.5"

// Chart renders the agent's HelmChart resource at the bundle's pin. The chart
// reference comes from the MINT (operator.chart_ref), not from a constant:
// the control plane composes it from the manifest's sources section, and a
// hardcoded registry is how kn-z6e4 shipped a chart_ref that did not exist.
func Chart(bundle *manifest.Manifest, creds *api.AgentCredentials) (k3s.HelmChart, error) {
	version, err := bundle.Core.Version("kubenest-agent")
	if err != nil {
		return k3s.HelmChart{}, err
	}
	if creds == nil || creds.Operator.ChartRef == "" {
		return k3s.HelmChart{}, fmt.Errorf("the minted credentials carry no operator chart reference")
	}
	namespace := creds.Operator.Namespace
	if namespace == "" {
		return k3s.HelmChart{}, fmt.Errorf("the minted credentials carry no operator namespace")
	}
	// Refuse rather than render a credential the pinned chart cannot consume.
	// An error naming both versions is recoverable; a green install with no Git
	// credential is discovered later, by whoever wonders why nothing deploys.
	if creds.RepoCredential != nil {
		cmp, err := manifest.CompareVersions(version, minChartWithRepoCredential)
		if err != nil {
			return k3s.HelmChart{}, fmt.Errorf("cannot tell whether kubenest-agent %s carries the GitOps deploy key values: %w", version, err)
		}
		if cmp < 0 {
			return k3s.HelmChart{}, fmt.Errorf(
				"bundle pins kubenest-agent %s, which has no gitSSHPrivateKey value: the per-cluster GitOps deploy key would be accepted by helm and silently discarded, leaving the cluster with no Git credential at all. %s or later carries it. Install this cluster from a bundle that pins %s or later, or register it without a GitOps repository",
				version, minChartWithRepoCredential, minChartWithRepoCredential)
		}
	}
	values, err := Values(creds)
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
func Install(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, creds *api.AgentCredentials, rep converge.Reporter) error {
	chart, err := Chart(bundle, creds)
	if err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
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
