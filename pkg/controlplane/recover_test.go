package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/sshx"
)

// secretRunner answers kubectl the way a host does: a Secret that is not there
// yet, and one that is, with whatever the host holds.
type secretRunner struct {
	held   map[string]string
	ran    []string
	inputs [][]byte
}

func (r *secretRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	r.ran = append(r.ran, command)
	if strings.Contains(command, "get secret "+SecretName) {
		if r.held == nil {
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): secrets "` + SecretName + `" not found`}, nil
		}
		body, _ := json.Marshal(map[string]any{"data": base64Map(r.held)})
		return sshx.Result{Stdout: string(body)}, nil
	}
	return sshx.Result{}, nil
}

func (r *secretRunner) RunInput(_ context.Context, command string, in io.Reader) (sshx.Result, error) {
	r.ran = append(r.ran, command)
	body, _ := io.ReadAll(in)
	r.inputs = append(r.inputs, body)
	if strings.Contains(command, "kubectl create -f -") {
		// The host now holds what was created.
		r.held = parseSecretManifest(string(body))
		return sshx.Result{}, nil
	}
	return sshx.Result{}, nil
}

func base64Map(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	return out
}

// parseSecretManifest reads the stringData out of the manifest the stage writes,
// with a real YAML parser: the CA fields are multi-line PEM blocks, and a
// hand-rolled reader mangles them into "differing" values that never differed.
func parseSecretManifest(doc string) map[string]string {
	var manifest struct {
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal([]byte(doc), &manifest); err != nil {
		return map[string]string{}
	}
	return manifest.StringData
}

func recoveryTestSecrets() Secrets {
	return Secrets{
		JWTSecret:            "VALUE-user-signing-key",
		AgentJWTSecret:       "VALUE-agent-key",
		EncryptionKey:        "VALUE-encryption-key",
		PostgresPassword:     "VALUE-pg",
		AdminPassword:        "VALUE-admin",
		GatewayCACertificate: "-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n",
		GatewayCAPrivateKey:  "-----BEGIN EC PRIVATE KEY-----\nkey\n-----END EC PRIVATE KEY-----\n",
	}
}

// TestAStoppedRecoveryResumesPastItsOwnInstallSecret is the hardware defect of
// 2026-09-28: `recovery-control-plane` is AlwaysRun (the checkpoint stage needs
// the chart values it holds in memory), so a resume applies its objects again —
// and its first write was a `create` that refused the Secret the first attempt
// had written. A recovery that stopped anywhere after that stage could never be
// resumed.
func TestAStoppedRecoveryResumesPastItsOwnInstallSecret(t *testing.T) {
	runner := &secretRunner{}
	sec := recoveryTestSecrets()
	if _, err := EnsureRecoverySecrets(context.Background(), runner, sec, false); err != nil {
		t.Fatalf("the first run could not write its own Secret: %v", err)
	}
	if runner.held == nil {
		t.Fatal("the first run created nothing, so the test proves nothing about a resume")
	}
	// The resume runs with FRESH per-run values, which is what the server does
	// every time it renders them: only the kit-derived fields can be compared.
	fresh := recoveryTestSecrets()
	fresh.JWTSecret = "VALUE-a-second-user-signing-key"
	fresh.PostgresPassword = "VALUE-a-second-database-password"
	fresh.AdminPassword = "VALUE-a-second-administrator-password"
	before := len(runner.ran)
	adopted, err := EnsureRecoverySecrets(context.Background(), runner, fresh, true)
	if err != nil {
		t.Fatalf("the resume refused the Secret the first attempt wrote: %v", err)
	}
	// It ADOPTS what is on the host, which is what the running database and the
	// running backend already use.
	if adopted.PostgresPassword != sec.PostgresPassword || adopted.JWTSecret != sec.JWTSecret || adopted.AdminPassword != sec.AdminPassword {
		t.Fatalf("the resume adopted generated fields that are not the ones on the host: postgres %v, jwt %v, admin %v (compared by equality with the first run's)",
			adopted.PostgresPassword == sec.PostgresPassword, adopted.JWTSecret == sec.JWTSecret, adopted.AdminPassword == sec.AdminPassword)
	}
	if adopted.EncryptionKey != sec.EncryptionKey || adopted.AgentJWTSecret != sec.AgentJWTSecret {
		t.Fatal("the resume adopted a kit-derived field from the host instead of the kit")
	}
	// AND THE CHART VALUES CARRY THE ADOPTED ONES: a different value would be a
	// different content-addressed revision, a rolling update, and a backend
	// that cannot authenticate to the database it was initialised with.
	values, err := Values(Settings{Domain: "example.test", AdminEmail: "admin@example.test"}, adopted)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(values, sec.PostgresPassword) {
		t.Fatal("the chart values do not carry the database password the host already holds: a resume would roll the backend with a password its database does not accept")
	}
	if strings.Contains(values, fresh.PostgresPassword) {
		t.Fatal("the chart values carry a freshly generated database password instead of the adopted one")
	}
	// Nothing was rewritten BY THE RESUME: only the commands it issued are
	// inspected, because the first run's create is still in the record.
	for _, command := range runner.ran[before:] {
		if strings.Contains(command, "kubectl create") || strings.Contains(command, "kubectl apply") {
			t.Fatalf("the resume wrote the Secret again: %s", command)
		}
	}
}

// TestAKitDerivedFieldThatDiffersIsRefusedOnResume: the comparison that IS the
// "is this ours" test survives the split. A Secret whose ENCRYPTION_KEY is not
// this kit's belongs to another instance, and a resume must not adopt it.
func TestAKitDerivedFieldThatDiffersIsRefusedOnResume(t *testing.T) {
	runner := &secretRunner{}
	other := recoveryTestSecrets()
	other.EncryptionKey = "VALUE-another-instances-encryption-key"
	runner.held = base64Map(map[string]string{
		keyJWTSecret:            other.JWTSecret,
		keyAgentJWTSecret:       other.AgentJWTSecret,
		keyEncryptionKey:        other.EncryptionKey,
		keyPostgresPassword:     other.PostgresPassword,
		keyAdminPassword:        other.AdminPassword,
		keyGatewayCACertificate: other.GatewayCACertificate,
		keyGatewayCAPrivateKey:  other.GatewayCAPrivateKey,
	})
	_, err := EnsureRecoverySecrets(context.Background(), runner, recoveryTestSecrets(), true)
	if err == nil {
		t.Fatal("a resume adopted a Secret whose kit-derived key material is not this kit's")
	}
	if !strings.Contains(err.Error(), keyEncryptionKey) {
		t.Fatalf("the refusal does not name the differing key: %v", err)
	}
}

// TestAPresentSecretOnAFirstRunIsRefused: adopting is only for a resume. On a
// first run a present Secret is somebody else's.
func TestAPresentSecretOnAFirstRunIsRefused(t *testing.T) {
	runner := &secretRunner{}
	runner.held = base64Map(map[string]string{
		keyJWTSecret: "VALUE-x", keyAgentJWTSecret: recoveryTestSecrets().AgentJWTSecret,
		keyEncryptionKey:    recoveryTestSecrets().EncryptionKey,
		keyPostgresPassword: "VALUE-x", keyAdminPassword: "VALUE-x",
		keyGatewayCACertificate: recoveryTestSecrets().GatewayCACertificate,
		keyGatewayCAPrivateKey:  recoveryTestSecrets().GatewayCAPrivateKey,
	})
	if _, err := EnsureRecoverySecrets(context.Background(), runner, recoveryTestSecrets(), false); err == nil {
		t.Fatal("a first run adopted a Secret that was already on the host")
	}
}

// TestAForeignInstallSecretIsRefused: the fresh-machine guard, which must hold
// on a FIRST run as well as a resume. A present Secret with different key
// material is not our object.
func TestAForeignInstallSecretIsRefused(t *testing.T) {
	runner := &secretRunner{}
	foreign := recoveryTestSecrets()
	foreign.EncryptionKey = "VALUE-someone-elses-encryption-key"
	foreign.AgentJWTSecret = "VALUE-someone-elses-agent-key"
	runner.held = map[string]string{
		keyJWTSecret:            foreign.JWTSecret,
		keyAgentJWTSecret:       foreign.AgentJWTSecret,
		keyEncryptionKey:        foreign.EncryptionKey,
		keyPostgresPassword:     foreign.PostgresPassword,
		keyAdminPassword:        foreign.AdminPassword,
		keyGatewayCACertificate: foreign.GatewayCACertificate,
		keyGatewayCAPrivateKey:  foreign.GatewayCAPrivateKey,
	}
	_, err := EnsureRecoverySecrets(context.Background(), runner, recoveryTestSecrets(), true)
	if err == nil {
		t.Fatal("a Secret belonging to another control plane was accepted")
	}
	for _, want := range []string{keyEncryptionKey, keyAgentJWTSecret} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name the differing key %q, so the operator cannot tell what drifted: %v", want, err)
		}
	}
	// And it never prints a value: the refusal names keys, not key material.
	if strings.Contains(err.Error(), foreign.EncryptionKey) || strings.Contains(err.Error(), recoveryTestSecrets().EncryptionKey) {
		t.Fatalf("the refusal printed a secret value: %v", err)
	}
	for _, command := range runner.ran {
		if strings.Contains(command, "kubectl create") {
			t.Fatalf("a refused Secret was overwritten anyway: %s", command)
		}
	}
}

// TestTheCheckpointLoadIsSkippedWhenTheDatabaseAlreadyHasIt: `pg_restore
// --exit-on-error` into a database that already holds the schema fails at its
// first table, so a resume after a loaded-but-not-started checkpoint must not
// load it again.
func TestTheCheckpointLoadIsSkippedWhenTheDatabaseAlreadyHasIt(t *testing.T) {
	loaded := &psqlRunner{rows: "3\n"}
	has, detail, err := DatabaseHasControlPlaneData(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !has || !strings.Contains(detail, "3") {
		t.Fatalf("a database holding three organisations was reported as %v (%s)", has, detail)
	}

	// An empty database: the schema's absence is what says so, and it is not an
	// error.
	empty := &psqlRunner{failWith: `ERROR: relation "organization" does not exist`}
	has, detail, err = DatabaseHasControlPlaneData(context.Background(), empty)
	if err != nil {
		t.Fatalf("an empty database was reported as a failure: %v", err)
	}
	if has {
		t.Fatalf("an empty database was reported as already loaded (%s)", detail)
	}

	// A schema with no rows is neither state, and is refused rather than
	// guessed at.
	norows := &psqlRunner{rows: "0\n"}
	if _, _, err := DatabaseHasControlPlaneData(context.Background(), norows); err == nil {
		t.Fatal("a schema with no organisations was treated as an empty database, so a checkpoint would be loaded over it")
	}
}

// psqlRunner answers the pod lookup and the row-count query.
type psqlRunner struct {
	rows     string
	failWith string
}

func (r *psqlRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	if strings.Contains(command, "get pods") {
		return sshx.Result{Stdout: "kubenest-cp-postgresql-0"}, nil
	}
	return sshx.Result{}, nil
}

func (r *psqlRunner) RunInput(_ context.Context, command string, _ io.Reader) (sshx.Result, error) {
	if strings.Contains(command, "psql") {
		if r.failWith != "" {
			return sshx.Result{ExitCode: 1, Stderr: r.failWith}, nil
		}
		return sshx.Result{Stdout: r.rows}, nil
	}
	return sshx.Result{}, nil
}

// TestALoadThatNeverConnectedSaysNothingWasLoaded: on hardware on 2026-11-17
// pg_restore met a database pod between its image's set-up server and its real
// one, and the failure was reported as a half-loaded database
// (kn-ship-1-2-closeout-lzjb.12). A connection failure loaded nothing and must
// say so; any other pg_restore failure keeps the stop-and-look.
func TestALoadThatNeverConnectedSaysNothingWasLoaded(t *testing.T) {
	noSocket := &pgRestoreRunner{stderr: `pg_restore: error: connection to server on socket "/tmp/.s.PGSQL.5432" failed: No such file or directory` + "\n\tIs the server running locally and accepting connections on that socket?"}
	err := loadDump(context.Background(), noSocket, "control-plane/x/control-plane.dump.age", []byte("dump"))
	if !errors.Is(err, ErrDatabaseNotAccepting) {
		t.Fatalf("a pg_restore that could not connect was not reported as the database not accepting connections: %v", err)
	}
	if strings.Contains(err.Error(), "neither the checkpoint nor empty") {
		t.Fatalf("a load that never connected is described as a half-loaded database: %v", err)
	}

	// The emptiness question can meet the same gap, and must read the same way
	// rather than as a database that could not be asked at all.
	unreachable := &psqlRunner{failWith: `psql: error: connection to server on socket "/tmp/.s.PGSQL.5432" failed: No such file or directory`}
	if _, _, err := DatabaseHasControlPlaneData(context.Background(), unreachable); !errors.Is(err, ErrDatabaseNotAccepting) {
		t.Fatalf("a psql that could not connect was not reported as the database not accepting connections: %v", err)
	}

	startingUp := &pgRestoreRunner{stderr: `pg_restore: error: connection to server on socket "/tmp/.s.PGSQL.5432" failed: FATAL:  the database system is starting up`}
	if err := loadDump(context.Background(), startingUp, "k", []byte("dump")); !errors.Is(err, ErrDatabaseNotAccepting) {
		t.Fatalf("a server that is starting up was not reported as not accepting connections: %v", err)
	}

	// The planted negative: pg_restore connected and failed at a statement.
	// Part of the dump is in the database, and that must never read as safe
	// to load again.
	halfLoaded := &pgRestoreRunner{stderr: `pg_restore: error: could not execute query: ERROR:  relation "organization" already exists`}
	err = loadDump(context.Background(), halfLoaded, "k", []byte("dump"))
	if err == nil {
		t.Fatal("a pg_restore that failed at a statement was reported as a success")
	}
	if errors.Is(err, ErrDatabaseNotAccepting) {
		t.Fatalf("a pg_restore that connected and failed at a statement is reported as safe to load again: %v", err)
	}
	if !strings.Contains(err.Error(), "neither the checkpoint nor empty") {
		t.Fatalf("a half-loaded database lost its stop-and-look: %v", err)
	}

	if err := loadDump(context.Background(), &pgRestoreRunner{}, "k", []byte("dump")); err != nil {
		t.Fatalf("a pg_restore that exited 0 was reported as a failure: %v", err)
	}
}

// pgRestoreRunner answers the pod lookup and fails pg_restore with stderr.
type pgRestoreRunner struct{ stderr string }

func (r *pgRestoreRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	if strings.Contains(command, "get pods") {
		return sshx.Result{Stdout: "kubenest-cp-postgresql-0"}, nil
	}
	return sshx.Result{}, nil
}

func (r *pgRestoreRunner) RunInput(_ context.Context, command string, _ io.Reader) (sshx.Result, error) {
	if strings.Contains(command, "pg_restore") && r.stderr != "" {
		return sshx.Result{ExitCode: 1, Stderr: r.stderr}, nil
	}
	return sshx.Result{}, nil
}
