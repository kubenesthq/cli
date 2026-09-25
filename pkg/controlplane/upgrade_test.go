package controlplane

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
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
	// The install revision the values carry, which is the revision every
	// workload is expected to be running afterwards.
	installRevision string
	// backend.image as the apply writes it. Empty fields mean the values
	// mention nothing there, so the CHART's own pin is what renders.
	imageRepository string
	imageTag        string
	imageDigest     string
	// raw is the applied values document verbatim, for the assertions about the
	// parts NOT decoded above — the migration Job's pod-template inputs.
	raw string
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
	// chartRevision is the revision the last chart apply carried. It is what
	// the cluster reports back: the migration Job the apply rendered, and every
	// workload the readiness probe reads.
	chartRevision string
	// runningImage is what the backend Deployment reports it runs.
	runningImage string
	// staleMigrationJob makes the FIRST migration-Job read report a Job
	// stamped for a previous revision: the state a cluster whose last
	// migration was an earlier release's is in, and the state that has to be
	// deleted before the Job can be created at this run's revision.
	staleMigrationJob bool
	// jobReads counts the migration-Job reads, so the fake can report the
	// previous release's Job first and this run's afterwards.
	jobReads int
	// deletedMigrationJob records that the release's Job was deleted, and
	// deletedAfter how many chart applies had happened when it was: what
	// matters is that the delete came BEFORE the apply that renders the Job.
	deletedMigrationJob      bool
	deletedMigrationJobAfter int
}

func newStageRunner(t *testing.T, values string) *stageRunner {
	t.Helper()
	s := &stageRunner{marker: checkpointMarker("cp/old.dump", 10), values: values,
		runningImage: runningBackendRepository + ":" + recordedPinTag}
	s.FakeRunner = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get helmchart ") && strings.Contains(command, "chartContent"):
			// The release the upgrade replaces, captured by the fence stage
			// before its first apply (kn-t70...4xso.2).
			return sshx.Result{Stdout: base64.StdEncoding.EncodeToString([]byte("previous-archive"))}, nil
		case strings.Contains(command, "get helmchart ") && strings.Contains(command, "valuesContent"):
			return sshx.Result{Stdout: "domain: kn.example.com\n"}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl apply -f -"):
			// The previous release's Secret.
			return sshx.Result{Stdout: "secret/configured"}, nil

		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			stdin := s.lastInput(t)
			switch {
			case strings.Contains(command, fenceName+".yaml"):
				s.fenceUp = true
				return sshx.Result{}, nil
			case strings.Contains(command, ReleaseName+".yaml"):
				applied := decodeApplied(t, stdin)
				s.applied = append(s.applied, applied)
				s.chartRevision = applied.installRevision
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
		case strings.Contains(command, "get deployment/"+fenceName):
			// THE FENCE STAGE WAITS FOR THE FENCE TO SERVE, because the route
			// switch is only real once helm-controller has re-rendered it and a
			// fence whose pod never starts is one the data plane answers for
			// incidentally. A stage now observes it, so the fake must answer.
			if !s.fenceUp {
				return sshx.Result{ExitCode: 1, Stderr: "not found"}, nil
			}
			return sshx.Result{Stdout: `{"metadata":{"generation":1},"spec":{"replicas":1},"status":{"observedGeneration":1,"availableReplicas":1}}`}, nil
		case strings.Contains(command, backendReplicasCmd):
			return sshx.Result{Stdout: "1"}, nil
		case strings.Contains(command, releaseObjectsCmd):
			// The field-ownership gate: no object has a field two managers own.
			return sshx.Result{Stdout: `{"items":[{"kind":"Deployment","metadata":{"name":"` + ReleaseName + `-backend","managedFields":[{"manager":"helm","fieldsV1":{"f:spec":{"f:replicas":{}}}}]}}]}`}, nil
		case strings.Contains(command, postgresStatefulSetImageCmd):
			// The chart's own PostgreSQL: the same distribution and major, so
			// the pin gate passes and the arm this test is about is reached.
			return sshx.Result{Stdout: "docker.io/bitnami/postgresql:18.3.0-debian-12-r0"}, nil
		case strings.Contains(command, backendDeploymentImageCmd):
			return sshx.Result{Stdout: s.runningImage}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl delete job "+MigrationJobName):
			s.deletedMigrationJob = true
			s.deletedMigrationJobAfter = len(s.applied)
			return sshx.Result{Stdout: "job deleted"}, nil
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
			s.jobReads++
			if s.staleMigrationJob && s.jobReads == 1 {
				return sshx.Result{Stdout: jobJSON(t, "Complete", "previous-release-revision")}, nil
			}
			// The Job the cluster has is the one the last apply rendered: its
			// chart stamps the install revision it was applied at.
			return sshx.Result{Stdout: jobJSON(t, "Complete", s.chartRevision)}, nil
		case strings.HasPrefix(command, checkpointJobGetPrefix):
			return sshx.Result{Stdout: checkpointJobJSON(t, "j", "Complete", "")}, nil
		case command == postgresCmd:
			return sshx.Result{Stdout: postgresReadyJSON()}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl get deployment/"+ReleaseName+"-"):
			// The readiness probe: every workload runs the revision the chart
			// stage applied.
			return sshx.Result{Stdout: deploymentJSON(s.chartRevision, 2, 2, 1, 1, 1)}, nil
		case strings.HasPrefix(command, conditionProbeCmd("certificate/"+tlsCertificate)):
			return conditionJSON("Ready", "True"), nil
		case strings.HasPrefix(command, conditionProbeCmd("gateway/"+gatewayName)):
			return conditionJSON("Programmed", "True"), nil
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
	out := appliedValues{raw: raw}
	out.installRevision, _ = values["installRevision"].(string)
	if fence, ok := values["fence"].(map[string]any); ok {
		out.fence, _ = fence["enabled"].(bool)
	}
	if migration, ok := values["migration"].(map[string]any); ok {
		out.migration, _ = migration["enabled"].(bool)
	}
	if backend, ok := values["backend"].(map[string]any); ok {
		out.replicas = backend["replicas"]
		if image, ok := backend["image"].(map[string]any); ok {
			out.imageRepository, _ = image["repository"].(string)
			out.imageTag, _ = image["tag"].(string)
			out.imageDigest, _ = image["digest"].(string)
		}
	}
	return out
}

func testUpgradeSession(t *testing.T, runner k3s.Runner, values string) *UpgradeSession {
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
	s.PollInterval = s.Opts.PollInterval
	s.WaitDeadline = s.Opts.WaitDeadline
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

	runner := newStageRunner(t, values)
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
	runner := newStageRunner(t, values)
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

// The image the backend Deployment in these tests runs, and the pin the
// previous release recorded in the values the chart was applied with. They
// differ from each other AND from the pin the chart this binary carries
// (values.yaml: tag 2a7dc07, digest sha256:9ebe7efc...), which is what makes an
// apply that reproduces either one visible.
const (
	runningBackendRepository = "ghcr.io/kubenesthq/kubenest-backend"
	runningBackendTag        = "675ff0e"
	runningBackendDigest     = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	recordedPinTag           = "132b7ea"
	recordedPinDigest        = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// upgradeTestValues is the values document a control plane is RUNNING with when
// the upgrade reads it: what Values wrote for an install, the migration the last
// install or upgrade ran (still enabled, because nothing turns it off), and —
// when image is not nil — the backend image pin that release recorded.
func upgradeTestValues(t *testing.T, image map[string]any) string {
	t.Helper()
	values, err := Values(Settings{Domain: "kn.example.com", AdminEmail: "admin@kn.example.com"},
		Secrets{JWTSecret: "j", AgentJWTSecret: "a", EncryptionKey: "e", PostgresPassword: "p", AdminPassword: "d",
			GatewayCACertificate: "ca", GatewayCAPrivateKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
		t.Fatal(err)
	}
	doc["migration"] = map[string]any{"enabled": true}
	backend, _ := doc["backend"].(map[string]any)
	if backend == nil {
		backend = map[string]any{}
	}
	if image != nil {
		backend["image"] = image
	}
	doc["backend"] = backend
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The fence stage's apply is the FIRST change of the whole procedure and it runs
// BEFORE the checkpoint and before the migration. The chart this binary carries
// pins a NEW backend image, so an apply that does not hold the backend to the
// image it runs now rolls the backend Deployment AND the checkpoint CronJob onto
// code that has never been migrated — and the checkpoint that follows would be
// taken by that code, over a schema it may already have moved.
//
// Its values must also turn the chart's migration Job OFF explicitly: the values
// the run starts from carry migration.enabled, and an apply that reproduced them
// would re-render the release's Job with the new pod template. Job.spec.template
// is immutable, so helm would try to PATCH it and the upgrade would fail
// outright, leaving the release failed under failurePolicy: abort (hardware,
// 2026-09-25).
func TestTheFenceApplyKeepsTheRunningBackendImageAndTurnsTheMigrationJobOff(t *testing.T) {
	recordedPin := map[string]any{
		"repository": runningBackendRepository,
		"tag":        recordedPinTag,
		"digest":     recordedPinDigest,
	}
	cases := []struct {
		name       string
		running    string
		wantTag    string
		wantDigest string
	}{
		{
			name:       "the backend runs a digest-pinned build",
			running:    runningBackendRepository + "@" + runningBackendDigest,
			wantDigest: runningBackendDigest,
		},
		{
			name:    "the backend runs a build named by tag",
			running: runningBackendRepository + ":" + runningBackendTag,
			wantTag: runningBackendTag,
		},
		{
			name:       "the backend runs a build named by tag and digest",
			running:    runningBackendRepository + ":" + runningBackendTag + "@" + runningBackendDigest,
			wantTag:    runningBackendTag,
			wantDigest: runningBackendDigest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := upgradeTestValues(t, recordedPin)
			runner := newStageRunner(t, values)
			runner.runningImage = tc.running
			s := testUpgradeSession(t, runner, values)

			if err := stageFence(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if len(runner.applied) != 1 {
				t.Fatalf("the fence stage applied %d chart(s), want the one that raises the fence: %+v", len(runner.applied), runner.applied)
			}
			got := runner.applied[0]
			if !got.fence {
				t.Error("the fence apply did not switch the API route to the fence")
			}
			if got.migration {
				t.Error("the fence apply left migration.enabled true, so helm would re-render the release's migration Job with a new pod template; that template is immutable and the upgrade would fail")
			}
			if got.imageRepository != runningBackendRepository {
				t.Errorf("the fence apply pins backend.image.repository = %q, want %q: whatever this apply leaves the backend and its checkpoint CronJob on is what takes the checkpoint", got.imageRepository, runningBackendRepository)
			}
			if got.imageDigest != tc.wantDigest {
				t.Errorf("the fence apply pins backend.image.digest = %q, want %q (the running reference %q)", got.imageDigest, tc.wantDigest, tc.running)
			}
			if got.imageTag != tc.wantTag {
				t.Errorf("the fence apply pins backend.image.tag = %q, want %q (the running reference %q)", got.imageTag, tc.wantTag, tc.running)
			}
			if got.imageTag == recordedPinTag || got.imageDigest == recordedPinDigest {
				t.Errorf("the fence apply left the pin the previous release recorded in the values (tag %q, digest %q): the chart renders whatever those two fields say", got.imageTag, got.imageDigest)
			}
		})
	}
}

// AN IMAGE THAT CANNOT BE READ OR SPLIT STOPS THE FENCE STAGE.
//
// Proceeding without the pin is the silent fallback this change exists to
// remove: the apply would roll the control plane onto the chart's new image,
// which is the code that is not allowed to run before the migration. The stage
// must also have changed NOTHING — a fence raised after a failed read is a
// route switched by a stage that then reported failure.
func TestTheFenceStageStopsWhenTheBackendImageCannotBeHeld(t *testing.T) {
	cases := []struct {
		name    string
		running string
	}{
		{name: "the Deployment names no image", running: "  "},
		{name: "the reference names neither a tag nor a digest", running: runningBackendRepository},
		{name: "the digest is not hexadecimal", running: runningBackendRepository + "@sha256:notahexdigest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := upgradeTestValues(t, nil)
			runner := newStageRunner(t, values)
			runner.runningImage = tc.running
			s := testUpgradeSession(t, runner, values)

			if err := stageFence(context.Background(), s); err == nil {
				t.Fatalf("the fence stage proceeded with the image %q it could not hold the backend to, instead of stopping", tc.running)
			}
			if len(runner.applied) != 0 {
				t.Errorf("the fence stage applied a chart with values from an image it could not hold the backend to: %+v", runner.applied)
			}
			if runner.fenceUp {
				t.Error("the fence stage wrote the fence's objects after failing to read the backend's image: a stage that failed must have changed nothing")
			}
		})
	}
}

// NOTHING BEFORE THE MIGRATION APPLY MAY RENDER THE MIGRATION JOB.
//
// The Job is a chart resource and its pod template is immutable. The values the
// run starts from carry migration.enabled — the migration the last install or
// upgrade ran — so an apply before this run's migration apply that reproduces
// them makes helm try to PATCH the release's existing Job with a new template,
// and the whole upgrade fails. The Job therefore has to be OFF on every one of
// those applies, which is what makes helm DELETE the release's Job, and the one
// apply that enables it has to come after the stale Job is gone.
func TestNoApplyBeforeTheMigrationApplyRendersTheMigrationJob(t *testing.T) {
	values := upgradeTestValues(t, nil)
	runner := newStageRunner(t, values)
	// The Job on the cluster is the previous release's: same fixed name, a
	// different install revision.
	runner.staleMigrationJob = true
	s := testUpgradeSession(t, runner, values)
	ctx := context.Background()

	for _, stage := range []func(context.Context, *UpgradeSession) error{stageFence, stageCheckpoint, stageMigration} {
		if err := stage(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.applied) < 3 {
		t.Fatalf("the stages applied %d chart(s), want the fence, the stopped backend and the migration: %+v", len(runner.applied), runner.applied)
	}
	enabled := -1
	for i, applied := range runner.applied {
		if applied.migration {
			if enabled == -1 {
				enabled = i
				continue
			}
			t.Errorf("apply %d rendered the migration Job after apply %d already did: %+v", i, enabled, applied)
			continue
		}
		if enabled != -1 {
			t.Errorf("apply %d turned the migration Job back off after the migration apply: %+v", i, applied)
		}
	}
	if enabled == -1 {
		t.Fatalf("no apply enabled the migration Job, so the schema is never migrated: %+v", runner.applied)
	}
	if !runner.deletedMigrationJob {
		t.Fatalf("the release's stale migration Job was never deleted, so the apply that renders the Job at this run's revision would fail on an immutable pod template: %+v", runner.applied)
	}
	if enabled < runner.deletedMigrationJobAfter {
		t.Errorf("the stale migration Job was deleted only after %d applies, but apply %d already rendered the Job at this run's revision", runner.deletedMigrationJobAfter, enabled)
	}
}

// THE APPLIES THAT RUN THE NEW CODE RUN THE CHART'S OWN PIN.
//
// Holding the backend to its running image is the FENCE apply's business and
// nothing else's. If that pin leaked into the values the later stages apply, the
// migration Job and the new backend would be held to the OLD image: the
// migration would run a schema chain the new chart's code does not expect, which
// is worse than the defect the pin exists to prevent.
func TestTheMigrationAndChartAppliesRunTheNewBackendImage(t *testing.T) {
	values := upgradeTestValues(t, nil)
	runner := newStageRunner(t, values)
	s := testUpgradeSession(t, runner, values)
	ctx := context.Background()

	for _, stage := range []func(context.Context, *UpgradeSession) error{stageFence, stageCheckpoint, stageMigration, stageChart} {
		if err := stage(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.applied) < 4 {
		t.Fatalf("the stages applied %d chart(s), want the fence, the stopped backend, the migration and the new chart: %+v", len(runner.applied), runner.applied)
	}
	if runner.applied[0].imageTag == "" && runner.applied[0].imageDigest == "" {
		t.Fatal("the fence apply carried no image pin, so this test could not see a pin leaking past it")
	}
	// The migration apply is the first one that enables the Job; the new
	// chart's apply is the last, with the backend running again. (The chart
	// apply inherits migration.enabled from the running values, as it always
	// has: it re-applies the Job at this run's revision, whose pod template the
	// migration apply already created.)
	migration := -1
	for i := range runner.applied {
		if runner.applied[i].migration {
			migration = i
			break
		}
	}
	if migration == -1 {
		t.Fatalf("no apply enabled the migration Job: %+v", runner.applied)
	}
	chart := len(runner.applied) - 1
	if !runner.applied[chart].fence || runner.applied[chart].replicas != 1 {
		t.Fatalf("apply %d is not the new chart rolling with the backend running again: %+v", chart, runner.applied[chart])
	}
	for _, i := range []int{migration, chart} {
		got := runner.applied[i]
		if got.imageRepository != "" || got.imageTag != "" || got.imageDigest != "" {
			t.Errorf("apply %d carried backend.image repository %q tag %q digest %q, want no override at all: the chart's own pin is what the migration Job and the new backend run", i, got.imageRepository, got.imageTag, got.imageDigest)
		}
	}
}

// AN IMAGE REFERENCE IS SPLIT BY THE SAME RULES THE CHART RENDERS IT WITH
// (templates/_helpers.tpl "kubenest.image": repository@digest when a digest is
// set, repository:tag otherwise), and the colon that belongs to a registry's
// port is not a tag. What the parser refuses matters as much as what it accepts:
// the fence stage holds the backend to the reference it reads, so a reference
// that cannot be split has to stop the stage rather than fall back to the
// chart's new pin.
func TestTheBackendImageReferenceIsSplitWhereTheChartSplitsIt(t *testing.T) {
	cases := []struct {
		name      string
		reference string
		want      BackendImage
		wantErr   bool
	}{
		{
			name:      "a repository and a tag",
			reference: runningBackendRepository + ":" + runningBackendTag,
			want:      BackendImage{Repository: runningBackendRepository, Tag: runningBackendTag},
		},
		{
			name:      "a repository and a digest",
			reference: runningBackendRepository + "@" + runningBackendDigest,
			want:      BackendImage{Repository: runningBackendRepository, Digest: runningBackendDigest},
		},
		{
			name:      "a repository, a tag and a digest",
			reference: runningBackendRepository + ":" + runningBackendTag + "@" + runningBackendDigest,
			want:      BackendImage{Repository: runningBackendRepository, Tag: runningBackendTag, Digest: runningBackendDigest},
		},
		{
			name:      "a registry whose host carries a port",
			reference: "registry.internal:5000/kubenest/backend:" + runningBackendTag,
			want:      BackendImage{Repository: "registry.internal:5000/kubenest/backend", Tag: runningBackendTag},
		},
		{
			name:      "a registry whose host carries a port, pinned by digest",
			reference: "registry.internal:5000/kubenest/backend@" + runningBackendDigest,
			want:      BackendImage{Repository: "registry.internal:5000/kubenest/backend", Digest: runningBackendDigest},
		},
		{
			name:      "a registry whose host carries a port and no tag",
			reference: "registry.internal:5000/kubenest/backend",
			wantErr:   true,
		},
		{
			name:      "a repository with neither tag nor digest",
			reference: runningBackendRepository,
			wantErr:   true,
		},
		{
			name:      "an empty tag",
			reference: runningBackendRepository + ":",
			wantErr:   true,
		},
		{
			name:      "an empty repository",
			reference: ":" + runningBackendTag,
			wantErr:   true,
		},
		{
			name:      "an empty digest",
			reference: runningBackendRepository + "@sha256:",
			wantErr:   true,
		},
		{
			name:      "a digest that is not hexadecimal",
			reference: runningBackendRepository + "@sha256:notahexdigest",
			wantErr:   true,
		},
		{
			name:      "nothing at all",
			reference: "  ",
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBackendImage(tc.reference)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseBackendImage(%q) = %+v, want a refusal: a reference that cannot be split must stop the stage, never fall back to the chart's new pin", tc.reference, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBackendImage(%q): %v", tc.reference, err)
			}
			if got != tc.want {
				t.Errorf("ParseBackendImage(%q) = %+v, want %+v", tc.reference, got, tc.want)
			}
		})
	}
}
