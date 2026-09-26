package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
)

// The tests below are the acceptance criterion for T2.3. Every one of them
// drives the real Store and the real Guarded against fakeKube, an in-memory
// kube-system with real resourceVersion semantics, so the compare-and-swap, the
// AlreadyExists refusal and the 409 are exercised rather than assumed.
//
// What a green run does NOT prove: that `sudo -n k3s kubectl` on a real host
// returns the messages this fake returns, that the API server's conflict text
// is classified correctly, or that a real S3 target honours If-None-Match —
// that is probe P3, and e2e/operation_record_test.go is the run on real hosts.

func testRequest(hosts ...string) Request {
	targets := make([]Target, 0, len(hosts))
	for _, h := range hosts {
		targets = append(targets, Target{
			HostID:  h,
			NodeUID: "node-uid-" + h,
			PVCUIDs: []string{"pvc-uid-" + h},
		})
	}
	return Request{
		Kind:      KindUpgrade,
		Cluster:   "gate-single-server",
		Targets:   targets,
		Artifacts: []Artifact{{Name: "bundle-1.1", Digest: "sha256:9f2c"}},
		Versions:  map[string]string{"bundle": "1.0 -> 1.1", "k3s": "v1.30.0+k3s1"},
	}
}

func clockStore(t *testing.T, k *fakeKube, operator string) (*Store, func(time.Time)) {
	t.Helper()
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s := &Store{Runner: k, Operator: operator, Now: func() time.Time { return at }}
	return s, func(next time.Time) { at = next }
}

func newStore(t *testing.T, k *fakeKube, operator string) *Store {
	t.Helper()
	s, _ := clockStore(t, k, operator)
	return s
}

func acquire(t *testing.T, s *Store, req Request) *Handle {
	t.Helper()
	h, err := s.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("acquiring the record: %v", err)
	}
	return h
}

// handleAt is a handle as another writer saw the record: the same token, an
// earlier resourceVersion. Copying a Handle would copy its mutex, so the test
// builds one — a stale handle is a state the real code can reach (a laptop
// that read the record before someone else wrote), and it is the only way to
// exercise the resourceVersion refusal without a second process.
func handleAt(base *Handle, rv string) *Handle {
	return &Handle{store: base.store, rec: base.rec, rv: rv, token: base.token}
}

// handleWithToken is the same, for a token that is no longer the owner's.
func handleWithToken(base *Handle, token string) *Handle {
	return &Handle{store: base.store, rec: base.rec, rv: base.rv, token: token}
}

func sshSpecs(postcondition, observe string) Specs {
	return func(stage, command string) (Spec, bool) {
		return Spec{Kind: ActionSSH, Postcondition: postcondition, Observe: observe}, true
	}
}

func onlyStep(t *testing.T, plan *Plan) Step {
	t.Helper()
	if len(plan.Steps) != 1 {
		t.Fatalf("expected exactly one step in the plan, got %d: %+v", len(plan.Steps), plan.Steps)
	}
	return plan.Steps[0]
}

// deadRunner is a cluster that cannot be reached at all: the network partition
// the take-over rules must never read as permission.
type deadRunner struct{}

func (deadRunner) Run(context.Context, string) (sshx.Result, error) {
	return sshx.Result{}, errors.New("dial tcp 10.0.0.4:22: connect: connection refused")
}

func (deadRunner) RunInput(context.Context, string, io.Reader) (sshx.Result, error) {
	return sshx.Result{}, errors.New("dial tcp 10.0.0.4:22: connect: connection refused")
}

var _ k3s.Runner = deadRunner{}

// TestTheLiveRecordIsTheLock: the record exists before anything else can
// happen, it holds the immutable request, and its existence is what refuses the
// next operation.
func TestTheLiveRecordIsTheLock(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	req := testRequest("host-1", "host-2")

	h := acquire(t, s, req)

	rec := k.liveRecord(Name)
	if rec.OperationID != h.OperationID() {
		t.Fatalf("the live record is operation %s, this handle owns %s", rec.OperationID, h.OperationID())
	}
	if rec.Terminal || rec.State() != StateRunning {
		t.Fatalf("a just-acquired record is terminal=%v state=%s", rec.Terminal, rec.State())
	}
	if got := rec.Request.Digest(); got != req.Digest() {
		t.Fatalf("the record's request digest is %s, the request acquired was %s", got, req.Digest())
	}
	// Targets are recorded by host id AND by Node UID and PVC UID: a host id
	// survives a reinstall and a Node UID does not.
	if len(rec.Request.Targets) != 2 || rec.Request.Targets[1].NodeUID != "node-uid-host-2" || rec.Request.Targets[1].PVCUIDs[0] != "pvc-uid-host-2" {
		t.Fatalf("the request's targets were not recorded with their cluster identities: %+v", rec.Request.Targets)
	}
	if rec.Executor.Token != h.Token() || rec.Executor.Operator != "ana@laptop" || rec.Executor.State != ExecutorRunning {
		t.Fatalf("the record's executor is %+v, this handle holds token %s", rec.Executor, h.Token())
	}

	// It is the only object, and the store can read it back with the revision
	// the CAS is anchored on.
	stored, err := s.Current(ctx)
	if err != nil {
		t.Fatalf("reading the live record: %v", err)
	}
	if stored.ResourceVersion != h.ResourceVersion() {
		t.Fatalf("the handle's resourceVersion is %s, the cluster's is %s", h.ResourceVersion(), stored.ResourceVersion)
	}
	if names := k.names(); len(names) != 1 || names[0] != Name {
		t.Fatalf("the lock is one object named %s; the cluster holds %v", Name, names)
	}

	// And it IS the lock: the next acquisition is refused, and the refusal
	// writes nothing.
	if _, err := s.Acquire(ctx, req); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second acquisition of a live record returned %v, want ErrLocked", err)
	}
	if names := k.names(); len(names) != 1 {
		t.Fatalf("the refused acquisition created something: %v", names)
	}
}

// TestASecondLaptopIsRefusedAndNamesTheOtherOperator: the refusal has to say
// who holds the record, because "another operation is running" is not something
// an operator can act on.
func TestASecondLaptopIsRefusedAndNamesTheOtherOperator(t *testing.T) {
	k := newFakeKube(t)
	ctx := context.Background()
	first := newStore(t, k, "ana@laptop")
	h := acquire(t, first, testRequest("host-1"))

	rv := k.resourceVersion(Name)
	second := newStore(t, k, "bo@other-laptop")
	_, err := second.Acquire(ctx, testRequest("host-1"))
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("the second laptop got %v, want ErrLocked", err)
	}
	for _, want := range []string{"ana@laptop", h.OperationID(), string(KindUpgrade)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name %q:\n%v", want, err)
		}
	}
	if k.resourceVersion(Name) != rv || k.liveRecord(Name).Executor.Token != h.Token() {
		t.Fatal("the refused second laptop wrote to the record")
	}
}

// TestATerminalRecordCanBeReplaced: a finished record does not hold the lock
// against the next operation — that is why terminal marking exists.
func TestATerminalRecordCanBeReplaced(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()

	h := acquire(t, s, testRequest("host-1", "host-2"))
	if err := s.Complete(ctx, h, ResultSucceeded); err != nil {
		t.Fatalf("completing the first operation: %v", err)
	}
	firstRV := k.resourceVersion(Name)

	next := acquire(t, s, testRequest("host-1", "host-2"))
	if next.OperationID() == h.OperationID() {
		t.Fatal("the second operation reused the first operation's id")
	}
	rec := k.liveRecord(Name)
	if rec.OperationID != next.OperationID() || rec.Terminal {
		t.Fatalf("the live record is %s terminal=%v, want the new operation", rec.OperationID, rec.Terminal)
	}
	if k.resourceVersion(Name) == firstRV {
		t.Fatal("replacing the terminal record did not change its resourceVersion")
	}
	if rec.Executor.Token != next.Token() {
		t.Fatal("the replacement record kept the previous executor's token")
	}
}

// TestEveryUpdateMustMatchResourceVersionAndToken: a write is refused when the
// record moved on under it, and refused when the token no longer owns the
// record — the second even when the resourceVersion is current, because
// ownership is not a version.
func TestEveryUpdateMustMatchResourceVersionAndToken(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))
	atAcquire := h.ResourceVersion()

	if err := h.Heartbeat(ctx, "kubernetes"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if h.ResourceVersion() == atAcquire {
		t.Fatal("a write did not move the handle's resourceVersion")
	}

	// (a) a handle that read an older revision is refused, and refused as
	// stale rather than as an ownership problem.
	stale := handleAt(h, atAcquire)
	err := stale.store.Update(ctx, stale, func(r *Record) error { return nil })
	if !errors.Is(err, ErrStale) {
		t.Fatalf("a write from a stale handle returned %v, want ErrStale", err)
	}
	if errors.Is(err, ErrNotOwner) {
		t.Fatalf("a stale handle was refused as an ownership problem: %v", err)
	}

	// (b) the record's token is replaced by another writer at the SAME
	// revision, which is the state a resourceVersion check cannot see.
	k.setTokenWithoutBumpingRevision(Name, "someone-elses-token")
	live, err := s.Current(ctx)
	if err != nil {
		t.Fatalf("reading the live record: %v", err)
	}
	if live.ResourceVersion != h.ResourceVersion() {
		t.Fatalf("the escape hatch changed the revision (%s != %s)", live.ResourceVersion, h.ResourceVersion())
	}
	err = s.Update(ctx, h, func(r *Record) error { return nil })
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("a write with a mismatched token returned %v, want ErrNotOwner", err)
	}
	if strings.Contains(err.Error(), "someone-elses-token") {
		t.Fatalf("the refusal printed the other token: %v", err)
	}
	if k.liveRecord(Name).Executor.Token != "someone-elses-token" {
		t.Fatal("the refused write changed the record")
	}
}

// TestAResumeContinuesTheSameImmutableRequestAndRefusesNewTargets.
func TestAResumeContinuesTheSameImmutableRequestAndRefusesNewTargets(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	req := testRequest("host-1", "host-2")

	h := acquire(t, s, req)
	if err := h.Heartbeat(ctx, "kubernetes"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// The laptop dies here: nothing completes the record.

	// The other laptop reads it. Knowing the operation id confers nothing.
	other := newStore(t, k, "bo@other-laptop")
	writesBefore := k.writeCount()
	plan, err := Resume(ctx, other, h.OperationID())
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if plan.Terminal {
		t.Fatal("an interrupted operation read as terminal")
	}
	if plan.Request.Digest() != req.Digest() {
		t.Fatalf("the resume continues request %s, the record holds %s", plan.Request.Digest(), req.Digest())
	}
	if diff := plan.Request.Differences(req); len(diff) != 0 {
		t.Fatalf("the resume's request differs from the one acquired: %v", diff)
	}
	if err := plan.Verify(req); err != nil {
		t.Fatalf("continuing the same request was refused: %v", err)
	}
	// Order is not a difference.
	if err := plan.Verify(testRequest("host-2", "host-1")); err != nil {
		t.Fatalf("the same targets in a different order were refused: %v", err)
	}
	// Changed targets are, and the refusal names them.
	err = plan.Verify(testRequest("host-1", "host-3"))
	if err == nil {
		t.Fatal("a resume into a different target set was accepted")
	}
	if !strings.Contains(err.Error(), "targets") {
		t.Fatalf("the refusal does not name the changed field: %v", err)
	}
	// So are changed versions.
	changed := testRequest("host-1", "host-2")
	changed.Versions = map[string]string{"bundle": "1.0 -> 1.2", "k3s": "v1.30.0+k3s1"}
	err = plan.Verify(changed)
	if err == nil || !strings.Contains(err.Error(), "versions") {
		t.Fatalf("a resume into a different bundle version was not refused by name: %v", err)
	}

	// And a resume submits nothing while it establishes all of that.
	if k.writeCount() != writesBefore {
		t.Fatal("resuming wrote to the record")
	}
}

// TestAnActionIsRecordedBeforeItIsSubmitted: at the moment the cluster sees the
// action, the record already carries its identity and its postcondition — and
// an action that cannot be recorded never reaches the cluster at all.
func TestAnActionIsRecordedBeforeItIsSubmitted(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	const command = "sudo -n k3s kubectl apply -f /var/lib/rancher/k3s/server/manifests/kubenest-k3s-server.yaml"
	const observe = "sudo -n k3s kubectl -n system-upgrade get plan kubenest-k3s-server"
	const postcondition = "the server Plan is applied and complete"

	var atSubmit Action
	var found bool
	spy := &spyRunner{t: t, onRun: func(string) {
		atSubmit, found = k.liveRecord(Name).ActionByID(ActionID("kubernetes", command))
	}}
	g := &Guarded{Inner: spy, Op: h, Stage: "kubernetes", Specs: sshSpecs(postcondition, observe)}
	if _, err := g.Run(ctx, command); err != nil {
		t.Fatalf("submitting the action: %v", err)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("the action was submitted %d times: %v", len(spy.calls), spy.calls)
	}
	if !found {
		t.Fatalf("at submission the record had no action %s", ActionID("kubernetes", command))
	}
	if atSubmit.Status != ActionSubmitted {
		t.Fatalf("at submission the record said the action was %q, want %q", atSubmit.Status, ActionSubmitted)
	}
	if atSubmit.Postcondition != postcondition || atSubmit.Observe != observe || atSubmit.Kind != ActionSSH {
		t.Fatalf("the recorded action does not carry its postcondition: %+v", atSubmit)
	}
	// The outcome is written on return.
	if a, _ := k.liveRecord(Name).ActionByID(ActionID("kubernetes", command)); a.Status != ActionSucceeded {
		t.Fatalf("after a successful return the record says %q", a.Status)
	}

	// An action that cannot be recorded first is NOT submitted.
	if err := h.Heartbeat(ctx, "kubernetes"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	spy2 := &spyRunner{t: t}
	g2 := &Guarded{Inner: spy2, Op: handleAt(h, "1"), Stage: "kubernetes", Specs: sshSpecs(postcondition, observe)}
	if _, err := g2.Run(ctx, command); !errors.Is(err, ErrStale) {
		t.Fatalf("submitting an action through a stale handle returned %v, want ErrStale", err)
	}
	if len(spy2.calls) != 0 {
		t.Fatalf("an action that could not be recorded was submitted anyway: %v", spy2.calls)
	}
}

// TestAResumeDoesNotSubmitAnActionThatAlreadySucceeded.
func TestAResumeDoesNotSubmitAnActionThatAlreadySucceeded(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	const server = "sudo -n k3s kubectl apply -f /manifests/server.yaml"
	const agent = "sudo -n k3s kubectl apply -f /manifests/agent.yaml"
	specs := func(stage, command string) (Spec, bool) {
		return Spec{Kind: ActionPlan, Postcondition: "applied: " + command, Observe: "sudo -n k3s kubectl get plan " + command}, true
	}
	first := &spyRunner{t: t}
	g := &Guarded{Inner: first, Op: h, Stage: "kubernetes", Specs: specs}
	for _, c := range []string{server, agent} {
		if _, err := g.Run(ctx, c); err != nil {
			t.Fatalf("submitting %q: %v", c, err)
		}
	}
	if len(first.calls) != 2 {
		t.Fatalf("the actions were submitted %d times", len(first.calls))
	}
	// The laptop dies here: both actions succeeded and neither was marked
	// terminal.

	plan, err := Resume(ctx, s, h.OperationID())
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if plan.Blocked != nil {
		t.Fatalf("a resume over succeeded actions blocked on %+v", plan.Blocked)
	}
	for _, s := range plan.Steps {
		if s.Decision != DecisionSkip {
			t.Fatalf("action %s was decided %q, want skip", s.ActionID, s.Decision)
		}
	}
	if len(plan.Skip()) != 2 {
		t.Fatalf("the plan carries %d skips for two succeeded actions", len(plan.Skip()))
	}

	// The re-run submits neither of them.
	again := &spyRunner{t: t}
	g2 := &Guarded{Inner: again, Op: h, Stage: "kubernetes", Specs: specs, Skip: plan.Skip()}
	for _, c := range []string{server, agent} {
		if _, err := g2.Run(ctx, c); err != nil {
			t.Fatalf("the resumed run failed on %q: %v", c, err)
		}
	}
	if len(again.calls) != 0 {
		t.Fatalf("the resume re-submitted %v", again.calls)
	}
}

// AN ACTION IS ITS COMMAND AND ITS INPUT
// (kn-t70-control-plane-version-identity-4xso.8).
//
// ActionID hashes the stage and the command alone, and a manifest write sends its
// document over stdin — so the control-plane migration stage's stop apply and its
// migration apply (both k3s.WriteManifest of one file) shared one identity, and a
// resume that recorded either one skipped both: on hardware (2026-09-26, run 26)
// the resumed stage waited ten minutes for a Job nothing had rendered, while the
// cluster held nothing from the migration apply.
func TestAnActionIdentityIncludesTheDocumentItStreams(t *testing.T) {
	const (
		stage   = "control-plane-migration"
		command = "sudo -n install -m 0600 /dev/stdin /var/lib/rancher/k3s/server/manifests/kubenest-cp.yaml.tmp && sudo -n mv -f " +
			"/var/lib/rancher/k3s/server/manifests/kubenest-cp.yaml.tmp /var/lib/rancher/k3s/server/manifests/kubenest-cp.yaml"
	)
	stopApply := []byte("backend:\n  replicas: 0\nmigration:\n  enabled: false\n")
	migrationApply := []byte("backend:\n  replicas: 0\nmigration:\n  enabled: true\n")

	// DIFFERENT DOCUMENTS ARE DIFFERENT ACTIONS, which is the defect: the two
	// writes of one chart file must not share an identity.
	if InputActionID(stage, command, stopApply) == InputActionID(stage, command, migrationApply) {
		t.Error("two writes of the same command with different documents share one identity, so a resume that recorded either one skips both")
	}
	// THE SAME DOCUMENT IS THE SAME ACTION, or a resume would repeat a write it
	// has already made.
	if InputActionID(stage, command, stopApply) != InputActionID(stage, command, stopApply) {
		t.Error("the same write has two identities, so resuming it would repeat it")
	}
	// AND IT CANNOT COLLIDE WITH A COMMAND THAT HAS NO INPUT, which is what keeps
	// every identity already written down exactly what it was.
	if ActionID(stage, command) == InputActionID(stage, command, nil) {
		t.Error("an input action's identity collides with the same command without input, so the records of operations in flight would change meaning")
	}
}

// A RESUME SUBMITS A WRITE WHOSE DOCUMENT DIFFERS, even when the record holds
// the same command as succeeded
// (kn-t70-control-plane-version-identity-4xso.8).
func TestAResumeSkipsOnlyTheWriteItActuallyMade(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	specs := func(stage, command string) (Spec, bool) {
		return Spec{Kind: ActionPlan, Postcondition: "written: " + command, Observe: "sudo -n test -s /manifests/kubenest-cp.yaml"}, true
	}
	// THE FIRST RUN writes the stop apply. Its transport is a spy: the write is
	// not record traffic.
	first := &spyRunner{t: t}
	wrote := &Guarded{Inner: first, Op: h, Stage: "control-plane-migration", Specs: specs}
	if err := k3s.WriteManifest(ctx, wrote, "kubenest-cp", []byte("the stop apply\n")); err != nil {
		t.Fatalf("the first run's write: %v", err)
	}
	// WHAT THE RECORD HOLDS is that write's identity, and nothing else.
	stored, err := s.Find(ctx, h.OperationID())
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if len(stored.Record.Actions) != 1 {
		t.Fatalf("the record holds %d action(s), want the one write: %+v", len(stored.Record.Actions), stored.Record.Actions)
	}
	recorded := stored.Record.Actions[0].ID

	// THE RESUME writes the MIGRATION apply: the same command, a different
	// document. It must reach the cluster.
	again := &spyRunner{t: t}
	resumed := &Guarded{Inner: again, Op: h, Stage: "control-plane-migration", Specs: specs, Skip: map[string]bool{recorded: true}}
	if err := k3s.WriteManifest(ctx, resumed, "kubenest-cp", []byte("the migration apply\n")); err != nil {
		t.Fatalf("the resumed run's write: %v", err)
	}
	if len(again.calls) != 1 {
		t.Fatalf("the resume submitted %d write(s), want the one whose document the record does not hold: an action's identity must include its input", len(again.calls))
	}
	// AND THE WRITE IT *DID* MAKE IS STILL SKIPPED.
	third := &spyRunner{t: t}
	repeated := &Guarded{Inner: third, Op: h, Stage: "control-plane-migration", Specs: specs, Skip: map[string]bool{recorded: true}}
	if err := k3s.WriteManifest(ctx, repeated, "kubenest-cp", []byte("the stop apply\n")); err != nil {
		t.Fatalf("repeating the recorded write: %v", err)
	}
	if len(third.calls) != 0 {
		t.Fatalf("the resume re-submitted the write it recorded: %v", third.calls)
	}
}

// TestAKillBeforeSubmissionResumesWithoutSubmitting: a record that says
// "recorded, never submitted" is safe to repeat, so the resume repeats it — and
// the resume itself submits nothing.
func TestAKillBeforeSubmissionResumesWithoutSubmitting(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	const command = "sudo -n k3s systemctl reboot"
	id := ActionID("node-reboot", command)
	if err := h.RecordAction(ctx, id, "node-reboot", Spec{
		Kind:          ActionSSH,
		Postcondition: "host-1 is back and Ready on the new kernel",
	}); err != nil {
		t.Fatalf("recording the action: %v", err)
	}
	// The laptop dies between the record write and the submission.
	rec := k.liveRecord(Name)
	a, ok := rec.ActionByID(id)
	if !ok || a.Status != ActionRecorded {
		t.Fatalf("the killed run left %+v", a)
	}

	writesBefore := k.writeCount()
	plan, err := Resume(ctx, s, h.OperationID())
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	step := onlyStep(t, plan)
	if step.Decision != DecisionRepeat || step.ActionID != id {
		t.Fatalf("an action that was never submitted was decided %q: %+v", step.Decision, step)
	}
	if !strings.Contains(step.Reason, "never sent") {
		t.Fatalf("the reason does not say the action was never sent: %q", step.Reason)
	}
	if plan.Blocked != nil {
		t.Fatalf("a never-submitted action blocked the resume: %+v", plan.Blocked)
	}
	// It never ran, so there is nothing to probe and nothing to skip: the
	// re-run must submit it.
	if len(plan.Observations) != 0 {
		t.Fatalf("the resume probed for an action that was never submitted: %+v", plan.Observations)
	}
	if len(plan.Skip()) != 0 {
		t.Fatalf("an action that never ran was skipped: %v", plan.Skip())
	}
	if k.writeCount() != writesBefore {
		t.Fatal("the resume submitted something")
	}
}

// TestAKillAfterSuccessBeforeTheProgressUpdateResumesWithoutSubmitting: the
// record says "submitted" and nothing more, so the resume establishes the
// outcome from the recorded postcondition instead of re-submitting.
func TestAKillAfterSuccessBeforeTheProgressUpdateResumesWithoutSubmitting(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	const command = "sudo -n k3s kubectl apply -f /manifests/server.yaml"
	const observe = "sudo -n k3s kubectl -n system-upgrade get plan kubenest-k3s-server"
	const postcondition = "the server Plan is complete"
	id := ActionID("kubernetes", command)
	spec := Spec{Kind: ActionPlan, Postcondition: postcondition, Observe: observe}
	if err := h.RecordAction(ctx, id, "kubernetes", spec); err != nil {
		t.Fatalf("recording the action: %v", err)
	}
	if err := h.SubmitAction(ctx, id); err != nil {
		t.Fatalf("marking the action submitted: %v", err)
	}
	// The laptop dies here: the action happened and the outcome was never
	// written back.
	k.script(observe, sshx.Result{})

	writesBefore := k.writeCount()
	plan, err := Resume(ctx, s, h.OperationID())
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	if plan.Blocked != nil {
		t.Fatalf("the recorded postcondition holds, so the resume must not block: %+v", plan.Blocked)
	}
	step := onlyStep(t, plan)
	if step.Decision != DecisionSkip {
		t.Fatalf("an action whose postcondition holds was decided %q", step.Decision)
	}
	if !strings.Contains(step.Reason, postcondition) {
		t.Fatalf("the reason does not name the postcondition it checked: %q", step.Reason)
	}
	if len(plan.Observations) != 1 || plan.Observations[0].ExitCode != 0 || plan.Observations[0].Command != observe {
		t.Fatalf("the resume did not run the recorded probe: %+v", plan.Observations)
	}
	if !plan.Skip()[id] {
		t.Fatal("the plan does not skip the action whose postcondition was established")
	}
	if k.writeCount() != writesBefore {
		t.Fatal("the resume submitted something while establishing the outcome")
	}

	// And the re-run does not submit it.
	spy := &spyRunner{t: t}
	g := &Guarded{Inner: spy, Op: h, Stage: "kubernetes", Specs: sshSpecs(postcondition, observe), Skip: plan.Skip()}
	if _, err := g.Run(ctx, command); err != nil {
		t.Fatalf("the resumed run failed: %v", err)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("the resume re-submitted %v", spy.calls)
	}
}

// TestAnUnestablishableOutcomeStopsAndNamesTheReconciliationStep: when the
// postcondition cannot be established, the resume stops and says what to do
// instead of re-submitting into the dark.
func TestAnUnestablishableOutcomeStopsAndNamesTheReconciliationStep(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	// (a) the recorded probe does not establish it — and a probe that fails is
	// not read as "it did not happen", because a probe that could not reach the
	// cluster looks exactly the same from here.
	const command = "sudo -n k3s kubectl apply -f /manifests/server.yaml"
	const observe = "sudo -n k3s kubectl -n system-upgrade get plan kubenest-k3s-server"
	const postcondition = "the server Plan is complete"
	id := ActionID("kubernetes", command)
	if err := h.RecordAction(ctx, id, "kubernetes", Spec{Kind: ActionPlan, Postcondition: postcondition, Observe: observe}); err != nil {
		t.Fatalf("recording the action: %v", err)
	}
	if err := h.SubmitAction(ctx, id); err != nil {
		t.Fatalf("marking the action submitted: %v", err)
	}
	k.script(observe, sshx.Result{ExitCode: 1, Stderr: "Error from server (NotFound): the API server is not reachable\n"})

	writesBefore := k.writeCount()
	plan, err := Resume(ctx, s, h.OperationID())
	if err == nil {
		t.Fatal("a resume whose outcome could not be established did not stop")
	}
	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("the resume returned %T (%v), want a *BlockedError", err, err)
	}
	if !errors.Is(err, ErrReconcile) {
		t.Fatalf("the stop is not errors.Is(ErrReconcile): %v", err)
	}
	if plan.Blocked == nil || plan.Blocked.ActionID != id {
		t.Fatalf("the plan does not name the action that blocked it: %+v", plan.Blocked)
	}
	if plan.Blocked.Reconciliation == "" {
		t.Fatal("the resume blocked without naming the reconciliation step")
	}
	for _, want := range []string{"kubernetes", postcondition, observe} {
		if !strings.Contains(plan.Blocked.Reconciliation, want) {
			t.Fatalf("the reconciliation step does not name %q: %q", want, plan.Blocked.Reconciliation)
		}
	}
	if plan.Skip()[id] {
		t.Fatalf("a blocked action was offered as skippable, so a re-run would treat an unknown outcome as done")
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("a blocked resume decided something anyway: %+v", plan.Steps)
	}
	if k.writeCount() != writesBefore {
		t.Fatal("the blocked resume submitted something")
	}

	// (b) an action with no recorded observation at all cannot be established
	// either, and that is a different reconciliation step. The first action is
	// marked from what the operator established by hand, so the resume reaches
	// the second one.
	if err := h.FinishAction(ctx, id, sshx.Result{}, nil); err != nil {
		t.Fatalf("recording the reconciled outcome: %v", err)
	}
	id2 := ActionID("kubernetes", "sudo -n k3s kubectl exec etcd-0 -- snapshot")
	if err := h.RecordAction(ctx, id2, "kubernetes", Spec{Kind: ActionSSH, Postcondition: "the etcd snapshot exists"}); err != nil {
		t.Fatalf("recording the second action: %v", err)
	}
	if err := h.SubmitAction(ctx, id2); err != nil {
		t.Fatalf("marking the second action submitted: %v", err)
	}
	plan2, err2 := Resume(ctx, s, h.OperationID())
	if !errors.Is(err2, ErrReconcile) {
		t.Fatalf("an action with no observable postcondition returned %v", err2)
	}
	if plan2.Blocked == nil || plan2.Blocked.Observe != "" || plan2.Blocked.ActionID != id2 {
		t.Fatalf("the block does not describe the unobservable action: %+v", plan2.Blocked)
	}
	if !strings.Contains(plan2.Blocked.Reconciliation, "the etcd snapshot exists") {
		t.Fatalf("the reconciliation step does not name what to establish: %q", plan2.Blocked.Reconciliation)
	}

	// (c) a status this build does not understand is not a reason to guess: a
	// record written by a newer CLI stops the resume like any other unknown
	// outcome.
	if err := h.FinishAction(ctx, id2, sshx.Result{}, nil); err != nil {
		t.Fatalf("recording the reconciled outcome: %v", err)
	}
	id3 := ActionID("kubernetes", "sudo -n k3s kubectl cordon node-1")
	if err := h.RecordAction(ctx, id3, "kubernetes", Spec{Kind: ActionSSH, Postcondition: "node-1 is cordoned"}); err != nil {
		t.Fatalf("recording the third action: %v", err)
	}
	if err := h.store.Update(ctx, h, func(r *Record) error {
		for i := range r.Actions {
			if r.Actions[i].ID == id3 {
				r.Actions[i].Status = ActionStatus("halfway")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("writing the unknown status: %v", err)
	}
	plan3, err3 := Resume(ctx, s, h.OperationID())
	if !errors.Is(err3, ErrReconcile) {
		t.Fatalf("an unknown action status returned %v, want ErrReconcile", err3)
	}
	if plan3.Blocked == nil || !strings.Contains(plan3.Blocked.Reconciliation, "halfway") {
		t.Fatalf("the block does not name the status it could not read: %+v", plan3.Blocked)
	}
	if plan3.Skip()[id3] {
		t.Fatal("an action with an unknown status was offered as skippable")
	}
}

// TestAPausedLiveExecutorPreventsTakeOver: a paused executor is coming back, so
// its record is not available even though every action of it is reconciled.
func TestAPausedLiveExecutorPreventsTakeOver(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	const command = "sudo -n k3s kubectl apply -f /manifests/server.yaml"
	id := ActionID("kubernetes", command)
	if err := h.RecordAction(ctx, id, "kubernetes", Spec{Kind: ActionPlan, Postcondition: "applied", Observe: "true"}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if err := h.SubmitAction(ctx, id); err != nil {
		t.Fatalf("submitting: %v", err)
	}
	if err := h.FinishAction(ctx, id, sshx.Result{}, nil); err != nil {
		t.Fatalf("finishing: %v", err)
	}
	// The maintenance window closed between stages: the executor is paused, not
	// gone.
	if err := s.Update(ctx, h, func(r *Record) error {
		r.Executor.State = ExecutorPaused
		return nil
	}); err != nil {
		t.Fatalf("pausing: %v", err)
	}

	rv, token := k.resourceVersion(Name), h.Token()
	other := newStore(t, k, "bo@other-laptop")
	if _, err := TakeOver(ctx, other, h.OperationID()); !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("a take-over of a paused executor returned %v, want ErrTakeOverRefused", err)
	} else if !strings.Contains(err.Error(), string(ExecutorPaused)) {
		t.Fatalf("the refusal does not say the executor is paused: %v", err)
	}
	if k.resourceVersion(Name) != rv || k.liveRecord(Name).Executor.Token != token {
		t.Fatal("a refused take-over wrote to the record")
	}
}

// TestAStaleRecordAloneDoesNotPermitTakeOver: an old heartbeat is how
// "interrupted" is read for display, and never a permission.
func TestAStaleRecordAloneDoesNotPermitTakeOver(t *testing.T) {
	k := newFakeKube(t)
	s, setClock := clockStore(t, k, "ana@laptop")
	ctx := context.Background()
	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	setClock(base)

	h := acquire(t, s, testRequest("host-1"))
	setClock(base.Add(6 * time.Hour))

	rec := k.liveRecord(Name)
	if age := base.Add(6 * time.Hour).Sub(rec.Executor.Heartbeat); age < time.Hour {
		t.Fatalf("the heartbeat is not stale: %s", age)
	}
	if rec.Executor.State != ExecutorRunning {
		t.Fatalf("the executor's recorded state is %q, want %q", rec.Executor.State, ExecutorRunning)
	}

	other := newStore(t, k, "bo@other-laptop")
	_, err := TakeOver(ctx, other, h.OperationID())
	if !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("a take-over on a stale record returned %v, want ErrTakeOverRefused", err)
	}
	if !strings.Contains(err.Error(), string(ExecutorRunning)) {
		t.Fatalf("the refusal does not say the executor is still running: %v", err)
	}
	if k.liveRecord(Name).Executor.Token != h.Token() {
		t.Fatal("the refused take-over changed the record's owner")
	}

	// A network partition is not permission either: a record this executor
	// cannot read is a record it cannot reason about.
	partitioned := &Store{Runner: deadRunner{}, Operator: "bo@other-laptop"}
	if _, err := TakeOver(ctx, partitioned, h.OperationID()); !errors.Is(err, ErrTakeOverRefused) {
		t.Fatalf("a take-over across a partition returned %v, want ErrTakeOverRefused", err)
	}
}

// TestAfterAPermittedTakeOverTheOldTokenCannotUpdate: once the rules are
// satisfied the transfer is real, and it is the token that makes it real.
func TestAfterAPermittedTakeOverTheOldTokenCannotUpdate(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	// Everything the old executor had outstanding is reconciled, and the
	// operator has stopped it.
	const command = "sudo -n k3s kubectl apply -f /manifests/server.yaml"
	id := ActionID("kubernetes", command)
	if err := h.RecordAction(ctx, id, "kubernetes", Spec{Kind: ActionPlan, Postcondition: "applied", Observe: "true"}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if err := h.SubmitAction(ctx, id); err != nil {
		t.Fatalf("submitting: %v", err)
	}
	if err := h.FinishAction(ctx, id, sshx.Result{}, nil); err != nil {
		t.Fatalf("finishing: %v", err)
	}
	if err := s.Stop(ctx, h); err != nil {
		t.Fatalf("stopping the old executor: %v", err)
	}
	rvBefore := k.resourceVersion(Name)

	other := newStore(t, k, "bo@other-laptop")
	h2, err := TakeOver(ctx, other, h.OperationID())
	if err != nil {
		t.Fatalf("a permitted take-over was refused: %v", err)
	}
	if h2.OperationID() != h.OperationID() || h2.Token() == h.Token() {
		t.Fatalf("the take-over returned operation %s token %s", h2.OperationID(), h2.Token())
	}
	rec := k.liveRecord(Name)
	if rec.Executor.Token != h2.Token() || rec.Executor.Operator != "bo@other-laptop" || rec.Executor.State != ExecutorRunning {
		t.Fatalf("the transferred record's executor is %+v", rec.Executor)
	}
	if k.resourceVersion(Name) == rvBefore {
		t.Fatal("the take-over did not write the record")
	}

	// The old token cannot write, and it is refused as an ownership problem —
	// not merely because the resourceVersion moved.
	err = s.Update(ctx, h, func(r *Record) error { return nil })
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("the old token's write returned %v, want ErrNotOwner", err)
	}
	if errors.Is(err, ErrStale) {
		t.Fatalf("the old token's write was refused as stale, so the token check is not what refused it: %v", err)
	}
	if err := h2.Heartbeat(ctx, "kubernetes"); err != nil {
		t.Fatalf("the new owner could not write: %v", err)
	}
}

// TestCompletionCopiesHistoryUnderTheOperationIDAndNeverRenames.
func TestCompletionCopiesHistoryUnderTheOperationIDAndNeverRenames(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1", "host-2"))

	const command = "sudo -n k3s kubectl apply -f /manifests/agent.yaml"
	id := ActionID("kubernetes", command)
	if err := h.RecordAction(ctx, id, "kubernetes", Spec{Kind: ActionPlan, Postcondition: "the agent Plan is complete", Observe: "true"}); err != nil {
		t.Fatalf("recording: %v", err)
	}
	if err := h.SubmitAction(ctx, id); err != nil {
		t.Fatalf("submitting: %v", err)
	}
	if err := h.FinishAction(ctx, id, sshx.Result{}, nil); err != nil {
		t.Fatalf("finishing: %v", err)
	}
	if err := s.Complete(ctx, h, ResultSucceeded); err != nil {
		t.Fatalf("completing: %v", err)
	}

	// The life record is still at the live name, marked terminal. It was
	// copied, not moved: a rename would be a delete plus a create, and between
	// those two the cluster holds no record at all.
	live := k.liveRecord(Name)
	if !live.Terminal || live.Result != string(ResultSucceeded) {
		t.Fatalf("the live record is terminal=%v result=%q", live.Terminal, live.Result)
	}
	if live.OperationID != h.OperationID() {
		t.Fatalf("the live object holds operation %s", live.OperationID)
	}
	if a, ok := live.ActionByID(id); !ok || a.Status != ActionSucceeded {
		t.Fatalf("the terminal record lost its action: %+v", a)
	}
	// The copy is a second object under the operation's own ID.
	historyName := HistoryPrefix + h.OperationID()
	hist := k.liveRecord(historyName)
	if hist.OperationID != h.OperationID() || !hist.Terminal {
		t.Fatalf("the history copy holds %s terminal=%v", hist.OperationID, hist.Terminal)
	}
	if _, ok := hist.ActionByID(id); !ok {
		t.Fatal("the history copy does not carry the actions")
	}

	// A later operation replaces the live record...
	next := acquire(t, s, testRequest("host-3"))
	if cur, err := s.Current(ctx); err != nil || cur.Record.OperationID != next.OperationID() {
		t.Fatalf("the live record after the next acquisition is %v (%v)", cur, err)
	}
	// ...and the finished one is still findable by --resume <operation-id>.
	found, err := s.Find(ctx, h.OperationID())
	if err != nil {
		t.Fatalf("finding a finished operation after a later one replaced the record: %v", err)
	}
	if found.Record.OperationID != h.OperationID() || !found.Record.Terminal {
		t.Fatalf("the history lookup returned %s terminal=%v", found.Record.OperationID, found.Record.Terminal)
	}
	if found.Record.Result != string(ResultSucceeded) {
		t.Fatalf("the history copy's result is %q", found.Record.Result)
	}
}

// TestInterruptedInventoryWriteStaysVisibleAsPending: completion distinguishes
// finished host work from the writes the operation still owes.
func TestInterruptedInventoryWriteStaysVisibleAsPending(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1", "host-2"))

	// host-2 joined and its inventory write was interrupted.
	if err := h.PendingWrite(ctx, "inventory", "host-2", "host joined, inventory write interrupted"); err != nil {
		t.Fatalf("recording the pending write: %v", err)
	}
	if err := h.PendingWrite(ctx, "inventory", "host-2", "again"); err != nil {
		t.Fatalf("recording the same pending write twice: %v", err)
	}
	owed := k.liveRecord(Name).Outstanding()
	if len(owed) != 1 || owed[0].Target != "host-2" || owed[0].Status != WritePending {
		t.Fatalf("the ledger holds %+v", owed)
	}

	if err := s.Complete(ctx, h, ResultSucceeded); err != nil {
		t.Fatalf("completing with a write still owed: %v", err)
	}
	live := k.liveRecord(Name)
	if len(live.Outstanding()) != 1 {
		t.Fatalf("completion dropped the outstanding write: %+v", live.Outstanding())
	}
	if !strings.Contains(live.Result, "host-2") {
		t.Fatalf("the terminal result does not name what is still owed: %q", live.Result)
	}
	// It survives the live record being replaced: the next operator still sees
	// the host it does not know about.
	next := acquire(t, s, testRequest("host-3"))
	hist, err := s.Find(ctx, h.OperationID())
	if err != nil {
		t.Fatalf("finding the finished record: %v", err)
	}
	owed = hist.Record.Outstanding()
	if len(owed) != 1 || owed[0].Target != "host-2" {
		t.Fatalf("the history copy lost the pending write: %+v", owed)
	}

	// A discharged write is not outstanding, and completion then says exactly
	// what it did.
	if err := next.PendingWrite(ctx, "inventory", "host-3", ""); err != nil {
		t.Fatalf("recording a pending write: %v", err)
	}
	if err := next.PendingWriteDone(ctx, "inventory", "host-3"); err != nil {
		t.Fatalf("discharging the write: %v", err)
	}
	if err := next.PendingWriteDone(ctx, "inventory", "host-3"); err != nil {
		t.Fatalf("discharging the same write twice: %v", err)
	}
	if err := s.Complete(ctx, next, ResultSucceeded); err != nil {
		t.Fatalf("completing: %v", err)
	}
	done := k.liveRecord(Name)
	if len(done.Outstanding()) != 0 {
		t.Fatalf("a discharged write is still outstanding: %+v", done.Outstanding())
	}
	if done.Result != string(ResultSucceeded) {
		t.Fatalf("nothing was owed, so the result is %q", done.Result)
	}
}

// fakeStringWriter captures what the copy writes.
type fakeStringWriter struct {
	objects map[string][]byte
	order   []string
	// conditional is whether the target can create an object conditionally —
	// what probe P3 settles.
	conditional  bool
	puts         int
	putsIfAbsent int
}

func newStringWriter(conditional bool) *fakeStringWriter {
	return &fakeStringWriter{objects: map[string][]byte{}, conditional: conditional}
}

func (w *fakeStringWriter) Put(_ context.Context, key string, body []byte) error {
	w.puts++
	w.objects[key] = append([]byte(nil), body...)
	w.order = append(w.order, "put "+key)
	return nil
}

func (w *fakeStringWriter) PutIfAbsent(_ context.Context, key string, body []byte) (bool, error) {
	w.putsIfAbsent++
	if !w.conditional {
		return false, ErrNoConditionalWrites
	}
	if _, ok := w.objects[key]; ok {
		return false, nil
	}
	w.objects[key] = append([]byte(nil), body...)
	w.order = append(w.order, "put-if-absent "+key)
	return true, nil
}

// xorSealer stands in for the fleet recovery key: enough to show that the copy
// is the SEALED bytes, and that the key never travels with them.
type xorSealer struct {
	key    byte
	sealed [][]byte
	inputs [][]byte
}

func (s *xorSealer) Seal(plain []byte) ([]byte, error) {
	out := make([]byte, len(plain))
	for i, b := range plain {
		out[i] = b ^ s.key
	}
	s.inputs = append(s.inputs, append([]byte(nil), plain...))
	s.sealed = append(s.sealed, out)
	return out, nil
}

// TestTheS3CopyCarriesNoCredentials: the copy is the sealed record and nothing
// else, the record's own shape cannot hold a credential, and ownership outside
// the cluster is taken by a CONDITIONAL create or not at all.
func TestTheS3CopyCarriesNoCredentials(t *testing.T) {
	k := newFakeKube(t)
	s := newStore(t, k, "ana@laptop")
	ctx := context.Background()
	h := acquire(t, s, testRequest("host-1"))

	// A remote action whose output echoed a credential. The record is copied
	// off-cluster and mirrored to the control plane, so what a remote shell
	// prints must not be able to put one there.
	const leaked = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJrdWJlbmVzdC1hZ2VudCJ9.c2lnbmF0dXJlLWZvci10aGUtYWdlbnQ"
	spy := &spyRunner{t: t, res: sshx.Result{
		ExitCode: 1,
		Stderr:   "curl -H 'Authorization: Bearer " + leaked + "' https://kubenestapp.com/api/v1/clusters\n",
	}}
	g := &Guarded{Inner: spy, Op: h, Stage: "kubernetes", Specs: sshSpecs("the cluster is registered", "")}
	res, err := g.Run(ctx, "sudo -n k3s kubectl get nodes")
	if err != nil {
		t.Fatalf("the transport failed: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("the scripted failure did not fail")
	}
	rec := k.liveRecord(Name)
	if a := rec.Actions[0]; a.Status != ActionFailed {
		t.Fatalf("a non-zero exit left the action %q", a.Status)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("encoding the record: %v", err)
	}
	if bytes.Contains(raw, []byte(leaked)) {
		t.Fatalf("the record carries the credential the remote shell printed: %s", raw)
	}
	if !bytes.Contains(raw, []byte("[redacted")) {
		t.Fatalf("the echoed credential was dropped rather than redacted: %s", raw)
	}
	// The record's shape is what makes "the copy carries no credentials" a
	// property rather than a hope.
	if bad := credentialFieldsIn(raw); len(bad) != 0 {
		t.Fatalf("the record has credential-shaped fields: %v", bad)
	}
	if bad := credentialFieldsIn([]byte(`{"backup":{"secret_access_key":"AKIA..."}}`)); len(bad) == 0 {
		t.Fatal("the guard does not see a credential-shaped field")
	}
	// The ownership token is a name, not a secret: a record that could not say
	// who owns it could not refuse anyone.
	if bad := credentialFieldsIn([]byte(`{"executor":{"token":"f00d"}}`)); len(bad) != 0 {
		t.Fatalf("the ownership token was treated as a credential: %v", bad)
	}

	// The copy exists for the operations that can destroy their own record. The
	// others' records outlive them, and asking for a copy of everything would
	// put every operation's record on a target it does not belong on.
	for _, kind := range []Kind{KindDatastoreRollback, KindHostRecovery, KindControlPlaneUpgrade} {
		if !kind.RecordDestroysItself() {
			t.Fatalf("%s can destroy its own record and must be copied off-cluster", kind)
		}
	}
	for _, kind := range []Kind{KindUpgrade, KindNodeReboot, KindNodeAdd, KindRestoreVolume, KindRestoreNamespace} {
		if kind.RecordDestroysItself() {
			t.Fatalf("%s leaves its record in the cluster, so it does not need an off-cluster copy", kind)
		}
	}

	// The copy.
	w := newStringWriter(true)
	sealer := &xorSealer{key: 0x5a}
	c := Copy{Writer: w, Sealer: sealer, Prefix: "fleet/recovery/"}
	key, err := c.Write(ctx, rec)
	if err != nil {
		t.Fatalf("writing the copy: %v", err)
	}
	if key != c.ObjectKey(rec.OperationID) {
		t.Fatalf("the copy landed at %q", key)
	}
	body := w.objects[key]
	plain, err := encodeRecord(rec)
	if err != nil {
		t.Fatalf("encoding the record: %v", err)
	}
	if bytes.Contains(body, plain) || bytes.Contains(body, []byte("operation_id")) {
		t.Fatal("the copy is the plaintext record: it was not encrypted")
	}
	if bytes.Contains(body, []byte(leaked)) {
		t.Fatal("the copy carries the credential")
	}
	// The sealer was asked to seal the record, and nothing else: no key
	// material and no target credentials are ever handed to it.
	if len(sealer.inputs) != 1 || !bytes.Equal(sealer.inputs[0], plain) {
		t.Fatalf("the sealer was asked to seal %d payload(s), and not the record", len(sealer.inputs))
	}
	if !bytes.Equal(body, sealer.sealed[0]) {
		t.Fatalf("the copy is %d bytes and the sealed record is %d: something else travelled with it", len(body), len(sealer.sealed[0]))
	}
	// Writing the copy does NOT take ownership: the copy is evidence, not a
	// lock.
	if _, ok := w.objects[c.OwnerKey(rec.OperationID)]; ok {
		t.Fatal("writing the copy created the recovery-owner object")
	}
	if w.putsIfAbsent != 0 {
		t.Fatal("writing the copy made a conditional create")
	}

	// Ownership outside the cluster is a conditional create, or nothing.
	ownerKey, err := c.Claim(ctx, rec.OperationID, "ana@laptop", time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if _, ok := w.objects[ownerKey]; !ok || w.putsIfAbsent != 1 {
		t.Fatalf("the claim did not create the object conditionally (puts=%d, exists=%v)", w.putsIfAbsent, ok)
	}
	if _, err := c.Claim(ctx, rec.OperationID, "bo@other-laptop", time.Now()); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("a second claim returned %v, want ErrAlreadyClaimed", err)
	}
	if bytes.Contains(w.objects[ownerKey], []byte(leaked)) {
		t.Fatal("the recovery-owner object carries the credential")
	}

	// A target without conditional creates stops, and does not quietly write
	// unconditionally: automated recovery there needs the documented
	// single-operator rule acknowledged first (F20).
	noConditional := newStringWriter(false)
	c2 := Copy{Writer: noConditional, Sealer: sealer}
	if _, err := c2.Claim(ctx, rec.OperationID, "ana@laptop", time.Now()); !errors.Is(err, ErrNoConditionalWrites) {
		t.Fatalf("a claim on a target without conditional creates returned %v", err)
	}
	if noConditional.puts != 0 || len(noConditional.objects) != 0 {
		t.Fatalf("the claim fell back to an unconditional write: %v", noConditional.order)
	}
}
