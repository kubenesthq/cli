package controlplane

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/window"
)

// openTestJournal gives a session a journal under a temporary HOME, so a test
// run never writes to the operator's own.
func openTestJournal(t *testing.T, o UpgradeOptions) (*stages.Journal, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path, err := JournalPath(o.Cluster)
	if err != nil {
		return nil, err
	}
	return stages.OpenJournal(path, o.Identity())
}

// appliedValues is one chart apply the stages made, decoded.
type appliedValues struct {
	stage     string
	fence     bool
	migration bool
	replicas  any
}

// stageRunner is a FakeRunner that answers the reads the fence, checkpoint and
// migration stages make, and records the order of the things that change the
// cluster.
type stageRunner struct {
	*componenttest.FakeRunner
	applied    []appliedValues
	fenceUp    bool
	marker     map[string]any
	checkpoint bool
	values     string
	revision   string
}

func newStageRunner(t *testing.T, values, revision string) *stageRunner {
	t.Helper()
	s := &stageRunner{marker: checkpointMarker("cp/old.dump", 10), values: values, revision: revision}
	s.FakeRunner = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			stdin := s.lastInput(t)
			switch {
			case strings.Contains(command, fenceName+".yaml"):
				s.fenceUp = true
				return sshx.Result{}, nil
			case strings.Contains(command, ReleaseName+".yaml"):
				s.applied = append(s.applied, decodeApplied(t, stdin))
				return sshx.Result{}, nil
			}
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get httproute "):
			ref := backendService
			if s.fenceUp {
				ref = FenceService
			}
			return sshx.Result{Stdout: `{"spec":{"rules":[{"backendRefs":[{"name":"` + ref + `"}]}]}}`}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get service "+FenceService):
			if s.fenceUp {
				return sshx.Result{Stdout: FenceService}, nil
			}
			return sshx.Result{ExitCode: 1, Stderr: "not found"}, nil
		case strings.Contains(command, backendReplicasCmd):
			return sshx.Result{Stdout: "1"}, nil
		case strings.Contains(command, backendDeploymentImageCmd):
			return sshx.Result{Stdout: "ghcr.io/kubenesthq/kubenest-backend:132b7ea"}, nil
		case command == checkpointStatusCmd:
			return sshx.Result{Stdout: checkpointMarkerJSON(t, s.marker)}, nil
		case strings.HasPrefix(command, checkpointCreatePrefix):
			// The runner publishes eligibility as its last write, so the new
			// marker appears only after the Job was created.
			s.checkpoint = true
			s.marker = checkpointMarker("cp/new.dump", 20)
			return sshx.Result{Stdout: "job created"}, nil
		case command == migrationJobCmd:
			// BEFORE the checkpoint-Job read: the migration Job's read is the
			// same `kubectl get job` shape and would otherwise be answered with
			// a document stamped for a different purpose.
			return sshx.Result{Stdout: jobJSON(t, "Complete", s.revision)}, nil
		case strings.HasPrefix(command, checkpointJobGetPrefix):
			return sshx.Result{Stdout: checkpointJobJSON(t, "j", "Complete", "")}, nil
		case command == postgresCmd:
			return sshx.Result{Stdout: postgresReadyJSON()}, nil
		default:
			return sshx.Result{}, nil
		}
	}}
	return s
}

func (s *stageRunner) lastInput(t *testing.T) []byte {
	t.Helper()
	inputs := s.Inputs()
	if len(inputs) == 0 {
		t.Fatal("a manifest was written without streaming a document")
	}
	return inputs[len(inputs)-1]
}

// decodeApplied reads the HelmChart document the CLI wrote and pulls out the
// values it carries.
func decodeApplied(t *testing.T, manifest []byte) appliedValues {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		t.Fatalf("the applied manifest is not valid YAML: %v", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	raw, _ := spec["valuesContent"].(string)
	var values map[string]any
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatalf("the applied values are not valid YAML: %v", err)
	}
	out := appliedValues{}
	if fence, ok := values["fence"].(map[string]any); ok {
		out.fence, _ = fence["enabled"].(bool)
	}
	if migration, ok := values["migration"].(map[string]any); ok {
		out.migration, _ = migration["enabled"].(bool)
	}
	if backend, ok := values["backend"].(map[string]any); ok {
		out.replicas = backend["replicas"]
	}
	return out
}

func testUpgradeSession(t *testing.T, runner *stageRunner, values string) *UpgradeSession {
	t.Helper()
	bundle := installBundle(t)
	s := &UpgradeSession{
		ID: "run-1",
		Opts: UpgradeOptions{
			Cluster:  "prod-1",
			From:     "1.1",
			To:       "1.2",
			Values:   values,
			Bundle:   bundle,
			Server:   runner,
			Reporter: converge.ReporterFunc(func(converge.Event) {}),
		},
	}
	journal, err := openTestJournal(t, s.Opts)
	if err != nil {
		t.Fatal(err)
	}
	s.Jnl = journal
	return s
}

// THE ORDER IS THE ACCEPTANCE CRITERION. The fence goes up before anything
// changes, the checkpoint is ELIGIBLE before the migration Job is applied, and
// the backend is stopped only for the migration — the interval in which the old
// code must not serve a migrated schema.
func TestTheFenceGoesUpFirstAndTheCheckpointIsEligibleBeforeTheMigration(t *testing.T) {
	values, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"},
		Secrets{JWTSecret: "j", AgentJWTSecret: "a", EncryptionKey: "e", PostgresPassword: "p", AdminPassword: "d",
			GatewayCACertificate: "ca", GatewayCAPrivateKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	// The migration applies the FENCED values with the backend stopped, so the
	// revision it computes is a function of those and not of the bare document.
	stoppedValues, err := FenceValues(values, FenceOptions{Up: true, BackendReplicas: int32Ptr(0)})
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := MigrationValues(stoppedValues)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := Revision(migrated)
	if err != nil {
		t.Fatal(err)
	}

	runner := newStageRunner(t, values, revision)
	s := testUpgradeSession(t, runner, values)
	ctx := context.Background()

	if err := stageFence(ctx, s); err != nil {
		t.Fatal(err)
	}
	if s.FenceState != FenceUp {
		t.Fatalf("the fence stage left the fence %q", s.FenceState)
	}
	if err := stageCheckpoint(ctx, s); err != nil {
		t.Fatal(err)
	}
	if !runner.checkpoint {
		t.Fatal("the checkpoint stage did not create a checkpoint Job")
	}
	if err := stageMigration(ctx, s); err != nil {
		t.Fatal(err)
	}

	if len(runner.applied) < 3 {
		t.Fatalf("the stages applied %d chart(s), want the fence, the stopped backend and the migration: %+v", len(runner.applied), runner.applied)
	}
	first := runner.applied[0]
	if !first.fence || first.migration || first.replicas != nil {
		t.Errorf("the first apply was %+v, want the fence ENABLED with the backend left running: the checkpoint that follows needs the backend's own CronJob and the route must already be switched when anything changes", first)
	}
	// The migration apply is the one carrying migration.enabled, and the
	// backend is stopped for it.
	var stopped *appliedValues
	for i := range runner.applied {
		if runner.applied[i].migration {
			stopped = &runner.applied[i]
		}
	}
	if stopped == nil {
		t.Fatalf("no apply enabled the migration Job: %+v", runner.applied)
	}
	if stopped.replicas != 0 {
		t.Errorf("the migration applied backend.replicas = %v, want 0: the old code must not be running while the schema moves", stopped.replicas)
	}
	if !stopped.fence {
		t.Error("the migration apply dropped the fence, so a customer request could reach a backend mid-migration")
	}
	if s.Record.MigratedRevision != revision {
		t.Errorf("the record says migration revision %q, want %q", s.Record.MigratedRevision, revision)
	}
	// The marker names the checkpoint THIS run published, not the one that was
	// eligible before it: a resume that could not tell them apart would report
	// an older checkpoint as its own recovery point.
	if !strings.Contains(s.Record.CheckpointMarker, "cp/new.dump") {
		t.Errorf("the record's checkpoint marker is %q, want the checkpoint this run published (cp/new.dump)", s.Record.CheckpointMarker)
	}
}

// A run whose window has closed pauses rather than proceeding, and the paused
// stage's exit is what the operator is told.
func TestTheGatesRunBeforeAnythingIsChanged(t *testing.T) {
	values := "domain: kn.example.com\n"
	runner := newStageRunner(t, values, "rev1")
	s := testUpgradeSession(t, runner, values)
	s.Opts.Gates = []Gate{
		{Name: "Control-plane compatibility", Passed: true, Detail: "era 3"},
		{Name: "Maintenance window", Passed: false, Detail: "outside the window", Fix: window.OutsideFix},
	}
	s.Opts.Window = nil

	err := stageGates(context.Background(), s)
	if err == nil {
		t.Fatal("a failed gate did not stop the upgrade")
	}
	if !strings.Contains(err.Error(), "Maintenance window") {
		t.Errorf("the refusal does not name the failed gate: %v", err)
	}
	if len(runner.applied) != 0 {
		t.Errorf("a chart was applied before the gates passed: %+v", runner.applied)
	}
}
