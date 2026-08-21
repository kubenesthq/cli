// Package agent is stage 10: the KubeNest agent — which IS the operator
// (decision G, 2026-08-20). The bundle pins the kubenest-operator-2 chart
// under the name kubenest-agent, and this package installs that chart with
// the identity minted in stage 2.
//
// CREDENTIAL HANDLING, which is the whole reason this package is small:
// the agent JWT reaches the cluster as chart values and nothing else. It is
// never an argument to a command (command lines are visible in the target
// host's process list), never a log line, and never journalled. The values
// file it lands in is chmod 600 on the server node, because the k3s
// auto-deploy directory is world-readable by default and a JWT sitting at
// 0644 on a customer's box is a finding.
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
//	              credential — the GitOps repo is the per-cluster one the
//	              broker issued, and an in-cluster Git server would be a
//	              second source of truth. Left at the chart default when no
//	              repo credential was issued, which is the chart's
//	              zero-external-dependency fallback.
//	argo-cd       left at the chart default: it is the operator's own
//	              deploy engine, not a platform component.
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
	}

	out, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

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
	values, err := Values(creds)
	if err != nil {
		return k3s.HelmChart{}, err
	}

	// The mint's chart_ref may already carry the version (oci://…:2.2.0).
	// The bundle pin is authoritative for the version, so strip a trailing
	// tag rather than letting two sources disagree silently.
	ref := stripTag(creds.Operator.ChartRef, version)

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

// stripTag removes a trailing :<version> from an OCI reference so the chart
// and its version are expressed once each.
func stripTag(ref, version string) string {
	if suffix := ":" + version; strings.HasSuffix(ref, suffix) {
		return strings.TrimSuffix(ref, suffix)
	}
	// A tag we did not expect is left alone and reported by the version
	// mismatch it will cause, rather than silently rewritten.
	return ref
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
