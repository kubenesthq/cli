package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

	"gopkg.in/yaml.v3"
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
	// cronJob is the CronJob the BACKUP holds in the covered namespace, and
	// Velero therefore recreates when the Restore completes — suspended by the
	// restore's resource modifier, carrying the suspend value the backup held
	// in the annotation that same modifier writes. It is empty for a recovery
	// whose namespace holds no scheduled work.
	cronJob string
	// restored is set the moment the Velero Restore reports Completed: the
	// namespace and its CronJob exist on the cluster from then on, and every
	// read before that finds a namespace that went with the dead host.
	restored bool
	// pauseLifted is set when the namespace restore's own activation removes
	// the pause annotation from the Project, which is the change a lost-server
	// recovery has to make and did not.
	pauseLifted bool
	// history is the finished record copies the CLI writes when an operation
	// completes, by object name. The live record is `record`.
	history map[string][]byte

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
	// setRecord is the coverage record the recovery set's entry carries; nil
	// means recoveryWaitRecord with no volumes. omitSetRecord publishes the
	// entry with none at all.
	setRecord     json.RawMessage
	omitSetRecord bool
	// coverage is the coverage record the CLI applied to this cluster. Until it
	// applies one, the cluster holds the record its new operator wrote after the
	// fact: the namespace, with no volumes (kn-jl7n).
	coverage json.RawMessage

	// namespace is the covered namespace this fake answers for; empty is
	// recoveryWaitNamespace. more are the backup's other covered namespaces,
	// each answered by its own syncingVelero for what is about that namespace
	// alone — its Project, the namespace, the objects in it and the Velero
	// Restore its own restore created — while the operation record, the
	// backup, the location and the coverage record stay this one's: a cluster
	// has one of each.
	namespace string
	more      []*syncingVelero

	// fault, when set, sees every command before the cluster answers it and
	// may break it: the crash windows a recovery has to survive are injected at
	// the command that marks them. input is the document the command streams,
	// meaningful only for a command that streams one.
	fault func(command string, input []byte) faultKind
}

// name is the namespace this fake answers for.
func (c *syncingVelero) name() string {
	if c.namespace != "" {
		return c.namespace
	}
	return recoveryWaitNamespace
}

// coverageNames is every namespace the backup covers: this one, then more.
func (c *syncingVelero) coverageNames() []string {
	names := []string{c.name()}
	for _, other := range c.more {
		names = append(names, other.name())
	}
	return names
}

// concerns reports whether a command is about this namespace alone. Every
// per-namespace command pkg/backup issues names the namespace after
// `project`, `namespace` or `-n`, and the Velero Restore of a namespace is
// named after the operation whose pause it wrote.
func (c *syncingVelero) concerns(command string) bool {
	ns := c.name()
	for _, about := range []string{"project " + ns + " ", "namespace " + ns + " ", " -n " + ns + " "} {
		if strings.Contains(command, about) {
			return true
		}
	}
	return c.opID != "" && strings.Contains(command, "kubenest-restore-"+c.opID)
}

// recoveryWaitRecord is a coverage record for the backup the recovery restores,
// naming the given volumes of its namespace.
func recoveryWaitRecord(volumes ...string) json.RawMessage {
	refs := []map[string]any{}
	for _, volume := range volumes {
		refs = append(refs, map[string]any{"namespace": recoveryWaitNamespace, "name": volume, "uid": volume + "-uid"})
	}
	record, _ := json.Marshal(map[string]any{
		"backup":     recoveryWaitBackup,
		"namespaces": []map[string]any{{"name": recoveryWaitNamespace, "uid": "namespace-uid", "volumes": refs}},
	})
	return record
}

// recoveryWaitRecordOf is the coverage record of the backup covering every one
// of the namespaces, expecting the volumes named for each (none when absent).
// A volume expected here has no copy in the fake's backup, which is what makes
// its namespace ineligible.
func recoveryWaitRecordOf(namespaces []string, volumes map[string][]string) json.RawMessage {
	covered := []map[string]any{}
	for _, ns := range namespaces {
		refs := []map[string]any{}
		for _, volume := range volumes[ns] {
			refs = append(refs, map[string]any{"namespace": ns, "name": volume, "uid": volume + "-uid"})
		}
		covered = append(covered, map[string]any{"name": ns, "uid": "namespace-uid", "volumes": refs})
	}
	record, _ := json.Marshal(map[string]any{"backup": recoveryWaitBackup, "namespaces": covered})
	return record
}

// mount wires the fake to the session's runner.
func (c *syncingVelero) mount(runner *recordingRunner) {
	runner.respond = func(command string) (sshx.Result, error) { return c.answer(runner, command) }
}

// faultKind is what an injected fault does to one command.
type faultKind int

const (
	// noFault answers the command as the cluster would.
	noFault faultKind = iota
	// failCommand fails the command before the cluster sees it.
	failCommand
	// loseReply carries the command out and loses its reply: a connection
	// that drops after the API server applied a write.
	loseReply
)

// errInjected is the transport failure an injected fault produces.
var errInjected = errors.New("ssh: the connection to the server node was lost (injected)")

func (c *syncingVelero) answer(runner *recordingRunner, command string) (sshx.Result, error) {
	kind := noFault
	if c.fault != nil {
		kind = c.fault(command, lastInput(runner))
	}
	switch kind {
	case failCommand:
		return sshx.Result{}, errInjected
	case loseReply:
		if _, err := c.respond(runner, command); err != nil {
			return sshx.Result{}, err
		}
		return sshx.Result{}, errInjected
	}
	return c.respond(runner, command)
}

func (c *syncingVelero) respond(runner *recordingRunner, command string) (sshx.Result, error) {
	for _, other := range c.more {
		if other.concerns(command) {
			return other.respond(runner, command)
		}
	}
	ns := c.name()
	switch {
	// The operation record: the CLI writes it with a create or a replace and
	// reads it back before every change, so the fake has to keep what it was
	// given and answer with the resourceVersion each write returned.
	//
	// THE HISTORY COPY IS A SECOND OBJECT, not the live record: completing an
	// operation writes a copy named after that operation, and a fake that kept
	// the copy as the live record would answer every later read with a terminal
	// one.
	case isOperationRecordWrite(command):
		doc := lastInput(runner)
		c.rv++
		if name := objectName(doc); strings.HasPrefix(name, operation.Name+"-") {
			if c.history == nil {
				c.history = map[string][]byte{}
			}
			c.history[name] = doc
		} else {
			c.record = doc
		}
		return sshx.Result{Stdout: fmt.Sprintf(`{"metadata":{"resourceVersion":%q}}`, strconv.Itoa(c.rv))}, nil
	case strings.Contains(command, "get configmap "+operation.Name+" -n "+operation.Namespace):
		if c.record == nil {
			return notFound("configmaps", operation.Name), nil
		}
		return sshx.Result{Stdout: c.recordDocument()}, nil
	case strings.Contains(command, "get configmap "+operation.Name+"-"):
		name := configMapObject(command)
		doc, found := c.history[name]
		if !found {
			return notFound("configmaps", name), nil
		}
		return sshx.Result{Stdout: string(doc)}, nil

	// The coverage record the recovery puts back from the set, owned by the
	// synced Backup.
	case strings.Contains(command, "get backup "+recoveryWaitBackup+" -n "+backup.Namespace+" -o jsonpath='{.metadata.uid}'"):
		return sshx.Result{Stdout: "backup-uid"}, nil
	case strings.Contains(command, "kubectl apply -f -") && strings.Contains(string(lastInput(runner)), backup.CoverageRecordName(recoveryWaitBackup)):
		var applied struct {
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal(lastInput(runner), &applied); err != nil {
			return sshx.Result{ExitCode: 1, Stderr: err.Error()}, nil
		}
		c.coverage = json.RawMessage(applied.Data["coverage.json"])
		return sshx.Result{}, nil

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
	case strings.Contains(command, "get project "+ns+" -n "+backup.ProjectCRNamespace):
		c.projectReads++
		if !c.projectPresent() {
			return notFound("projects.kubenest.io", ns), nil
		}
		return sshx.Result{Stdout: c.holdDocument()}, nil
	case strings.Contains(command, "annotate project "+ns):
		// kubectl annotate READS the object before it patches it, so annotating
		// a Project the operator has not created yet is the NotFound the
		// hardware produced: `kubectl annotate project s6-data -n
		// kubenest-system ... exit 1`.
		c.projectReads++
		if !c.projectPresent() {
			return notFound("projects.kubenest.io", ns), nil
		}
		switch {
		case strings.Contains(command, backup.PauseAnnotationKey+"-"):
			// The namespace restore's own activation clears the pause it left,
			// which is the change this fake exists to observe.
			c.pauseLifted = true
		case strings.Contains(command, backup.PauseAnnotationKey+"="):
			// A new pause holds the project again, whatever lifted the last.
			c.opID = pauseOpID(command)
			c.pauseLifted = false
		}
		return sshx.Result{}, nil

	// The namespace went with the host, so the restore recreates it, and the
	// restore's own Velero Restore completes at once. A namespace read from
	// then on finds what that Restore created, until a later restore deletes it.
	case strings.Contains(command, "delete namespace "+ns+" "):
		c.restored = false
		return sshx.Result{}, nil
	case strings.Contains(command, "get namespace "+ns+" -o json"):
		if !c.restored {
			return notFound("namespaces", ns), nil
		}
		return sshx.Result{Stdout: `{"metadata":{"name":` + strconv.Quote(ns) + `,"uid":"namespace-uid-after-restore"}}`}, nil
	case strings.Contains(command, "get restore "):
		c.restored = true
		return sshx.Result{Stdout: `{"status":{"phase":"Completed","progress":{"itemsRestored":1}}}`}, nil

	// The CronJobs of the namespace: none before the Restore completes, and
	// the backup's own after it — suspended by the restore's modifier, carrying
	// the suspend value the backup held.
	case strings.Contains(command, "get cronjobs -n "+ns):
		if c.cronJob == "" || !c.restored {
			return sshx.Result{Stdout: `{"items":[]}`}, nil
		}
		return sshx.Result{Stdout: c.cronJobDocument()}, nil

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
	record := c.coverage
	if record == nil {
		record = recoveryWaitRecord()
	}
	doc, err := json.Marshal(map[string]any{"data": map[string]string{"coverage.json": string(record)}})
	if err != nil {
		return "{}"
	}
	return string(doc)
}

// holdDocument is the Project the restore pauses. Before the pause annotation
// has been written the project holds nothing; afterwards the annotation names
// the operation — which is how the restore's own activation is told which
// operation it is finishing — and the condition carries the same operation. The
// annotation goes when that activation lifts it.
func (c *syncingVelero) holdDocument() string {
	if c.opID == "" || c.pauseLifted {
		return `{"status":{"conditions":[]}}`
	}
	return `{"metadata":{"annotations":{` + strconv.Quote(backup.PauseAnnotationKey) + `:` + strconv.Quote(c.opID) + `}},` +
		`"status":{"conditions":[{"type":` + strconv.Quote(backup.ConditionReconcilePaused) +
		`,"status":"True","reason":` + strconv.Quote(backup.ReasonPausedByOperation) +
		`,"message":` + strconv.Quote("holding reconciliation for operation "+c.opID) + `}]}}`
}

// cronJobDocument is the CronJob Velero brings back with the namespace: the
// restore's own resource modifier has suspended it and written the value the
// BACKUP held into the annotation activation reads.
func (c *syncingVelero) cronJobDocument() string {
	doc := map[string]any{
		"items": []any{map[string]any{
			"metadata": map[string]any{
				"name":        c.cronJob,
				"annotations": map[string]string{backup.CronJobSuspendedAnnotationKey: "false"},
			},
			"spec": map[string]any{"schedule": "* * * * *", "suspend": true},
		}},
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// objectName reads the metadata.name of one of the documents the CLI writes, so
// the fake can tell a live operation record from its finished copy.
func objectName(doc []byte) string {
	var obj struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(doc, &obj); err != nil {
		return ""
	}
	return obj.Metadata.Name
}

// configMapObject reads the object name out of a `kubectl get configmap` line.
func configMapObject(command string) string {
	const marker = "get configmap "
	at := strings.Index(command, marker)
	if at < 0 {
		return ""
	}
	rest := command[at+len(marker):]
	if end := strings.IndexByte(rest, ' '); end >= 0 {
		return rest[:end]
	}
	return rest
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
	coverage := cluster.coverageNames()
	setRecord := cluster.setRecord
	if setRecord == nil && !cluster.omitSetRecord {
		setRecord = recoveryWaitRecordOf(coverage, nil)
	}
	f.publish(t, f.bind, "20260928T113029Z-cccccccc", f.secrets, nil, true, recoverykit.Backup{
		Name:           recoveryWaitBackup,
		CompletedAt:    time.Date(2026, 9, 28, 11, 30, 29, 0, time.UTC),
		Coverage:       coverage,
		Status:         "Completed",
		CoverageRecord: setRecord,
	})
	sel, err := recovery.Select(context.Background(), f.bucket, recoveryTestScope, f.request())
	if err != nil {
		t.Fatalf("selecting the recovery set the hardware's backup lives in: %v", err)
	}
	if sel.Backup.Name != recoveryWaitBackup || strings.Join(sel.Backup.Coverage, ",") != strings.Join(coverage, ",") {
		t.Fatalf("the selection is %s covering %v, so this test is not about the measured backup covering %v", sel.Backup.Name, sel.Backup.Coverage, coverage)
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
	// The one write is the backup's coverage record, put back from the set.
	if runner.ranAny("annotate", "delete ", "create -f -", "replace -f -") {
		t.Errorf("a recovery whose wait for the project ran out still changed the cluster:\n%s", runner.ranAll())
	}
	for _, input := range runner.inputs {
		if len(input) > 0 && !strings.Contains(string(input), backup.CoverageRecordName(recoveryWaitBackup)) {
			t.Errorf("a recovery whose wait for the project ran out applied something other than the coverage record:\n%s", input)
		}
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

// THE SECOND HARDWARE DEFECT OF LAB S6 (2026-09-28, kn-t49-…5). The recovery
// restored s6-data's data — the proof digest matched — and then activated
// nothing that ran: CronJob kn-recovery-sentinel stayed suspended, the Project
// carried BOTH the recovery's own activation annotation and the namespace
// restore's pause, and the sentinel receiver never saw a call.
//
// THE RESTORE IS TWO STEPS OF ONE OPERATION. `backup.RunRestore` in mode 1
// stops at `restored — awaiting activation` with the project paused and every
// restored CronJob suspended, and `backup.RunRestore --activate <id>` is what
// lifts the pause, puts each CronJob back to the suspend value the BACKUP held
// and scales the workloads back. The recovery wrote the operator's recovery-mode
// release and stopped there, so half of the restore's own state was never
// reached.
const (
	// recoveryActivateCronJob is the CronJob the fixture's backup holds, and
	// therefore the one the gate's sentinel runs on.
	recoveryActivateCronJob = "kn-recovery-sentinel"
	// recoveryActivateOpID is the recovery's own operation, which the operator's
	// recovery-mode annotation names.
	recoveryActivateOpID = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
)

// TestTheActivateStageActivatesTheRestoreItRan is the defect: the stage has to
// write the operator's recovery-mode release AND perform the activation of the
// namespace restore that put the data back.
//
// THE POSITIVE OBSERVABLE: the pause annotation the restore left on the Project
// is removed, and the CronJob goes back to the suspend value the BACKUP held —
// both of which only the restore's OWN activation does.
//
// THE PLANTED NEGATIVE IS THIS SAME FAKE ON THE OLD CODE: the recovery's
// annotation is written, the restore's pause stays on the Project and the
// CronJob is never patched, which is the state the hardware was left in.
func TestTheActivateStageActivatesTheRestoreItRan(t *testing.T) {
	cluster := &syncingVelero{locationPhase: "Available", cronJob: recoveryActivateCronJob}
	s, runner, _ := recoveryWaitSession(t, cluster)
	s.Record.RecoveryOperationID = recoveryActivateOpID

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the restore stage failed, so there is nothing for activation to activate: %v", err)
	}
	// The restore of one namespace is an operation of its own, and the stage
	// recorded which one so that a later stage can finish it. It is the
	// operation whose pause is on the Project, which is the same fact read two
	// ways.
	restoreOp := s.Record.RecoveryRestoreOperations[recoveryWaitNamespace]
	if restoreOp == "" {
		t.Fatalf("the restore stage did not record the namespace restore's operation id, so nothing can activate it later: %v", s.Record.RecoveryRestoreOperations)
	}
	if restoreOp != cluster.opID {
		t.Errorf("the recorded operation id %q is not the operation that paused the Project (%q)", restoreOp, cluster.opID)
	}

	before := len(runner.ran)
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("the activate stage failed on a namespace whose data is back: %v", err)
	}
	if !containsString(s.Record.RecoveryNamespacesActivated, recoveryWaitNamespace) {
		t.Errorf("activation recorded %v, and the namespace it activated (%s) is not among them", s.Record.RecoveryNamespacesActivated, recoveryWaitNamespace)
	}
	activation := strings.Join(runner.ran[before:], "\n")

	// The operator's recovery-mode release, which is what the stage did before.
	if !strings.Contains(activation, "annotate project "+recoveryWaitNamespace+" -n "+backup.ProjectCRNamespace+" "+backup.ActivateAnnotationKey+"=") {
		t.Errorf("activation did not write the operator's recovery-mode annotation on %s, so the operator keeps holding the project:\n%s", recoveryWaitNamespace, activation)
	}
	// AND the restore's own activation, which is the half that was missing: the
	// pause it left is lifted...
	if !strings.Contains(activation, backup.PauseAnnotationKey+"-") {
		t.Errorf("activation did not lift the pause the namespace restore left on the Project, so the project stays held by an operation that is waiting for exactly this:\n%s", activation)
	}
	if !cluster.pauseLifted {
		t.Errorf("the Project still carries %s after activation:\n%s", backup.PauseAnnotationKey, activation)
	}
	// ...and the CronJob goes back to the suspend value the BACKUP held
	// (kn-x0wv.3): the restored one is suspended by the restore's modifier, and
	// the backup had it running, so this is what starts the scheduled work.
	wantPatch := "patch cronjob " + recoveryActivateCronJob + " -n " + recoveryWaitNamespace
	if !strings.Contains(activation, wantPatch) || !strings.Contains(activation, `"suspend":false`) {
		t.Errorf("activation did not put CronJob %s back to the suspend value the backup held, so the scheduled work stays off for ever:\n%s", recoveryActivateCronJob, activation)
	}
}

// TestAResumedRecoveryKeepsTheRestoreOperationItRecorded: the operation id is
// what a resume has instead of the restore in memory, so the journal has to
// carry it. The stage that finds a namespace already restored must neither
// restore it again nor forget which operation did — the activation that follows
// reads that id out of the record.
func TestAResumedRecoveryKeepsTheRestoreOperationItRecorded(t *testing.T) {
	cluster := &syncingVelero{locationPhase: "Available", cronJob: recoveryActivateCronJob}
	s, runner, _ := recoveryWaitSession(t, cluster)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the restore stage failed: %v", err)
	}
	restoreOp := s.Record.RecoveryRestoreOperations[recoveryWaitNamespace]
	if restoreOp == "" {
		t.Fatalf("the restore stage did not record the namespace restore's operation id: %v", s.Record.RecoveryRestoreOperations)
	}

	// The resume: the next attempt reads the record back out of the journal
	// rather than holding it in memory.
	record, err := Recorded(s.Jnl)
	if err != nil {
		t.Fatalf("reading the record back out of the journal: %v", err)
	}
	if record.RecoveryRestoreOperations[recoveryWaitNamespace] != restoreOp {
		t.Fatalf("the journal kept %v for the restored namespaces, so a resumed activation has no operation to activate", record.RecoveryRestoreOperations)
	}
	s.Record = record

	before := len(runner.ran)
	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the resumed restore stage failed: %v", err)
	}
	if again := strings.Join(runner.ran[before:], "\n"); strings.Contains(again, "annotate") {
		t.Errorf("a resumed restore ran over a namespace it had already restored:\n%s", again)
	}
	if got := s.Record.RecoveryRestoreOperations[recoveryWaitNamespace]; got != restoreOp {
		t.Fatalf("a resumed stage recorded %q for %s, and the operation that restored it is %q", got, recoveryWaitNamespace, restoreOp)
	}

	// And the activation the resume reaches is THAT operation's: the pause it
	// left is lifted and the CronJob goes back.
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("the resumed activation failed: %v", err)
	}
	activated := strings.Join(runner.ran[before:], "\n")
	requireContains(t, "the resumed activation", activated,
		backup.PauseAnnotationKey+"-", "patch cronjob "+recoveryActivateCronJob)
	if !containsString(s.Record.RecoveryNamespacesActivated, recoveryWaitNamespace) {
		t.Errorf("the resumed activation recorded %v, and the namespace it activated is not among them", s.Record.RecoveryNamespacesActivated)
	}
}

// TestAResumedActivationDoesNotActivateTheRestoreTwice is the crash between the
// two steps: the operator's annotation is written, the restore's own activation
// runs and CLOSES its operation, and the write that records the namespace in
// the journal is the one that never lands. The resumed stage finds the restore
// already finished, and finishing it again is both refused by `activate`
// ("there is nothing to do for it") and wrong — a second activation would run
// the workloads and the CronJobs a second time.
//
// THE PLANTED NEGATIVE IS THE SAME FAKE WITHOUT THAT RECOGNITION: the resume
// fails on the terminal operation, and the namespace is never recorded as
// activated.
func TestAResumedActivationDoesNotActivateTheRestoreTwice(t *testing.T) {
	cluster := &syncingVelero{locationPhase: "Available", cronJob: recoveryActivateCronJob}
	s, runner, _ := recoveryWaitSession(t, cluster)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the restore stage failed: %v", err)
	}
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("the activate stage failed: %v", err)
	}
	// The crash: the operation is closed and the journal write did not land.
	s.Record.RecoveryNamespacesActivated = nil

	before := len(runner.ran)
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("a resume after the namespace restore's own activation completed failed, which is the crash between the two steps the record cannot see: %v", err)
	}
	if !containsString(s.Record.RecoveryNamespacesActivated, recoveryWaitNamespace) {
		t.Fatalf("the resumed activation recorded %v, so the namespace it activated is still not marked as activated", s.Record.RecoveryNamespacesActivated)
	}
	again := strings.Join(runner.ran[before:], "\n")
	if strings.Contains(again, "patch cronjob") || strings.Contains(again, backup.PauseAnnotationKey+"-") {
		t.Errorf("a resumed activation activated the namespace restore a second time, which would run its workloads and its CronJobs again:\n%s", again)
	}
}

// TestAnOlderRecoveryActivationReadsTheRestoreFromTheProject: a journal written
// by a CLI that did not record the operation id per namespace — every recovery
// started before this fix, including lab s6's — names the namespaces it
// restored and nothing about the operations that restored them. The pause
// annotation the restore wrote on the Project is the same fact from the same
// actor, and reading it is what lets such a recovery finish instead of leaving
// the namespace held.
//
// THE PLANTED NEGATIVE IS THE SAME FAKE WITHOUT THAT FALLBACK: the stage writes
// the operator's release and the restore's pause stays, which is the state the
// hardware is in.
func TestAnOlderRecoveryActivationReadsTheRestoreFromTheProject(t *testing.T) {
	cluster := &syncingVelero{locationPhase: "Available", cronJob: recoveryActivateCronJob}
	s, runner, _ := recoveryWaitSession(t, cluster)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the restore stage failed: %v", err)
	}
	if cluster.opID == "" {
		t.Fatal("the fixture's restore paused no project, so this test cannot tell the two sources of the operation id apart")
	}
	// The journal of a recovery started by the CLI that did not record it.
	s.Record.RecoveryRestoreOperations = nil
	s.Record.RecoveryNamespacesRestored = []string{recoveryWaitNamespace}

	before := len(runner.ran)
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("activating a namespace whose restore operation only the Project names failed: %v", err)
	}
	activated := strings.Join(runner.ran[before:], "\n")
	if !strings.Contains(activated, backup.PauseAnnotationKey+"-") {
		t.Errorf("activation did not lift the pause the namespace restore left on the Project, so the namespace stays held:\n%s", activated)
	}
	if !strings.Contains(activated, "patch cronjob "+recoveryActivateCronJob) {
		t.Errorf("activation did not put the restored CronJob back, so the scheduled work stays off:\n%s", activated)
	}
	if !containsString(s.Record.RecoveryNamespacesActivated, recoveryWaitNamespace) {
		t.Errorf("the activation recorded %v, and the namespace it activated is not among them", s.Record.RecoveryNamespacesActivated)
	}
}

// TestARecoveryJudgesTheBackupAgainstTheRecordTheSetCarries is kn-jl7n. The
// backup's coverage record died with the old host, and the rebuilt cluster's
// operator wrote one after the fact with no volumes in it, which passes "no
// missing or failed volume" without checking anything. The recovery puts the
// set's own copy back and judges against that.
//
// PLANTED NEGATIVES: the set's record names s6-data/data and the backup holds
// no copy of it, so the namespace is held; and an entry that carries no
// record cannot be judged, so it is held too. The positive is every other test
// in this file, whose set record names no volumes and restores.
func TestARecoveryJudgesTheBackupAgainstTheRecordTheSetCarries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cluster *syncingVelero
		reason  string
	}{
		{"a volume the backup never copied", &syncingVelero{locationPhase: "Available", setRecord: recoveryWaitRecord("data")}, "incomplete-coverage"},
		{"no record in the set", &syncingVelero{locationPhase: "Available", omitSetRecord: true}, "carries no coverage record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, runner, _ := recoveryWaitSession(t, tc.cluster)
			if err := stageRecoveryRestore(context.Background(), s); err != nil {
				t.Fatalf("the stage failed instead of holding the namespace: %v", err)
			}
			if containsString(s.Record.RecoveryNamespacesRestored, recoveryWaitNamespace) || runner.ranAny("annotate project "+recoveryWaitNamespace) {
				t.Fatalf("namespace %s was restored from a backup whose volumes were judged against nothing:\n%s", recoveryWaitNamespace, runner.ranAll())
			}
			if !containsString(s.Record.RecoveryNamespacesNotRestored, recoveryWaitNamespace) || !strings.Contains(s.Record.RecoveryRestoreSkipReason, tc.reason) {
				t.Errorf("namespace %s is not held with reason %q: held %v, reason %q", recoveryWaitNamespace, tc.reason, s.Record.RecoveryNamespacesNotRestored, s.Record.RecoveryRestoreSkipReason)
			}
		})
	}
}

// THE THIRD HARDWARE DEFECT OF LAB S6 (2026-11-12, kn-ship-1-2-closeout-lzjb.5).
// The cluster held two projects, e2e-restore-ns (which the S4 gate left) and
// s6-gate. The recovery restored the first and was refused the second: "another
// operation holds the record: operation 67f3… (restore-namespace) is stopped on
// this cluster … at stage restored — awaiting activation". A namespace restore
// is an operation that keeps the cluster's one operation record until it is
// activated, and a recovery activates nothing until every namespace's data is
// back (PLAN 7.9 steps 5 and 6). Every earlier pass had one project.

// recoveryGateNamespace is the second project of that cluster.
const recoveryGateNamespace = "s6-gate"

// twoProjectCluster is that cluster: two covered namespaces, each holding the
// CronJob the gate's sentinel runs on.
func twoProjectCluster() (first, second *syncingVelero) {
	second = &syncingVelero{namespace: recoveryGateNamespace, cronJob: recoveryActivateCronJob}
	first = &syncingVelero{locationPhase: "Available", cronJob: recoveryActivateCronJob, more: []*syncingVelero{second}}
	return first, second
}

// liveOperation reads the cluster's operation record as the fake holds it:
// which operation it is, whether it is terminal, and what is parked under it.
func liveOperation(t *testing.T, c *syncingVelero) (operationID string, terminal bool, parked []string) {
	t.Helper()
	var doc struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(c.record, &doc); err != nil {
		t.Fatalf("decoding the live operation record: %v", err)
	}
	var rec struct {
		OperationID string   `json:"operation_id"`
		Terminal    bool     `json:"terminal"`
		Parked      []string `json:"parked"`
	}
	if err := json.Unmarshal([]byte(doc.Data["record.json"]), &rec); err != nil {
		t.Fatalf("decoding the live operation record's JSON: %v", err)
	}
	return rec.OperationID, rec.Terminal, rec.Parked
}

// TestARecoveryRestoresEveryNamespaceBeforeActivatingAny is that defect: the
// restore stage puts both namespaces' data back, each by its own operation, and
// activates neither.
//
// THE PLANTED NEGATIVE IS THIS SAME FAKE ON THE OLD CODE: the second namespace's
// restore is refused with "another operation holds the record", naming the
// first namespace's restore at `restored — awaiting activation`.
func TestARecoveryRestoresEveryNamespaceBeforeActivatingAny(t *testing.T) {
	first, second := twoProjectCluster()
	s, runner, _ := recoveryWaitSession(t, first)

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the restore stage failed on the second of two namespaces, which is the hardware defect: %v", err)
	}
	for _, ns := range []*syncingVelero{first, second} {
		if !ns.restored || !containsString(s.Record.RecoveryNamespacesRestored, ns.name()) {
			t.Errorf("namespace %s was not restored (the stage recorded %v)", ns.name(), s.Record.RecoveryNamespacesRestored)
		}
		// Each namespace is restored by its OWN operation, and the stage wrote
		// down which: the activate stage finishes that operation.
		if op := s.Record.RecoveryRestoreOperations[ns.name()]; op == "" || op != ns.opID {
			t.Errorf("the stage recorded operation %q for %s, and the operation whose pause is on its Project is %q", op, ns.name(), ns.opID)
		}
		if ns.pauseLifted {
			t.Errorf("the restore stage lifted the pause on %s, which is activation, and activation belongs to the activate stage", ns.name())
		}
	}
	if first.opID == second.opID {
		t.Errorf("both Projects are paused by operation %s: one namespace restore cannot have restored two namespaces", first.opID)
	}
	// NOTHING IS RELEASED BEFORE EVERY NAMESPACE'S DATA IS BACK: one namespace's
	// workloads may need another's data.
	if runner.ranAny(backup.ActivateAnnotationKey, backup.PauseAnnotationKey+"-") {
		t.Errorf("the restore stage released a project before the activate stage:\n%s", runner.ranAll())
	}
}

// TestTheActivateStageActivatesEveryNamespaceTheRecoveryRestored is the second
// half: the activate stage releases both projects from recovery mode and
// activates both namespace restores — each pause lifted, each CronJob back to
// the value its backup held — and closes every restore operation, so the
// cluster's record no longer holds anything against the next operator.
func TestTheActivateStageActivatesEveryNamespaceTheRecoveryRestored(t *testing.T) {
	first, second := twoProjectCluster()
	s, runner, _ := recoveryWaitSession(t, first)
	// The ownership stage of this attempt took the recovery operation.
	s.recoveryOpID = recoveryActivateOpID
	s.Record.RecoveryOperationID = recoveryActivateOpID

	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the restore stage failed: %v", err)
	}
	before := len(runner.ran)
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("the activate stage failed on namespaces whose data is back: %v", err)
	}
	activation := strings.Join(runner.ran[before:], "\n")
	for _, ns := range []*syncingVelero{first, second} {
		name := ns.name()
		if !containsString(s.Record.RecoveryNamespacesActivated, name) {
			t.Errorf("activation recorded %v, and %s is not among them", s.Record.RecoveryNamespacesActivated, name)
		}
		if !strings.Contains(activation, "annotate project "+name+" -n "+backup.ProjectCRNamespace+" "+backup.ActivateAnnotationKey+"="+recoveryActivateOpID+" ") {
			t.Errorf("activation did not release %s from recovery mode with the recovery's operation id:\n%s", name, activation)
		}
		if !ns.pauseLifted {
			t.Errorf("the pause of %s's own restore is still on its Project, so the project stays held:\n%s", name, activation)
		}
		if !strings.Contains(activation, "patch cronjob "+recoveryActivateCronJob+" -n "+name+" ") {
			t.Errorf("CronJob %s/%s was not put back to the value its backup held:\n%s", name, recoveryActivateCronJob, activation)
		}
	}
	if op, terminal, parked := liveOperation(t, first); !terminal || len(parked) != 0 {
		t.Errorf("after activation the cluster's operation record is %s, terminal=%t, with %v parked under it: it still refuses the next operation", op, terminal, parked)
	}
}

// TestARecoveryInterruptedBetweenNamespacesRestoresOnlyTheRest: the identical
// command resumes a recovery that stopped after its first namespace — here
// because the second namespace's Project did not arrive within the bound, which
// changes nothing of that namespace. The resume restores the second namespace
// only, behind the first namespace's restore that still awaits activation, and
// then activates both.
//
// THE RESUME IS A NEW PROCESS, so the recovery's own operation id comes from
// the journal: the ownership stage that holds it in memory is skipped. It is
// the value the operator's release must carry, because op3 treats an empty
// kubenest.io/reconcile-activated as no release at all
// (op3/internal/controller/reconcile_hold.go).
//
// THE PLANTED NEGATIVES ARE THE OLD CODE: the resumed restore stage is refused
// by the first namespace's restore, as the hardware's single attempt was; and
// an activate stage that writes the in-memory id writes an empty one here.
func TestARecoveryInterruptedBetweenNamespacesRestoresOnlyTheRest(t *testing.T) {
	first, second := twoProjectCluster()
	second.projectAbsentFor = 1000
	s, runner, _ := recoveryWaitSession(t, first)
	s.Record.RecoveryOperationID = recoveryActivateOpID

	if err := stageRecoveryRestore(context.Background(), s); err == nil {
		t.Fatal("the first attempt restored a namespace whose Project never arrived")
	}
	if !first.restored || second.restored {
		t.Fatalf("the first attempt was meant to stop between the two namespaces: %s restored=%t, %s restored=%t", first.name(), first.restored, second.name(), second.restored)
	}
	firstOp := first.opID

	// The resume: the journal and nothing else, and the second Project has
	// arrived since.
	record, err := Recorded(s.Jnl)
	if err != nil {
		t.Fatalf("reading the record back out of the journal: %v", err)
	}
	s.Record = record
	second.projectAbsentFor = second.projectReads
	before := len(runner.ran)
	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the resumed restore stage failed: %v", err)
	}
	if resumed := strings.Join(runner.ran[before:], "\n"); strings.Contains(resumed, "annotate project "+first.name()+" ") {
		t.Errorf("the resume restored %s again, over the data it had already put back:\n%s", first.name(), resumed)
	}
	if !second.restored || second.opID == "" || s.Record.RecoveryRestoreOperations[second.name()] != second.opID {
		t.Fatalf("the resume did not restore %s: recorded %v", second.name(), s.Record.RecoveryRestoreOperations)
	}
	if s.Record.RecoveryRestoreOperations[first.name()] != firstOp {
		t.Errorf("the resume lost the operation that restored %s (%s): %v", first.name(), firstOp, s.Record.RecoveryRestoreOperations)
	}

	before = len(runner.ran)
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("the resumed activation failed: %v", err)
	}
	activation := strings.Join(runner.ran[before:], "\n")
	for _, ns := range []*syncingVelero{first, second} {
		if !ns.pauseLifted || !containsString(s.Record.RecoveryNamespacesActivated, ns.name()) {
			t.Errorf("the resumed recovery did not activate %s's restore:\n%s", ns.name(), activation)
		}
		if !strings.Contains(activation, "annotate project "+ns.name()+" -n "+backup.ProjectCRNamespace+" "+backup.ActivateAnnotationKey+"="+recoveryActivateOpID+" ") {
			t.Errorf("the resumed recovery did not release %s with its operation id %s, so recovery mode keeps holding it:\n%s", ns.name(), recoveryActivateOpID, activation)
		}
	}
}

// TestAnOperatorsRestoreIsRefusedWhileARecoveredNamespaceAwaitsActivation is
// the planted negative the fix must keep: the recovery restores behind its OWN
// restores, and nothing else may start. An operator's restore is refused by the
// cluster's operation record while the last restore holds it, and still after
// that one is activated, while the first — parked under it — awaits
// activation. The refusal writes nothing.
//
// THE POSITIVE CONTROL is the end: once the recovery has activated every
// namespace, the record holds nothing and a new operation may take it.
func TestAnOperatorsRestoreIsRefusedWhileARecoveredNamespaceAwaitsActivation(t *testing.T) {
	first, second := twoProjectCluster()
	s, _, clock := recoveryWaitSession(t, first)
	s.recoveryOpID = recoveryActivateOpID
	s.Record.RecoveryOperationID = recoveryActivateOpID
	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the restore stage failed: %v", err)
	}
	server, err := s.Server()
	if err != nil {
		t.Fatal(err)
	}
	// The operator's own restore, on the recovery's clock: a restore the record
	// failed to refuse ends on a bounded wait instead of the wall clock.
	deps := recoveryRestoreDeps(server)
	deps.Now = func() time.Time { return clock.now }
	deps.Poll = time.Minute
	deps.Sleep = recoveryRestoreSync.Sleep
	refused := func(when, namespace, holder string) {
		t.Helper()
		writes := first.rv
		err := backup.RunRestore(context.Background(), &bytes.Buffer{}, nil, backup.RestoreOptions{
			Cluster: s.recoveryName(), Namespace: namespace, From: recoveryWaitBackup,
			Replace: true, Confirm: true, AcceptDataAge: true, Bundle: s.Bundle,
		}, deps)
		if !errors.Is(err, operation.ErrLocked) || !strings.Contains(err.Error(), holder) {
			t.Fatalf("%s, an operator's restore of %s returned %v; want the record's refusal naming operation %s", when, namespace, err, holder)
		}
		if first.rv != writes {
			t.Errorf("%s, the refused restore wrote an operation record", when)
		}
	}

	refused("while both namespace restores await activation", first.name(), second.opID)

	// The second namespace is activated by hand: its restore is closed, and the
	// first namespace's restore, parked under it, still awaits activation.
	if err := backup.RunRestore(context.Background(), &bytes.Buffer{}, nil, backup.RestoreOptions{
		Cluster: s.recoveryName(), Namespace: second.name(), Activate: second.opID,
		Confirm: true, KeepDesired: true, Bundle: s.Bundle,
	}, deps); err != nil {
		t.Fatalf("activating the restore of %s: %v", second.name(), err)
	}
	refused("after the second namespace was activated", second.name(), first.opID)

	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("the activate stage failed: %v", err)
	}
	if op, terminal, parked := liveOperation(t, first); !terminal || len(parked) != 0 {
		t.Fatalf("after the recovery activated every namespace the record is %s, terminal=%t, with %v parked under it", op, terminal, parked)
	}
	if _, err := deps.Store.Acquire(context.Background(), operation.Request{Kind: operation.KindRestoreNamespace, Cluster: s.recoveryName()}); err != nil {
		t.Errorf("after the recovery activated every namespace, a new operation is still refused the record: %v", err)
	}
}

// recoveryThirdNamespace is a third project, so a recovery has a namespace on
// each side of the one a test breaks.
const recoveryThirdNamespace = "s6-logs"

// threeProjectCluster is a rebuilt cluster whose backup covers three projects,
// each holding the CronJob the gate's sentinel runs on.
func threeProjectCluster() (first, second, third *syncingVelero) {
	second = &syncingVelero{namespace: recoveryGateNamespace, cronJob: recoveryActivateCronJob}
	third = &syncingVelero{namespace: recoveryThirdNamespace, cronJob: recoveryActivateCronJob}
	first = &syncingVelero{locationPhase: "Available", cronJob: recoveryActivateCronJob, more: []*syncingVelero{second, third}}
	return first, second, third
}

// recordDoc decodes an operation record document as the CLI writes it: the
// ConfigMap's name and the record it holds. Anything else decodes to nil.
func recordDoc(doc []byte) (name string, rec *operation.Record) {
	var cm struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(doc, &cm) != nil || cm.Data["record.json"] == "" {
		return "", nil
	}
	rec = &operation.Record{}
	if json.Unmarshal([]byte(cm.Data["record.json"]), rec) != nil {
		return "", nil
	}
	return cm.Metadata.Name, rec
}

// recordSummary names a record's operation, stage and executor state.
func recordSummary(rec *operation.Record) string {
	if rec == nil {
		return "absent"
	}
	return fmt.Sprintf("%s at %q, executor %s", rec.OperationID, rec.Stage, rec.Executor.State)
}

// liveID is the operation the fake's live record holds now.
func (c *syncingVelero) liveID() string {
	if _, rec := recordDoc(c.record); rec != nil {
		return rec.OperationID
	}
	return ""
}

// takesRecordFor reports whether a command is the write that hands the live
// record to a new operation restoring namespace: the compare-and-swap at the
// start of that namespace's restore.
func (c *syncingVelero) takesRecordFor(namespace, command string, input []byte) bool {
	name, rec := recordDoc(input)
	return isOperationRecordWrite(command) && rec != nil && name == operation.Name &&
		rec.Request.Versions["namespace"] == namespace && rec.OperationID != c.liveID()
}

// editLive rewrites the fake's live operation record, as a write by another
// executor would.
func (c *syncingVelero) editLive(t *testing.T, edit func(*operation.Record)) {
	t.Helper()
	var cm map[string]any
	if err := json.Unmarshal(c.record, &cm); err != nil {
		t.Fatalf("decoding the live record: %v", err)
	}
	data, _ := cm["data"].(map[string]any)
	raw, _ := data["record.json"].(string)
	var rec operation.Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("decoding the live record's JSON: %v", err)
	}
	edit(&rec)
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	data["record.json"] = string(encoded)
	if c.record, err = json.Marshal(cm); err != nil {
		t.Fatal(err)
	}
	c.rv++
}

// restoresOf counts the Velero Restores the runs created for one namespace.
func restoresOf(runner *recordingRunner, namespace string) int {
	count := 0
	for _, input := range runner.inputs {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				IncludedNamespaces []string `yaml:"includedNamespaces"`
			} `yaml:"spec"`
		}
		if yaml.Unmarshal(input, &doc) == nil && doc.Kind == "Restore" && slices.Contains(doc.Spec.IncludedNamespaces, namespace) {
			count++
		}
	}
	return count
}

// ranCount counts the commands the runs issued that contain fragment.
func ranCount(runner *recordingRunner, fragment string) int {
	count := 0
	for _, command := range runner.ran {
		if strings.Contains(command, fragment) {
			count++
		}
	}
	return count
}

// resumeFromJournal is what the next run of the identical command starts from:
// the journal FILE in dir, and nothing the run before it held in memory. The
// journal object a run holds is not that: SetState updates it before the save
// that may fail.
func resumeFromJournal(t *testing.T, s *Session, dir string) {
	t.Helper()
	jnl, err := OpenJournal(filepath.Join(dir, "journal.json"), s.Opts.Identity())
	if err != nil {
		t.Fatalf("opening the journal in %s again: %v", dir, err)
	}
	record, err := Recorded(jnl)
	if err != nil {
		t.Fatalf("reading the record back out of the journal: %v", err)
	}
	s.Jnl, s.Record, s.recoveryOpID = jnl, record, ""
}

// recoveryAttempt is one run of the identical recovery command over the two
// stages these tests drive, as the install engine runs them: the restore stage
// unless an earlier attempt completed it, then the activate stage.
func recoveryAttempt(s *Session, restoreDone *bool) error {
	return recoveryAttemptWithin(context.Background(), s, restoreDone)
}

// recoveryAttemptWithin is recoveryAttempt bounded by ctx, for a run that must
// stop at once and would otherwise wait on the wall clock if it did not.
func recoveryAttemptWithin(ctx context.Context, s *Session, restoreDone *bool) error {
	if !*restoreDone {
		if err := stageRecoveryRestore(ctx, s); err != nil {
			return err
		}
		*restoreDone = true
	}
	return stageRecoveryActivate(ctx, s)
}

// journalIn moves the session's journal into a directory of its own and
// returns it, so a test can read the journal back as a new process would, and
// can fail exactly one save by making the directory unwritable at that instant.
func journalIn(t *testing.T, s *Session) string {
	t.Helper()
	dir := t.TempDir()
	jnl, err := OpenJournal(filepath.Join(dir, "journal.json"), s.Opts.Identity())
	if err != nil {
		t.Fatalf("opening the journal in %s: %v", dir, err)
	}
	s.Jnl = jnl
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return dir
}

// requireRecoveredOnce is the end state a recovery must reach whatever
// interrupted it: each namespace's data brought back by exactly one Velero
// Restore; each namespace restore activated exactly once, its pause lifted once
// and its CronJob put back once; each namespace recorded as activated once; and
// the cluster's operation record terminal with nothing parked under it.
func requireRecoveredOnce(t *testing.T, s *Session, runner *recordingRunner, cluster *syncingVelero, namespaces ...*syncingVelero) {
	t.Helper()
	for _, ns := range namespaces {
		name := ns.name()
		if got := restoresOf(runner, name); got != 1 {
			t.Errorf("namespace %s was brought back by %d Velero Restores, want exactly 1", name, got)
		}
		if got := ranCount(runner, "annotate project "+name+" -n "+backup.ProjectCRNamespace+" "+backup.PauseAnnotationKey+"-"); got != 1 {
			t.Errorf("the pause on %s was lifted %d times, want exactly 1", name, got)
		}
		if got := ranCount(runner, "patch cronjob "+recoveryActivateCronJob+" -n "+name+" "); got != 1 {
			t.Errorf("CronJob %s/%s was put back %d times, want exactly 1", name, recoveryActivateCronJob, got)
		}
		activated := 0
		for _, done := range s.Record.RecoveryNamespacesActivated {
			if done == name {
				activated++
			}
		}
		if activated != 1 {
			t.Errorf("namespace %s is recorded as activated %d times (%v), want exactly 1", name, activated, s.Record.RecoveryNamespacesActivated)
		}
	}
	if op, terminal, parked := liveOperation(t, cluster); !terminal || len(parked) != 0 {
		t.Errorf("the cluster's operation record ends as %s, terminal=%t, with %v parked under it", op, terminal, parked)
	}
}

// THE REVIEW OF 3f398cb, FINDING 1 (2026-11-12, kn-ship-1-2-closeout-lzjb.5). A
// namespace restore's operation exists in the cluster from the instant it takes
// the record, and the install journal learned its id only after the whole
// restore had returned. A run that stopped in between left an operation the
// identical command could not name: it started that namespace again as a new
// operation and was refused by the one it had lost, and every restore parked
// under that one could never be activated.
//
// EACH CASE BREAKS THE SECOND OF THREE NAMESPACES at one boundary, then runs
// the identical command again from the journal alone. The end state is the same
// in every case.
//
// THE PLANTED NEGATIVE IS 3f398cb: the second run is refused by the operation
// the first one lost, and in the last case the first run restores the
// namespace although the journal could not name its operation.
func TestARecoveryWhoseJournalMissedAnOperationFinishes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fault breaks the second namespace's restore; dir is the journal's
		// directory, which a fault makes unwritable to fail a save.
		fault func(first, second *syncingVelero, dir string) func(string, []byte) faultKind
		// changedNothing is set when the first run must stop before the second
		// namespace's first change.
		changedNothing bool
	}{
		{
			name: "the journal save after the operation returned fails",
			fault: func(first, second *syncingVelero, dir string) func(string, []byte) faultKind {
				return func(command string, input []byte) faultKind {
					// The restore's last write stops its executor at `restored —
					// awaiting activation`; the journal save after the return fails.
					if _, rec := recordDoc(input); isOperationRecordWrite(command) && rec != nil && rec.OperationID == second.opID &&
						rec.Stage == backup.RestoredStage && rec.Executor.State == operation.ExecutorStopped {
						_ = os.Chmod(dir, 0o500)
					}
					return noFault
				}
			},
		},
		{
			name: "the reply to the record's compare-and-swap is lost",
			fault: func(first, second *syncingVelero, _ string) func(string, []byte) faultKind {
				lost := false
				return func(command string, input []byte) faultKind {
					if !lost && first.takesRecordFor(second.name(), command, input) {
						lost = true
						return loseReply
					}
					return noFault
				}
			},
		},
		{
			name: "the journal save naming the operation fails",
			fault: func(first, second *syncingVelero, dir string) func(string, []byte) faultKind {
				return func(command string, input []byte) faultKind {
					if first.takesRecordFor(second.name(), command, input) {
						_ = os.Chmod(dir, 0o500)
					}
					return noFault
				}
			},
			changedNothing: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second, third := threeProjectCluster()
			s, runner, _ := recoveryWaitSession(t, first)
			s.Record.RecoveryOperationID = recoveryActivateOpID
			dir := journalIn(t, s)
			first.fault = tc.fault(first, second, dir)

			restoreDone := false
			if err := recoveryAttempt(s, &restoreDone); err == nil {
				t.Fatal("the first run finished although its second namespace's restore was broken")
			}
			if tc.changedNothing && runner.ranAny("annotate project "+second.name()+" ") {
				t.Errorf("the restore of %s changed its Project although the journal could not name its operation:\n%s", second.name(), runner.ranAll())
			}
			first.fault = nil
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			resumeFromJournal(t, s, dir)
			if err := recoveryAttempt(s, &restoreDone); err != nil {
				t.Fatalf("the identical command failed after the interruption: %v", err)
			}
			requireRecoveredOnce(t, s, runner, first, first, second, third)
		})
	}
}

// THE REVIEW'S FINDING 2. Activation takes a namespace restore back as the live
// record with its executor running. On 3f398cb an error right after that
// returned without releasing the record, and a run that died there left it the
// same way: the identical command was refused by that operation's own running
// executor, and every restore parked under it stayed parked.
//
// THE PLANTED NEGATIVE IS 3f398cb: the second run is refused with "still
// running" on the second namespace's operation.
func TestARecoveryWhoseActivationWasInterruptedFinishes(t *testing.T) {
	// takesBack reports whether a command is the write that takes the second
	// namespace's restore back as the live record for its activation.
	takesBack := func(first, second *syncingVelero, command string, input []byte) bool {
		name, rec := recordDoc(input)
		return isOperationRecordWrite(command) && rec != nil && name == operation.Name && rec.OperationID == second.opID &&
			rec.Stage == backup.RestoredStage && rec.Executor.State == operation.ExecutorRunning && first.liveID() != second.opID
	}
	for _, tc := range []struct {
		name  string
		fault func(first, second *syncingVelero) func(string, []byte) faultKind
		// released is set when the first run must hand the record back itself:
		// an error, unlike a lost connection, leaves it able to.
		released bool
	}{
		{
			name:     "an error immediately after the take-over",
			released: true,
			fault: func(first, second *syncingVelero) func(string, []byte) faultKind {
				armed := false
				return func(command string, input []byte) faultKind {
					if takesBack(first, second, command, input) {
						armed = true
						return noFault
					}
					if armed && strings.Contains(command, "get project "+second.name()+" ") {
						armed = false
						return failCommand
					}
					return noFault
				}
			},
		},
		{
			name: "the reply to the take-over is lost",
			fault: func(first, second *syncingVelero) func(string, []byte) faultKind {
				lost := false
				return func(command string, input []byte) faultKind {
					if !lost && takesBack(first, second, command, input) {
						lost = true
						return loseReply
					}
					return noFault
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second, third := threeProjectCluster()
			s, runner, _ := recoveryWaitSession(t, first)
			s.recoveryOpID = recoveryActivateOpID
			s.Record.RecoveryOperationID = recoveryActivateOpID
			dir := journalIn(t, s)
			first.fault = tc.fault(first, second)

			restoreDone := false
			if err := recoveryAttempt(s, &restoreDone); err == nil {
				t.Fatal("the first run finished although the second namespace's activation was broken")
			}
			if !restoreDone {
				t.Fatal("the first run stopped in the restore stage, so this test is not about activation")
			}
			if _, live := recordDoc(first.record); tc.released && (live == nil || live.Executor.State != operation.ExecutorStopped) {
				t.Errorf("the failed activation did not hand back the record it claimed: the live record is %s", recordSummary(live))
			}
			first.fault = nil
			resumeFromJournal(t, s, dir)
			if err := recoveryAttempt(s, &restoreDone); err != nil {
				t.Fatalf("the identical command failed after the interrupted activation: %v", err)
			}
			requireRecoveredOnce(t, s, runner, first, first, second, third)
		})
	}
}

// TestARecoveryWithAnIneligibleMiddleNamespaceActivatesTheOthers is the trace
// the review followed and no test ran: the middle of three namespaces has no
// copy of a volume its backup expected, so it is held and not restored; the
// first and the last are restored, the last parking the first, and both are
// activated, while nothing is written to the middle one's Project.
func TestARecoveryWithAnIneligibleMiddleNamespaceActivatesTheOthers(t *testing.T) {
	first, second, third := threeProjectCluster()
	first.setRecord = recoveryWaitRecordOf(first.coverageNames(), map[string][]string{second.name(): {"data"}})
	s, runner, _ := recoveryWaitSession(t, first)
	s.recoveryOpID = recoveryActivateOpID
	s.Record.RecoveryOperationID = recoveryActivateOpID

	restoreDone := false
	if err := recoveryAttempt(s, &restoreDone); err != nil {
		t.Fatalf("a recovery with an ineligible middle namespace failed: %v", err)
	}
	requireRecoveredOnce(t, s, runner, first, first, third)
	if !containsString(s.Record.RecoveryNamespacesNotRestored, second.name()) ||
		containsString(s.Record.RecoveryNamespacesRestored, second.name()) || containsString(s.Record.RecoveryNamespacesActivated, second.name()) {
		t.Errorf("the ineligible namespace %s is recorded as not-restored=%v restored=%v activated=%v; want held and nothing else",
			second.name(), s.Record.RecoveryNamespacesNotRestored, s.Record.RecoveryNamespacesRestored, s.Record.RecoveryNamespacesActivated)
	}
	if restoresOf(runner, second.name()) != 0 || runner.ranAny("project "+second.name()+" ") {
		t.Errorf("the recovery restored or touched the Project of %s, whose backup is ineligible:\n%s", second.name(), runner.ranAll())
	}
}

// TestARecoveryTakesOverOnlyAnIdleEarlierRunOfItsOwn is the planted negative for
// the take-over the identical command now makes on its own. It takes over a
// running executor only when that executor is this operator's and the record
// shows nothing of it in flight. Anything else is the operator's to assert, and
// the command says how and changes nothing.
func TestARecoveryTakesOverOnlyAnIdleEarlierRunOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(first, second *syncingVelero) func(string, []byte) faultKind
		// then is what happens between the two runs.
		then  func(t *testing.T, first *syncingVelero)
		wants []string
	}{
		{
			name: "an action is in flight",
			fault: func(first, second *syncingVelero) func(string, []byte) faultKind {
				gone := false
				return func(command string, input []byte) faultKind {
					if gone {
						return failCommand
					}
					// The first write that marks one of the second namespace's
					// actions submitted lands; then the connection is gone for
					// good, so nothing can record the outcome or stop the executor.
					name, rec := recordDoc(input)
					if isOperationRecordWrite(command) && rec != nil && name == operation.Name && rec.Request.Versions["namespace"] == second.name() {
						for _, action := range rec.Actions {
							if action.Status == operation.ActionSubmitted {
								gone = true
								return loseReply
							}
						}
					}
					return noFault
				}
			},
			wants: []string{"in flight", "--take-over", "--confirm"},
		},
		{
			name: "another operator's executor holds it",
			fault: func(first, second *syncingVelero) func(string, []byte) faultKind {
				lost := false
				return func(command string, input []byte) faultKind {
					if !lost && first.takesRecordFor(second.name(), command, input) {
						lost = true
						return loseReply
					}
					return noFault
				}
			},
			then: func(t *testing.T, first *syncingVelero) {
				first.editLive(t, func(rec *operation.Record) { rec.Executor.Operator = "ops@another-laptop" })
			},
			wants: []string{"ops@another-laptop", "--take-over", "--confirm"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := twoProjectCluster()
			s, runner, _ := recoveryWaitSession(t, first)
			s.Record.RecoveryOperationID = recoveryActivateOpID
			dir := journalIn(t, s)
			first.fault = tc.fault(first, second)

			restoreDone := false
			if err := recoveryAttempt(s, &restoreDone); err == nil {
				t.Fatal("the first run finished although its second namespace's restore was broken")
			}
			first.fault = nil
			if tc.then != nil {
				tc.then(t, first)
			}
			_, before := recordDoc(first.record)
			if before == nil || before.Executor.State != operation.ExecutorRunning {
				t.Fatalf("the first run left the live record %s; this test needs one whose executor is still running", recordSummary(before))
			}
			resumeFromJournal(t, s, dir)
			restoredBefore := restoresOf(runner, second.name())

			// Bounded: a run that wrongly took the record over goes on to wait for
			// a pause nobody wrote, on the wall clock.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := recoveryAttemptWithin(ctx, s, &restoreDone)
			if err == nil {
				t.Fatal("the identical command took over a running executor it could not show was idle and its own")
			}
			for _, want := range append(tc.wants, before.OperationID) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q, so the operator cannot tell what to do:\n%v", want, err)
				}
			}
			_, after := recordDoc(first.record)
			if after.OperationID != before.OperationID || after.Executor.Token != before.Executor.Token || len(after.TakeOvers) != len(before.TakeOvers) {
				t.Errorf("the refusal changed the record: it was %s held by token %s with %d take-over(s), it is %s held by %s with %d",
					before.OperationID, before.Executor.Token, len(before.TakeOvers), after.OperationID, after.Executor.Token, len(after.TakeOvers))
			}
			if got := restoresOf(runner, second.name()); got != restoredBefore {
				t.Errorf("the refused run restored %s again (%d Velero Restores, %d before it)", second.name(), got, restoredBefore)
			}
		})
	}
}

// TestAnAbortedParkedRestoreIsHeldByItsPauseAlone is the review's finding 3,
// which is the abort policy of a single restore, kept as it is, applied to a
// parked one. Aborting the last parked restore closes it, and the cluster's
// operation record then holds nothing: the namespace is held by its Project's
// pause and by nothing else. The test pins that, and that every message says
// a step that can be taken — `--activate` refuses a closed operation.
func TestAnAbortedParkedRestoreIsHeldByItsPauseAlone(t *testing.T) {
	first, second := twoProjectCluster()
	s, runner, clock := recoveryWaitSession(t, first)
	s.recoveryOpID = recoveryActivateOpID
	s.Record.RecoveryOperationID = recoveryActivateOpID
	dir := journalIn(t, s)
	if err := stageRecoveryRestore(context.Background(), s); err != nil {
		t.Fatalf("the restore stage failed: %v", err)
	}
	server, err := s.Server()
	if err != nil {
		t.Fatal(err)
	}
	deps := recoveryRestoreDeps(server)
	deps.Now = func() time.Time { return clock.now }
	deps.Poll = time.Minute
	deps.Sleep = recoveryRestoreSync.Sleep
	run := func(opts backup.RestoreOptions) (string, error) {
		var out bytes.Buffer
		opts.Cluster, opts.Bundle = s.recoveryName(), s.Bundle
		err := backup.RunRestore(context.Background(), &out, nil, opts, deps)
		return out.String(), err
	}

	// The second namespace is activated by hand, and the first, parked under
	// it, is aborted. An abort whose closing write fails releases its claim,
	// so the same abort finishes it.
	if _, err := run(backup.RestoreOptions{Namespace: second.name(), Activate: second.opID, Confirm: true, KeepDesired: true}); err != nil {
		t.Fatalf("activating %s by hand: %v", second.name(), err)
	}
	aborted := first.opID
	failed := false
	first.fault = func(command string, input []byte) faultKind {
		if _, rec := recordDoc(input); !failed && isOperationRecordWrite(command) && rec != nil && rec.OperationID == aborted && rec.Terminal {
			failed = true
			return failCommand
		}
		return noFault
	}
	if _, err := run(backup.RestoreOptions{Abort: aborted}); err == nil {
		t.Fatal("the abort reported success although its closing write failed")
	}
	first.fault = nil
	out, err := run(backup.RestoreOptions{Abort: aborted})
	if err != nil {
		t.Fatalf("the same abort, run again after its closing write failed, was refused: %v", err)
	}

	// THE RECORD HOLDS NOTHING NOW, which is the existing abort policy: a new
	// operation may take it.
	if op, terminal, parked := liveOperation(t, first); !terminal || len(parked) != 0 {
		t.Fatalf("after the abort the record is %s, terminal=%t, with %v parked", op, terminal, parked)
	}
	other, err := deps.Store.Acquire(context.Background(), operation.Request{Kind: operation.KindUpgrade, Cluster: s.recoveryName()})
	if err != nil {
		t.Fatalf("after the abort another operation was refused the record: %v", err)
	}
	if err := deps.Store.Complete(context.Background(), other, operation.ResultSucceeded); err != nil {
		t.Fatal(err)
	}
	// WHAT HOLDS THE NAMESPACE IS ITS PAUSE: the abort neither lifted nor
	// replaced it, and did not put the suspended CronJob back.
	if first.pauseLifted || first.opID != aborted {
		t.Errorf("the abort lifted or replaced the pause on %s (pause by %q, lifted=%t)", first.name(), first.opID, first.pauseLifted)
	}
	if ranCount(runner, "patch cronjob "+recoveryActivateCronJob+" -n "+first.name()+" ") != 0 {
		t.Errorf("the abort put CronJob %s/%s back", first.name(), recoveryActivateCronJob)
	}
	if strings.Contains(out, "--activate "+aborted) {
		t.Errorf("the abort advises --activate on the operation it just closed, which refuses it:\n%s", out)
	}
	requireContains(t, "the abort's advice", out, "kubenest backup restore --namespace "+first.name(), "--replace")

	// The recovery, run again, does not activate the aborted restore, keeps its
	// pause, and says what to do instead.
	resumeFromJournal(t, s, dir)
	err = stageRecoveryActivate(context.Background(), s)
	if err == nil {
		t.Fatal("the recovery activated a namespace whose restore was aborted")
	}
	if strings.Contains(err.Error(), "--activate "+aborted) {
		t.Errorf("the recovery advises --activate on an aborted operation, which refuses it:\n%v", err)
	}
	requireContains(t, "the recovery's refusal", err.Error(), aborted, "kubenest backup restore --namespace "+first.name())
	if first.pauseLifted {
		t.Errorf("the recovery lifted the pause on %s, whose restore was aborted", first.name())
	}

	// The operator does what both messages say: restores the namespace again
	// and activates that restore. The identical command then finishes.
	if _, err := run(backup.RestoreOptions{Namespace: first.name(), From: recoveryWaitBackup, Replace: true, Confirm: true, AcceptDataAge: true}); err != nil {
		t.Fatalf("restoring %s again by hand: %v", first.name(), err)
	}
	if first.opID == aborted {
		t.Fatal("the restore by hand did not pause the project with its own operation")
	}
	if _, err := run(backup.RestoreOptions{Namespace: first.name(), Activate: first.opID, Confirm: true, KeepDesired: true}); err != nil {
		t.Fatalf("activating the restore by hand: %v", err)
	}
	resumeFromJournal(t, s, dir)
	if err := stageRecoveryActivate(context.Background(), s); err != nil {
		t.Fatalf("after the operator restored and activated %s again, the identical command still failed: %v", first.name(), err)
	}
	if !containsString(s.Record.RecoveryNamespacesActivated, first.name()) {
		t.Errorf("the recovery recorded %v, and %s is not among them", s.Record.RecoveryNamespacesActivated, first.name())
	}
}
