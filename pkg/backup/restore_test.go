package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
)

// T4.1's acceptance: the restore plan and its refusals, the identity
// re-verification, a non-Completed Velero Restore, and the three follow-on
// commands. Every one of them is decided in this package, so every one of them
// is provable with no cluster.
//
// THE FAKES ARE TWO: a BackupSource that answers what the backups are, and a
// RestoreCluster that holds the namespace, its claims and the Velero objects.
// The store runner is a third, smaller one: it is an in-memory `kube-system`
// holding the operation record, so a resume and an activation exercise the real
// pkg/operation path rather than a stub of it.

// restoreNow is the tests' clock. It is fixed so a data age is a number a test
// can assert rather than one it has to recompute.
var restoreNow = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func at(offset time.Duration) time.Time { return restoreNow.Add(offset) }

// testManifest is the bundle the tests restore against: the deadlines every
// wait reads, and T2.0's recovery-point policy.
func testRestoreManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	body := `bundle: "1.2"
limits:
  timeouts:
    backup: 30m
    restore-drill: 45m
    component-ready: 2m
health:
  backup:
    max-backup-age: 48h
    recovery-point-age: 26h
    max-restore-drill-age: 336h
    unconfigured-target-grace: 168h
`
	parsed, err := manifest.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parsing the test manifest: %v", err)
	}
	return parsed
}

// fakeBackupSource is the eligibility source a test scripts.
type fakeBackupSource struct {
	byNamespace map[string][]BackupFacts
}

func (f *fakeBackupSource) TerminalBackups(_ context.Context, namespace string) ([]BackupFacts, error) {
	list := append([]BackupFacts(nil), f.byNamespace[namespace]...)
	return list, nil
}

func (f *fakeBackupSource) NamedBackup(_ context.Context, namespace, name string) (BackupFacts, error) {
	for _, facts := range f.byNamespace[namespace] {
		if facts.Name == name {
			return facts, nil
		}
	}
	return BackupFacts{}, fmt.Errorf("%w: %s", ErrNoBackup, name)
}

// factsFor builds one completed, eligible backup whose single claim is
// "data-0", started `age` before the test's now.
func eligibleFacts(t *testing.T, name string, captureStart, completed time.Time, claims ...string) BackupFacts {
	t.Helper()
	facts := BackupFacts{
		Name:                 name,
		Phase:                "Completed",
		CaptureStartedAt:     captureStart.UTC().Format(time.RFC3339),
		CompletedAt:          completed.UTC().Format(time.RFC3339),
		StorageLocation:      "default",
		StorageLocationPhase: "Available",
		ConsistencyMethod:    "uncoordinated-copy",
		Consistency:          "uncoordinated copy",
	}
	for _, claim := range claims {
		facts.Expected = append(facts.Expected, VolumeRef{Namespace: "payments", Name: claim, UID: "uid-" + claim})
	}
	return facts
}

// fakeCluster is the cluster side of a restore. Every field a test does not set
// has a benign default, and the WRITES go through the runner the run decorated
// with the operation record — so skipping a recorded action really does stop
// the command from reaching the transport.
type fakeCluster struct {
	runner k3s.Runner

	namespace     *NamespaceState
	claims        []VolumeRef
	claimLabels   map[string]map[string]string
	workloads     []WorkloadState
	pods          []PodState
	cronjobs      []CronJobState
	jobs          []JobState
	apps          []ApplicationState
	hold          *ProjectHold
	drill         *DrillRestore
	volumes       []VolumeRestoreState
	outcome       *RestoreOutcome
	bindings      map[string]*ClaimBinding
	readyNodes    map[string]bool
	selectorByPod map[string]map[string]string
	// onProjectHold runs when the operator's acknowledgement is read, which is
	// the window between the confirmed plan and the destructive step: a test
	// moves an identity here.
	onProjectHold func()
	// resolveReplicaSetOwner is what a ReplicaSet resolves to, for the pod ->
	// Deployment walk. Nil models a ReplicaSet nobody owns.
	resolveReplicaSetOwner *OwnerRef

	// observations
	annotations   map[string]string
	cleared       []string
	deleted       bool
	deletedClaims []string
	claimsGone    map[string]bool
	scaled        []string
	suspended     map[string]bool
	jobsSuspended map[string]bool
	backupsMade   []string
	restoresMade  []restoreRequest
	modifiersMade []string
	// appliedDocs are the raw documents that reached the API server, so a test
	// can assert what a modifier RULE says rather than only its name.
	restoreDocs []restoreDoc
}

// restoreDoc is one document a restore's run streamed to the cluster.
type restoreDoc struct {
	What string
	Doc  []byte
}

func newFakeCluster(namespace *NamespaceState, runner k3s.Runner) *fakeCluster {
	return &fakeCluster{
		runner:        runner,
		namespace:     namespace,
		claimLabels:   map[string]map[string]string{},
		annotations:   map[string]string{},
		suspended:     map[string]bool{},
		bindings:      map[string]*ClaimBinding{},
		readyNodes:    map[string]bool{},
		selectorByPod: map[string]map[string]string{},
		claimsGone:    map[string]bool{},
	}
}

// WithRunner binds the record-decorated transport for a stage's writes. The
// SAME cluster is returned rather than a copy: the run's later reads must see
// what the writes did, which is what makes "the namespace is gone" observable
// without a second fake.
func (c *fakeCluster) WithRunner(r k3s.Runner) RestoreCluster {
	c.runner = r
	return c
}

func (c *fakeCluster) command(ctx context.Context, args string) error {
	if c.runner == nil {
		return nil
	}
	res, err := c.runner.Run(ctx, "sudo -n k3s kubectl "+args)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("kubectl %s: exit %d: %s", args, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func (c *fakeCluster) Namespace(_ context.Context, name string) (*NamespaceState, error) {
	if c.namespace == nil || c.namespace.Name != name {
		return nil, nil
	}
	return c.namespace, nil
}

func (c *fakeCluster) Claims(_ context.Context, namespace string) ([]VolumeRef, error) {
	out := []VolumeRef{}
	for _, claim := range c.claims {
		if claim.Namespace != namespace || c.deletedFor(claim.Name) {
			continue
		}
		if labels := c.claimLabels[claim.Name]; len(labels) > 0 {
			claim.Detail = "phase Bound, labels " + describeLabels(labels)
		}
		out = append(out, claim)
	}
	return out, nil
}

func (c *fakeCluster) deletedFor(name string) bool {
	return c.claimsGone[name]
}

func (c *fakeCluster) Workloads(_ context.Context, _ string) ([]WorkloadState, error) {
	return c.workloads, nil
}

func (c *fakeCluster) Pods(_ context.Context, namespace string) ([]PodState, error) {
	out := []PodState{}
	for _, pod := range c.pods {
		if pod.Namespace != namespace || c.deleted {
			continue
		}
		out = append(out, pod)
	}
	return out, nil
}

func (c *fakeCluster) CronJobs(_ context.Context, _ string) ([]CronJobState, error) {
	return c.cronjobs, nil
}

func (c *fakeCluster) Jobs(_ context.Context, _ string) ([]JobState, error) {
	return c.jobs, nil
}

func (c *fakeCluster) Applications(_ context.Context, _ string) ([]ApplicationState, error) {
	return c.apps, nil
}

func (c *fakeCluster) ProjectHold(_ context.Context, _ string) (*ProjectHold, error) {
	if c.onProjectHold != nil {
		c.onProjectHold()
	}
	if c.hold == nil {
		return nil, nil
	}
	hold := *c.hold
	// The acknowledgement names the operation whose annotation holds the
	// project, which is what the run waits for; a fixture's canned message
	// cannot know the id the record mints.
	if id := c.annotations[PauseAnnotationKey]; id != "" {
		hold.ConditionStatus = "True"
		hold.ConditionReason = ReasonPausedByOperation
		hold.ConditionMessage = fmt.Sprintf("Reconciliation of project %q is paused by operation %q", "payments", id)
	}
	return &hold, nil
}

func (c *fakeCluster) DrillRestore(_ context.Context) (*DrillRestore, error) {
	return c.drill, nil
}

func (c *fakeCluster) VolumeRestores(_ context.Context, _ string) ([]VolumeRestoreState, error) {
	return c.volumes, nil
}

func (c *fakeCluster) RestoreOutcome(_ context.Context, name string) (*RestoreOutcome, error) {
	if c.outcome == nil {
		return &RestoreOutcome{Name: name, Phase: "InProgress"}, nil
	}
	outcome := *c.outcome
	outcome.Name = name
	return &outcome, nil
}

func (c *fakeCluster) OperatorImage(_ context.Context) (string, error) {
	return "ghcr.io/kubenesthq/kubenest-operator:1.2", nil
}

func (c *fakeCluster) ClaimBinding(_ context.Context, _, claim string) (*ClaimBinding, error) {
	if binding, ok := c.bindings[claim]; ok {
		return binding, nil
	}
	return &ClaimBinding{Claim: claim, Phase: "Pending"}, nil
}

func (c *fakeCluster) NodeReady(_ context.Context, node string) (bool, error) {
	return c.readyNodes[node], nil
}

func (c *fakeCluster) ControllerOwner(_ context.Context, _, kind, _ string) (*OwnerRef, error) {
	if strings.EqualFold(kind, "replicaset") {
		return c.resolveReplicaSetOwner, nil
	}
	return nil, nil
}

func (c *fakeCluster) AnnotateProject(ctx context.Context, namespace, key, value string) error {
	if err := c.command(ctx, "annotate project "+namespace+" -n "+ProjectCRNamespace+" "+key+"="+value+" --overwrite"); err != nil {
		return err
	}
	c.annotations[key] = value
	return nil
}

func (c *fakeCluster) ClearProjectAnnotation(ctx context.Context, namespace, key string) error {
	if err := c.command(ctx, "annotate project "+namespace+" -n "+ProjectCRNamespace+" "+key+"-"); err != nil {
		return err
	}
	delete(c.annotations, key)
	c.cleared = append(c.cleared, key)
	return nil
}

func (c *fakeCluster) ScaleWorkload(ctx context.Context, kind, name, namespace string, replicas int32) error {
	if err := c.command(ctx, fmt.Sprintf("scale %s %s -n %s --replicas=%d", kind, name, namespace, replicas)); err != nil {
		return err
	}
	c.scaled = append(c.scaled, fmt.Sprintf("%s/%s=%d", kind, name, replicas))
	if replicas == 0 {
		// The pods go with the scale-down, which is what the run waits for.
		c.pods = nil
	}
	return nil
}

func (c *fakeCluster) DeleteNamespace(ctx context.Context, name string) error {
	if err := c.command(ctx, "delete namespace "+name+" --wait=false"); err != nil {
		return err
	}
	c.deleted = true
	c.namespace = nil
	return nil
}

func (c *fakeCluster) DeleteClaim(ctx context.Context, namespace, name string) error {
	if err := c.command(ctx, "delete persistentvolumeclaim "+name+" -n "+namespace+" --wait=false"); err != nil {
		return err
	}
	// Two records, on purpose: deletedClaims is the LOG of what this run
	// deleted (a test asserts on it), and claimsGone is the STATE the restore
	// then undoes when it puts the claim back.
	c.deletedClaims = append(c.deletedClaims, name)
	c.claimsGone[name] = true
	return nil
}

func (c *fakeCluster) SuspendCronJob(ctx context.Context, namespace, name string, suspend bool) error {
	if err := c.command(ctx, fmt.Sprintf("patch cronjob %s -n %s --type=merge -p '{\"spec\":{\"suspend\":%t}}'", name, namespace, suspend)); err != nil {
		return err
	}
	c.suspended[name] = suspend
	return nil
}

func (c *fakeCluster) SuspendJob(ctx context.Context, namespace, name string, suspend bool) error {
	if err := c.command(ctx, fmt.Sprintf("patch job %s -n %s --type=merge -p '{\"spec\":{\"suspend\":%t}}'", name, namespace, suspend)); err != nil {
		return err
	}
	if c.jobsSuspended == nil {
		c.jobsSuspended = map[string]bool{}
	}
	c.jobsSuspended[name] = suspend
	return nil
}

func (c *fakeCluster) CreateBackup(ctx context.Context, name string, doc []byte) error {
	if err := c.applyDocument(ctx, name, doc); err != nil {
		return err
	}
	c.backupsMade = append(c.backupsMade, name)
	return nil
}

func (c *fakeCluster) CreateRestore(ctx context.Context, name string, doc []byte) error {
	if err := c.applyDocument(ctx, name, doc); err != nil {
		return err
	}
	spec := restoreRequest{Name: name}
	var parsed struct {
		Spec struct {
			Backup             string           `yaml:"backupName"`
			IncludedNamespaces []string         `yaml:"includedNamespaces"`
			IncludedResources  []string         `yaml:"includedResources"`
			ExcludedResources  []string         `yaml:"excludedResources"`
			LabelSelector      map[string]any   `yaml:"labelSelector"`
			OrLabelSelectors   []map[string]any `yaml:"orLabelSelectors"`
			RestorePVs         bool             `yaml:"restorePVs"`
			ResourceModifier   map[string]any   `yaml:"resourceModifier"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(doc, &parsed); err == nil {
		spec.Backup = parsed.Spec.Backup
		spec.IncludedResources = parsed.Spec.IncludedResources
		if len(parsed.Spec.IncludedNamespaces) > 0 {
			spec.Namespace = parsed.Spec.IncludedNamespaces[0]
		}
	}
	c.restoresMade = append(c.restoresMade, spec)
	// THE RESTORE PUTS THE NAMESPACE AND ITS CLAIMS BACK, as new objects: a
	// namespace the reconcilers recreated and a claim Velero provisioned fresh
	// both have new UIDs, which is why activation re-reads them.
	if spec.Namespace != "" {
		c.namespace = &NamespaceState{Name: spec.Namespace, UID: "uid-ns-restored"}
		c.claimsGone = map[string]bool{}
	}
	return nil
}

func (c *fakeCluster) Apply(ctx context.Context, what string, doc []byte) error {
	if err := c.applyDocument(ctx, what, doc); err != nil {
		return err
	}
	c.modifiersMade = append(c.modifiersMade, what)
	c.restoreDocs = append(c.restoreDocs, restoreDoc{What: what, Doc: append([]byte(nil), doc...)})
	return nil
}

func (c *fakeCluster) applyDocument(ctx context.Context, name string, doc []byte) error {
	if c.runner == nil {
		return nil
	}
	res, err := c.runner.RunInput(ctx, "sudo -n k3s kubectl apply -f -", strings.NewReader(string(doc)))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("apply %s: exit %d: %s", name, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func (c *fakeCluster) Delete(ctx context.Context, args string) error {
	return c.command(ctx, "delete "+args)
}

// fakeOpKube is an in-memory `kube-system` for the operation record, and a
// transport that answers every other command successfully. It exists so a
// resume and an activation drive the real pkg/operation path.
type fakeOpKube struct {
	mu       sync.Mutex
	objects  map[string]map[string]any
	rev      int
	commands []string
}

func newFakeOpKube() *fakeOpKube {
	return &fakeOpKube{objects: map[string]map[string]any{}}
}

func (k *fakeOpKube) Run(_ context.Context, command string) (sshx.Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.commands = append(k.commands, command)
	if strings.Contains(command, "get backup ") && strings.Contains(command, "-n velero") {
		// The safety backup's settle probe reads Velero's own object; the fake
		// cluster's backup is Completed as soon as it exists.
		return sshx.Result{Stdout: `{"status":{"phase":"Completed"}}`}, nil
	}
	if name, ok := configMapNameIn(command); ok {
		obj, found := k.objects[name]
		if !found {
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "` + name + `" not found`}, nil
		}
		raw, err := json.Marshal(obj)
		if err != nil {
			return sshx.Result{}, err
		}
		return sshx.Result{Stdout: string(raw)}, nil
	}
	// The Observe probes a resume runs answer "the postcondition holds" when a
	// test asks them to; by default they exit 0 so a recorded action is treated
	// as established.
	return sshx.Result{ExitCode: 0}, nil
}

func (k *fakeOpKube) RunInput(_ context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	body, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		// The record ConfigMap is JSON; anything else (a Velero document the
		// run only submits) is accepted as it is.
		k.commands = append(k.commands, command)
		return sshx.Result{ExitCode: 0}, nil
	}
	metadata, _ := doc["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	if name == "" {
		k.commands = append(k.commands, command)
		return sshx.Result{ExitCode: 0}, nil
	}
	k.commands = append(k.commands, command)
	k.rev++
	metadata["resourceVersion"] = fmt.Sprintf("%d", k.rev)
	doc["metadata"] = metadata
	k.objects[name] = doc
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": fmt.Sprintf("%d", k.rev)}})
	if err != nil {
		return sshx.Result{}, err
	}
	return sshx.Result{ExitCode: 0, Stdout: string(raw)}, nil
}

// recordFor decodes the record the in-memory cluster holds, or nil.
func (k *fakeOpKube) recordFor(name string) *operation.Record {
	k.mu.Lock()
	defer k.mu.Unlock()
	obj, found := k.objects[name]
	if !found {
		return nil
	}
	data, _ := obj["data"].(map[string]any)
	raw, _ := data["record.json"].(string)
	if raw == "" {
		return nil
	}
	var rec operation.Record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil
	}
	return &rec
}

func (k *fakeOpKube) sawCommand(needle string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, command := range k.commands {
		if strings.Contains(command, needle) {
			return true
		}
	}
	return false
}

func (k *fakeOpKube) commandCount(needle string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	count := 0
	for _, command := range k.commands {
		if strings.Contains(command, needle) {
			count++
		}
	}
	return count
}

func configMapNameIn(command string) (string, bool) {
	i := strings.Index(command, "get configmap ")
	if i < 0 {
		return "", false
	}
	rest := command[i+len("get configmap "):]
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// restoreFixture is a namespace with one claim, one workload and one due
// CronJob, an eligible backup, and a cluster that acknowledges the pause.
type restoreFixture struct {
	kube    *fakeOpKube
	cluster *fakeCluster
	backups *fakeBackupSource
	now     time.Time
	clock   *advancingClock
}

func newRestoreFixture(t *testing.T, facts ...BackupFacts) *restoreFixture {
	t.Helper()
	now := restoreNow
	kube := newFakeOpKube()
	cluster := newFakeCluster(&NamespaceState{Name: "payments", UID: "uid-ns"}, kube)
	cluster.claims = []VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0"}}
	cluster.claimLabels["data-0"] = map[string]string{"app": "payments"}
	cluster.workloads = []WorkloadState{{Kind: "deployment", Name: "payments", Replicas: 2, Selector: map[string]string{"app": "payments"}}}
	cluster.cronjobs = []CronJobState{{Name: "nightly", Schedule: "0 2 * * *", Suspend: false}}
	cluster.hold = &ProjectHold{
		ConditionStatus:  "True",
		ConditionReason:  ReasonPausedByOperation,
		ConditionMessage: "Reconciliation of project \"payments\" is paused by operation \"op-1\"",
	}
	cluster.outcome = &RestoreOutcome{Name: "restore", Phase: "Completed", ItemsRestored: 12, ProgressSeen: true}
	cluster.volumes = []VolumeRestoreState{{Name: "pvr-1", Pod: "payments-1", Volume: "data-0", ClaimName: "data-0", Phase: "Completed"}}
	cluster.bindings["data-0"] = &ClaimBinding{Claim: "data-0", Volume: "pvc-uid-data-0", Node: "lab-node-1", Phase: "Bound", Bound: true}
	cluster.readyNodes["lab-node-1"] = true
	source := &fakeBackupSource{byNamespace: map[string][]BackupFacts{"payments": facts}}
	return &restoreFixture{kube: kube, cluster: cluster, backups: source, now: now, clock: &advancingClock{now: now}}
}

// advancingClock moves forward by the poll interval on every Sleep, so a test
// that means "the wait ran out" does not endure the deadline.
type advancingClock struct {
	now time.Time
}

func (c *advancingClock) Now() time.Time { return c.now }

func (c *advancingClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}

func (f *restoreFixture) deps() RestoreDeps {
	return RestoreDeps{
		Cluster: f.cluster,
		Backups: f.backups,
		Store:   &operation.Store{Runner: f.kube, Operator: "test@laptop", Now: f.clock.Now},
		Runner:  f.kube,
		Now:     f.clock.Now,
		Poll:    time.Second,
		Sleep:   f.clock.Sleep,
	}
}

func (f *restoreFixture) options(t *testing.T, opts RestoreOptions) RestoreOptions {
	t.Helper()
	if opts.Bundle == nil {
		opts.Bundle = testRestoreManifest(t)
	}
	if opts.Cluster == "" {
		opts.Cluster = "prod-1"
	}
	return opts
}

// planOutput runs the command with a confirmation typed on stdin and returns
// what it printed.
func runRestorePlan(t *testing.T, f *restoreFixture, opts RestoreOptions, confirm string) (string, error) {
	t.Helper()
	var out strings.Builder
	err := RunRestore(context.Background(), &out, strings.NewReader(confirm), f.options(t, opts), f.deps())
	return out.String(), err
}

// TestNamespaceRestorePlan is the plan itself: it names the backup, its
// completion time and conservative data age, its coverage and consistency
// method, the namespace and claim identities, what will be discarded, and the
// recovery-point verdict — and it changes NOTHING until it is confirmed.
func TestNamespaceRestorePlan(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-9*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Replace: true}, "")
	if err == nil {
		t.Fatal("an unconfirmed restore must stop")
	}
	for _, want := range []string{
		"daily-good",                        // the backup
		at(-time.Hour).Format(time.RFC3339), // its completion time
		"9h0m0s",                            // conservative age, from the capture start
		"complete:",                         // coverage
		"uncoordinated-copy",                // consistency method
		"uncoordinated copy",                // and what it claims
		"namespace",                         // the namespace identity
		"uid-ns",
		"data-0", // every claim's identity
		"uid-data-0",
		"deployment payments", // the workloads to stop
		"discarded",           // what will be lost
		"26h0m0s",             // the recovery-point policy
		"health.backup.recovery-point-age",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan does not state %q:\n%s", want, out)
		}
	}
	if f.cluster.deleted {
		t.Error("an unconfirmed plan deleted the namespace: nothing may change before the plan is confirmed")
	}
	if len(f.cluster.backupsMade) != 0 || len(f.cluster.restoresMade) != 0 {
		t.Error("an unconfirmed plan took a backup or created a restore")
	}
	if f.kube.recordFor(operation.Name) != nil {
		t.Error("an unconfirmed plan took the operation lock: the record must exist before the first side effect, not before the plan")
	}

	// The confirmed run behaves, and the plan is printed either way.
	out, err = runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Replace: true}, "yes\n")
	if err != nil {
		t.Fatalf("a confirmed restore failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, RestoredStage) {
		t.Errorf("a successful restore did not stop at %q:\n%s", RestoredStage, out)
	}
	if !strings.Contains(out, "--activate") || !strings.Contains(out, "--resume") || !strings.Contains(out, "--abort") {
		t.Errorf("the run does not print the three follow-on commands:\n%s", out)
	}
	if f.cluster.annotations[PauseAnnotationKey] == "" {
		t.Error("the run did not write the pause annotation: reconciliation would sync over the restore")
	}
	if len(f.cluster.backupsMade) != 1 {
		t.Fatalf("safety backups taken = %d, want exactly one", len(f.cluster.backupsMade))
	}
	if !f.cluster.deleted {
		t.Error("the confirmed restore did not delete the namespace")
	}
	if len(f.cluster.scaled) == 0 || f.cluster.scaled[0] != "deployment/payments=0" {
		t.Errorf("the writers were not stopped before the delete: %v", f.cluster.scaled)
	}
	if got := f.cluster.suspended["nightly"]; !got {
		t.Error("the restored CronJob was not suspended: it would run before activation")
	}
	if !f.kube.sawCommand("kubenest-operation") {
		t.Error("no operation record was written")
	}
}

// TestNamespaceRestoreLatestSkipsIneligibleBackup is the planted negative for
// --latest: a newer backup whose volume copy is missing is passed over in
// favour of an older complete one, the plan says so, and it prints the older
// one's data age.
func TestNamespaceRestoreLatestSkipsIneligibleBackup(t *testing.T) {
	newer := eligibleFacts(t, "daily-new", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	newer.Missing = []VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0", Detail: "the backup recorded no copy of this volume"}}
	older := eligibleFacts(t, "daily-old", at(-30*time.Hour), at(-29*time.Hour), "data-0")
	f := newRestoreFixture(t, newer, older)

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, "")
	if err == nil {
		t.Fatalf("a 30h-old backup must need --accept-data-age against a 26h policy:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--accept-data-age") {
		t.Errorf("the refusal does not name --accept-data-age: %v", err)
	}
	if !strings.Contains(err.Error(), "26h0m0s") {
		t.Errorf("the refusal does not name the policy it breached: %v", err)
	}
	// Passing over an ineligible backup is SAID, not silent.
	if !strings.Contains(out, "daily-new") || !strings.Contains(out, "no copy record") {
		t.Errorf("--latest passed over daily-new without saying why:\n%s", out)
	}
	if !strings.Contains(out, "30h0m0s") {
		t.Errorf("the plan does not print the chosen backup's data age:\n%s", out)
	}

	// With the age accepted, the older complete backup is the one restored.
	out, err = runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, AcceptDataAge: true, Confirm: true, Replace: true}, "")
	if err != nil {
		t.Fatalf("an accepted data age must restore: %v\n%s", err, out)
	}
	if len(f.cluster.restoresMade) != 1 || f.cluster.restoresMade[0].Backup != "daily-old" {
		t.Errorf("restored from %v, want the older complete backup daily-old", f.cluster.restoresMade)
	}
}

// TestNamespaceRestoreRefusesAnIneligibleNamedBackup: a backup chosen BY NAME
// that is not eligible is refused before anything is touched, naming the
// missing volume.
func TestNamespaceRestoreRefusesAnIneligibleNamedBackup(t *testing.T) {
	facts := eligibleFacts(t, "daily-new", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	facts.Missing = []VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0"}}
	f := newRestoreFixture(t, facts)

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", From: "daily-new", Confirm: true, Replace: true}, "")
	if err == nil {
		t.Fatalf("an ineligible backup passed by name must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "data-0") {
		t.Errorf("the refusal does not name the missing volume: %v", err)
	}
	if f.cluster.deleted || len(f.cluster.backupsMade) != 0 {
		t.Error("an ineligible backup was refused after something changed")
	}
	if f.kube.recordFor(operation.Name) != nil {
		t.Error("an ineligible backup took the lock")
	}

	// And a name the cluster does not have is refused as such.
	if _, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", From: "typo", Confirm: true, Replace: true}, ""); err == nil || !strings.Contains(err.Error(), "typo") {
		t.Errorf("a backup name the cluster does not have must be refused by name, got %v", err)
	}
}

// TestNamespaceRestoreReplace: without --replace, restoring over an existing
// namespace is refused BEFORE the safety backup; with it, the run works.
func TestNamespaceRestoreReplace(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true}, "")
	if err == nil {
		t.Fatalf("restoring over an existing namespace without --replace must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--replace") {
		t.Errorf("the refusal does not name --replace: %v", err)
	}
	if len(f.cluster.backupsMade) != 0 {
		t.Error("the refusal happened AFTER the safety backup: it must be before it")
	}
	if f.cluster.deleted {
		t.Error("the namespace was deleted by a refused restore")
	}

	// An ABSENT namespace needs no --replace, and the run says so.
	absent := newRestoreFixture(t, facts)
	absent.cluster.namespace = nil
	out, err = runRestorePlan(t, absent, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true}, "")
	if err != nil {
		t.Fatalf("an absent namespace must restore without --replace: %v\n%s", err, out)
	}
	if !strings.Contains(out, "does not exist") {
		t.Errorf("the run did not say the namespace was absent:\n%s", out)
	}
}

// TestNamespaceRestoreStopsWhenAnIdentityMoves is the primary planted negative:
// if the namespace or a claim changes between the confirmed plan and the
// destructive step, the restore stops and says which identity moved.
func TestNamespaceRestoreStopsWhenAnIdentityMoves(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	// The namespace is recreated under the same name between the plan and the
	// execution: a different namespace wearing the same name.
	moved := false
	f.cluster.onProjectHold = func() {
		if moved {
			return
		}
		moved = true
		f.cluster.namespace = &NamespaceState{Name: "payments", UID: "uid-ns-recreated"}
	}

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, "")
	if err == nil {
		t.Fatalf("an identity that moved between the plan and the execution must stop the restore:\n%s", out)
	}
	if !strings.Contains(err.Error(), "uid-ns") || !strings.Contains(err.Error(), "uid-ns-recreated") {
		t.Errorf("the refusal does not name the identity that moved: %v", err)
	}
	if f.cluster.deleted {
		t.Error("the namespace was deleted after the identity check failed")
	}
	if len(f.cluster.backupsMade) != 0 {
		t.Error("the safety backup ran after the identity check failed")
	}
	// The lock is released so the operator can resume or abort, and the pause
	// stays exactly as it is.
	record := f.kube.recordFor(operation.Name)
	if record == nil {
		t.Fatal("the run left no record behind")
	}
	if record.Executor.State != operation.ExecutorStopped {
		t.Errorf("the record was not released after the refusal (executor state %s)", record.Executor.State)
	}
	if record.Terminal {
		t.Error("a refused restore marked its record terminal: there is then nothing to abort")
	}
}

// TestNamespaceRestorePartiallyFailed is the other planted negative: a restore
// that is not Completed exits non-zero and names the phase, Velero's failure
// reason and every incomplete PodVolumeRestore.
func TestNamespaceRestorePartiallyFailed(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	f.cluster.outcome = &RestoreOutcome{
		Name:          "r",
		Phase:         "PartiallyFailed",
		FailureReason: "pod volume restore failed",
		Errors:        1,
		Warnings:      2,
		ProgressSeen:  true,
		ItemsRestored: 4,
	}
	f.cluster.volumes = []VolumeRestoreState{{Name: "pvr-data-0", Pod: "payments-1", Volume: "data-0", ClaimName: "data-0", Phase: "Failed", Message: "the volume could not be written"}}

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, "")
	if err == nil {
		t.Fatalf("a PartiallyFailed restore must exit non-zero:\n%s", out)
	}
	for _, want := range []string{"PartiallyFailed", "pod volume restore failed", "pvr-data-0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not name %q: %v", want, err)
		}
	}
	if strings.Contains(out, "finished as Completed") {
		t.Errorf("a PartiallyFailed restore was reported as done:\n%s", out)
	}
}

// TestNamespaceRestoreRefusesCompletedWithNothingFilled is probe P5's run 4:
// Velero reported "Completed with 0 errors" and had restored nothing at all,
// because without persistentvolumes in the type filter it creates no
// PodVolumeRestore while still injecting its restore-wait init container. The
// command must refuse that restore rather than report a namespace that is back.
func TestNamespaceRestoreRefusesCompletedWithNothingFilled(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	f.cluster.outcome = &RestoreOutcome{Name: "r", Phase: "Completed", Errors: 0, Warnings: 0, ProgressSeen: true, ItemsRestored: 3}
	f.cluster.volumes = nil

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, "")
	if err == nil {
		t.Fatalf("a restore that filled no volume must not be reported as done:\n%s", out)
	}
	for _, want := range []string{"PodVolumeRestore", "data-0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestNamespaceRestoreIncludeJobsSuspendsThemUntilActivation: --include-jobs
// restores the namespace's Jobs DELIBERATELY, and a Job that appears
// unsuspended starts its pod at once — so it is suspended with the CronJobs and
// started by activation, which is what "they run at activation" means.
func TestNamespaceRestoreIncludeJobsSuspendsThemUntilActivation(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	f.cluster.jobs = []JobState{{Name: "migrate", Suspend: false}}

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true, IncludeJobs: true}, "")
	if err != nil {
		t.Fatalf("the restore failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "migrate") || !strings.Contains(out, "runs at activation") {
		t.Errorf("the run does not say the restored Job waits for activation:\n%s", out)
	}
	if !f.cluster.jobsSuspended["migrate"] {
		t.Error("the restored Job was left unsuspended: it would run before activation")
	}
	if len(f.cluster.restoresMade) != 1 {
		t.Fatalf("Velero restores = %d, want one", len(f.cluster.restoresMade))
	}
	if len(f.cluster.restoresMade[0].IncludedResources) == 0 && f.cluster.restoresMade[0].Namespace == "" {
		t.Log("the fake recorded no type filter for this request")
	}
	if strings.Contains(fmt.Sprint(f.cluster.restoresMade[0].IncludedResources), "jobs") {
		t.Error("--include-jobs narrowed the type filter to Jobs: it restores the namespace, Jobs included")
	}

	opID := f.kube.recordFor(operation.Name).OperationID
	var activated strings.Builder
	if err := RunRestore(context.Background(), &activated, strings.NewReader(""), f.options(t, RestoreOptions{
		Namespace: "payments",
		Activate:  opID,
	}), f.deps()); err != nil {
		t.Fatalf("activation failed: %v\n%s", err, activated.String())
	}
	if f.cluster.jobsSuspended["migrate"] {
		t.Error("activation left the one-shot Job suspended")
	}
	if !strings.Contains(activated.String(), "Job migrate runs now") {
		t.Errorf("activation does not show the one-shot work that will start:\n%s", activated.String())
	}
}

// TestNamespaceRestoreResume: after an interrupted run the record is released,
// the pause is still in place, and --resume finishes the restore without
// repeating a step whose postcondition holds and without activating anything.
func TestNamespaceRestoreResume(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	// The first run's restore never reaches a terminal phase: the wait runs out.
	f.cluster.outcome = &RestoreOutcome{Name: "r", Phase: "InProgress"}

	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, "")
	if err == nil {
		t.Fatalf("a restore that never reaches a terminal phase must fail:\n%s", out)
	}
	record := f.kube.recordFor(operation.Name)
	if record == nil {
		t.Fatal("an interrupted restore left no record")
	}
	if record.Executor.State != operation.ExecutorStopped {
		t.Errorf("the interrupted record was not released: a resume would be refused")
	}
	opID := record.OperationID
	if f.cluster.annotations[PauseAnnotationKey] != opID {
		t.Errorf("the pause is %q, want the interrupted operation %s: the pause must survive an interruption", f.cluster.annotations[PauseAnnotationKey], opID)
	}

	// The resume: same cluster state (the process died, not the cluster), a
	// restore that now completes.
	resumed := newRestoreFixture(t, facts)
	resumed.kube = f.kube
	resumed.cluster.deleted = f.cluster.deleted
	resumed.cluster.namespace = f.cluster.namespace
	resumed.cluster.annotations = f.cluster.annotations
	before := resumed.kube.commandCount("delete namespace payments")
	var resumedOut strings.Builder
	err = RunRestore(context.Background(), &resumedOut, strings.NewReader(""), resumed.options(t, RestoreOptions{
		Namespace: "payments",
		Resume:    opID,
	}), resumed.deps())
	if err != nil {
		t.Fatalf("--resume failed: %v\n%s", err, resumedOut.String())
	}
	if after := resumed.kube.commandCount("delete namespace payments"); after != before {
		t.Errorf("--resume re-submitted the namespace deletion (%d -> %d): a step whose postcondition holds is not repeated", before, after)
	}
	if !strings.Contains(resumedOut.String(), "Resuming operation "+opID) {
		t.Errorf("the resume did not say which operation it continues:\n%s", resumedOut.String())
	}
	if !strings.Contains(resumedOut.String(), RestoredStage) {
		t.Errorf("the resume did not finish at %q:\n%s", RestoredStage, resumedOut.String())
	}
	// A RESUME NEVER ACTIVATES: the pause is still there and no CronJob was
	// un-suspended.
	if resumed.cluster.annotations[PauseAnnotationKey] != opID {
		t.Errorf("--resume lifted the pause (annotations %v)", resumed.cluster.annotations)
	}
	if suspended, ok := resumed.cluster.suspended["nightly"]; ok && !suspended {
		t.Error("--resume un-suspended a CronJob: activating is not resuming")
	}
}

// TestNamespaceRestoreActivation: --activate finds the held project, puts the
// workloads and CronJobs back the way they were, lifts the pause and closes the
// record.
func TestNamespaceRestoreActivation(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	out, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, "")
	if err != nil {
		t.Fatalf("the restore before activation failed: %v\n%s", err, out)
	}
	record := f.kube.recordFor(operation.Name)
	if record == nil {
		t.Fatal("no record to activate")
	}
	if record.Stage != RestoredStage {
		t.Fatalf("the record is at %q, want %q", record.Stage, RestoredStage)
	}
	opID := record.OperationID

	var activated strings.Builder
	err = RunRestore(context.Background(), &activated, strings.NewReader(""), f.options(t, RestoreOptions{
		Namespace: "payments",
		Activate:  opID,
	}), f.deps())
	if err != nil {
		t.Fatalf("--activate failed: %v\n%s", err, activated.String())
	}
	if !strings.Contains(activated.String(), "CronJob nightly") {
		t.Errorf("activation does not show which scheduled work will start:\n%s", activated.String())
	}
	if suspended := f.cluster.suspended["nightly"]; suspended {
		t.Error("activation left the CronJob suspended")
	}
	found := false
	for _, scaled := range f.cluster.scaled {
		if scaled == "deployment/payments=2" {
			found = true
		}
	}
	if !found {
		t.Errorf("activation did not put the workload back to its recorded 2 replicas: %v", f.cluster.scaled)
	}
	if _, still := f.cluster.annotations[PauseAnnotationKey]; still {
		t.Error("activation did not lift the pause")
	}
	after := f.kube.recordFor(operation.Name)
	if after == nil || !after.Terminal {
		t.Errorf("activation did not close the record: %+v", after)
	}
	if after != nil && after.Result != string(operation.ResultSucceeded) {
		t.Errorf("the record closed as %q, want succeeded", after.Result)
	}
}

// TestNamespaceRestoreAbortLeavesTheProjectPaused: aborting is not activating.
func TestNamespaceRestoreAbortLeavesTheProjectPaused(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	f.cluster.outcome = &RestoreOutcome{Name: "r", Phase: "InProgress"}
	if _, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, ""); err == nil {
		t.Fatal("the interrupted run must fail")
	}
	opID := f.kube.recordFor(operation.Name).OperationID

	var out strings.Builder
	if err := RunRestore(context.Background(), &out, strings.NewReader(""), f.options(t, RestoreOptions{
		Namespace: "payments",
		Abort:     opID,
	}), f.deps()); err != nil {
		t.Fatalf("--abort failed: %v\n%s", err, out.String())
	}
	if f.cluster.annotations[PauseAnnotationKey] != opID {
		t.Error("--abort lifted the pause: aborting is not activating")
	}
	record := f.kube.recordFor(operation.Name)
	if !record.Terminal || !strings.HasPrefix(record.Result, "aborted") {
		t.Errorf("the record is %+v, want a terminal aborted record", record)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("abort does not say what it did:\n%s", out.String())
	}
}

// TestRestoreRefusesToResumeADifferentNamespace: a resume continues the SAME
// immutable request, so an identity that moved is refused by name.
func TestRestoreRefusesToResumeIntoChangedIdentities(t *testing.T) {
	facts := eligibleFacts(t, "daily-good", at(-2*time.Hour), at(-1*time.Hour), "data-0")
	f := newRestoreFixture(t, facts)
	f.cluster.outcome = &RestoreOutcome{Name: "r", Phase: "InProgress"}
	if _, err := runRestorePlan(t, f, RestoreOptions{Namespace: "payments", Latest: true, Confirm: true, Replace: true}, ""); err == nil {
		t.Fatal("the interrupted run must fail")
	}
	opID := f.kube.recordFor(operation.Name).OperationID

	resumed := newRestoreFixture(t, facts)
	resumed.kube = f.kube
	// The claim was recreated: a different claim wearing the same name.
	resumed.cluster.claims = []VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0-recreated"}}
	var out strings.Builder
	err := RunRestore(context.Background(), &out, strings.NewReader(""), resumed.options(t, RestoreOptions{
		Namespace: "payments",
		Resume:    opID,
	}), resumed.deps())
	if err == nil {
		t.Fatalf("a resume whose claim identity moved must be refused:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "uid-data-0") {
		t.Errorf("the refusal does not name the claim identity: %v", err)
	}
	if resumed.cluster.deleted {
		t.Error("a refused resume changed the cluster")
	}
}

// TestRestoreSpecsRecordsEveryAction: every stage the run submits is an action
// with a postcondition and a read-only way to establish it, which is what makes
// --resume possible at all.
func TestRestoreSpecsRecordsEveryAction(t *testing.T) {
	cases := []struct {
		stage   string
		command string
		want    string
	}{
		{"pause", "sudo -n k3s kubectl annotate project payments -n kubenest-system kubenest.io/reconcile-paused=abc123 --overwrite", "kubenest.io/reconcile-paused=abc123"},
		{"backup/kubenest-safety-abc123", "sudo -n k3s kubectl apply -f -", "Velero Backup kubenest-safety-abc123 exists"},
		{"restore/kubenest-restore-abc123", "sudo -n k3s kubectl apply -f -", "Velero Restore kubenest-restore-abc123 exists"},
		{"scale/deployment/payments", "sudo -n k3s kubectl scale deployment payments -n payments --replicas=0", "has 0 replicas"},
		{"delete-namespace/payments", "sudo -n k3s kubectl delete namespace payments --wait=false", "namespace payments is gone"},
		{"suspend/nightly", "sudo -n k3s kubectl patch cronjob nightly -n payments --type=merge -p '{\"spec\":{\"suspend\":true}}'", "CronJob payments/nightly is suspended"},
		{"delete-claim/payments/data-0", "sudo -n k3s kubectl delete persistentvolumeclaim data-0 -n payments --wait=false", "claim payments/data-0 is gone"},
		{"modifier/kubenest-restore-modifier-abc123", "sudo -n k3s kubectl apply -f -", "resource modifier kubenest-restore-modifier-abc123 exists"},
	}
	for _, c := range cases {
		spec, ok := restoreSpecs(c.stage, c.command)
		if !ok {
			t.Errorf("stage %s is not recorded as an action, so no resume can tell whether it happened", c.stage)
			continue
		}
		if !strings.Contains(spec.Postcondition, c.want) {
			t.Errorf("stage %s postcondition = %q, want it to contain %q", c.stage, spec.Postcondition, c.want)
		}
		if spec.Observe == "" {
			t.Errorf("stage %s has no read-only way to establish its postcondition; a resume would stop on it", c.stage)
		}
	}
	// A read is not an action: recording every observation would fill the
	// record with steps a successor could not safely skip.
	if _, ok := restoreSpecs("pause", "sudo -n k3s kubectl get project payments -n kubenest-system -o json"); ok {
		t.Error("a read was recorded as an action")
	}
}

// TestTheEligibilityJudgementNamesWhatIsMissing is the CLI's own reading of
// T2.10's evidence. The rule is the operator's (op3/pkg/restoredrill/backup.go)
// and this is the same judgement over the same JSON, because a restore that
// judged coverage differently from the report an operator reads would pick a
// backup the report calls incomplete — which is exactly the support incident
// the record exists to prevent.
func TestTheEligibilityJudgementNamesWhatIsMissing(t *testing.T) {
	const backupJSON = `{"metadata":{"name":"daily-1"},"spec":{"storageLocation":"default"},"status":{"phase":"Completed","startTimestamp":"2026-09-27T02:00:00Z","completionTimestamp":"2026-09-27T03:00:00Z"}}`
	record := &coverageRecord{
		Backup: "daily-1",
		Namespaces: []coverageNamespace{{
			Name: "payments",
			UID:  "uid-ns",
			Volumes: []coverageVolume{
				{Namespace: "payments", Name: "data-0", UID: "uid-data-0"},
				{Namespace: "payments", Name: "data-1", UID: "uid-data-1"},
			},
		}},
	}
	available := map[string]string{"default": "Available"}
	completed := func(volume, uid string) copyRecord {
		return copyRecord{namespace: "payments", volume: volume, volumeUID: uid, successful: true}
	}

	cases := []struct {
		name    string
		copies  []copyRecord
		record  *coverageRecord
		backup  string
		reason  string
		missing []string
		failed  []string
	}{
		{
			name:    "a claim that got no copy at all is missing",
			copies:  []copyRecord{completed("data-0", "uid-data-0")},
			record:  record,
			reason:  "incomplete-coverage",
			missing: []string{"data-1"},
		},
		{
			name:   "a copy that failed is named",
			copies: []copyRecord{completed("data-0", "uid-data-0"), {namespace: "payments", volume: "data-1", volumeUID: "uid-data-1", successful: false, detail: "PodVolumeBackup reached Failed"}},
			record: record,
			reason: "incomplete-coverage",
			failed: []string{"data-1"},
		},
		{
			name:   "every expected volume copied",
			copies: []copyRecord{completed("data-0", "uid-data-0"), completed("data-1", "uid-data-1")},
			record: record,
			reason: "",
		},
		{
			name: "a copy for a volume the backup never claimed is not a failure",
			copies: []copyRecord{completed("data-0", "uid-data-0"), completed("data-1", "uid-data-1"),
				{namespace: "payments", volume: "data-2", volumeUID: "uid-data-2", successful: false, detail: "created after the record was taken"}},
			record: record,
			reason: "",
		},
		{
			name:    "a claim recreated under the same name is a different volume",
			copies:  []copyRecord{{namespace: "payments", volume: "data-0", volumeUID: "uid-recreated", successful: true}},
			record:  record,
			reason:  "incomplete-coverage",
			missing: []string{"data-0", "data-1"},
		},
		{
			name:   "no record at all is coverage unknown, never complete",
			copies: []copyRecord{completed("data-0", "uid-data-0"), completed("data-1", "uid-data-1")},
			record: nil,
			reason: "coverage-unknown",
		},
		{
			name:   "a storage location that is not Available is not off-cluster",
			copies: []copyRecord{completed("data-0", "uid-data-0"), completed("data-1", "uid-data-1")},
			record: record,
			backup: `{"metadata":{"name":"daily-1"},"spec":{"storageLocation":"default"},"status":{"phase":"Completed","startTimestamp":"2026-09-27T02:00:00Z"}}`,
			reason: "storage-location-unavailable",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := c.backup
			if raw == "" {
				raw = backupJSON
			}
			locations := available
			if c.reason == "storage-location-unavailable" {
				locations = map[string]string{}
			}
			facts := judgeBackup(raw, c.copies, c.record, locations, "payments")
			if got := facts.IneligibleReason(); got != c.reason {
				t.Errorf("ineligible reason = %q, want %q (%+v)", got, c.reason, facts)
			}
			missing := []string{}
			for _, ref := range facts.Missing {
				missing = append(missing, ref.Name)
			}
			sort.Strings(missing)
			if strings.Join(missing, ",") != strings.Join(c.missing, ",") {
				t.Errorf("missing = %v, want %v", missing, c.missing)
			}
			failed := []string{}
			for _, ref := range facts.Failed {
				failed = append(failed, ref.Name)
			}
			if strings.Join(failed, ",") != strings.Join(c.failed, ",") {
				t.Errorf("failed = %v, want %v", failed, c.failed)
			}
		})
	}

	// The conservative age runs from the capture start, never from completion:
	// a three-hour backup that completed an hour ago is three hours old.
	facts := judgeBackup(backupJSON, []copyRecord{completed("data-0", "uid-data-0"), completed("data-1", "uid-data-1")}, record, available, "payments")
	age, known := facts.Age(time.Date(2026, 9, 27, 4, 0, 0, 0, time.UTC))
	if !known || age != 2*time.Hour {
		t.Errorf("age = %s (known=%v), want 2h from the 02:00 capture start", age, known)
	}
}

// TestRestorePolicyPrefersTheDedicatedThreshold: bundle 1.2's
// health.backup.recovery-point-age is the recovery-point policy; a bundle that
// predates it falls back to max-backup-age and says which it used.
func TestRestorePolicyPrefersTheDedicatedThreshold(t *testing.T) {
	policy, err := RecoveryPointPolicyFor(testRestoreManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	if policy.Max != 26*time.Hour || policy.Source != "health.backup.recovery-point-age" {
		t.Errorf("policy = %v, want the dedicated 26h key", policy)
	}

	older, err := manifest.Parse([]byte("bundle: \"1.1\"\nlimits:\n  timeouts:\n    backup: 1h\nhealth:\n  backup:\n    max-backup-age: 48h\n"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err = RecoveryPointPolicyFor(older)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Max != 48*time.Hour || !strings.Contains(policy.Source, "max-backup-age") {
		t.Errorf("policy = %v, want max-backup-age named as the fallback", policy)
	}
	if _, err := RecoveryPointPolicyFor(nil); err == nil {
		t.Error("a missing manifest must be refused rather than defaulted")
	}
}

// TestRecoveryPointAgeIsInTheManifest pins the field the policy reads: without
// it a 1.2 bundle's dedicated threshold would be silently ignored.
func TestRecoveryPointAgeIsInTheManifest(t *testing.T) {
	parsed, err := manifest.Load("../../pkg/bundles/manifests/platform-1.1.yaml")
	if err != nil {
		t.Fatalf("the shipped 1.1 manifest must parse: %v", err)
	}
	if parsed.Health.Backup.MaxBackupAge.Duration() != 48*time.Hour {
		t.Errorf("the shipped manifest's max-backup-age read as %s", parsed.Health.Backup.MaxBackupAge.Duration())
	}
	if parsed.Health.Backup.RecoveryPointAge.Duration() != 0 {
		t.Errorf("bundle 1.1 declares no recovery-point-age, but it read as %s", parsed.Health.Backup.RecoveryPointAge.Duration())
	}
}

// guard against the file being unused by the vet-less test build.
var _ = os.Getenv
