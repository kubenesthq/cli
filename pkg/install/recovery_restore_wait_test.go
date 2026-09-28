package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/recovery"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/sshx"
)

// THE HARDWARE DEFECT THESE TESTS ARE ABOUT (lab s6, 2026-09-28). Stage 16 ran
// on a host that had just been built, asked Velero for the backup the recovery
// set named, and failed with "backup manual-20260928-113029 is not on this
// cluster: nothing has been changed". Nothing was wrong with the backup: the
// bucket held it, and Velero's backup-sync controller had simply not listed it
// yet. On that cluster the Velero Deployment was created at 11:34:52Z and the
// location validated, with status.lastSyncedTime, at 11:36:58Z — the moment the
// Backup object appeared. A fresh cluster reads the bucket's backups in on
// Velero's own schedule, and only after the storage location validates, so a
// recovery that demands the backup on its first look is racing a controller it
// installed seconds earlier.
const (
	recoveryWaitBackup    = "manual-20260928-113029"
	recoveryWaitNamespace = "s6-data"
	// recoveryWaitSyncedAt is the measured first sync of the location on that
	// cluster, and the last validation before it.
	recoveryWaitSyncedAt    = "2026-09-28T11:36:58Z"
	recoveryWaitValidatedAt = "2026-09-28T11:34:52Z"
)

// fakeRestoreClock is the clock the wait is driven on, injected in place of the
// real one: no test sleeps for the ten minutes the bundle allows a component to
// become ready, and the test can read how far the wait got.
type fakeRestoreClock struct {
	now   time.Time
	slept int
}

func injectRestoreClock(t *testing.T, poll time.Duration) *fakeRestoreClock {
	t.Helper()
	c := &fakeRestoreClock{now: time.Date(2026, 9, 28, 11, 37, 0, 0, time.UTC)}
	previous := recoveryRestoreSync
	recoveryRestoreSync = recoveryRestoreClock{
		Now:  func() time.Time { return c.now },
		Poll: poll,
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			c.now = c.now.Add(d)
			c.slept++
			return nil
		},
	}
	t.Cleanup(func() { recoveryRestoreSync = previous })
	return c
}

// syncingVelero is a cluster mid-recovery: Velero is installed, the storage
// location has validated, and the backup the recovery set names arrives on the
// cluster a few reads later — which is what the backup-sync controller does on
// its own schedule. It answers the reads and writes backup.RunRestore makes, so
// a stage test can follow the restore into its first changes.
type syncingVelero struct {
	// notListedFor is how many reads of the backup report NotFound before
	// Velero's backup-sync has copied it in. Zero models a cluster whose Velero
	// has already listed it.
	notListedFor int
	// locationPhase and locationMessage are what Velero says about the storage
	// location.
	locationPhase   string
	locationMessage string
	// projectAbsentFor is how many times the cluster is asked about the
	// namespace's Project before the operator has created it. Zero models a
	// cluster whose operator has already written the CR.
	projectAbsentFor int

	listed int
	// projectReads counts the looks at that Project: every read, and the read
	// `kubectl annotate` makes before it patches. It is the only clock this
	// fake has for "the operator has created it by now".
	projectReads int
	// opID is the operation id the restore wrote in its pause annotation on the
	// Project, which is what its own acknowledgement has to carry.
	opID string
	// record is the last operation record document the CLI wrote, which the next
	// read of it has to answer with.
	record []byte
	rv     int
}

// mount wires the fake to the session's runner.
func (c *syncingVelero) mount(runner *recordingRunner) {
	runner.respond = func(command string) (sshx.Result, error) { return c.answer(runner, command) }
}

func (c *syncingVelero) answer(runner *recordingRunner, command string) (sshx.Result, error) {
	switch {
	// The operation record: the CLI writes it with a create or a replace and
	// reads it back before every change, so the fake has to keep what it was
	// given and answer with the resourceVersion each write returned.
	case isOperationRecordWrite(command):
		c.record = lastInput(runner)
		c.rv++
		return sshx.Result{Stdout: fmt.Sprintf(`{"metadata":{"resourceVersion":%q}}`, strconv.Itoa(c.rv))}, nil
	case strings.Contains(command, "get configmap "+operation.Name+" -n "+operation.Namespace):
		if c.record == nil {
			return notFound("configmaps", operation.Name), nil
		}
		return sshx.Result{Stdout: c.recordDocument()}, nil

	// The backup the recovery restores. The bucket holds it; whether Velero has
	// listed it is exactly what the stage under test has to wait for.
	case strings.Contains(command, "get backup "+recoveryWaitBackup+" -n "+backup.Namespace+" -o json"):
		c.listed++
		if c.listed <= c.notListedFor {
			return notFound("backups.velero.io", recoveryWaitBackup), nil
		}
		return sshx.Result{Stdout: c.backupDocument()}, nil
	// The restore's own safety backup settles immediately; it is not what this
	// test is about.
	case strings.Contains(command, "get backup kubenest-safety-"):
		return sshx.Result{Stdout: `{"status":{"phase":"Completed"}}`}, nil
	case strings.Contains(command, "get backupstoragelocations.velero.io -n "+backup.Namespace):
		return sshx.Result{Stdout: `{"items":[{"metadata":{"name":` + strconv.Quote(backup.StorageLocationName) + `},"status":{"phase":` + strconv.Quote(c.locationPhase) + `}}]}`}, nil
	case strings.Contains(command, "get backupstoragelocation "+backup.StorageLocationName+" -n "+backup.Namespace):
		return sshx.Result{Stdout: c.locationDocument()}, nil
	case strings.Contains(command, "get configmap "+backup.CoverageRecordName(recoveryWaitBackup)+" -n "+backup.Namespace):
		return sshx.Result{Stdout: c.coverageDocument()}, nil
	case strings.Contains(command, "get podvolumebackups.velero.io"),
		strings.Contains(command, "get datauploads.velero.io"),
		strings.Contains(command, "get volumesnapshots.snapshot.storage.k8s.io"):
		return sshx.Result{Stdout: `{"items":[]}`}, nil

	// The operator that has to acknowledge the pause this restore writes.
	case strings.Contains(command, "get deployment "+agent.DeploymentName+" -n "+backup.ProjectCRNamespace):
		return sshx.Result{Stdout: `{"metadata":{"labels":{"helm.sh/chart":` + strconv.Quote(agent.ChartName+"-2.7.0") + `}}}`}, nil
	case strings.Contains(command, "get project "+recoveryWaitNamespace+" -n "+backup.ProjectCRNamespace):
		c.projectReads++
		if !c.projectPresent() {
			return notFound("projects.kubenest.io", recoveryWaitNamespace), nil
		}
		return sshx.Result{Stdout: c.holdDocument()}, nil
	case strings.Contains(command, "annotate project "+recoveryWaitNamespace):
		// kubectl annotate READS the object before it patches it, so annotating
		// a Project the operator has not created yet is the NotFound the
		// hardware produced: `kubectl annotate project s6-data -n
		// kubenest-system ... exit 1`.
		c.projectReads++
		if !c.projectPresent() {
			return notFound("projects.kubenest.io", recoveryWaitNamespace), nil
		}
		c.opID = pauseOpID(command)
		return sshx.Result{}, nil

	// The namespace went with the host, so the restore recreates it, and the
	// restore's own Velero Restore completes at once.
	case strings.Contains(command, "get namespace "+recoveryWaitNamespace+" -o json"):
		return notFound("namespaces", recoveryWaitNamespace), nil
	case strings.Contains(command, "get restore "):
		return sshx.Result{Stdout: `{"status":{"phase":"Completed","progress":{"itemsRestored":1}}}`}, nil

	// Everything else the plan and the restore read back is an empty list: this
	// cluster has a namespace with no workloads of its own.
	case strings.Contains(command, "get "):
		return sshx.Result{Stdout: `{"items":[]}`}, nil
	}
	return sshx.Result{}, nil
}

// projectPresent reports whether the operator has created the Project of the
// namespace by now: the first projectAbsentFor looks at it report NotFound, and
// the object is there from the next one on. Counting LOOKS and not seconds is
// the only clock a fake runner has.
func (c *syncingVelero) projectPresent() bool { return c.projectReads > c.projectAbsentFor }

// recordDocument is what a read of the operation record has to answer: the
// document the CLI last wrote, at the resourceVersion the write returned. A
// mismatched version is what the CLI refuses as a stale handle.
func (c *syncingVelero) recordDocument() string {
	var doc map[string]any
	if err := json.Unmarshal(c.record, &doc); err != nil {
		return string(c.record)
	}
	metadata, _ := doc["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		doc["metadata"] = metadata
	}
	metadata["resourceVersion"] = strconv.Itoa(c.rv)
	out, err := json.Marshal(doc)
	if err != nil {
		return string(c.record)
	}
	return string(out)
}

// backupDocument is the Velero Backup the backup-sync controller copies in from
// the bucket: completed, naming the one location kubenest manages.
func (c *syncingVelero) backupDocument() string {
	return `{"metadata":{"name":` + strconv.Quote(recoveryWaitBackup) + `},` +
		`"spec":{"storageLocation":` + strconv.Quote(backup.StorageLocationName) + `},` +
		`"status":{"phase":"Completed",` +
		`"startTimestamp":` + strconv.Quote("2026-09-28T11:30:29Z") + `,` +
		`"completionTimestamp":` + strconv.Quote("2026-09-28T11:31:10Z") + `}}`
}

// locationDocument is the measured shape of the location on that cluster.
func (c *syncingVelero) locationDocument() string {
	doc := map[string]any{
		"metadata": map[string]any{"name": backup.StorageLocationName},
		"status": map[string]any{
			"phase":              c.locationPhase,
			"lastValidationTime": recoveryWaitValidatedAt,
			"lastSyncedTime":     recoveryWaitSyncedAt,
		},
	}
	if c.locationMessage != "" {
		doc["status"].(map[string]any)["message"] = c.locationMessage
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// coverageDocument is the expected-coverage record the operator writes for a
// backup: the namespace it covers, with no volumes, because a namespace with
// nothing to copy is still covered and eligible.
func (c *syncingVelero) coverageDocument() string {
	record, err := json.Marshal(map[string]any{
		"backup": recoveryWaitBackup,
		"namespaces": []map[string]any{{
			"name":    recoveryWaitNamespace,
			"uid":     "namespace-uid",
			"volumes": []map[string]any{},
		}},
	})
	if err != nil {
		return "{}"
	}
	doc, err := json.Marshal(map[string]any{"data": map[string]string{"coverage.json": string(record)}})
	if err != nil {
		return "{}"
	}
	return string(doc)
}

// holdDocument is the Project the restore pauses. Before the pause annotation
// has been written the project holds nothing; afterwards the condition carries
// the operation the restore asked it to pause for.
func (c *syncingVelero) holdDocument() string {
	if c.opID == "" {
		return `{"status":{"conditions":[]}}`
	}
	return `{"status":{"conditions":[{"type":` + strconv.Quote(backup.ConditionReconcilePaused) +
		`,"status":"True","reason":` + strconv.Quote(backup.ReasonPausedByOperation) +
		`,"message":` + strconv.Quote("holding reconciliation for operation "+c.opID) + `}]}}`
}

func notFound(kind, name string) sshx.Result {
	return sshx.Result{ExitCode: 1, Stderr: fmt.Sprintf("Error from server (NotFound): %s %q not found", kind, name)}
}

func isOperationRecordWrite(command string) bool {
	return strings.Contains(command, "create -f - -o json") || strings.Contains(command, "replace -f - -o json")
}

func lastInput(runner *recordingRunner) []byte {
	if len(runner.inputs) == 0 {
		return nil
	}
	return runner.inputs[len(runner.inputs)-1]
}

// pauseOpID reads the operation id out of the pause annotation the restore
// wrote, which its own acknowledgement has to name.
func pauseOpID(command string) string {
	marker := backup.PauseAnnotationKey + "="
	at := strings.Index(command, marker)
	if at < 0 {
		return ""
	}
	value := command[at+len(marker):]
	if end := strings.IndexByte(value, ' '); end >= 0 {
		value = value[:end]
	}
	return value
}

// recoveryWaitSession builds one recovery of the measured shape: the bucket
// holds the hardware's own backup, covering one namespace, and the cluster is
// described by the fake. The bundle is 1.2's own manifest — the bundle the
// hardware ran, whose limits.timeouts.component-ready is the bound the wait
// takes — rather than a hand-written one, so the test asserts against the
// deadline the product ships.
func recoveryWaitSession(t *testing.T, cluster *syncingVelero) (*Session, *recordingRunner, *fakeRestoreClock) {
	t.Helper()
	f := newRecoveryFixture(t, "01a02362-f8a3-7dd6-aa07-2f10ed7a5c50", "org-1", "inst-1", nil)
	f.publish(t, f.bind, "20260928T113029Z-cccccccc", f.secrets, nil, true, recoverykit.Backup{
		Name:        recoveryWaitBackup,
		CompletedAt: time.Date(2026, 9, 28, 11, 30, 29, 0, time.UTC),
		Coverage:    []string{recoveryWaitNamespace},
		Status:      "Completed",
	})
	sel, err := recovery.Select(context.Background(), f.bucket, recoveryTestScope, f.request())
	if err != nil {
		t.Fatalf("selecting the recovery set the hardware's backup lives in: %v", err)
	}
	if sel.Backup.Name != recoveryWaitBackup || len(sel.Backup.Coverage) != 1 || sel.Backup.Coverage[0] != recoveryWaitNamespace {
		t.Fatalf("the selection is %s covering %v, so this test is not about the measured backup", sel.Backup.Name, sel.Backup.Coverage)
	}
	s, runner := recoverySession(t, recoveryOptions(f, f.fleet.SecretKeyString()))
	bundle, err := bundles.Manifest("1.2")
	if err != nil {
		t.Fatalf("reading the 1.2 bundle manifest this binary carries: %v", err)
	}
	s.Bundle = bundle
	s.Out = &bytes.Buffer{}
	s.recoverySel = sel
	cluster.mount(runner)
	clock := injectRestoreClock(t, time.Minute)
	return s, runner, clock
}

func stageOutput(t *testing.T, s *Session) string {
	t.Helper()
	out, ok := s.Out.(*bytes.Buffer)
	if !ok {
		t.Fatal("the session's output is not the buffer this test gave it")
	}
	return out.String()
}

func requireContains(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s does not say %q, so a reader cannot tell what it was waiting for:\n%s", what, want, text)
		}
	}
}

// TestTheRestoreWaitsForVeleroToListTheBackup is the defect: the bucket holds
// the backup, Velero's backup-sync lists it two reads later, and the stage has
// to wait for that rather than asking RunRestore for a backup the cluster does
// not have yet.
//
// THE PLANTED NEGATIVE IS THIS SAME FAKE ON THE OLD CODE: the stage reaches
// RunRestore at once, and `backup manual-20260928-113029 is not on this
// cluster: nothing has been changed` is the failure the hardware produced.
func TestTheRestoreWaitsForVeleroToListTheBackup(t *testing.T) {
	cluster := &syncingVelero{notListedFor: 2, locationPhase: "Available"}
	s, runner, clock := recoveryWaitSession(t, cluster)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the stage failed on a backup Velero listed a moment later, which is the hardware defect: %v", err)
	}

	// It waited, and it said what it was waiting for: the backup, and the state
	// of the location Velero would sync through.
	requireContains(t, "the wait's progress", stageOutput(t, s), recoveryWaitBackup, "backupstoragelocation "+backup.StorageLocationName, "Available")
	if clock.slept == 0 {
		t.Fatal("the stage restored without once looking again, so nothing waited for Velero's backup-sync")
	}
	if cluster.listed <= cluster.notListedFor {
		t.Fatalf("the backup was read %d time(s) and reported missing on the first %d, so the run never saw it appear", cluster.listed, cluster.notListedFor)
	}

	// And then it restored: the restores are what the stage recorded, and the
	// pause is the first change a namespace restore makes.
	if !containsString(s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace) {
		t.Errorf("the stage restored %v, and the namespace the backup covers (%s) is not among them", s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace)
	}
	if !runner.ranAny("annotate project " + recoveryWaitNamespace) {
		t.Errorf("the restore never paused %s, so no restore ran after the wait:\n%s", recoveryWaitNamespace, runner.ranAll())
	}
}

// TestTheRestoreFailsNamingTheLocationWhenVeleroNeverListsTheBackup is the
// other half: the bound runs out, and what the operator is told is why. A
// location that cannot reach the store is reported AS THAT, with Velero's own
// message — not as a backup missing from the cluster, which is what the
// hardware said and what sent a reader looking for a backup that was never the
// problem.
func TestTheRestoreFailsNamingTheLocationWhenVeleroNeverListsTheBackup(t *testing.T) {
	cluster := &syncingVelero{
		notListedFor:    1000,
		locationPhase:   "Unavailable",
		locationMessage: "AccessDenied: Access Denied: failed to list objects",
	}
	s, runner, clock := recoveryWaitSession(t, cluster)
	start := clock.now

	err := stageRecoveryRestore(context.Background(), s)
	if err == nil {
		t.Fatal("the stage reported success on a backup Velero never listed")
	}
	if strings.Contains(err.Error(), "is not on this cluster") {
		t.Fatalf("the failure is the plain missing-backup refusal the hardware produced, which says nothing about why Velero never listed it: %v", err)
	}
	requireContains(t, "the failure", err.Error(), recoveryWaitBackup, backup.StorageLocationName, "Unavailable", "AccessDenied", recoveryWaitValidatedAt)
	if !strings.Contains(err.Error(), "did not start") {
		t.Errorf("the failure does not say the restore did not start, so a reader cannot tell how far the recovery got: %v", err)
	}

	// The wait was bounded, not endless.
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		t.Fatalf("the 1.2 manifest has no component-ready deadline: %v", err)
	}
	if clock.now.Before(start.Add(deadline)) {
		t.Errorf("the wait gave up after %s of a %s bound, so it did not wait for Velero as long as the bundle allows", clock.now.Sub(start), deadline)
	}

	// And NOTHING was restored: no pause, no Restore, no namespace recorded. A
	// recovery that fails must be a recovery that changed nothing here.
	if runner.ranAny("annotate", "delete ", "apply -f -", "create -f -", "replace -f -") {
		t.Errorf("a recovery whose wait ran out still changed the cluster:\n%s", runner.ranAll())
	}
	for i, input := range runner.inputs {
		if len(input) == 0 {
			continue
		}
		if strings.Contains(string(input), `"kind":"Restore"`) || strings.Contains(string(input), "kind: Restore") {
			t.Errorf("a Velero Restore was created (input %d) although Velero never listed the backup:\n%s", i, input)
		}
	}
	if len(s.Record.RecoveryNamespacesRestored) != 0 {
		t.Errorf("namespaces were recorded as restored (%v) although the wait ran out", s.Record.RecoveryNamespacesRestored)
	}
}

// TestARestoreThatFindsTheBackupListedDoesNotWait guards the other direction: a
// resumed recovery, or one run against a cluster whose Velero has already
// synced, finds the backup on its first look and goes straight on. A wait that
// always spent its bound would make every resume of a healthy recovery slower
// than the recovery itself.
func TestARestoreThatFindsTheBackupListedDoesNotWait(t *testing.T) {
	cluster := &syncingVelero{notListedFor: 0, locationPhase: "Available"}
	s, runner, clock := recoveryWaitSession(t, cluster)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the stage failed on a backup the cluster already listed: %v", err)
	}
	if clock.slept != 0 {
		t.Errorf("the stage waited %d time(s) for a backup that was already on the cluster", clock.slept)
	}
	if out := stageOutput(t, s); strings.Contains(out, "backupstoragelocation "+backup.StorageLocationName) {
		t.Errorf("the stage logged a wait for a backup that was already listed:\n%s", out)
	}
	if !containsString(s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace) {
		t.Errorf("the stage restored %v, and the namespace the backup covers (%s) is not among them", s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace)
	}
	if !runner.ranAny("annotate project " + recoveryWaitNamespace) {
		t.Errorf("the restore never paused %s, so no restore ran:\n%s", recoveryWaitNamespace, runner.ranAll())
	}

	// A RESUME IS THE SAME PATH: the namespaces this operation already restored
	// are skipped, and the wait in front of that loop has to be harmless for a
	// backup that is already on the cluster. It is the same first look, so a
	// resumed run neither waits nor touches the cluster again.
	before := len(runner.ran)
	restored := len(s.Record.RecoveryNamespacesRestored)
	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("a resumed recovery failed on a backup the cluster already lists: %v", err)
	}
	if clock.slept != 0 {
		t.Errorf("a resumed recovery waited %d time(s) for a backup that was already on the cluster", clock.slept)
	}
	if again := strings.Join(runner.ran[before:], "\n"); strings.Contains(again, "annotate") {
		t.Errorf("a resumed recovery restored again what it had already restored:\n%s", again)
	}
	if len(s.Record.RecoveryNamespacesRestored) != restored {
		t.Errorf("a resumed recovery recorded %v, and the namespaces it had already restored are listed twice", s.Record.RecoveryNamespacesRestored)
	}
}

// TestTheRestoreWaitsForTheProjectItsOwnerCreates is the second half of the
// hardware defect (lab s6, 2026-09-28). The restore's first change is the
// reconcile-pause annotation on the namespace's Project, and on a cluster built
// minutes earlier that Project does not exist yet: the control plane re-delivers
// a cluster's projects once its new machine has registered, and the operator
// then creates each Project CR from the desired state it holds. So the restore
// has to wait for `get project s6-data -n kubenest-system` to answer before it
// starts, rather than annotating an object that is not there.
//
// THE PLANTED NEGATIVE IS THIS SAME FAKE ON THE OLD CODE: the stage goes
// straight to RunRestore, whose first change is the annotation, and kubectl
// answers NotFound — "annotate project s6-data -n kubenest-system ... exit 1",
// the failure the hardware produced, which names the tool rather than the
// missing object.
func TestTheRestoreWaitsForTheProjectItsOwnerCreates(t *testing.T) {
	cluster := &syncingVelero{locationPhase: "Available", projectAbsentFor: 2}
	s, runner, clock := recoveryWaitSession(t, cluster)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the stage failed on a Project the operator created a moment later, which is the hardware defect: %v", err)
	}

	// It waited for the Project, and it said what it was waiting for: the
	// namespace and the re-delivery that brings it back.
	if clock.slept == 0 {
		t.Fatal("the stage restored without once looking again, so nothing waited for the operator to create the Project")
	}
	if cluster.projectReads <= cluster.projectAbsentFor {
		t.Fatalf("the Project was looked at %d time(s) and was absent on the first %d, so the run never saw the operator create it", cluster.projectReads, cluster.projectAbsentFor)
	}
	requireContains(t, "the wait's progress", stageOutput(t, s),
		backup.ProjectCRNamespace+"/"+recoveryWaitNamespace, "re-delivers")

	// And then it restored, with the pause on the Project the operator wrote.
	if !containsString(s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace) {
		t.Errorf("the stage restored %v, and the namespace the backup covers (%s) is not among them", s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace)
	}
	if !runner.ranAny("annotate project " + recoveryWaitNamespace) {
		t.Errorf("the restore never paused %s, so no restore ran after the wait:\n%s", recoveryWaitNamespace, runner.ranAll())
	}
}

// TestTheRestoreFailsNamingTheNamespaceWhenTheProjectNeverArrives is what
// happens when the control plane never re-delivers the project: the bound runs
// out and the operator is told which namespace came back empty and that running
// the same command again is the resume.
//
// THE PLANTED NEGATIVE IS WHAT THE HARDWARE SAID: on the old code the stage
// fails with the raw kubectl annotate failure, which names neither the
// namespace as a whole nor the fact that nothing was restored for it, and reads
// as a bug in the CLI rather than a project that has not arrived.
func TestTheRestoreFailsNamingTheNamespaceWhenTheProjectNeverArrives(t *testing.T) {
	cluster := &syncingVelero{locationPhase: "Available", projectAbsentFor: 1000}
	s, runner, clock := recoveryWaitSession(t, cluster)
	start := clock.now

	err := stageRecoveryRestore(context.Background(), s)
	if err == nil {
		t.Fatal("the stage reported success on a namespace whose Project never arrived")
	}
	if strings.Contains(err.Error(), "annotate project") {
		t.Fatalf("the failure is the raw kubectl annotate failure the hardware produced, which names the tool rather than the Project that never arrived: %v", err)
	}
	requireContains(t, "the failure", err.Error(),
		recoveryWaitNamespace, backup.ProjectCRNamespace,
		"never arrived on the rebuilt cluster",
		"nothing was restored for namespace "+recoveryWaitNamespace,
		"identical command")

	// The wait was bounded, not endless.
	deadline, err := s.Bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		t.Fatalf("the 1.2 manifest has no component-ready deadline: %v", err)
	}
	if clock.now.Before(start.Add(deadline)) {
		t.Errorf("the wait gave up after %s of a %s bound, so it did not wait for the project as long as the bundle allows", clock.now.Sub(start), deadline)
	}

	// And NOTHING was restored: no pause, no Restore, no namespace recorded.
	// The namespace is not even recorded as not-restored, because the failure is
	// resumable: the same command waits for the project again and restores then.
	if runner.ranAny("annotate", "delete ", "apply -f -", "create -f -", "replace -f -") {
		t.Errorf("a recovery whose wait for the project ran out still changed the cluster:\n%s", runner.ranAll())
	}
	for i, input := range runner.inputs {
		if len(input) == 0 {
			continue
		}
		if strings.Contains(string(input), `"kind":"Restore"`) || strings.Contains(string(input), "kind: Restore") {
			t.Errorf("a Velero Restore was created (input %d) although the namespace's Project never arrived:\n%s", i, input)
		}
	}
	if len(s.Record.RecoveryNamespacesRestored) != 0 {
		t.Errorf("namespaces were recorded as restored (%v) although the wait for the project ran out", s.Record.RecoveryNamespacesRestored)
	}
	if len(s.Record.RecoveryNamespacesNotRestored) != 0 {
		t.Errorf("the namespace was recorded as not restorable (%v), and a resume would then leave it held instead of restoring it", s.Record.RecoveryNamespacesNotRestored)
	}
}

// TestARestoreWhoseProjectIsAlreadyThereDoesNotWait guards the other direction:
// a cluster whose operator has already written the Project — and every resume —
// restores without spending a look's sleep on it. A wait that always spent its
// bound would make every resume of a healthy recovery slower than the recovery
// itself.
func TestARestoreWhoseProjectIsAlreadyThereDoesNotWait(t *testing.T) {
	cluster := &syncingVelero{locationPhase: "Available"}
	s, runner, clock := recoveryWaitSession(t, cluster)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the stage failed although the Project its restore pauses was already on the cluster: %v", err)
	}
	if clock.slept != 0 {
		t.Errorf("the stage slept %d time(s) although both the backup and the Project were there on the first look", clock.slept)
	}
	if cluster.projectReads == 0 {
		t.Error("the stage never looked for the Project, so it cannot have known it was there before pausing it")
	}
	if !containsString(s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace) {
		t.Errorf("the stage restored %v, and the namespace the backup covers (%s) is not among them", s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace)
	}
	if !runner.ranAny("annotate project " + recoveryWaitNamespace) {
		t.Errorf("the restore never paused %s, so no restore ran:\n%s", recoveryWaitNamespace, runner.ranAll())
	}
}

// TestAResumedRunDoesNotWaitForAProjectItAlreadyRestored is the third shape of
// the same wait: a namespace this operation has already restored is skipped
// before anything looks for its Project, so a resume neither waits for a
// project nothing will use nor touches the cluster again. The same command is
// what the operator runs to carry on after a failure, so a wait in front of the
// skip would make every resume pay the bundle's whole bound.
func TestAResumedRunDoesNotWaitForAProjectItAlreadyRestored(t *testing.T) {
	cluster := &syncingVelero{locationPhase: "Available", projectAbsentFor: 1000}
	s, runner, clock := recoveryWaitSession(t, cluster)
	s.Record.RecoveryNamespacesRestored = []string{recoveryWaitNamespace}
	before := len(runner.ran)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("a resumed recovery waited for the Project of a namespace it had already restored: %v", err)
	}
	if clock.slept != 0 {
		t.Errorf("a resumed recovery slept %d time(s) on a namespace it had already restored", clock.slept)
	}
	if again := strings.Join(runner.ran[before:], "\n"); strings.Contains(again, "project "+recoveryWaitNamespace) || strings.Contains(again, "annotate") {
		t.Errorf("a resumed recovery touched the Project of a namespace it had already restored:\n%s", again)
	}
	if len(s.Record.RecoveryNamespacesRestored) != 1 {
		t.Errorf("a resumed recovery recorded %v, and the namespace it had already restored is listed twice", s.Record.RecoveryNamespacesRestored)
	}
}
