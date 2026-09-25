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
//     target credentials, and installs the cluster's own repository-password
//     Secret before the chart (EnsureRepositoryPassword) — Velero's kopia
//     encryption key, which must exist before the server can create a
//     repository. Per the backup-restore page the targetless state is a
//     legitimate, VISIBLE one — the cluster reports `backup: unconfigured`
//     (see Unconfigured) until a target is set, and the install never blocks
//     on one.
//   - Configure (target.go) later creates the credentials Secret, the
//     BackupStorageLocation and the default Schedule as plain resources.
//     Velero picks up per-location credentials through the API, so setting
//     or changing a target never restarts the deployment and never touches
//     the Helm release.
package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

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

// The repository password is Velero's own, not the backup target's: kopia
// encrypts every volume upload with it, and Velero reads it from this Secret
// through the Kubernetes API — not from a chart value, an env var or a mounted
// file. That is why nothing about it appears in Chart's values, and why
// installing the Secret before the server starts is the whole mechanism.
const (
	// RepositorySecretName is the Secret Velero reads its backup-repository
	// password from. It is Velero's own name (pkg/repository/keys), read
	// through the Kubernetes API — the server's Role already grants it.
	RepositorySecretName = "velero-repo-credentials"
	// RepositoryPasswordKey is the key inside it.
	RepositoryPasswordKey = "repository-password"
)

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

// GenerateRepositoryPassword draws one fresh password from crypto/rand. It is
// 32 hex characters: the value has to survive a Secret, a kubectl printout and
// a terminal, so it is drawn from [0-9a-f] alone. The vendored default it
// replaces is static-passw0rd, printed in Velero's own source.
func GenerateRepositoryPassword() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating the velero repository password: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// RepositoryPasswordSecret renders the Secret, in the velero namespace.
// stringData keeps the value out of the document as base64 noise and lets the
// API server do the encoding — the same reason controlplane's install Secret
// uses it.
func RepositoryPasswordSecret(password string) ([]byte, error) {
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      RepositorySecretName,
			"namespace": Namespace,
		},
		"type":       "Opaque",
		"stringData": map[string]any{RepositoryPasswordKey: password},
	})
}

// errNoRepositoryPassword reports that the repository-password Secret is not on
// the cluster yet. Every other error out of RepositoryPassword means the object
// WAS found and cannot be used as it stands.
var errNoRepositoryPassword = errors.New("the velero repository-password Secret is not on this cluster yet")

// RepositoryPassword reads the stored password. It is an error when the Secret
// is absent — the caller must not generate one over a live repository.
//
// A read failure is reported as absent rather than distinguished from one: the
// create that follows an absent answer fails on an object that already exists,
// so a transient read failure ends as a failed install, never as a repository
// re-encrypted under a password the running server does not hold.
func RepositoryPassword(ctx context.Context, r k3s.Runner) (string, error) {
	out, err := k3s.Kubectl(ctx, r, "get secret "+RepositorySecretName+" -n "+Namespace+" -o json")
	if err != nil {
		return "", fmt.Errorf("%w: %v", errNoRepositoryPassword, err)
	}
	var stored struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &stored); err != nil {
		return "", fmt.Errorf("secret %s/%s is unparsable: %w", Namespace, RepositorySecretName, err)
	}
	encoded, ok := stored.Data[RepositoryPasswordKey]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no %q key: restore it, or delete the Secret and accept that the existing repository becomes unreadable",
			Namespace, RepositorySecretName, RepositoryPasswordKey)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("secret %s/%s key %q is not base64: %w", Namespace, RepositorySecretName, RepositoryPasswordKey, err)
	}
	// An empty value is as unusable as a missing one — kopia would encrypt
	// under the empty password — and is what a Secret created by hand usually
	// looks like.
	if len(raw) == 0 {
		return "", fmt.Errorf("secret %s/%s has an empty %q key: restore it, or delete the Secret and accept that the existing repository becomes unreadable",
			Namespace, RepositorySecretName, RepositoryPasswordKey)
	}
	return string(raw), nil
}

// EnsureRepositoryPassword returns the cluster's repository password,
// generating and creating it on the first call and reading the stored value
// back on every later one. created reports whether this call generated it.
//
// Creation is `kubectl create` (never `apply`), and that is load-bearing: two
// concurrent installs both generating and one applying over the other would
// leave a repository encrypted with a password nobody holds. Create fails on
// the second writer instead. The document travels over STDIN, never in the
// command string: it is a credential.
//
// This must run before the Velero chart does. Velero's own
// EnsureCommonRepositoryKey creates this Secret holding static-passw0rd when it
// is absent, and a kopia repository created under that password keeps it for
// its lifetime — the password is a repository key, not a rotation candidate.
func EnsureRepositoryPassword(ctx context.Context, r k3s.Runner) (password string, created bool, err error) {
	stored, err := RepositoryPassword(ctx, r)
	switch {
	case err == nil:
		return stored, false, nil
	case !errors.Is(err, errNoRepositoryPassword):
		// The Secret is there and unusable — a missing, undecodable or empty
		// key. It is not regenerated: the repository already encrypted under
		// those bytes cannot be re-derived from a new password.
		return "", false, err
	}
	generated, err := GenerateRepositoryPassword()
	if err != nil {
		return "", false, err
	}
	doc, err := RepositoryPasswordSecret(generated)
	if err != nil {
		return "", false, fmt.Errorf("rendering secret %s/%s: %w", Namespace, RepositorySecretName, err)
	}
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl create -f -", bytes.NewReader(doc))
	if err != nil {
		return "", false, fmt.Errorf("creating secret %s/%s: %w", Namespace, RepositorySecretName, err)
	}
	if res.ExitCode != 0 {
		// An exit here is most often a concurrent install losing the race,
		// which is exactly the outcome create-not-apply exists to force.
		return "", false, fmt.Errorf("creating secret %s/%s: exit %d: %s (a cluster that already backs up keeps its repository password: remove the Secret only if you intend to abandon that repository)",
			Namespace, RepositorySecretName, res.ExitCode, firstLine(res.Stderr))
	}
	// Read back rather than return the generated value, so what the install
	// carries is what the cluster actually stores.
	stored, err = RepositoryPassword(ctx, r)
	if err != nil {
		return "", false, err
	}
	return stored, true, nil
}

// Install applies the chart and converges until every Velero pod (server and
// node-agent) is Ready. It does not require, create or wait for any backup
// target — an unconfigured target is visible, never blocking.
//
// It does install the cluster's own repository-password Secret first
// (EnsureRepositoryPassword). That is not a backup target and it is not
// optional: the server creates a kopia repository under Velero's vendored
// default password if the Secret is absent when it starts, and a repository
// keeps the password it was created with.
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
	// The repository password goes in BEFORE the chart does, which is the whole
	// point: the Secret must exist before the server pod can start, because a
	// server that finds it absent creates the repository under the vendored
	// default password instead. No chart value carries this and none needs to —
	// the server reads the Secret through the API with the Role the chart
	// already grants it.
	if _, _, err := EnsureRepositoryPassword(ctx, r); err != nil {
		return fmt.Errorf("installing the backup repository password: %w", err)
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
	// Single-quoted for the remote shell — jsonpath braces and globs are
	// shell syntax otherwise.
	out, err := k3s.Kubectl(ctx, r, "get backupstoragelocations -n "+Namespace+" -o jsonpath='{.items[*].metadata.name}'")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "", nil
}
