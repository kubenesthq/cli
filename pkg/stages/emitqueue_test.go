package stages_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/stages"
)

// received is one install-event POST as the control plane saw it.
type received struct {
	clusterID string
	entry     api.InstallJournalEntry
}

// controlPlane is a real HTTP server standing in for the backend's
// install-events endpoint, so the queue is exercised through the actual
// client and the actual JSON.
func controlPlane(t *testing.T, fail func(n int) bool) (*api.Client, *[]received) {
	t.Helper()
	var got []received
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// /api/v1/clusters/{id}/install-events
		if len(parts) < 5 || parts[4] != "install-events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n++
		if fail != nil && fail(n) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"detail":"control plane is restarting"}`))
			return
		}
		var entry api.InstallJournalEntry
		if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got = append(got, received{clusterID: parts[3], entry: entry})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	client, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatal(err)
	}
	return client, &got
}

// The whole point of the queue: preflight's two transitions and register's
// `started` happen before a cluster id exists, and they must still arrive —
// in order — once registration produces one.
func TestPreRegistrationTransitionsAreQueuedAndFlushedInOrder(t *testing.T) {
	client, got := controlPlane(t, nil)
	rec := &recorder{}
	s := newSession(t, rec)
	emitter := stages.NewControlPlaneEmitter(client, func() string { return s.journal.ClusterID })
	ctx := context.Background()

	emit := func(stage string, status stages.Status) {
		if err := emitter.Emit(ctx, stages.Event{Stage: stage, Status: status}); err != nil {
			t.Fatalf("%s %s: %v", stage, status, err)
		}
	}

	// Stage 1 writes nothing anywhere and there is no cluster yet.
	emit(stagePreflight, stages.StatusStarted)
	emit(stagePreflight, stages.StatusCompleted)
	emit(stageRegister, stages.StatusStarted)
	if len(*got) != 0 {
		t.Fatalf("nothing can be sent before a cluster exists, sent %d", len(*got))
	}
	if emitter.Pending() != 3 {
		t.Fatalf("queued %d transitions, want 3", emitter.Pending())
	}

	// Registration returns the id mid-stage.
	s.journal.ClusterID = "01a02362-f8a3-7dd6-aa07-2f10ed7a5c17"
	emit(stageRegister, stages.StatusCompleted)

	var order []string
	for _, r := range *got {
		order = append(order, r.entry.Stage+":"+string(r.entry.Status))
		if r.clusterID != "01a02362-f8a3-7dd6-aa07-2f10ed7a5c17" {
			t.Errorf("event went to cluster %q", r.clusterID)
		}
	}
	want := []string{
		"preflight:started", "preflight:completed",
		"register:started", "register:completed",
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("flushed\n  %v\nwant\n  %v", order, want)
	}
	if emitter.Pending() != 0 {
		t.Errorf("%d transitions still queued after the flush", emitter.Pending())
	}
}

// A queued transition carries the time it HAPPENED, not the time it was
// flushed — otherwise preflight would appear to have run after registration.
func TestQueuedTransitionsKeepTheirOwnTimestamps(t *testing.T) {
	client, got := controlPlane(t, nil)
	rec := &recorder{}
	s := newSession(t, rec)
	emitter := stages.NewControlPlaneEmitter(client, func() string { return s.journal.ClusterID })
	ctx := context.Background()

	if err := emitter.Emit(ctx, stages.Event{Stage: stagePreflight, Status: stages.StatusStarted}); err != nil {
		t.Fatal(err)
	}
	s.journal.ClusterID = "cluster-1"
	if err := emitter.Emit(ctx, stages.Event{Stage: stageRegister, Status: stages.StatusCompleted}); err != nil {
		t.Fatal(err)
	}

	if len(*got) != 2 {
		t.Fatalf("got %d events, want 2", len(*got))
	}
	first, second := (*got)[0].entry, (*got)[1].entry
	if first.At == nil || second.At == nil {
		t.Fatal("every entry must carry a timestamp")
	}
	if first.At.After(*second.At) {
		t.Errorf("the queued transition is stamped after the one that flushed it: %v vs %v", first.At, second.At)
	}
}

// `started` is the ONLY status that clears install_failed on the backend
// (kubenest-backend 9c19e5e). A stage whose `started` never arrives is a
// stage whose failure can never be cleared, so the queue must not drop one.
func TestEveryStartedSurvivesTheQueue(t *testing.T) {
	client, got := controlPlane(t, nil)
	rec := &recorder{}
	s := newSession(t, rec)
	emitter := stages.NewControlPlaneEmitter(client, func() string { return s.journal.ClusterID })
	ctx := context.Background()

	for _, stage := range stageNames {
		if stage == stageK3sServer {
			// The id appears during register, before the first machine is touched.
			s.journal.ClusterID = "cluster-1"
		}
		for _, status := range []stages.Status{stages.StatusStarted, stages.StatusCompleted} {
			if err := emitter.Emit(ctx, stages.Event{Stage: stage, Status: status}); err != nil {
				t.Fatalf("%s %s: %v", stage, status, err)
			}
		}
	}

	starts := map[string]bool{}
	for _, r := range *got {
		if r.entry.Status == api.StageStarted {
			starts[r.entry.Stage] = true
		}
	}
	for _, stage := range stageNames {
		if !starts[stage] {
			t.Errorf("stage %s never reported `started`: its failure could never be cleared", stage)
		}
	}
	if len(*got) != 2*len(stageNames) {
		t.Errorf("sent %d events, want %d (started+terminal for all thirteen)", len(*got), 2*len(stageNames))
	}
}

// A control plane that is briefly away must not lose the queue: the unsent
// remainder stays queued, in order, and goes out with the next transition.
func TestAFailedFlushKeepsTheRemainderQueued(t *testing.T) {
	// Fail the first POST after the id appears, then accept everything.
	client, got := controlPlane(t, func(n int) bool { return n == 1 })
	rec := &recorder{}
	s := newSession(t, rec)
	emitter := stages.NewControlPlaneEmitter(client, func() string { return s.journal.ClusterID })
	ctx := context.Background()

	_ = emitter.Emit(ctx, stages.Event{Stage: stagePreflight, Status: stages.StatusStarted})
	_ = emitter.Emit(ctx, stages.Event{Stage: stagePreflight, Status: stages.StatusCompleted})
	s.journal.ClusterID = "cluster-1"

	if err := emitter.Emit(ctx, stages.Event{Stage: stageRegister, Status: stages.StatusStarted}); err == nil {
		t.Fatal("a failed flush must be reported to the caller")
	}
	if emitter.Pending() == 0 {
		t.Fatal("the unsent transitions were dropped")
	}

	if err := emitter.Emit(ctx, stages.Event{Stage: stageRegister, Status: stages.StatusCompleted}); err != nil {
		t.Fatalf("the retry must succeed: %v", err)
	}
	var order []string
	for _, r := range *got {
		order = append(order, r.entry.Stage+":"+string(r.entry.Status))
	}
	want := []string{"preflight:started", "preflight:completed", "register:started", "register:completed"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("after the retry the control plane saw\n  %v\nwant\n  %v", order, want)
	}
}

// A preflight refusal never registers a cluster, so its queued transitions
// have nowhere to go. Discarding them is correct — nothing was written to any
// machine and no cluster record exists.
func TestAPreflightRefusalDiscardsTheQueue(t *testing.T) {
	client, got := controlPlane(t, nil)
	rec := &recorder{}
	s := newSession(t, rec)
	emitter := stages.NewControlPlaneEmitter(client, func() string { return s.journal.ClusterID })
	ctx := context.Background()

	_ = emitter.Emit(ctx, stages.Event{Stage: stagePreflight, Status: stages.StatusStarted})
	_ = emitter.Emit(ctx, stages.Event{Stage: stagePreflight, Status: stages.StatusFailed,
		ReasonCode: "PREFLIGHT_FAILED", Message: "sudo -n true failed"})

	if len(*got) != 0 {
		t.Errorf("sent %d events for a cluster that was never registered", len(*got))
	}
	if emitter.Pending() != 2 {
		t.Errorf("pending = %d, want the two transitions held", emitter.Pending())
	}
}
