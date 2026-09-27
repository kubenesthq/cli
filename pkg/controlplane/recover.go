// Recovery of the KubeNest control plane (bead T4.8).
//
// The control plane's Postgres holds every organisation, member, role, window,
// alert route, inventory and token floor in the installation, and the
// management cluster it runs in may have exactly one host. This file is the
// control-plane half of rebuilding that host: it renders the chart values a
// recovery needs, holds the backend while the checkpoint is loaded, loads it,
// and decides whether the migration Job has to run.
//
// THE ORDERING IS THE WHOLE FILE. Probe P3 question 3 measured both arms on
// hardware: a backend started on an empty database BUILDS the schema from its
// models and stamps head, after which `pg_restore` fails at its first table
// (`relation "addon_definition" already exists`) and 0 rows are restored. So the
// backend is held at zero replicas until `pg_restore` has exited 0, and the
// migration Job runs only when the chart is newer than the checkpoint — after
// which the backend starts and serves.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/recoverykit"
)

// postgresUser and postgresDatabase are the chart's own values for the
// database the control plane dumps from and restores into (values.yaml,
// postgresql.auth). They are read from the chart rather than from the caller
// because a restore that names a different database would load a control plane
// into a database the backend never reads.
const (
	postgresUser     = "kubenest"
	postgresDatabase = "kubenest"
)

// splitCABundle separates the kit's CONTROL_PLANE_CA entry into the serving
// certificate and the private key that renews it.
//
// The kit carries them as ONE PEM stream because a certificate without its key
// cannot reissue the identity every CLI and agent pinned; the chart wants them
// as two values.
func splitCABundle(bundle string) (certificate, privateKey string, err error) {
	var certs, keys []string
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		text := string(pem.EncodeToMemory(block))
		switch {
		case strings.Contains(block.Type, "CERTIFICATE"):
			certs = append(certs, text)
		case strings.Contains(block.Type, "PRIVATE KEY"):
			keys = append(keys, text)
		}
	}
	if len(certs) == 0 {
		return "", "", errors.New("the kit's control-plane CA carries no certificate")
	}
	if len(keys) == 0 {
		return "", "", errors.New("the kit's control-plane CA carries no private key: without it the restored control plane cannot renew the certificate every CLI and agent already trusts, and their trust would have to be re-established by hand on every host")
	}
	return strings.Join(certs, ""), strings.Join(keys, ""), nil
}

// RecoverySecrets builds the control plane's Secret material for a recovery
// from the kit's key material.
//
// WHAT COMES FROM THE KIT AND WHAT IS NEW, and why:
//
//	ENCRYPTION_KEY     from the kit. It reads stored configuration; a new one
//	                   makes every stored credential unreadable.
//	AGENT_JWT_SECRET   from the kit. Agents that were not revoked must reconnect
//	                   by themselves after the recovery, and the hub verifies
//	                   their tokens with this key.
//	CONTROL_PLANE_CA   from the kit, split into certificate and key. It is what
//	                   the existing CLIs and agents already pin, so the restored
//	                   control plane keeps its identity instead of asking every
//	                   host in the fleet to learn a new authority.
//	JWTSecret          NEW. The user signing key never leaves the backend and
//	                   never travels in a kit, which is exactly what ends every
//	                   session issued before the recovery with no extra logic.
//	PostgresPassword   NEW. The database is a fresh one; the dump is taken with
//	                   --no-owner --no-privileges and carries no role passwords.
//	AdminPassword      NEW. The administrator accounts themselves are in the
//	                   restored database, so this is only the fresh cluster's
//	                   bootstrap value and is shown once.
func RecoverySecrets(kitSecrets map[string]string) (Secrets, error) {
	sec, err := generateSecrets()
	if err != nil {
		return Secrets{}, err
	}
	enc := kitSecrets[recoverykit.KeyEncryptionKey]
	jwt := kitSecrets[recoverykit.KeyAgentJWTSecret]
	ca := kitSecrets[recoverykit.KeyControlPlaneCA]
	switch {
	case enc == "":
		return Secrets{}, fmt.Errorf("the control-plane kit carries no %s: a control plane restored without it cannot read the configuration it stored, so every stored credential becomes unreadable", recoverykit.KeyEncryptionKey)
	case jwt == "":
		return Secrets{}, fmt.Errorf("the control-plane kit carries no %s: a control plane restored without it cannot verify the agent tokens that were never revoked, so no surviving cluster would reconnect", recoverykit.KeyAgentJWTSecret)
	case ca == "":
		return Secrets{}, fmt.Errorf("the control-plane kit carries no %s: a control plane restored without the authority its CLIs and agents already pin is a control plane nobody can talk to, and the check is live rather than assumed — an existing CLI refuses it with an unknown-authority error", recoverykit.KeyControlPlaneCA)
	}
	cert, key, err := splitCABundle(ca)
	if err != nil {
		return Secrets{}, err
	}
	sec.EncryptionKey = enc
	sec.AgentJWTSecret = jwt
	sec.GatewayCACertificate = cert
	sec.GatewayCAPrivateKey = key
	return sec, nil
}

// EnsureRecoverySecrets writes the recovery's Secret material to the fresh
// management cluster, and refuses to write over an install Secret that is
// already there.
//
// A PRESENT SECRET MEANS THIS IS NOT A FRESH HOST: some control plane's
// install Secret is on it, and overwriting that with a kit's material would
// point a running control plane at another instance's database — or at the same
// one twice. `kubectl create` (never `apply`) fails instead, and the refusal is
// the answer.
func EnsureRecoverySecrets(ctx context.Context, r k3s.Runner, sec Secrets) error {
	nsDoc := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + Namespace + "\n")
	if res, err := r.RunInput(ctx, "sudo -n k3s kubectl apply -f -", bytes.NewReader(nsDoc)); err != nil {
		return fmt.Errorf("creating namespace %s: %w", Namespace, err)
	} else if res.ExitCode != 0 {
		return fmt.Errorf("creating namespace %s: exit %d: %s", Namespace, res.ExitCode, firstLineOf(res.Stderr))
	}
	doc, err := secretManifest(sec)
	if err != nil {
		return err
	}
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl create -f -", bytes.NewReader(doc))
	if err != nil {
		return fmt.Errorf("writing secret %s/%s: %w", Namespace, SecretName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("secret %s/%s already exists on this host (exit %d: %s). A recovery installs onto a FRESH machine: a present install Secret belongs to some control plane already running here, and writing another instance's key material over it is not something this command will do", Namespace, SecretName, res.ExitCode, firstLineOf(res.Stderr))
	}
	return nil
}

// HoldBackend returns the chart values with the backend held at zero replicas.
//
// IT IS NOT A NICETY. The backend builds the schema from its models when the
// database is empty and stamps head; a restore that starts after that fails at
// its first table, and the recovery ends with a control plane that has no fleet
// in it (probe P3 question 3, arm B).
func HoldBackend(valuesYAML string) (string, error) {
	return FenceValues(valuesYAML, FenceOptions{Up: false, BackendReplicas: int32Ptr(0)})
}

// StartBackend returns the chart values with the backend running again.
func StartBackend(valuesYAML string, replicas int32) (string, error) {
	return FenceValues(valuesYAML, FenceOptions{Up: false, BackendReplicas: int32Ptr(replicas)})
}

// RestoreCheckpoint loads one checkpoint's dump into the control plane's
// database.
//
// THE DUMP IS DECRYPTED HERE, IN MEMORY, AND NEVER WRITTEN TO A HOST: it holds
// every member, every token floor and every stored credential, and a rebuilt
// host is not a place to leave a copy of it. It goes straight into `pg_restore`
// through the local SSH connection into the database pod.
//
// `--no-owner --no-privileges --exit-on-error` are the flags the probe used:
// the roles a restore would create are not in the dump, and a restore that
// continues past its first error is a database that is neither the old one nor
// an empty one.
func RestoreCheckpoint(ctx context.Context, r k3s.Runner, checkpointKey string, sealedDump []byte, fleetKey string, rep converge.Reporter) error {
	if checkpointKey == "" {
		return errors.New("a checkpoint restore needs the object key of the dump it loads")
	}
	if len(sealedDump) == 0 {
		return errors.New("the checkpoint object is empty, so there is nothing to load")
	}
	plaintext, err := recoverykit.Open(fleetKey, sealedDump)
	if err != nil {
		return fmt.Errorf("opening the checkpoint at %s with the fleet key supplied: %w. A checkpoint that does not open is not restored by retrying it: this key is not the one the checkpoint was sealed to, or it was mistyped", checkpointKey, err)
	}
	pod, err := postgresPod(ctx, r)
	if err != nil {
		return err
	}
	command := "sudo -n k3s kubectl exec -i -n " + Namespace + " " + pod + " -- pg_restore --no-owner --no-privileges --exit-on-error -U " + postgresUser + " -d " + postgresDatabase
	res, err := r.RunInput(ctx, command, bytes.NewReader(plaintext))
	if err != nil {
		return fmt.Errorf("loading the checkpoint at %s into %s: %w", checkpointKey, pod, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("pg_restore refused the checkpoint at %s (exit %d): %s. The database is now neither the checkpoint nor empty, so this is a stop-and-look, not a retry: the dump and the Postgres major are what to compare", checkpointKey, res.ExitCode, firstLineOf(res.Stderr))
	}
	return nil
}

// postgresPod finds the database pod the restore runs inside, by the labels the
// chart's Postgres subchart sets, and falls back to the release's own name.
func postgresPod(ctx context.Context, r k3s.Runner) (string, error) {
	selector := "app.kubernetes.io/instance=" + ReleaseName + ",app.kubernetes.io/name=postgresql"
	out, err := k3s.Kubectl(ctx, r, "get pods -n "+Namespace+" -l "+selector+" -o jsonpath={.items[0].metadata.name}")
	if err == nil {
		if name := strings.TrimSpace(out); name != "" {
			return name, nil
		}
	}
	fallback := ReleaseName + "-postgresql-0"
	if _, err := k3s.Kubectl(ctx, r, "get pod "+fallback+" -n "+Namespace+" -o name"); err == nil {
		return fallback, nil
	}
	return "", fmt.Errorf("the control plane's database pod is not on the cluster: looked for %s by its labels and for %s by name. A recovery restores into the chart's own Postgres, and there is no other database to load the checkpoint into", selector, fallback)
}

// MigrationNeeded reports whether the migration Job has to run after the
// checkpoint was loaded.
//
// ONLY WHEN THE CHART IS NEWER (T4.8, and the chart's own note on
// migration.enabled). The checkpoint's dump carries the schema of the control
// plane version that took it: a chart at the same version has nothing to do,
// and running the Job anyway would migrate a database that is already at its
// revision — which is how a recovery turns into an upgrade nobody asked for.
func MigrationNeeded(checkpoint ControlPlaneVersion, chart string) (bool, error) {
	if strings.TrimSpace(checkpoint.Version) == "" {
		return false, errors.New("the checkpoint records no control-plane version, so the CLI cannot tell whether the chart is newer than the schema in the dump. Migrating on a guess is how a recovery becomes an upgrade, and not migrating leaves code serving a schema it may not match")
	}
	if strings.TrimSpace(chart) == "" {
		return false, errors.New("this CLI cannot say which control-plane version its chart installs, so it cannot tell whether the chart is newer than the checkpoint")
	}
	return compareDotted(checkpoint.Version, chart) < 0, nil
}

// ControlPlaneVersion is the part of a checkpoint this decision needs.
type ControlPlaneVersion struct{ Version string }

// ChartVersionOf reports the control-plane version this CLI's bundled chart
// carries, in the form the checkpoint records.
func ChartVersionOf() (ControlPlaneVersion, error) {
	v, err := ChartVersion()
	if err != nil {
		return ControlPlaneVersion{}, err
	}
	return ControlPlaneVersion{Version: v}, nil
}

// MigrateIfNewer runs the migration Job only when the chart is newer than the
// checkpoint that was loaded, and reports the revision the control plane is at
// afterwards.
func MigrateIfNewer(ctx context.Context, r k3s.Runner, valuesYAML string, bundle *manifest.Manifest, checkpoint ControlPlaneVersion, rep converge.Reporter) (string, bool, error) {
	chart, err := ChartVersionOf()
	if err != nil {
		return "", false, err
	}
	needed, err := MigrationNeeded(checkpoint, chart.Version)
	if err != nil {
		return "", false, err
	}
	revision, err := Revision(valuesYAML)
	if err != nil {
		return "", false, err
	}
	if !needed {
		return revision, false, nil
	}
	revision, err = Migrate(ctx, r, valuesYAML, bundle, rep)
	if err != nil {
		return "", true, err
	}
	return revision, true, nil
}

func compareDotted(a, b string) int {
	parse := func(v string) []int {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		if i := strings.IndexAny(v, "-+"); i >= 0 {
			v = v[:i]
		}
		var out []int
		for _, part := range strings.Split(v, ".") {
			n := 0
			for _, r := range part {
				if r < '0' || r > '9' {
					break
				}
				n = n*10 + int(r-'0')
			}
			out = append(out, n)
		}
		return out
	}
	pa, pb := parse(a), parse(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// firstLineOf is the first line of a command's stderr, so an error names the
// cause rather than three lines of kubectl noise.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return strings.TrimSpace(s)
}

// manifestObject and ciphertextObject are the two objects one checkpoint is,
// under its own directory (backend/app/services/checkpoint_runner.py).
const (
	manifestObject   = "manifest.json"
	ciphertextObject = "control-plane.dump.age"
)

// BucketCheckpoint is one checkpoint as the bucket describes it: its manifest,
// and where its dump is.
//
// The recovery reads this and NOT the cluster's status ConfigMap, because the
// ConfigMap lived on the host that is gone. What makes a checkpoint eligible
// here is the same thing the runner guarantees when it publishes: the manifest
// is uploaded LAST, after the ciphertext has been read back from the store and
// compared, so a manifest at a key means that key's dump is whole.
type BucketCheckpoint struct {
	// Key is the checkpoint's directory in the bucket, from the manifest.
	Key string `json:"key"`
	// At is when the checkpoint was taken, from the manifest.
	At string `json:"at"`
	// ControlPlaneVersion is the control plane that took it.
	ControlPlaneVersion string `json:"control_plane_version"`
	// ManagementClusterID is the management cluster it came from, so a
	// checkpoint restored into a different cluster is visible rather than
	// silent.
	ManagementClusterID string `json:"management_cluster_id"`
	// IncludesSecurityChangeAt is the latest security change the checkpoint
	// contains. It is what the recovery reports so the operator can re-apply
	// what came after.
	IncludesSecurityChangeAt string `json:"includes_security_change_at"`
	// SecurityChangeAt is the latest security change known when it was taken.
	SecurityChangeAt string `json:"security_change_at"`
	// RotationSeconds is how long the checkpoint is kept.
	RetentionSeconds int64 `json:"retention_seconds"`
	// Dump describes the database the dump came from.
	Dump struct {
		PostgresMajor int    `json:"postgres_major"`
		PostgresImage string `json:"postgres_image"`
	} `json:"dump"`
	// Envelope describes the sealed dump.
	Envelope struct {
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"size_bytes"`
	} `json:"envelope"`

	// ManifestKey is where this manifest was read from, so every message can
	// name the object rather than describe it.
	ManifestKey string `json:"-"`
	// DumpKey is where the sealed dump is.
	DumpKey string `json:"-"`
}

// FindCheckpoints reads every checkpoint manifest under the control plane's
// prefix. A manifest that cannot be read or parsed fails the listing rather
// than being skipped: a recovery that silently dropped a newer checkpoint from
// the list would offer an older database as "latest".
func FindCheckpoints(ctx context.Context, list func(ctx context.Context, prefix string) ([]string, bool, error), get func(ctx context.Context, key string) ([]byte, error), prefix string) ([]*BucketCheckpoint, error) {
	root := strings.Trim(prefix, "/")
	if root != "" {
		root += "/"
	}
	keys, truncated, err := list(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("listing the control plane's checkpoints under %s: %w", root, err)
	}
	if truncated {
		return nil, fmt.Errorf("the checkpoint listing under %s is truncated: the newest checkpoint could be in the part that was not returned, and offering an older database as latest is a data-loss bug rather than a slow recovery", root)
	}
	var out []*BucketCheckpoint
	for _, key := range keys {
		if !strings.HasSuffix(key, "/"+manifestObject) {
			continue
		}
		raw, err := get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("reading the checkpoint manifest at %s: %w", key, err)
		}
		var cp BucketCheckpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			return nil, fmt.Errorf("the checkpoint manifest at %s is not readable JSON: %w", key, err)
		}
		cp.ManifestKey = key
		dir := strings.TrimSuffix(key, "/"+manifestObject)
		if cp.Key == "" {
			cp.Key = dir
		}
		cp.DumpKey = dir + "/" + ciphertextObject
		out = append(out, &cp)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out, nil
}

// Eligible reports whether this checkpoint could be restored.
//
// The three facts that matter: the manifest names a dump, the dump has a digest
// to check it against, and the checkpoint records the control-plane version and
// Postgres major a restore has to match. A checkpoint missing any of them is not
// offered as latest — it is not a refusal, it is simply not a candidate.
func (c *BucketCheckpoint) Eligible() bool {
	if c == nil || c.Key == "" || c.Envelope.SHA256 == "" || c.At == "" {
		return false
	}
	return c.ControlPlaneVersion != "" && c.Dump.PostgresMajor != 0
}

// NewestEligible returns the newest checkpoint that could be restored.
func NewestEligible(points []*BucketCheckpoint) (*BucketCheckpoint, bool) {
	for i := range points {
		if points[i].Eligible() {
			return points[i], true
		}
	}
	return nil, false
}

// SelectControlPlaneCheckpoint lists, verifies, and picks the newest eligible
// control-plane checkpoint, and returns the sealed dump alongside it.
//
// THE DIGEST IS CHECKED AGAINST THE OBJECT IN THE BUCKET, not against the
// manifest's own copy of it. The manifest was uploaded after the runner read
// the ciphertext back and compared it; a checkpoint whose object no longer
// matches its manifest is one the store changed under us, and loading it would
// be loading something nobody measured.
func SelectControlPlaneCheckpoint(ctx context.Context, list func(ctx context.Context, prefix string) ([]string, bool, error), get func(ctx context.Context, key string) ([]byte, error), prefix string) (*BucketCheckpoint, []byte, error) {
	points, err := FindCheckpoints(ctx, list, get, prefix)
	if err != nil {
		return nil, nil, err
	}
	if len(points) == 0 {
		return nil, nil, fmt.Errorf("there is no checkpoint object under %s/manifest.json: the management cluster's checkpoint CronJob uploads one per run, and a recovery with no checkpoint has nothing to rebuild the control plane from", prefix)
	}
	cp, ok := NewestEligible(points)
	if !ok {
		return nil, nil, fmt.Errorf("the %d checkpoint(s) under %s are all unusable: each one is missing the dump digest, the control-plane version or the Postgres major that a restore has to match. A checkpoint nobody can check is not one to restore from", len(points), prefix)
	}
	sealed, err := get(ctx, cp.DumpKey)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the dump at %s: %w", cp.DumpKey, err)
	}
	if got := DigestOf(sealed); got != cp.Envelope.SHA256 {
		return nil, nil, fmt.Errorf("the dump at %s digests %s and its manifest records %s: the object is not the one this checkpoint measured, so it is not the one to load", cp.DumpKey, got, cp.Envelope.SHA256)
	}
	if cp.Envelope.SizeBytes != 0 && int64(len(sealed)) != cp.Envelope.SizeBytes {
		return nil, nil, fmt.Errorf("the dump at %s is %d bytes and its manifest records %d", cp.DumpKey, len(sealed), cp.Envelope.SizeBytes)
	}
	return cp, sealed, nil
}

// DigestOf is the sha256 a checkpoint manifest records, in the same
// "sha256:…" form recoverykit uses, so a manifest and a kit digest read alike.
func DigestOf(doc []byte) string { return recoverykit.Digest(doc) }

// ChartPinnedPostgres is the PostgreSQL this CLI's bundled chart runs: its
// major, and the image the chart pins, so a refusal can name what a checkpoint
// would have had to come from.
//
// The major comes from the Postgres subchart's own appVersion, which is what
// the running StatefulSet is built from; the image is the chart's own pin,
// named in refusals rather than compared, because the checkpoint records the
// image the DUMP came from and two references to the same major are compared by
// major for the reason `CheckPostgresUnchanged` gives.
func ChartPinnedPostgres() (major int, image string, err error) {
	raw, err := subchartPostgresMajor()
	if err != nil {
		return 0, "", err
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, "", fmt.Errorf("the chart's PostgreSQL subchart declares major %q, which is not a number: the checkpoint comparison must not guess", raw)
	}
	body, err := ChartFile("values.yaml")
	if err != nil {
		return n, "the chart's pinned bitnami/postgresql", nil
	}
	var doc struct {
		Postgresql struct {
			Image struct {
				Repository string `yaml:"repository"`
				Tag        string `yaml:"tag"`
				Digest     string `yaml:"digest"`
			} `yaml:"image"`
		} `yaml:"postgresql"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return n, "the chart's pinned bitnami/postgresql", nil
	}
	ref := doc.Postgresql.Image.Repository
	switch {
	case doc.Postgresql.Image.Digest != "":
		ref += "@" + doc.Postgresql.Image.Digest
	case doc.Postgresql.Image.Tag != "":
		ref += ":" + doc.Postgresql.Image.Tag
	}
	if ref == "" {
		ref = "the chart's pinned bitnami/postgresql"
	}
	return n, ref, nil
}
