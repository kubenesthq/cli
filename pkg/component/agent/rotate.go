package agent

import (
	"context"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
)

// ManifestPath is where the agent's HelmChart resource lives on a server node.
const ManifestPath = k3s.ManifestDir + "/" + manifestName + ".yaml"

// ReplaceJWTSecret rewrites the agent's HelmChart manifest so that
// kubenest.jwtSecret carries token, and changes NOTHING else.
//
// WHY A PATCH RATHER THAN A RE-RENDER, because the obvious implementation is
// wrong in a way that is invisible until a cluster loses something. Rendering
// values from scratch needs the repo credential and the operator's namespace,
// and a token rotation response carries neither: POST /clusters/{id}/rotate-token
// returns the new JWT alone. Fetching the rest means re-minting, and minting is
// not idempotent — every mint rotates the JWT again, so a rotate built that way
// invalidates the token it just delivered.
//
// So this reads what the cluster already has and replaces one leaf. An operator
// who set a value by hand keeps it, a gate-closed cluster stays gate-closed,
// and the GitOps deploy key survives. Same reasoning as kn-zod2's carrier
// preserving valuesContent rather than rewriting it.
//
// It REFUSES rather than guesses in every case where the file is not the shape
// this function was written for. A patch that quietly ADDS kubenest.jwtSecret
// to a values tree that never had one has written a key nobody reads, which is
// indistinguishable from success until the cluster fails to connect.
// It returns the patched document and the namespace the release is installed
// into, read from the manifest rather than assumed: the namespace comes from
// the minted credentials at install time and is not a constant, so the node's
// own copy is the only thing that knows it.
func ReplaceJWTSecret(manifest []byte, token string) ([]byte, string, error) {
	if token == "" {
		return nil, "", fmt.Errorf("refusing to write an empty agent JWT: the operator would authenticate as nobody")
	}

	var doc map[string]any
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		return nil, "", fmt.Errorf("parsing %s: %w", ManifestPath, err)
	}
	if kind, _ := doc["kind"].(string); kind != "HelmChart" {
		return nil, "", fmt.Errorf("%s is kind %q, not HelmChart: this is not the agent's manifest", ManifestPath, kind)
	}
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("%s has no spec", ManifestPath)
	}
	raw, ok := spec["valuesContent"].(string)
	if !ok || raw == "" {
		// Every install renders a jwtSecret, so a missing valuesContent means
		// this is not an agent manifest at all. Adding the key here would
		// write a value nothing reads.
		return nil, "", fmt.Errorf("%s carries no spec.valuesContent: this cluster was not installed with an agent JWT, so there is nothing to rotate", ManifestPath)
	}

	var values map[string]any
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		return nil, "", fmt.Errorf("parsing spec.valuesContent of %s: %w", ManifestPath, err)
	}
	kubenest, ok := values["kubenest"].(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("spec.valuesContent of %s has no kubenest section", ManifestPath)
	}
	if _, present := kubenest["jwtSecret"]; !present {
		return nil, "", fmt.Errorf("spec.valuesContent of %s sets no kubenest.jwtSecret: adding one would write a value this install does not read", ManifestPath)
	}
	kubenest["jwtSecret"] = token

	patched, err := yaml.Marshal(values)
	if err != nil {
		return nil, "", fmt.Errorf("rendering patched values: %w", err)
	}
	spec["valuesContent"] = string(patched)
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, "", fmt.Errorf("rendering patched %s: %w", ManifestPath, err)
	}
	namespace, _ := spec["targetNamespace"].(string)
	if namespace == "" {
		return nil, "", fmt.Errorf("%s sets no spec.targetNamespace, so there is no release to wait for", ManifestPath)
	}
	return out, namespace, nil
}

// MoveChart moves the agent to another chart version — and to another chart
// source when source is set — by rewriting the agent's manifest on the node.
//
// THE FILE, NOT THE OBJECT (kn-oymc). k3s's deploy controller owns the
// HelmChart this manifest declares and re-applies the file whenever k3s
// starts. A `kubectl patch` of the HelmChart moved the operator only until the
// upgrade's own Kubernetes stage restarted k3s: then the file put the previous
// chart back, and a 1.1 -> 1.2 upgrade reported success with the 1.1 operator
// running (lab w1, 2026-11-05). valuesContent carries the agent's credentials
// and is written back exactly as it was read.
func MoveChart(ctx context.Context, r k3s.Runner, version, source string) error {
	res, err := r.Run(ctx, "sudo -n cat "+ManifestPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", ManifestPath, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("reading %s: exit %d: the cluster has no agent manifest to move", ManifestPath, res.ExitCode)
	}
	moved, err := setChart([]byte(res.Stdout), version, source)
	if err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, manifestName, moved); err != nil {
		return err
	}
	return restrict(ctx, r, ManifestPath)
}

// setChart sets spec.version, and spec.chart when source is set, in the agent's
// manifest and leaves every other field as it was.
func setChart(manifest []byte, version, source string) ([]byte, error) {
	if version == "" {
		return nil, fmt.Errorf("refusing to move the agent chart to an empty version")
	}
	var doc map[string]any
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ManifestPath, err)
	}
	if kind, _ := doc["kind"].(string); kind != "HelmChart" {
		return nil, fmt.Errorf("%s is kind %q, not HelmChart: this is not the agent's manifest", ManifestPath, kind)
	}
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s has no spec", ManifestPath)
	}
	spec["version"] = version
	if source != "" {
		spec["chart"] = source
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", ManifestPath, err)
	}
	return out, nil
}

// DeliverJWT reads the agent's manifest off a server node, replaces the JWT in
// place, writes it back at 0600 and waits for the operator to become Available.
//
// It is deliberately the whole of step 2 and step 3 of a rotation: returning
// after the write would report success while the cluster is still disconnected,
// which is the outcome this command exists to prevent.
func DeliverJWT(ctx context.Context, r k3s.Runner, token string, deadline time.Duration, rep converge.Reporter) error {
	res, err := r.Run(ctx, "sudo -n cat "+ManifestPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", ManifestPath, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("reading %s: exit %d: the cluster has no agent manifest to rotate", ManifestPath, res.ExitCode)
	}
	patched, namespace, err := ReplaceJWTSecret([]byte(res.Stdout), token)
	if err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, manifestName, patched); err != nil {
		return err
	}
	if err := restrict(ctx, r, ManifestPath); err != nil {
		return err
	}
	result, err := converge.Wait(ctx,
		component.ConditionProbe(r, "deployment/"+DeploymentName, namespace, "Available"),
		converge.Options{Name: "kubenest-agent", Deadline: deadline, Reporter: rep})
	if err != nil {
		return err
	}
	return result.Err()
}
