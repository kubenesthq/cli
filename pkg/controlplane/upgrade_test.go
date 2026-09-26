package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
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
	// backend.image as the apply writes it: the CHART's pin, and the image the
	// checkpoint CronJob and the migration Job run. Empty fields mean the values
	// mention nothing there, so the chart's own pin is what renders.
	imageRepository string
	imageTag        string
	imageDigest     string
	// backend.heldImage, which only the backend Deployment reads: the image the
	// fence stage or the failed-migration restore holds it on. Empty means the
	// Deployment takes the chart's own pin too.
	heldRepository string
	heldTag        string
	heldDigest     string
	// raw is the applied values document verbatim, for the assertions about the
	// parts NOT decoded above — the migration Job's pod-template inputs.
	raw string
}

// held reports whether the apply holds the backend Deployment on its own image.
func (a appliedValues) held() bool {
	return a.heldRepository != "" || a.heldTag != "" || a.heldDigest != ""
}

// overriding reports whether the apply names any image at all, under either key,
// so the chart's own pins are not what render.
func (a appliedValues) overriding() bool {
	return a.held() || a.imageRepository != "" || a.imageTag != "" || a.imageDigest != ""
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

	// recordMu, recordDoc and recordRV are the in-memory kube-system the
	// operation Store reads and writes: one ConfigMap and its resourceVersion,
	// so a test can drive the REAL store, the REAL record and a REAL handle
	// (the fields of operation.Handle are unexported, so there is no way to fake
	// one).
	recordMu  sync.Mutex
	recordDoc []byte
	recordRV  int
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
		case strings.HasPrefix(command, "sudo -n k3s kubectl get configmap kubenest-operation"):
			stdout, missing := s.readRecord()
			if missing {
				return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "kubenest-operation" not found`}, nil
			}
			return sshx.Result{Stdout: stdout}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl create -f -") || strings.HasPrefix(command, "sudo -n k3s kubectl replace -f -"):
			return sshx.Result{Stdout: s.writeRecord(t, s.lastInput(t))}, nil
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

// readRecord renders the record ConfigMap the store reads back, and says
// whether there is one at all.
func (s *stageRunner) readRecord() (string, bool) {
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	if s.recordDoc == nil {
		return "", true
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "kubenest-operation", "resourceVersion": fmt.Sprint(s.recordRV)},
		"data":     map[string]any{"record.json": string(s.recordDoc)},
	})
	if err != nil {
		return "", true
	}
	return string(body), false
}

// writeRecord stores one accepted write and answers with the new revision, the
// way the API server does.
func (s *stageRunner) writeRecord(t *testing.T, doc []byte) string {
	t.Helper()
	var object struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(doc, &object); err != nil {
		t.Fatalf("the operation Store wrote a document this fake cannot read: %v", err)
	}
	s.recordMu.Lock()
	defer s.recordMu.Unlock()
	s.recordDoc = []byte(object.Data["record.json"])
	s.recordRV++
	return fmt.Sprintf(`{"metadata":{"resourceVersion":"%d"}}`, s.recordRV)
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
		if held, ok := backend["heldImage"].(map[string]any); ok {
			out.heldRepository, _ = held["repository"].(string)
			out.heldTag, _ = held["tag"].(string)
			out.heldDigest, _ = held["digest"].(string)
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
	// A PUBLIC URL THAT ANSWERS FROM THE BACKEND: every production session
	// carries one (the client the command's own version check used), and the
	// unfence refuses without one, so the helper provides the smallest one that
	// answers — a 200 from GET /api/v1/version. The tests about the fence
	// measurement replace it with an endpoint they control.
	s.Opts.Public = publicBackendStub(t)
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

// upgradeTestValuesHolding is upgradeTestValues with the backend Deployment HELD
// on an older image: the values a HelmChart carries while a fence is up, and what
// a run's own read strips before it applies anything (ValuesWithoutTheFencePin).
func upgradeTestValuesHolding(t *testing.T, held map[string]any) string {
	t.Helper()
	values := upgradeTestValues(t, nil)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
		t.Fatal(err)
	}
	backend, _ := doc["backend"].(map[string]any)
	if backend == nil {
		backend = map[string]any{}
	}
	backend["heldImage"] = held
	doc["backend"] = backend
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The fence stage's apply is the FIRST change of the whole procedure and it runs
// BEFORE the checkpoint and before the migration. The chart this binary carries
// pins a NEW backend image, so an apply that does not hold the backend DEPLOYMENT
// to the image it runs now rolls it onto code that has never been migrated.
//
// AND IT HOLDS ONLY THE DEPLOYMENT: the pin goes on backend.heldImage, never on
// backend.image, because backend.image is also the image the checkpoint CronJob
// and the migration Job run and their commands are this chart's. A pin written
// there ran the new chart's `dump` on an old image, which printed a usage line for
// a command it did not have (hardware, 2026-09-26;
// kn-t70-control-plane-version-identity-4xso.5).
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
			values := upgradeTestValuesHolding(t, recordedPin)
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
			if got.heldRepository != runningBackendRepository {
				t.Errorf("the fence apply holds backend.heldImage.repository = %q, want %q: the backend Deployment is what this pin is for", got.heldRepository, runningBackendRepository)
			}
			if got.heldDigest != tc.wantDigest {
				t.Errorf("the fence apply holds backend.heldImage.digest = %q, want %q (the running reference %q)", got.heldDigest, tc.wantDigest, tc.running)
			}
			if got.heldTag != tc.wantTag {
				t.Errorf("the fence apply holds backend.heldImage.tag = %q, want %q (the running reference %q)", got.heldTag, tc.wantTag, tc.running)
			}
			if got.imageRepository != "" || got.imageTag != "" || got.imageDigest != "" {
				t.Errorf("the fence apply also wrote backend.image (%q/%q/%q), which is the chart's own pin AND the image the checkpoint CronJob and the migration Job run: a pin there holds the new chart's Jobs on an older image, whose commands they do not have",
					got.imageRepository, got.imageTag, got.imageDigest)
			}
			if got.heldTag == recordedPinTag || got.heldDigest == recordedPinDigest {
				t.Errorf("the fence apply left the pin the previous release recorded in the values (tag %q, digest %q): the chart renders whatever those two fields say", got.heldTag, got.heldDigest)
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
	if !runner.applied[0].held() {
		t.Fatal("the fence apply carried no held image, so this test could not see a pin leaking past it")
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
		if got.overriding() {
			t.Errorf("apply %d names an image (%s), want no override at all under either key: the chart's own pin is what the migration Job and the new backend run, and a held image belongs only in an apply that is holding an old Deployment",
				i, pinnedDescription(got))
		}
	}
}

// pinnedDescription names what an apply overrode, for a failure message: a
// mismatch between two runs prints both keys, and which one moved is the answer.
func pinnedDescription(a appliedValues) string {
	out := ""
	if a.held() {
		out = "backend.heldImage=" + a.heldRepository + ":" + a.heldTag + "@" + a.heldDigest
	}
	if a.imageRepository != "" || a.imageTag != "" || a.imageDigest != "" {
		if out != "" {
			out += " and "
		}
		out += "backend.image=" + a.imageRepository + ":" + a.imageTag + "@" + a.imageDigest
	}
	if out == "" {
		return "no image"
	}
	return out
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

// A RE-RUN MUST ROLL THE CHART'S OWN IMAGE, NOT THE ONE THE FENCE PINNED
// (kn-t70-control-plane-version-identity-4xso.4).
//
// The fence stage pins backend.image to the image the backend RUNS so that its
// own apply does not roll the new chart's image before the migration, and the
// failed-migration restore writes the same kind of pin. Both land in the
// HelmChart's valuesContent, and every run takes its base values from there — so
// a re-run or `--resume` used to start from a document that pinned the OLD image,
// and its stop apply, its migration Job, its chart stage and its unfence apply
// all rendered it. The upgrade rolled nothing.
//
// AND THE VALIDATION COULD NOT TELL. The image the expectations call "declared"
// came out of those same pinned values, so it equalled the image the Deployment
// was running, `ValidationExpectations` cleared StaleBuild and WantBuild, and the
// only check left was the era floor — which the old code behind the fence meets
// too, because the "before" facts were read from it. This test asserts both
// halves: the applies render the chart's own pin, and the expectations demand the
// build that pin belongs to.
func TestAResumedRunRollsTheChartsOwnImage(t *testing.T) {
	ctx := context.Background()
	// The values a second process reads off the cluster after the fence stage:
	// this installation's settings and secrets, and the pin the fence apply
	// wrote into the HelmChart.
	pinned := upgradeTestValuesHolding(t, map[string]any{
		"repository": runningBackendRepository,
		"tag":        recordedPinTag,
		"pullPolicy": "IfNotPresent",
	})
	values, err := ValuesWithoutTheFencePin(pinned)
	if err != nil {
		t.Fatal(err)
	}
	runner := newStageRunner(t, values)
	s := testUpgradeSession(t, runner, values)
	s.Opts.Before = api.ControlPlaneVersion{Contract: 3, Build: "c121ed887750b1d3"}

	for _, stage := range []func(context.Context, *UpgradeSession) error{stageFence, stageCheckpoint, stageMigration, stageChart} {
		if err := stage(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.applied) < 4 {
		t.Fatalf("the stages applied %d chart(s), want the fence, the stopped backend, the migration and the new chart: %+v", len(runner.applied), runner.applied)
	}
	pinned_ := func(a appliedValues) bool { return a.overriding() }
	// THE FENCE APPLY IS THE ONE APPLY THAT MUST PIN, and asserting it here is
	// what makes the rest of this test able to see a pin at all: a run whose base
	// values carried no pin would prove nothing about a run whose did.
	if !pinned_(runner.applied[0]) {
		t.Fatal("the fence apply carried no image pin, so this test could not see a pin leaking past it")
	}
	for i, applied := range runner.applied[1:] {
		if pinned_(applied) {
			t.Errorf("apply %d names an image (%s), want no override at all: the chart's own pins are the code this run is upgrading to, and it is the fence's pin — not the run's — that has to stay out of the later applies",
				i+1, pinnedDescription(applied))
		}
	}
	// THE VALIDATION DEMANDS THE NEW BUILD, because the chart's own pin is now
	// the declared image and it differs from the one the Deployment is running.
	expectations, err := ValidationExpectations(s.Opts.Before, values)
	if err != nil {
		t.Fatal(err)
	}
	if expectations.WantBuild == "" {
		t.Errorf("the validation demands no build (%+v), so a control plane still running the old image would pass it", expectations)
	}
	if expectations.StaleBuild != s.Opts.Before.Build {
		t.Errorf("the validation would not refuse the build that was serving before the upgrade (%q), want %q", expectations.StaleBuild, s.Opts.Before.Build)
	}
}

// THE FENCE AND THE RESTORE HOLD THE DEPLOYMENT, AND ONLY THE DEPLOYMENT
// (kn-t70-control-plane-version-identity-4xso.5).
//
// Their pin used to be backend.image, which is ALSO the image the checkpoint
// CronJob and the migration Job run. While the fence was up, the new chart's
// checkpoint Job therefore ran the old backend: on hardware (2026-09-26) its
// `dump` container printed `usage: python -m app.services.checkpoint_runner ...`
// and the upgrade died at the checkpoint stage. The pin belongs on
// backend.heldImage, which only the backend Deployment reads, and it must leave
// backend.image exactly as the chart declares it.
func TestTheFenceAndTheRestorePinOnlyTheHeldImage(t *testing.T) {
	base := upgradeTestValues(t, nil)
	held := BackendImage{Repository: runningBackendRepository, Tag: runningBackendTag, Digest: runningBackendDigest}

	// THE FENCE STAGE'S VALUES.
	fenced, err := FenceValues(base, FenceOptions{Up: true, MigrationOff: true, HeldImage: &held})
	if err != nil {
		t.Fatal(err)
	}
	assertHeldPin(t, fenced, held.Repository, held.Tag, held.Digest)
	if image, ok := backendGroupKey(t, fenced, "image"); ok {
		t.Errorf("the fence's values carry backend.image %v, which is the chart's own pin AND the image the checkpoint and migration Jobs run", image)
	}

	// THE FAILED-MIGRATION RESTORE'S VALUES.
	reference := runningBackendRepository + "@" + runningBackendDigest
	restored, err := withHeldBackendImage(base, reference)
	if err != nil {
		t.Fatal(err)
	}
	// The reference it parses is digest-pinned, so the held pin carries no tag.
	assertHeldPin(t, restored, runningBackendRepository, "", runningBackendDigest)
	if image, ok := backendGroupKey(t, restored, "image"); ok {
		t.Errorf("the restore's values carry backend.image %v, which is the chart's own pin AND the image the checkpoint and migration Jobs run", image)
	}

	// AND A RUN'S OWN READ DROPS THE HEld PIN, under either spelling: what this
	// read returns becomes every apply the run makes.
	stripped, err := ValuesWithoutTheFencePin(restored)
	if err != nil {
		t.Fatal(err)
	}
	if pin, ok := backendGroupKey(t, stripped, "heldImage"); ok {
		t.Errorf("a run's base values still carry backend.heldImage %v, so its stop apply, its migration Job, its chart stage and its unfence apply would all render the image the fence was holding", pin)
	}
	if pin, ok := backendGroupKey(t, stripped, "image"); ok {
		t.Errorf("a run's base values still carry backend.image %v, so the applies would not render the chart's own pin", pin)
	}
}

// assertHeldPin checks the values hold the backend Deployment on exactly this
// reference. A digest is compared as written — including an empty one, which is
// what a tag-only reference carries — and a tag is only required when one was
// given.
func assertHeldPin(t *testing.T, valuesYAML, wantRepository, wantTag, wantDigest string) {
	t.Helper()
	pin, ok := backendGroupKey(t, valuesYAML, "heldImage")
	if !ok {
		t.Fatalf("the values carry no backend.heldImage, so the Deployment would run the chart's new image:\n%s", valuesYAML)
	}
	if pin["repository"] != wantRepository || pin["digest"] != wantDigest {
		t.Errorf("backend.heldImage is %v, want repository %s and digest %s", pin, wantRepository, wantDigest)
	}
	if wantTag != "" && pin["tag"] != wantTag {
		t.Errorf("backend.heldImage carries tag %v, want %s", pin["tag"], wantTag)
	}
}

// backendGroupKey returns one key of the values' backend group, and whether it is
// there at all.
func backendGroupKey(t *testing.T, valuesYAML, key string) (map[string]any, bool) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(valuesYAML), &doc); err != nil {
		t.Fatalf("the values are not readable YAML: %v", err)
	}
	backend, _ := doc["backend"].(map[string]any)
	value, ok := backend[key].(map[string]any)
	return value, ok
}

// THE FENCE IS REPORTED DOWN ONLY WHEN THE URL A CLIENT USES ANSWERS FROM THE
// BACKEND (kn-t70-control-plane-version-identity-4xso.6).
//
// stageUnfence measured the HTTPRoute OBJECT: it waited for helm-controller to
// re-render kubenest-cp-api at the backend Service, then dismantled the fence and
// reported it down. On hardware (2026-09-26, run 21) the upgrade printed "the
// fence is down ... answers from the backend again" and the client's very next
// request to the public URL got HTTP 503 — the gateway was still reprogramming,
// or still routing to the fence Service the apply had just deleted. A client that
// acts on "Upgraded" then gets a 503.
//
// THE ROUTE OBJECT IS NOT THE ROUTE A CLIENT USES, so the stage now asks the
// public URL — the same client, CA and endpoint the command's version check used
// before the fence went up — until the BACKEND answers it, and only then deletes
// the fence's objects.
func TestTheFenceIsReportedDownOnlyWhenThePublicURLAnswersFromTheBackend(t *testing.T) {
	ctx := context.Background()
	values := upgradeTestValues(t, nil)
	kube := newStageRunner(t, values)
	s := testUpgradeSession(t, kube, values)
	s.PollInterval = time.Millisecond
	s.WaitDeadline = 2 * time.Second

	// THE PUBLIC URL'S ANSWERS, in order: the fence's own 503, then a plain 503
	// (a gateway that has not switched yet and carries no fence header), then the
	// backend's 401 — which is the backend ANSWERING a request, not failing to.
	var (
		mu               sync.Mutex
		asked            int
		deletedWhenAsked []int
	)
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		deletedWhenAsked = append(deletedWhenAsked, fenceObjectsDeleted(kube))
		n := asked
		mu.Unlock()
		switch n {
		case 1:
			w.Header().Set(api.FenceHeader, api.FenceHeaderUp)
			http.Error(w, "The KubeNest control plane is being upgraded.", http.StatusServiceUnavailable)
		case 2:
			http.Error(w, "no endpoints", http.StatusServiceUnavailable)
		default:
			http.Error(w, `{"code": "unauthenticated"}`, http.StatusUnauthorized)
		}
	}))
	t.Cleanup(public.Close)
	client, err := api.New(public.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatal(err)
	}
	s.Opts.Public = client

	if err := stageUnfence(ctx, s); err != nil {
		t.Fatalf("the fence was not reported down although the backend answered the public URL: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 3 {
		t.Errorf("the public URL was asked %d time(s), want the fence's 503, the plain 503 and then the backend's 401: a stage that reported the fence down without waiting through both 503s would have asked fewer", asked)
	}
	for i, deleted := range deletedWhenAsked {
		if deleted != 0 {
			t.Errorf("fence object(s) had already been deleted when the public URL was asked for the %dth time: the fence's objects are what the route names while the URL does not answer from the backend", i+1)
		}
	}
	if fenceObjectsDeleted(kube) == 0 {
		t.Error("the fence's objects were never deleted, so the fence is still up")
	}
	if s.FenceState != FenceDown {
		t.Errorf("the stage left FenceState = %q, want it down now that the URL a client uses answers from the backend", s.FenceState)
	}
}

// AN UNFENCE WHOSE PUBLIC URL NEVER RECOVERS FAILS AT THE DEADLINE, AND LEAVES
// THE FENCE STANDING (kn-t70-control-plane-version-identity-4xso.6).
//
// The fence's objects are what the public route names while the gateway has not
// switched, so deleting them on a URL that still answers 503 is how a customer
// gets a 503 from a route that has nowhere to go. The stage fails instead, naming
// the last answer, with the fence still up for the operator to look at.
func TestAnUnfenceWhosePublicURLNeverRecoversFailsAtTheDeadline(t *testing.T) {
	ctx := context.Background()
	values := upgradeTestValues(t, nil)
	kube := newStageRunner(t, values)
	s := testUpgradeSession(t, kube, values)
	// SHORT, because this arm is about the deadline being reached: the wait's
	// real bound comes from the bundle's component-ready limit.
	s.PollInterval = time.Millisecond
	s.WaitDeadline = 50 * time.Millisecond

	// A gateway that never reaches the backend: a 503 with NO fence header,
	// which is exactly what hardware saw after the fence was dismantled too early.
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no endpoints", http.StatusServiceUnavailable)
	}))
	t.Cleanup(public.Close)
	client, err := api.New(public.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatal(err)
	}
	s.Opts.Public = client

	err = stageUnfence(ctx, s)
	if err == nil {
		t.Fatal("the fence was reported down although the public URL never answered from the backend")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("the failure does not say what the public URL last answered:\n%v", err)
	}
	if deleted := fenceObjectsDeleted(kube); deleted != 0 {
		t.Errorf("the fence's objects were deleted (%d command(s)) although the public URL never answered from the backend: the route then names a Service that is gone", deleted)
	}
	if s.FenceState == FenceDown {
		t.Error("the stage reported the fence down although the public URL still answered 503")
	}
}

// A SESSION WITH NO PUBLIC URL CANNOT MEASURE THE FENCE, so it refuses rather
// than taking the route object for the route a client uses. Every production run
// carries the client its own version check used.
func TestAnUnfenceWithNoPublicURLRefusesRatherThanReportingDown(t *testing.T) {
	ctx := context.Background()
	values := upgradeTestValues(t, nil)
	kube := newStageRunner(t, values)
	s := testUpgradeSession(t, kube, values)
	s.Opts.Public = nil

	err := stageUnfence(ctx, s)
	if err == nil {
		t.Fatal("the fence was reported down by a session that has no public URL to measure")
	}
	if deleted := fenceObjectsDeleted(kube); deleted != 0 {
		t.Errorf("the fence's objects were deleted (%d command(s)) although nothing measured the URL a client uses", deleted)
	}
}

// publicBackendStub is a public URL whose backend answers GET /api/v1/version, so
// a session that is not about the fence measurement still has a URL to ask.
func publicBackendStub(t *testing.T) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"contract": 3, "build": "c121ed887750b1d3"}`))
	}))
	t.Cleanup(srv.Close)
	client, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// fenceObjectsDeleted counts the commands that remove the fence's own objects:
// what a stage that reported the fence down has done, and what a stage that has
// not must not have done.
func fenceObjectsDeleted(runner *stageRunner) int {
	deleted := 0
	for _, command := range runner.Commands() {
		if strings.Contains(command, "delete deployment/"+fenceName) {
			deleted++
		}
	}
	return deleted
}

// A RESUMED MIGRATION STAGE STILL SUBMITS ITS OWN APPLY
// (kn-t70-control-plane-version-identity-4xso.8).
//
// THE STAGE'S TWO WRITES ARE ONE COMMAND. The stop apply and the migration apply
// are both k3s.WriteManifest of the control plane's chart, so an identity that
// hashed the command alone was the same for both: a resume whose record held the
// stop apply skipped the migration apply too, and waited out its whole deadline
// for a Job nothing had rendered (hardware, 2026-09-26, run 26 — `skip ee8549cc…
// (control-plane-migration): the record says this action succeeded`, then ten
// minutes of `job kubenest-cp-migrate ... is not found yet`).
//
// THIS TEST DRIVES THE REAL STORE, THE REAL RECORD AND A REAL HANDLE. The
// predecessor's stop apply is submitted through the same decorator the stage
// uses; its identity is read back out of the record; and the resumed stage runs
// with exactly that identity in its skip set, which is what a resume of an
// interrupted run carries.
func TestAResumedMigrationStageStillSubmitsItsMigrationApply(t *testing.T) {
	ctx := context.Background()
	values := upgradeTestValues(t, nil)
	kube := newStageRunner(t, values)

	// THE RECORD OF THE INTERRUPTED RUN, in the cluster the Store reads.
	store := &operation.Store{Runner: kube}
	handle, err := store.Acquire(ctx, operation.Request{Kind: operation.KindControlPlaneUpgrade, Cluster: "prod-1"})
	if err != nil {
		t.Fatalf("taking the operation record: %v", err)
	}

	// ITS STOP APPLY, submitted exactly as the stage submits it: the same values
	// composition, the same Apply, the same decorator with the same Specs — so the
	// streamed document is byte-identical to the one a stage would write.
	stopped, err := FenceValues(values, FenceOptions{Up: true, BackendReplicas: int32Ptr(0), MigrationOff: true})
	if err != nil {
		t.Fatal(err)
	}
	first := &operation.Guarded{Inner: kube, Op: handle, Stage: StageMigration, Specs: actionSpecs}
	if _, err := Apply(ctx, first, stopped); err != nil {
		t.Fatalf("the interrupted run's stop apply: %v", err)
	}
	stored, err := store.Find(ctx, handle.OperationID())
	if err != nil {
		t.Fatalf("reading the record back: %v", err)
	}
	if len(stored.Record.Actions) != 1 {
		t.Fatalf("the record holds %d action(s), want the stop apply alone: %+v", len(stored.Record.Actions), stored.Record.Actions)
	}
	skip := map[string]bool{stored.Record.Actions[0].ID: true}

	// THE RESUME: the stage's own runner carries the record's identity in its skip
	// set, which is what resumableSkip hands it.
	s := testUpgradeSession(t, kube, values)
	s.WithOperation(handle, skip)
	s.PollInterval = time.Millisecond
	s.WaitDeadline = 2 * time.Second

	// The predecessor's own write is already on the fake's list; what matters is
	// what the RESUMED stage adds.
	writesBefore := len(kube.applied)
	if err := stageMigration(ctx, s); err != nil {
		t.Fatalf("the resumed migration stage failed: %v", err)
	}
	// THE MIGRATION APPLY REACHED THE CLUSTER: the stop apply was skipped (the
	// record holds it) and the migration apply, whose document is different, was
	// not.
	added := kube.applied[writesBefore:]
	if len(added) != 1 {
		t.Fatalf("the resumed stage wrote %d chart document(s), want the migration apply alone: %+v", len(added), added)
	}
	if !added[0].migration {
		t.Errorf("the resumed stage's write is not the migration apply, so the wait that follows would be for nothing: %s", added[0].raw)
	}
}
