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
	"encoding/base64"
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
// management cluster, and is IDEMPOTENT AGAINST ITS OWN EARLIER WRITE.
//
// A RESUME RE-RUNS THE STAGE THAT CALLS IT. `recovery-control-plane` is
// AlwaysRun — the checkpoint stage needs the chart values it holds in memory,
// and a journal cannot carry them — so on a resume it applies its objects
// again. A first run's `kubectl create` refused the Secret the first run had
// written, and a recovery that stopped anywhere after it could never be
// resumed (found on hardware 2026-09-28).
//
// SO THE RULE IS: create when absent; ACCEPT when present with the content this
// recovery would write; REFUSE when present with different content, naming the
// key names that differ and never their values. The refusal covers both a
// foreign Secret found on a first run and a Secret that has drifted since — in
// either case this is not our object, and overwriting it would point a running
// control plane at another instance's database or silently change the keys that
// read its stored configuration.
func EnsureRecoverySecrets(ctx context.Context, r k3s.Runner, sec Secrets, ours bool) (Secrets, error) {
	present, err := readInstallSecret(ctx, r)
	if err != nil {
		return Secrets{}, err
	}
	if present != nil {
		return adoptOrRefuse(present, sec, ours)
	}
	nsDoc := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + Namespace + "\n")
	if res, err := r.RunInput(ctx, "sudo -n k3s kubectl apply -f -", bytes.NewReader(nsDoc)); err != nil {
		return Secrets{}, fmt.Errorf("creating namespace %s: %w", Namespace, err)
	} else if res.ExitCode != 0 {
		return Secrets{}, fmt.Errorf("creating namespace %s: exit %d: %s", Namespace, res.ExitCode, firstLineOf(res.Stderr))
	}
	doc, err := secretManifest(sec)
	if err != nil {
		return Secrets{}, err
	}
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl create -f -", bytes.NewReader(doc))
	if err != nil {
		return Secrets{}, fmt.Errorf("writing secret %s/%s: %w", Namespace, SecretName, err)
	}
	if res.ExitCode != 0 {
		// Losing a race with ourselves (a concurrent second recovery) lands
		// here: re-read and decide from what is there rather than reporting an
		// unknown failure.
		if again, readErr := readInstallSecret(ctx, r); readErr == nil && again != nil {
			return adoptOrRefuse(again, sec, ours)
		}
		return Secrets{}, fmt.Errorf("creating secret %s/%s: exit %d: %s", Namespace, SecretName, res.ExitCode, firstLineOf(res.Stderr))
	}
	return sec, nil
}

// kitDerived are the fields a recovery takes from the KIT, and they are the
// test of "is this Secret ours": the same kit must produce the same
// ENCRYPTION_KEY, the same AGENT_JWT_SECRET and the same authority. A Secret
// whose kit-derived fields differ belongs to another kit or another instance,
// and no rule may adopt it.
var kitDerived = []struct {
	name string
	get  func(Secrets) string
}{
	{keyEncryptionKey, func(s Secrets) string { return s.EncryptionKey }},
	{keyAgentJWTSecret, func(s Secrets) string { return s.AgentJWTSecret }},
	{keyGatewayCACertificate, func(s Secrets) string { return s.GatewayCACertificate }},
	{keyGatewayCAPrivateKey, func(s Secrets) string { return s.GatewayCAPrivateKey }},
}

// adoptOrRefuse decides what a PRESENT install Secret means.
//
// THE SECRET'S FIELDS ARE NOT ALL THE SAME KIND. The kit-derived four must
// match exactly — they are the identity the fleet pinned. The OTHER three are
// generated per run: the user signing key, the database password and the
// chart's bootstrap administrator password. They are generated freshly by every
// `RecoverySecrets` call, so a resume that compared them would refuse its own
// Secret for ever (found on hardware 2026-09-28, one step after the create-only
// write was fixed) — and, worse, writing new ones would be a DIFFERENT
// DATABASE: the Postgres pod has already initialised its data volume with the
// first run's password, and a new signing key would invalidate every session
// while a new database password would stop the backend authenticating at all.
//
// SO A RESUME ADOPTS THEM, AND `ours` IS THE OWNERSHIP TEST.
//
// It answers "does THIS OPERATION own the objects on this cluster", NOT "did
// this stage complete": a stage that failed in a later attempt clears its own
// completion, so a run that completed this stage, then failed it twice on its
// own bugs, would have been told its own Secret was somebody else's — which is
// what happened on hardware on 2026-09-28. The caller answers with the
// journal's record of having BUILT this host (the stage that installed k3s
// under this same journal): if this operation installed the machine's
// Kubernetes, every object inside that cluster came from it. A first run cannot
// reach this stage on a host that already runs Kubernetes, because preflight
// refuses existing Kubernetes unless this journal is the one that put it there.
//
// On a FIRST run a present Secret is somebody else's, whatever it contains, and
// is refused.
func adoptOrRefuse(present map[string]string, want Secrets, ours bool) (Secrets, error) {
	var differs []string
	for _, field := range kitDerived {
		got, ok := present[field.name]
		switch {
		case !ok:
			differs = append(differs, field.name+" (absent)")
		case got != field.get(want):
			differs = append(differs, field.name)
		}
	}
	if len(differs) > 0 {
		sort.Strings(differs)
		return Secrets{}, fmt.Errorf("secret %s/%s already exists on this host and is NOT the one this kit describes: %s differ (values are never printed). Those fields come from the recovery kit, so a Secret that disagrees with them belongs to another instance or another kit, and adopting it would point this recovery at a different control plane. Nothing was changed",
			Namespace, SecretName, strings.Join(differs, ", "))
	}
	if !ours {
		return Secrets{}, fmt.Errorf("secret %s/%s already exists on this host and this operation did not build this cluster: a recovery writes only its own key material onto a FRESH machine, and a control plane that is already running here keeps its own keys, its own databases and the sessions they sign. Nothing was changed. If this host really is the machine this recovery is for, run the identical command from the laptop whose journal records installing it",
			Namespace, SecretName)
	}
	adopted := want
	if value, ok := present[keyJWTSecret]; ok {
		adopted.JWTSecret = value
	}
	if value, ok := present[keyPostgresPassword]; ok {
		adopted.PostgresPassword = value
	}
	if value, ok := present[keyAdminPassword]; ok {
		adopted.AdminPassword = value
	}
	return adopted, nil
}

// readInstallSecret reads the control plane's install Secret, or nil when it is
// not there yet.
func readInstallSecret(ctx context.Context, r k3s.Runner) (map[string]string, error) {
	out, err := k3s.Kubectl(ctx, r, "get secret "+SecretName+" -n "+Namespace+" -o json")
	if err != nil {
		text := err.Error()
		if strings.Contains(text, "NotFound") || strings.Contains(text, "not found") {
			return nil, nil
		}
		return nil, fmt.Errorf("reading secret %s/%s: %w", Namespace, SecretName, err)
	}
	var doc struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("secret %s/%s is unparsable: %w", Namespace, SecretName, err)
	}
	values := make(map[string]string, len(doc.Data))
	for key, encoded := range doc.Data {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("secret %s/%s key %q is not base64: %w", Namespace, SecretName, key, err)
		}
		values[key] = string(raw)
	}
	return values, nil
}

// DatabaseHasControlPlaneData reports whether the database already holds a
// restored control plane, and says what it observed.
//
// IT IS WHAT MAKES THE CHECKPOINT STAGE RESUMABLE. `pg_restore
// --exit-on-error` into a database that already has the schema fails at its
// first table, so a resume that had loaded the checkpoint and then failed
// before starting the backend would never get past the load. The marker is the
// `organization` table: absent means a database nothing has ever been loaded
// into, which is the state the restore needs; present with rows means the load
// happened; present with no rows is neither, and is refused rather than guessed
// at.
// podPGPassword sets PGPASSWORD inside the database pod from the pod's own
// credential, so no command this CLI sends carries it in an argv. The chart's
// PostgreSQL mounts it as a file (POSTGRES_PASSWORD_FILE); POSTGRES_PASSWORD is
// read first for an image that sets the variable instead. Without it psql and
// pg_restore stop at "Password for user kubenest:" (S11 on lab demo,
// 2026-09-28). It is meant for a single-quoted `bash -c` in the pod.
const podPGPassword = `PGPASSWORD="${POSTGRES_PASSWORD:-$(cat "${POSTGRES_PASSWORD_FILE:-/dev/null}")}"`

func DatabaseHasControlPlaneData(ctx context.Context, r k3s.Runner) (bool, string, error) {
	pod, err := DatabasePod(ctx, r)
	if err != nil {
		return false, "", err
	}
	query := podPGPassword + " psql -U " + postgresUser + " -d " + postgresDatabase + " -tAc \"SELECT count(*) FROM organization\""
	res, err := r.RunInput(ctx, "sudo -n k3s kubectl exec -i -n "+Namespace+" "+pod+" -- bash -c '"+query+"'", strings.NewReader(""))
	if err != nil {
		return false, "", fmt.Errorf("asking the control plane's database whether it already holds a restored control plane: %w", err)
	}
	if res.ExitCode != 0 {
		text := res.Stderr + res.Stdout
		if strings.Contains(text, "does not exist") || strings.Contains(text, "relation") {
			return false, "the database has no control-plane schema yet", nil
		}
		return false, "", fmt.Errorf("asking the control plane's database whether it already holds a restored control plane: exit %d: %s", res.ExitCode, firstLineOf(text))
	}
	trimmed := strings.TrimSpace(res.Stdout)
	rows, err := strconv.Atoi(trimmed)
	if err != nil {
		return false, "", fmt.Errorf("the database's row count came back as %q, which is not a number", trimmed)
	}
	switch {
	case rows > 0:
		return true, fmt.Sprintf("the database already holds %d organisation(s)", rows), nil
	default:
		return false, "", fmt.Errorf("the database has the control-plane schema and no organisations in it, which is neither an empty database nor a restored one: a previous restore did not finish, and this recovery will not load a checkpoint over it. Look at the database before choosing how to continue")
	}
}

// HoldBackend returns the chart values with the backend held// HoldBackend returns the chart values with the backend held at zero replicas.
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

// DatabasePodState observes whether the database pod EXISTS and is READY, and
// says what it is doing when it is not.
//
// IT IS A CONVERGE PROBE, NOT A CHECK, because the chart goes on asynchronously:
// `recovery-control-plane` writes the HelmChart and returns, k3s's Helm
// controller installs it afterwards, and three seconds later the pod is there
// (found on hardware 2026-09-28 — the restore looked for it immediately and
// refused). The state it reports while waiting is the point: "Pending" with the
// scheduler's reason names a stuck claim or an unpullable image, where "not
// found yet" names nothing.
func DatabasePodState(ctx context.Context, r k3s.Runner) (bool, converge.State, error) {
	pod, err := DatabasePod(ctx, r)
	if err != nil {
		return false, converge.State{
			Object: "the database pod in " + Namespace,
			Status: "absent",
			Detail: "the chart's install Job has not created it yet (looked for " + postgresSelector() + ")",
		}, nil
	}
	ready, state, err := postgresReady(ctx, r)
	if err != nil {
		return false, converge.State{Object: "pod " + pod, Status: "not observed yet", Detail: err.Error()}, nil
	}
	state.Object = "pod " + pod
	if ready {
		return true, state, nil
	}
	if reason := podWaitingReason(ctx, r, pod); reason != "" {
		state.Detail = reason
	}
	return false, state, nil
}

// podWaitingReason reads why a pod that exists is not Ready: its phase, and the
// first container's waiting reason and message.
func podWaitingReason(ctx context.Context, r k3s.Runner, pod string) string {
	out, err := k3s.Kubectl(ctx, r, "get pod "+pod+" -n "+Namespace+" -o json")
	if err != nil {
		return ""
	}
	var doc struct {
		Status struct {
			Phase      string `json:"phase"`
			Reason     string `json:"reason"`
			Message    string `json:"message"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				State struct {
					Waiting *struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return ""
	}
	var parts []string
	if doc.Status.Phase != "" {
		parts = append(parts, doc.Status.Phase)
	}
	if doc.Status.Reason != "" || doc.Status.Message != "" {
		parts = append(parts, strings.TrimSpace(doc.Status.Reason+" "+doc.Status.Message))
	}
	for _, condition := range doc.Status.Conditions {
		if condition.Status == "False" {
			parts = append(parts, condition.Type+": "+strings.TrimSpace(condition.Reason+" "+condition.Message))
		}
	}
	for _, container := range doc.Status.ContainerStatuses {
		if container.State.Waiting != nil {
			parts = append(parts, strings.TrimSpace(container.State.Waiting.Reason+" "+container.State.Waiting.Message))
		}
	}
	return strings.Join(parts, "; ")
}

// postgresSelector is the label selector the chart's Postgres subchart sets.
func postgresSelector() string {
	return "app.kubernetes.io/instance=" + ReleaseName + ",app.kubernetes.io/name=postgresql"
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
	pod, err := DatabasePod(ctx, r)
	if err != nil {
		return err
	}
	command := "sudo -n k3s kubectl exec -i -n " + Namespace + " " + pod + " -- bash -c '" + podPGPassword + " exec pg_restore --no-owner --no-privileges --exit-on-error -U " + postgresUser + " -d " + postgresDatabase + "'"
	res, err := r.RunInput(ctx, command, bytes.NewReader(plaintext))
	if err != nil {
		return fmt.Errorf("loading the checkpoint at %s into %s: %w", checkpointKey, pod, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("pg_restore refused the checkpoint at %s (exit %d): %s. The database is now neither the checkpoint nor empty, so this is a stop-and-look, not a retry: the dump and the Postgres major are what to compare", checkpointKey, res.ExitCode, firstLineOf(res.Stderr))
	}
	return nil
}

// DatabasePod finds the database pod the recovery works inside, by the labels
// the chart's Postgres subchart sets, and falls back to the release's own name.
func DatabasePod(ctx context.Context, r k3s.Runner) (string, error) {
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
	if got := DigestOf(sealed); !SameDigest(got, cp.Envelope.SHA256) {
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

// SameDigest reports whether two sha256 digests name the same bytes, whether
// or not each carries the "sha256:" prefix. The chart's checkpoint Job records
// the bare hex sha256sum prints; recoverykit writes the prefixed form. On lab
// demo (2026-09-28) the recovery compared the two as strings and refused a
// checkpoint whose digests were equal.
func SameDigest(a, b string) bool {
	a, b = strings.TrimPrefix(a, "sha256:"), strings.TrimPrefix(b, "sha256:")
	return a != "" && strings.EqualFold(a, b)
}

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
