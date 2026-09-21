package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/install"
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

// RecordStore is where a cluster's bundle record lives: the control plane for
// a registered cluster, the cluster's own ConfigMap for a standalone one.
//
// It is one seam with two implementations rather than a nil-check at each
// call site, because the record is READ at the start of a run and WRITTEN at
// the end of it. A run that could read one place and write another would
// leave the two disagreeing about what is installed, and every later day-2
// operation trusts that answer.
type RecordStore interface {
	Load(ctx context.Context) (Recorded, error)
	Save(ctx context.Context, record api.BundleRecord) error
}

// ControlPlaneRecords is the record of a cluster that registered with a
// control plane.
type ControlPlaneRecords struct {
	Client    *api.Client
	ClusterID string
}

func (r ControlPlaneRecords) Load(ctx context.Context) (Recorded, error) {
	if r.Client == nil {
		return Recorded{}, fmt.Errorf("no control plane configured: run `kubenest login` first")
	}
	record, err := r.Client.BundleRecord(ctx, r.ClusterID)
	if err != nil {
		return Recorded{}, fmt.Errorf("reading what this cluster has installed: %w", err)
	}
	if record.BundleVersion == "" {
		return Recorded{}, fmt.Errorf("this cluster has no recorded bundle version, so there is nothing to upgrade FROM. A cluster installed by this CLI records one at its record stage")
	}
	return Recorded{ClusterBundle: record}, nil
}

func (r ControlPlaneRecords) Save(ctx context.Context, record api.BundleRecord) error {
	if r.Client == nil || r.ClusterID == "" {
		return fmt.Errorf("no registered cluster to record against")
	}
	return r.Client.PutBundleRecord(ctx, r.ClusterID, record)
}

// ClusterRecords is the record of a standalone cluster, which lives on the
// cluster itself because in that mode there is nowhere else (kn-y3gt).
//
// The install journal is deliberately not carried here. On the control plane
// the journal is how an operator reads an install they did not run; on a
// standalone cluster it stays on the machine that ran it, and what the
// cluster records is what an upgrade needs in order to know its own
// starting point.
type ClusterRecords struct {
	Runner k3s.Runner
}

func (r ClusterRecords) Load(ctx context.Context) (Recorded, error) {
	record, err := install.ReadClusterRecord(ctx, r.Runner)
	if err != nil {
		return Recorded{}, err
	}
	return Recorded{ClusterBundle: api.ClusterBundle{
		BundleVersion:        record.BundleVersion,
		Profiles:             record.Profiles,
		HATier:               record.HATier,
		VolumeGroupOwnership: record.VolumeGroupOwnership,
	}}, nil
}

// Save moves the recorded bundle forward while preserving the fields an
// upgrade has no business changing: the cluster's identity, its name, and
// that it is standalone. It re-reads rather than reconstructing them, so a
// field added to the record later is carried through instead of erased.
func (r ClusterRecords) Save(ctx context.Context, record api.BundleRecord) error {
	current, err := install.ReadClusterRecord(ctx, r.Runner)
	if err != nil {
		return err
	}
	current.BundleVersion = record.BundleVersion
	current.Profiles = record.Profiles
	current.HATier = record.HATier
	current.VolumeGroupOwnership = record.VolumeGroupOwnership
	return install.WriteClusterRecord(ctx, r.Runner, current)
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
	// The object and its key come from the package that WRITES them
	// (pkg/backup, kn-f9lm) rather than from strings repeated here. A
	// consumer that carries its own copy of a producer's names is a
	// consumer that will one day read an object nobody writes any more and
	// report "no drill has ever run" about a cluster that drills weekly.
	namespace, name := d.Namespace, d.Name
	if namespace == "" {
		namespace = backup.Namespace
	}
	if name == "" {
		name = backup.DrillResultName
	}
	out, err := k3s.Kubectl(ctx, d.Runner, fmt.Sprintf(
		"get configmap %s -n %s -o jsonpath='{.data.%s}' --ignore-not-found",
		name, namespace, strings.ReplaceAll(backup.DrillResultDataKey, ".", `\.`)))
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
