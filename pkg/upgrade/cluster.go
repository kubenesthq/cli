package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/k3s"
)

// The cluster's own record is the authority on what it IS: which bundle,
// which profiles, which tier. An upgrade reads it rather than being told,
// because an upgrade that took its starting point from a command-line
// argument could move a cluster it had misidentified.

// Recorded is the cluster's bundle record, read once at the start of a run.
type Recorded struct {
	api.ClusterBundle
}

// LoadRecord reads the cluster's recorded bundle from the control plane.
func LoadRecord(ctx context.Context, client *api.Client, clusterID string) (Recorded, error) {
	if client == nil {
		return Recorded{}, fmt.Errorf("no control plane configured: run `kubenest login` first")
	}
	record, err := client.BundleRecord(ctx, clusterID)
	if err != nil {
		return Recorded{}, fmt.Errorf("reading what this cluster has installed: %w", err)
	}
	if record.BundleVersion == "" {
		return Recorded{}, fmt.Errorf("this cluster has no recorded bundle version, so there is nothing to upgrade FROM. A cluster installed by this CLI records one at its record stage")
	}
	return Recorded{ClusterBundle: record}, nil
}

// installedProfiles is the profile set the cluster has, which does not change
// during an upgrade.
func (s *Session) installedProfiles() []string { return s.Cluster.Profiles }

// haTier is the cluster's permanent tier.
func (s *Session) haTier() string { return s.Cluster.HATier }

// volumeGroupOwnership is carried through unchanged: an upgrade never touches
// block devices, so who owns the volume group is exactly what it was.
func (s *Session) volumeGroupOwnership() string { return s.Cluster.VolumeGroupOwnership }

// drill reads the cluster's last restore-drill evidence.
//
// The SOURCE is kn-f9lm's to produce and this is only its consumer, so it
// goes through the session's DrillSource — what matters here is the policy,
// which is the same whichever way the evidence is read. When no source is
// wired the gate still runs and still refuses, because no evidence is
// refused rather than passed.
func (s *Session) drill(ctx context.Context) (DrillStatus, error) {
	if s.Drills == nil {
		return DrillStatus{}, fmt.Errorf("no restore-drill evidence is available to this CLI")
	}
	return s.Drills.LastRestoreDrill(ctx)
}

// InClusterDrills reads the drill result from the cluster itself — the same
// object the agent reads to build its heartbeat, so the CLI and the control
// plane are looking at one source of truth rather than two representations
// of it.
type InClusterDrills struct {
	Runner    k3s.Runner
	Namespace string
	Name      string
}

// LastRestoreDrill reads the recorded result. An absent object is never_run,
// which the gate refuses — a cluster that has never drilled has no evidence
// that its restore works.
func (d InClusterDrills) LastRestoreDrill(ctx context.Context) (DrillStatus, error) {
	namespace, name := d.Namespace, d.Name
	if namespace == "" {
		namespace = "velero"
	}
	if name == "" {
		name = "kubenest-restore-drill-result"
	}
	out, err := k3s.Kubectl(ctx, d.Runner,
		fmt.Sprintf("get configmap %s -n %s -o jsonpath='{.data.result\\.json}' --ignore-not-found", name, namespace))
	if err != nil {
		return DrillStatus{}, err
	}
	body := strings.Trim(strings.TrimSpace(out), "'")
	if body == "" {
		return DrillStatus{Status: "never_run"}, nil
	}
	var result DrillStatus
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		return DrillStatus{}, fmt.Errorf("the recorded restore-drill result could not be read: %w", err)
	}
	if result.Status == "" {
		return DrillStatus{}, fmt.Errorf("the recorded restore-drill result carries no status")
	}
	return result, nil
}
