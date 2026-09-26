package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/sshx"
)

// AFTER A FAILED MIGRATION THE PREVIOUS CODE MUST RUN BEHIND THE FENCE
// (kn-t70-control-plane-version-identity-4xso.2).
//
// The migration stage applies the chart with `backend.replicas: 0` and the NEW
// image (one image value for the Job and the Deployment), so a failed migration
// leaves the control plane running NOTHING: every read, through the fenced route
// or through the node's tunnel, is refused with "connection refused".
//
// THE PREVIOUS CHART CANNOT BE THE THING THAT IS RE-APPLIED, and that is the
// finding from the sixteenth hardware run: a real 1.1 control plane's chart has
// no fence template, so re-applying it with `fence.enabled: true` renders the api
// route back onto the backend and the fence comes DOWN. Only the NEW chart can
// express "the old code, fenced", so the restore applies the new archive with the
// previous image pinned.
const previousBackendImage = "ghcr.io/kubenesthq/kubenest-backend:1acd82d"

// previousKube is the smallest in-memory kube the restore and the recovery need:
// the fence Deployment's annotations, the backend's replica count, the migration
// Job, and the chart applies it observes.
type previousKube struct {
	mu sync.Mutex
	// fenceFacts is what the fence Deployment records, by annotation name.
	fenceFacts map[string]string
	// backendReady is how many replicas the fake reports READY.
	backendReady int32
	// backendReplicas is the backend Deployment's DESIRED replica count. Zero is
	// the state the migration stage's stop apply leaves behind, and the one the
	// recovery exists for.
	backendReplicas int32
	// migrationCondition is the migration Job's own condition: "Complete",
	// "Failed", "running" for a Job that has no verdict yet, or "" for no Job in
	// the cluster at all.
	migrationCondition string
	// migrationRevision is the install revision the Job carries. Empty means the
	// revision the last apply stamped, which is what the migration stage's wait
	// compares against.
	migrationRevision string
	// migrationFailsAfter makes the fake report a RUNNING Job for that many reads
	// and a Failed one afterwards: an interrupt inside the migration, where the
	// outcome is only knowable later.
	migrationFailsAfter int
	// migrationReads counts the migration Job reads, for that arm, and
	// migrationRunningSeen records that it answered "running" at least once, so
	// an arm that never exercised the wait cannot pass silently.
	migrationReads       int
	migrationRunningSeen bool
	// migrationFailedAt is the command index of the read at which the fake first
	// answered Failed, so a test can assert that nothing was applied before the
	// verdict was known.
	migrationFailedAt int
	// applied records every chart apply, in order, and appliedAt the command
	// index each one was submitted at.
	applied   []appliedRelease
	appliedAt []int
	// chartRevision is the install revision the last apply stamped.
	chartRevision string
	*componenttest.FakeRunner
}

type appliedRelease struct {
	chartContent string
	values       map[string]any
}

func newPreviousKube(t *testing.T) *previousKube {
	t.Helper()
	k := &previousKube{backendReady: 1, backendReplicas: 1, migrationCondition: "Failed"}
	k.FakeRunner = &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get deployment/"+fenceName):
			if k.fenceFacts == nil {
				return sshx.Result{ExitCode: 1, Stderr: "not found"}, nil
			}
			body, err := json.Marshal(map[string]any{
				"metadata": map[string]any{"name": fenceName, "annotations": k.fenceFacts},
			})
			if err != nil {
				return sshx.Result{}, err
			}
			return sshx.Result{Stdout: string(body)}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 ") && strings.Contains(command, ReleaseName+".yaml"):
			k.appliedAt = append(k.appliedAt, len(k.Commands()))
			k.recordApply(t, k.lastInput(t))
			return sshx.Result{}, nil
		case strings.HasPrefix(command, "sudo -n install -m 0600 "):
			return sshx.Result{}, nil
		case strings.Contains(command, backendReplicasCmd):
			// BEFORE the general Deployment read below, which this command also
			// matches: the replica count is a bare number and the readiness
			// probes read whole objects.
			return sshx.Result{Stdout: fmt.Sprint(k.backendReplicas)}, nil
		case strings.Contains(command, "get deployment/"):
			revision := k.chartRevision
			return sshx.Result{Stdout: fmt.Sprintf(`{"metadata":{"generation":1},"spec":{"replicas":1,"template":{"metadata":{"annotations":{"kubenest.io/install-revision":%q}}}},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":%d}}`, revision, k.backendReady)}, nil
		case strings.Contains(command, "get statefulset "):
			return sshx.Result{Stdout: `{"spec":{"replicas":1},"status":{"readyReplicas":1}}`}, nil
		case strings.Contains(command, "get certificate/") || strings.Contains(command, "get gateway/"):
			return sshx.Result{Stdout: `{"status":{"conditions":[{"type":"Ready","status":"True"},{"type":"Programmed","status":"True"}]}}`}, nil
		case command == migrationJobCmd:
			condition := k.migrationCondition
			if k.migrationFailsAfter > 0 {
				k.migrationReads++
				if k.migrationReads <= k.migrationFailsAfter {
					condition = "running"
					k.migrationRunningSeen = true
				} else {
					if k.migrationFailedAt == 0 {
						k.migrationFailedAt = len(k.Commands())
					}
					condition = "Failed"
				}
			}
			if condition == "" {
				return sshx.Result{ExitCode: 1, Stderr: "not found"}, nil
			}
			revision := k.migrationRevision
			if revision == "" {
				revision = k.chartRevision
			}
			return sshx.Result{Stdout: jobJSON(t, condition, revision)}, nil
		case strings.HasPrefix(command, "sudo -n k3s kubectl delete job "):
			return sshx.Result{}, nil
		default:
			return sshx.Result{}, nil
		}
	}}
	return k
}

func (k *previousKube) lastInput(t *testing.T) []byte {
	t.Helper()
	inputs := k.Inputs()
	if len(inputs) == 0 {
		t.Fatal("a document was written without streaming one")
	}
	return inputs[len(inputs)-1]
}

func (k *previousKube) recordApply(t *testing.T, manifest []byte) {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(manifest, &doc); err != nil {
		t.Fatalf("the applied manifest is not valid YAML: %v", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	content, _ := spec["chartContent"].(string)
	raw, _ := spec["valuesContent"].(string)
	var values map[string]any
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatalf("the applied values are not valid YAML: %v", err)
	}
	if revision, ok := values["installRevision"].(string); ok {
		k.chartRevision = revision
	}
	k.mu.Lock()
	k.applied = append(k.applied, appliedRelease{chartContent: content, values: values})
	k.mu.Unlock()
}

func (k *previousKube) lastApply(t *testing.T) appliedRelease {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.applied) == 0 {
		t.Fatal("no chart was applied")
	}
	return k.applied[len(k.applied)-1]
}

func migrationSession(t *testing.T, kube *previousKube, values string) *UpgradeSession {
	t.Helper()
	s := testUpgradeSession(t, kube, values)
	s.PollInterval = time.Millisecond
	s.WaitDeadline = 2 * time.Second
	s.Opts.Before = api.ControlPlaneVersion{Contract: 3, Build: "c121ed88"}
	return s
}

// A FAILED MIGRATION PUTS THE PREVIOUS CODE BACK, BEHIND THE STILL-RAISED FENCE.
func TestAFailedMigrationPutsThePreviousCodeBackBehindTheFence(t *testing.T) {
	ctx := context.Background()
	values := "domain: kn.example.com\njwtSecret: s\nbackend:\n  admin:\n    email: a@b.c\n"
	kube := newPreviousKube(t)
	kube.fenceFacts = map[string]string{
		fencePreviousImageAnnotation:    previousBackendImage,
		fencePreviousReplicasAnnotation: "2",
	}
	s := migrationSession(t, kube, values)

	err := stageMigration(ctx, s)
	if err == nil {
		t.Fatal("a migration Job that failed did not fail the stage")
	}
	if !strings.Contains(err.Error(), MigrationJobName) {
		t.Errorf("the failure does not name the migration Job:\n%v", err)
	}

	applied := kube.lastApply(t)
	// THE NEW ARCHIVE, because only it has the fence template: applying the
	// previous archive with the fence value would render the api route back onto
	// the backend and LOWER the fence, exposing code nobody has validated.
	if applied.chartContent != base64.StdEncoding.EncodeToString(ChartArchive()) {
		t.Error("the last apply did not carry the chart this binary embeds, which is the only one that can keep the fence up over the old code")
	}
	// THE PREVIOUS IMAGE, held on the backend Deployment — and NOT on
	// backend.image, which is also the image the checkpoint CronJob and the
	// migration Job run: pinned there, the new chart's Jobs would run the old
	// image and their commands are this chart's
	// (kn-t70-control-plane-version-identity-4xso.5).
	backend, _ := applied.values["backend"].(map[string]any)
	if backend == nil {
		t.Fatalf("the restored values carry no backend group: %v", applied.values)
	}
	pin, _ := backend["heldImage"].(map[string]any)
	if pin == nil || pin["tag"] != "1acd82d" || pin["repository"] != "ghcr.io/kubenesthq/kubenest-backend" {
		t.Errorf("the restored release holds backend.heldImage %v, want the image the backend ran before the upgrade (%s)", pin, previousBackendImage)
	}
	if image, ok := backend["image"]; ok {
		t.Errorf("the restored release also pins backend.image %v, which the checkpoint CronJob and the migration Job run", image)
	}
	// THE FENCE STAYS UP.
	fence, _ := applied.values["fence"].(map[string]any)
	if fence == nil || fence["enabled"] != true {
		t.Errorf("the restored release has fence = %v, want it still up", applied.values["fence"])
	}
	// THE MIGRATION IS OFF: the chart has one image value for the Job and the
	// Deployment and the Job's pod template is immutable.
	if migration, ok := applied.values["migration"].(map[string]any); ok && migration["enabled"] == true {
		t.Error("the restored release re-enabled the migration Job: helm would patch an immutable pod template onto the Job the failed attempt left")
	}
	// AND THE BACKEND IS AT THE SIZE IT RAN AT, taken from the fence because a
	// new process has no journal.
	if backend["replicas"] != 2 {
		t.Errorf("the restored release has backend.replicas = %v, want the 2 the fence recorded", backend["replicas"])
	}
}

// THE PREVIOUS IMAGE SURVIVES A NEW PROCESS, because it is recorded on the fence
// itself — the object that exists while the fence is up — rather than in a
// process's memory or a journal on one machine.
func TestThePreviousBackendImageSurvivesANewProcess(t *testing.T) {
	ctx := context.Background()
	writer := newPreviousKube(t)
	writer.fenceFacts = map[string]string{
		fencePreviousImageAnnotation: "ghcr.io/kubenesthq/kubenest-backend@sha256:abc",
	}

	// A FRESH fake with no memory of the raise, reading what the cluster holds.
	reader := newPreviousKube(t)
	reader.fenceFacts = writer.fenceFacts
	facts, ok, err := FenceDeploymentFacts(ctx, reader)
	if err != nil || !ok {
		t.Fatalf("a new process could not read the fence's facts (ok=%v): %v", ok, err)
	}
	if facts.PreviousImage != "ghcr.io/kubenesthq/kubenest-backend@sha256:abc" {
		t.Errorf("the previous image is %q, want the one the fence records", facts.PreviousImage)
	}
	// AND WITH NO FENCE THERE ARE NO FACTS: the ordinary state, not an error.
	if _, ok, err := FenceDeploymentFacts(ctx, newPreviousKube(t)); err != nil || ok {
		t.Errorf("with no fence the read answered ok=%v err=%v, want nothing and no error", ok, err)
	}
}

// A BACKEND THAT ROLLS OUT BUT NEVER BECOMES READY STILL COUNTS AS RESTORED.
//
// The migration failed because PostgreSQL went away, so the previous backend's
// pods start against a database that is not there: they roll out at the previous
// revision, run the previous image, and sit NotReady. A restore that waited for
// readiness would time out and report "the control plane is running nothing"
// about a control plane whose previous code IS what runs.
func TestARestoreCountsARolledOutBackendThatIsNotReady(t *testing.T) {
	ctx := context.Background()
	values := "domain: kn.example.com\njwtSecret: s\n"
	kube := newPreviousKube(t)
	kube.backendReady = 0 // the database is gone
	kube.fenceFacts = map[string]string{fencePreviousImageAnnotation: previousBackendImage}
	s := migrationSession(t, kube, values)

	err := stageMigration(ctx, s)
	if err == nil {
		t.Fatal("a failed migration did not fail the stage")
	}
	if s.Restored == nil {
		t.Fatalf("the restore did not happen, so the previous code is not what runs:\n%v", err)
	}
	if !s.Restored.RolledOut() {
		t.Fatalf("the restore reports %+v, want the backend rolled out at the restored revision", *s.Restored)
	}
	if s.Restored.Ready != 0 {
		t.Fatalf("the fake reports %d ready replicas, so this arm is not exercising the unready case", s.Restored.Ready)
	}
	// THE MESSAGE SAYS BOTH THINGS: the code is back, and readiness is a separate
	// observation with its own reason.
	if strings.Contains(err.Error(), "running nothing") {
		t.Errorf("the failure tells the operator the control plane is running nothing, although the previous code was restored:\n%v", err)
	}
	if !strings.Contains(err.Error(), "not Ready") {
		t.Errorf("the failure does not say the restored backend is not Ready yet:\n%v", err)
	}
	if applied := kube.lastApply(t); applied.chartContent != base64.StdEncoding.EncodeToString(ChartArchive()) {
		t.Error("the last apply did not carry the chart this binary embeds")
	}
}

// A FENCE WITH NO RECORDED IMAGE CANNOT BE RESTORED, and says so rather than
// applying a chart at whatever image happens to be pinned now: that would be the
// NEW image against the OLD schema, which is the failure the whole procedure
// exists to prevent.
func TestARestoreWithoutARecordedImageRefuses(t *testing.T) {
	ctx := context.Background()
	kube := newPreviousKube(t)
	kube.fenceFacts = map[string]string{fenceRevisionOnly: "x"}
	s := migrationSession(t, kube, "domain: kn.example.com\n")

	err := stageMigration(ctx, s)
	if err == nil {
		t.Fatal("a failed migration did not fail the stage")
	}
	if !strings.Contains(err.Error(), fencePreviousImageAnnotation) {
		t.Errorf("the refusal does not name the annotation that is missing:\n%v", err)
	}
	if s.Restored != nil {
		t.Error("a restore was reported although no image was recorded")
	}
}

// fenceRevisionOnly is an annotation map with something in it, so the fence is
// present but carries no image.
const fenceRevisionOnly = fenceRaiseAnnotation

// A RE-RUN THAT ADOPTS THE FENCE BRINGS THE BACKEND BACK AT THE COUNT THE FENCE
// RECORDED. On hardware (2026-09-26) the identical command after a failed
// migration adopted the fence without reading a replica count, its migration
// stage stopped the backend, and its chart stage then read the Deployment's
// CURRENT count — zero — and applied the new chart with no backend, so the
// validation waited out its deadline against nothing.
func TestAnAdoptedFenceSuppliesTheBackendReplicaCount(t *testing.T) {
	ctx := context.Background()
	kube := newPreviousKube(t)
	kube.fenceFacts = map[string]string{
		fencePreviousImageAnnotation:    "ghcr.io/kubenesthq/kubenest-backend@sha256:abc",
		fencePreviousReplicasAnnotation: "2",
	}
	got, err := adoptedBackendReplicas(ctx, kube, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("a re-run adopting the fence would run the backend at %d replica(s), want the 2 the fence recorded", got)
	}
	// A count this run already recorded is kept.
	if got, _ := adoptedBackendReplicas(ctx, kube, 3); got != 3 {
		t.Errorf("a recorded count of 3 was replaced by %d", got)
	}
}

// A RESUME THAT FINDS A STOPPED BACKEND PUTS ONE BACK BEFORE IT READS
// (kn-t70-control-plane-version-identity-4xso.3).
//
// The migration stage's first apply stops the backend — `backend.replicas: 0`
// with the fence up — and an interrupt after that apply, or a laptop that died,
// leaves the cluster there with no process left to put one back. Every read a
// resume then makes (the record, the window, the bundle manifests) needs a
// running control plane, so the resume died on its FIRST read: `control plane
// kubenest-backend is unreachable: ... connect failed ("Connection refused")`
// (hardware, 2026-09-26).
//
// WHICH CODE COMES BACK IS THE CLUSTER'S ANSWER, because a second laptop holds
// no journal: the migration Job is the only witness to how far the schema moved.
const resumeValues = "domain: kn.example.com\njwtSecret: s\nbackend:\n  admin:\n    email: a@b.c\n"

// recoverLikeTheCommand runs the recovery the way runControlPlaneUpgrade does:
// it asks the cluster whether a backend is stopped behind a fence, and only then
// spends the values read. Tests go through both steps so the PAIR — the check
// that decides whether to act, and the act — is what they exercise.
func recoverLikeTheCommand(ctx context.Context, kube *previousKube, values string) error {
	facts, stopped, err := StoppedBackendBehindAFence(ctx, kube)
	if err != nil || !stopped {
		return err
	}
	return RecoverStoppedBackend(ctx, kube, RecoveryOptions{
		Values:   values,
		Facts:    facts,
		Deadline: 2 * time.Second,
		Every:    time.Millisecond,
		Reporter: converge.ReporterFunc(func(converge.Event) {}),
	})
}

// broughtBack is what one chart apply put behind the fence: which code the backend
// Deployment is HELD on, whether the fence stays up, whether the migration Job is
// on, and at what size the backend runs. Decoded once, so each test reads as the
// question it asks.
type broughtBack struct {
	repository string
	tag        string
	digest     string
	fenceUp    bool
	migration  bool
	replicas   any
}

// pinned reports whether the apply held the backend Deployment on an image of its
// own instead of taking the chart's own pin, which is the whole difference
// between the previous code and the new one.
func (b broughtBack) pinned() bool {
	return b.repository != "" || b.tag != "" || b.digest != ""
}

func decodeBroughtBack(t *testing.T, applied appliedRelease) broughtBack {
	t.Helper()
	var out broughtBack
	if fence, ok := applied.values["fence"].(map[string]any); ok {
		out.fenceUp, _ = fence["enabled"].(bool)
	}
	if migration, ok := applied.values["migration"].(map[string]any); ok {
		out.migration, _ = migration["enabled"].(bool)
	}
	backend, _ := applied.values["backend"].(map[string]any)
	if backend == nil {
		t.Fatalf("the applied values carry no backend group: %v", applied.values)
	}
	out.replicas = backend["replicas"]
	// The HELD pin: the image the fence or the restore holds the Deployment on.
	if image, ok := backend["heldImage"].(map[string]any); ok {
		out.repository, _ = image["repository"].(string)
		out.tag, _ = image["tag"].(string)
		out.digest, _ = image["digest"].(string)
	}
	return out
}

// NO MIGRATION JOB MEANS THE SCHEMA HAS NOT MOVED, so the code that matches it
// is the code the control plane ran before this upgrade — and only the fence
// knows that image, because the chart this binary carries pins the new one.
func TestAResumeWithTheBackendStoppedAndNoMigrationRestoresThePreviousCode(t *testing.T) {
	ctx := context.Background()
	kube := newPreviousKube(t)
	kube.fenceFacts = map[string]string{
		fencePreviousImageAnnotation:    previousBackendImage,
		fencePreviousReplicasAnnotation: "2",
	}
	kube.backendReplicas = 0
	kube.migrationCondition = "" // the Job was never created

	if err := recoverLikeTheCommand(ctx, kube, resumeValues); err != nil {
		t.Fatal(err)
	}

	applied := kube.lastApply(t)
	// THE CHART THIS BINARY CARRIES, because only it has the fence template: the
	// previous release's chart predates the fence entirely, so re-applying it
	// with the fence value would render the api route back onto the backend.
	if applied.chartContent != base64.StdEncoding.EncodeToString(ChartArchive()) {
		t.Error("the recovery did not apply the chart this binary embeds, which is the only one that can keep the fence up over the old code")
	}
	got := decodeBroughtBack(t, applied)
	want := parsePostgresImage(previousBackendImage)
	if got.repository != want.repository || got.tag != want.tag {
		t.Errorf("the recovery pinned backend.image %s:%s, want the previous code the fence recorded (%s)", got.repository, got.tag, previousBackendImage)
	}
	if !got.fenceUp {
		t.Error("the recovery lowered the fence, and the code it put back has not been validated")
	}
	if got.migration {
		t.Error("the recovery re-enabled the migration Job over a schema that has not moved")
	}
	if got.replicas != 2 {
		t.Errorf("the recovery ran the backend at %v replica(s), want the 2 the fence recorded", got.replicas)
	}
	// AND NOTHING WAS APPLIED WHILE THE JOB WAS... there was no Job at all, so
	// the first apply is the recovery's own.
	if len(kube.appliedAt) != 1 {
		t.Errorf("the recovery made %d applies, want exactly the one that brought the backend back", len(kube.appliedAt))
	}
}

// A MIGRATION THAT SUCCEEDED MEANS THE SCHEMA IS THE NEW ONE, so the NEW code
// goes back — fence up, the migration Job still enabled so the operator keeps
// the record of the schema step, and the size the fence recorded.
func TestAResumeAfterASucceededMigrationBringsBackTheNewCode(t *testing.T) {
	ctx := context.Background()
	kube := newPreviousKube(t)
	kube.fenceFacts = map[string]string{
		fencePreviousImageAnnotation:    previousBackendImage,
		fencePreviousReplicasAnnotation: "2",
	}
	kube.backendReplicas = 0
	kube.migrationCondition = "Complete"

	if err := recoverLikeTheCommand(ctx, kube, resumeValues); err != nil {
		t.Fatal(err)
	}

	got := decodeBroughtBack(t, kube.lastApply(t))
	if got.pinned() {
		t.Errorf("the recovery pinned backend.image %s:%s@%s over a migrated schema, want no override at all: the chart's own pin is the new code the migration moved to",
			got.repository, got.tag, got.digest)
	}
	if !got.fenceUp {
		t.Error("the recovery lowered the fence, and the code it put back has not been validated")
	}
	if !got.migration {
		t.Error("the recovery turned the migration Job off, so the release would lose the operator-visible record of the schema step")
	}
	if got.replicas != 2 {
		t.Errorf("the recovery ran the backend at %v replica(s), want the 2 the fence recorded", got.replicas)
	}
}

// NOTHING IS STARTED WHILE THE MIGRATION RUNS. A backend against a schema in
// flight is the state this whole procedure exists to prevent, so the recovery
// waits for the verdict and acts on the outcome — here a failure, which puts the
// previous code back.
func TestAResumeWaitsForARunningMigrationBeforeStartingAnyBackend(t *testing.T) {
	ctx := context.Background()
	kube := newPreviousKube(t)
	kube.fenceFacts = map[string]string{
		fencePreviousImageAnnotation:    previousBackendImage,
		fencePreviousReplicasAnnotation: "2",
	}
	kube.backendReplicas = 0
	kube.migrationFailsAfter = 1 // running on the first read, failed on every one after

	if err := recoverLikeTheCommand(ctx, kube, resumeValues); err != nil {
		t.Fatal(err)
	}

	if !kube.migrationRunningSeen {
		t.Fatal("the fake never reported a running Job, so this arm never exercised the wait")
	}
	if kube.migrationFailedAt == 0 {
		t.Fatal("the fake never reported the Job's failure, so the recovery never saw the outcome it had to act on")
	}
	if len(kube.appliedAt) == 0 {
		t.Fatal("the recovery applied nothing, so the failed migration was not answered at all")
	}
	if kube.appliedAt[0] < kube.migrationFailedAt {
		t.Errorf("a chart was applied at command %d, before the migration Job's failure was observed at command %d: a backend must never be started while the schema is moving",
			kube.appliedAt[0], kube.migrationFailedAt)
	}
	got := decodeBroughtBack(t, kube.lastApply(t))
	want := parsePostgresImage(previousBackendImage)
	if got.repository != want.repository || got.tag != want.tag {
		t.Errorf("after the failure the recovery pinned backend.image %s:%s, want the previous code (%s)", got.repository, got.tag, previousBackendImage)
	}
	if !got.fenceUp {
		t.Error("the recovery lowered the fence after a failed migration")
	}
}

// A RUN THAT NEEDS NO RECOVERY MAKES NONE. A fence that is not up is the
// ordinary run, and a fence with a backend still running behind it is a resume
// after an interrupt at the checkpoint: in both, the reads the caller is about
// to make reach a control plane, and applying a chart here would be an
// unrecorded change before the gates.
func TestARunWithTheFenceDownOrABackendRunningNeedsNoRecovery(t *testing.T) {
	ctx := context.Background()
	for _, arm := range []struct {
		name     string
		fence    map[string]string
		replicas int32
	}{
		{"no fence at all", nil, 0},
		{"a backend running behind the fence", map[string]string{fencePreviousImageAnnotation: previousBackendImage}, 2},
	} {
		t.Run(arm.name, func(t *testing.T) {
			kube := newPreviousKube(t)
			kube.fenceFacts = arm.fence
			kube.backendReplicas = arm.replicas

			if _, stopped, err := StoppedBackendBehindAFence(ctx, kube); err != nil {
				t.Fatal(err)
			} else if stopped {
				t.Fatalf("%s was read as a stopped backend, and a recovery there would apply a chart over a control plane that is up", arm.name)
			}
			if err := recoverLikeTheCommand(ctx, kube, resumeValues); err != nil {
				t.Fatal(err)
			}
			if len(kube.applied) != 0 {
				t.Errorf("%s: the recovery applied %d chart(s), want none", arm.name, len(kube.applied))
			}
		})
	}
}
