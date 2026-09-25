package operation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
)

// The tests below drive the mirror through the REAL api.Client against an
// httptest control plane, and the record through the real Store against
// fakeKube: the point is the pair — every successful write to the cluster
// produces exactly one mirror write, and a mirror that fails is reported
// without the operation losing anything.

const testMirrorClusterID = "8f14e45f-ceea-4a1e-9a0f-2b3c4d5e6f70"

// mirrorServer is a control plane's mirror route: it records what it was sent,
// and can be told to fail or to stall.
type mirrorServer struct {
	mu     sync.Mutex
	states []api.ClusterOperationState
	bodies [][]byte
	// status, when set, is the answer to every write; 0 means 200.
	status int
	// delay is how long the route takes to answer.
	delay time.Duration
}

func (s *mirrorServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var state api.ClusterOperationState
	json.Unmarshal(body, &state)

	s.mu.Lock()
	s.states = append(s.states, state)
	s.bodies = append(s.bodies, body)
	status, delay := s.status, s.delay
	s.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if status != 0 {
		w.WriteHeader(status)
		io.WriteString(w, `{"detail": "the mirror is unavailable"}`)
		return
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, `{"operation_id":"`+state.OperationID+`"}`)
}

func (s *mirrorServer) sent(t *testing.T) []api.ClusterOperationState {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.ClusterOperationState(nil), s.states...)
}

func (s *mirrorServer) raw(t *testing.T, i int) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		t.Fatalf("the mirror saw %d write(s); there is no %d", len(s.bodies), i)
	}
	var doc map[string]any
	if err := json.Unmarshal(s.bodies[i], &doc); err != nil {
		t.Fatalf("mirror write %d is not JSON: %v", i, err)
	}
	return doc
}

func (s *mirrorServer) fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *mirrorServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.states)
}

// mirroredStore is a Store against fakeKube with a real api.Client pointed at
// the httptest mirror.
func mirroredStore(t *testing.T, k *fakeKube, m *mirrorServer) *Store {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	client, err := api.New(srv.URL, api.WithToken("knp_report"))
	if err != nil {
		t.Fatalf("building the mirror client: %v", err)
	}
	s := newStore(t, k, "ana@laptop")
	s.Mirror = client
	s.MirrorClusterID = testMirrorClusterID
	return s
}

// TestTheMirrorIsPostedAfterEveryRecordWrite: the record is what the console
// and `check_upgrade` display, and every write that moves it is mirrored — the
// acquire that takes the lock, every update, and the terminal write. The
// history copy is not: it is the same revision and the same content.
func TestTheMirrorIsPostedAfterEveryRecordWrite(t *testing.T) {
	ctx := context.Background()
	k := newFakeKube(t)
	m := &mirrorServer{}
	s := mirroredStore(t, k, m)
	req := testRequest("host-1")

	h, err := s.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquiring: %v", err)
	}
	first := m.sent(t)
	if len(first) != 1 {
		t.Fatalf("taking the lock mirrored %d write(s), want 1", len(first))
	}
	if first[0].OperationID != h.OperationID() || first[0].Revision != h.ResourceVersion() {
		t.Fatalf("the mirror was sent %+v, want operation %s at revision %s", first[0], h.OperationID(), h.ResourceVersion())
	}
	if first[0].Terminal || first[0].State != api.ClusterOperationRunning {
		t.Fatalf("the mirrored record is %q terminal=%t", first[0].State, first[0].Terminal)
	}
	if first[0].RequestDigest != req.Digest() {
		t.Fatalf("the mirror's request digest is %q, want the immutable request's %q", first[0].RequestDigest, req.Digest())
	}
	if first[0].Stage != "" {
		t.Fatalf("a record with no stage was mirrored as stage %q", first[0].Stage)
	}
	if want := "kubenest platform upgrade --resume " + h.OperationID(); first[0].ResumeCommand != want {
		t.Fatalf("the mirror's resume command is %q, want %q", first[0].ResumeCommand, want)
	}
	if first[0].ExecutorHeartbeatAt == nil || !first[0].ExecutorHeartbeatAt.Equal(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("the mirror's heartbeat is %v, want the store's clock", first[0].ExecutorHeartbeatAt)
	}
	if err := h.MirrorError(); err != nil {
		t.Fatalf("a mirror write that succeeded is reported as a failure: %v", err)
	}

	// Every update: the heartbeat that tells a reader "running" from
	// "interrupted" is one of them.
	if err := h.Heartbeat(ctx, "kubernetes"); err != nil {
		t.Fatalf("heartbeating: %v", err)
	}
	second := m.sent(t)
	if len(second) != 2 {
		t.Fatalf("an update mirrored %d write(s) in total, want 2", len(second))
	}
	if second[1].Stage != "kubernetes" || second[1].Revision == first[0].Revision {
		t.Fatalf("the update was mirrored as %+v (previous revision %s)", second[1], first[0].Revision)
	}

	// Completion: the terminal state, with the stage it ended at, and no resume
	// command — a finished operation has nothing to resume.
	if err := s.Complete(ctx, h, ResultSucceeded); err != nil {
		t.Fatalf("completing: %v", err)
	}
	third := m.sent(t)
	if len(third) != 3 {
		t.Fatalf("completion mirrored %d write(s) in total, want 3", len(third))
	}
	last := third[2]
	if !last.Terminal || last.State != api.ClusterOperationTerminal {
		t.Fatalf("the terminal write was mirrored as %q terminal=%t", last.State, last.Terminal)
	}
	if last.Stage != "kubernetes" {
		t.Fatalf("the terminal write lost the stage it ended at: %+v", last)
	}
	if _, ok := m.raw(t, 2)["resume_command"]; ok {
		t.Fatalf("a terminal record was mirrored with a resume command: %s", m.bodies[2])
	}
	// The history object was written to the cluster and NOT mirrored: it is the
	// same revision and the same content as the terminal write.
	if k.writeCount() != 4 {
		t.Fatalf("the cluster accepted %d writes, want create + heartbeat + terminal + history", k.writeCount())
	}
	if m.count() != 3 {
		t.Fatalf("the mirror saw %d write(s) for 4 cluster writes: the history copy is mirrored again", m.count())
	}
}

// TestTheMirrorNeverNamesACommandTheKindHasNoVerbFor: the resume command is
// shown to a human, so it must be one this CLI can actually run. A kind whose
// verb the plan does not name yet mirrors without one, and the control plane
// says exactly that rather than printing a command that does not exist.
func TestTheMirrorNeverNamesACommandTheKindHasNoVerbFor(t *testing.T) {
	ctx := context.Background()
	k := newFakeKube(t)
	m := &mirrorServer{}
	s := mirroredStore(t, k, m)

	req := testRequest("host-1")
	req.Kind = KindControlPlaneUpgrade
	h, err := s.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquiring: %v", err)
	}
	if h.Record().ResumeCommand() != "" {
		t.Fatalf("a control-plane upgrade named a resume command: %q", h.Record().ResumeCommand())
	}
	if _, ok := m.raw(t, 0)["resume_command"]; ok {
		t.Fatalf("the mirror carried a resume command for a kind with no verb: %s", m.bodies[0])
	}
	// A verb this CLI does have is named, with the operation id the operator
	// types.
	if got, want := testRequest("host-1").Kind.resumeVerb(), "kubenest platform upgrade"; got != want {
		t.Fatalf("an upgrade resumes with %q, want %q", got, want)
	}
}

// TestAFailedMirrorIsReportedAndNeverBlocksTheOperation: the record in the
// cluster is the lock and the authority, and it keeps refusing a second
// operator while the control plane is unreachable. So a mirror write that
// fails is reported and the operation carries on — and the next write retries
// it at the revision that then exists.
func TestAFailedMirrorIsReportedAndNeverBlocksTheOperation(t *testing.T) {
	ctx := context.Background()
	k := newFakeKube(t)
	m := &mirrorServer{}
	m.fail(http.StatusInternalServerError)
	s := mirroredStore(t, k, m)

	h, err := s.Acquire(ctx, testRequest("host-1"))
	if err != nil {
		t.Fatalf("a control plane that refuses the mirror failed the acquire: %v", err)
	}
	if !k.exists(Name) {
		t.Fatal("the record was not written to the cluster")
	}
	if err := h.MirrorError(); err == nil {
		t.Fatal("a failed mirror write is not reported at all")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, h.OperationID()) {
			t.Fatalf("the reported mirror failure does not name the operation: %q", msg)
		}
		if !strings.Contains(msg, "the record in the cluster is unaffected") {
			t.Fatalf("the reported mirror failure does not say the operation is unaffected: %q", msg)
		}
	}

	// The operation continues, writes the record, and the mirror succeeds
	// again: a later success clears the report.
	if err := h.Heartbeat(ctx, "kubernetes"); err != nil {
		t.Fatalf("an operation with a broken mirror could not heartbeat: %v", err)
	}
	if got := k.liveRecord(Name).Stage; got != "kubernetes" {
		t.Fatalf("the cluster's record is at stage %q, want kubernetes", got)
	}
	m.fail(0)
	if err := h.Heartbeat(ctx, "apply-plan"); err != nil {
		t.Fatalf("heartbeating: %v", err)
	}
	if err := h.MirrorError(); err != nil {
		t.Fatalf("a mirror write that succeeded after a failure is still reported as failed: %v", err)
	}
}

// TestAMirrorThatDoesNotAnswerDoesNotHoldTheOperation: the mirror is display.
// A control plane that accepts the connection and then does not answer costs a
// bounded pause, not a stalled operation, and the record it should have
// mirrored is still written to the cluster.
func TestAMirrorThatDoesNotAnswerDoesNotHoldTheOperation(t *testing.T) {
	ctx := context.Background()
	k := newFakeKube(t)
	m := &mirrorServer{delay: 300 * time.Millisecond}
	s := mirroredStore(t, k, m)
	s.MirrorTimeout = 50 * time.Millisecond

	started := time.Now()
	h, err := s.Acquire(ctx, testRequest("host-1"))
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("a mirror that does not answer failed the acquire: %v", err)
	}
	if !k.exists(Name) {
		t.Fatal("the record was not written to the cluster")
	}
	if err := h.MirrorError(); err == nil {
		t.Fatal("a mirror write that timed out is not reported")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the reported mirror failure is %v, want the deadline it hit", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("taking the lock waited %s for a mirror that does not answer", elapsed)
	}
}

// TestAMirrorWithoutAClusterIDIsReportedRatherThanGuessed: the mirror is
// addressed by the control plane's cluster id, which the record does not hold —
// it holds the cluster's NAME. Posting to the wrong cluster would put one
// customer's operation on another's console, so a CLI that does not know the id
// reports that instead of guessing, and writes nothing.
func TestAMirrorWithoutAClusterIDIsReportedRatherThanGuessed(t *testing.T) {
	ctx := context.Background()
	k := newFakeKube(t)
	m := &mirrorServer{}
	s := mirroredStore(t, k, m)
	s.MirrorClusterID = ""

	h, err := s.Acquire(ctx, testRequest("host-1"))
	if err != nil {
		t.Fatalf("acquiring without a cluster id: %v", err)
	}
	if err := h.MirrorError(); err == nil {
		t.Fatal("a configured mirror that could not post reported nothing")
	} else if !strings.Contains(err.Error(), "cluster id") {
		t.Fatalf("the report does not name what is missing: %v", err)
	}
	if m.count() != 0 {
		t.Fatalf("the mirror was posted to with no cluster id: %d write(s)", m.count())
	}
	if !k.exists(Name) {
		t.Fatal("the record was not written to the cluster")
	}

	// A CLI with no mirror at all is not a failure: the record stays in the
	// cluster, which is where the lock lives, and there is nothing to report.
	unmirrored := newFakeKube(t)
	h2 := acquire(t, newStore(t, unmirrored, "ana@laptop"), testRequest("host-2"))
	if err := h2.MirrorError(); err != nil {
		t.Fatalf("a CLI with no mirror reported a mirror failure: %v", err)
	}
}
