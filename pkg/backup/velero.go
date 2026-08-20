// Package backup installs Velero (stage: platform-backup) and configures
// per-cluster S3-compatible backup targets, per
// docs.kubenest.io/platform/backup-restore.
//
// Wave-1 scope is the INSTALL half of kn-mzn: Velero pinned in core, the
// default workload schedule and retention from the bundle manifest, and a
// working customer-supplied target. The scheduled verified restore drill and
// the k3s datastore snapshots are wave 3 and are not here.
//
// Install and target are deliberately separate layers:
//
//   - Install applies the pinned chart with NO storage location and NO
//     credentials. Per the backup-restore page this is a legitimate,
//     VISIBLE state — the cluster reports `backup: unconfigured` (see
//     Unconfigured) until a target is set, and the install never blocks
//     on one.
//   - Configure (target.go) later creates the credentials Secret, the
//     BackupStorageLocation and the default Schedule as plain resources.
//     Velero picks up per-location credentials through the API, so setting
//     or changing a target never restarts the deployment and never touches
//     the Helm release.
package backup

import (
	"context"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// Namespace is Velero's own namespace (upstream default). The target Secret
// and every Velero resource (locations, schedules, backups) live here.
const Namespace = "velero"

// chartRepo is where the velero CHART releases. The source repo moved to the
// velero-io org in 2026, but the chart did not follow — velero-io hosts no
// chart index, so "fixing" this URL to match the source move is a 404.
const chartRepo = "https://vmware-tanzu.github.io/helm-charts"

// pluginImage maps the manifest's object-store plugin pin to its image. The
// image org is velero/ on Docker Hub (unlike the source, it did not move to
// velero-io). Only the aws provider exists in the bundle today; it serves
// every S3-compatible target, not only AWS.
func pluginImage(p manifest.ObjectStorePlugin) (string, error) {
	if p.Provider != "aws" {
		return "", fmt.Errorf("backup.object-store-plugin.provider %q is not supported by this CLI build: only aws (velero-plugin-for-aws) exists in the bundle", p.Provider)
	}
	return "velero/velero-plugin-for-aws:" + p.Version, nil
}

// Chart renders the pinned HelmChart custom resource for Velero,
// unconfigured: no BackupStorageLocation, no VolumeSnapshotLocation, no
// credentials Secret. The node agent ships from day one with file-system
// backup as the volume default — core storage is Local PV LVM, whose CSI
// snapshots stay on the node's own disk, so kopia uploads are the only path
// that actually gets volume data into the bucket.
func Chart(bundle *manifest.Manifest) (k3s.HelmChart, error) {
	version, err := bundle.Core.Version("velero")
	if err != nil {
		return k3s.HelmChart{}, err
	}
	plugin, err := bundle.Backup.Plugin()
	if err != nil {
		return k3s.HelmChart{}, err
	}
	image, err := pluginImage(plugin)
	if err != nil {
		return k3s.HelmChart{}, err
	}
	values := fmt.Sprintf(`backupsEnabled: false
snapshotsEnabled: false
credentials:
  useSecret: false
deployNodeAgent: true
configuration:
  defaultVolumesToFsBackup: true
initContainers:
  - name: object-store-plugin
    image: %s
    imagePullPolicy: IfNotPresent
    volumeMounts:
      - mountPath: /target
        name: plugins
`, image)
	return k3s.HelmChart{
		Name:            "kubenest-velero",
		Repo:            chartRepo,
		Chart:           "velero",
		Version:         version,
		TargetNamespace: Namespace,
		ValuesYAML:      values,
	}, nil
}

// Install applies the chart and converges until every Velero pod (server and
// node-agent) is Ready. It does not require, create or wait for any backup
// target — an unconfigured target is visible, never blocking.
func Install(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	chart, err := Chart(bundle)
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
	if err := k3s.WriteManifest(ctx, r, chart.Name, doc); err != nil {
		return err
	}

	res, err := converge.Wait(ctx, k3s.PodsReadyProbe(r, Namespace), converge.Options{
		Name:     "velero-ready",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// Unconfigured reports whether the cluster has no backup target: Velero is
// installed but no BackupStorageLocation exists. This is the state the
// backup-restore page requires to be LOUD — every telemetry heartbeat
// carries `backup: unconfigured` (fleet wiring is kn-j5s, wave 3) — and it
// is not an error here for the same reason it does not block the install.
func Unconfigured(ctx context.Context, r k3s.Runner) (bool, error) {
	out, err := k3s.Kubectl(ctx, r, "get backupstoragelocations -n "+Namespace+" -o jsonpath={.items[*].metadata.name}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}
