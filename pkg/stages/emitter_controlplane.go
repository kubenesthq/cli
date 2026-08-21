package stages

import (
	"context"
	"fmt"
	"sync"
	"time"

	"kubenest.io/cli/pkg/api"
)

// ControlPlaneEmitter publishes stage transitions to the control plane, where
// they become live SSE progress and the server-side journal.
//
// THE QUEUE IS THE POINT. An event needs a cluster to belong to, and the
// cluster id does not exist until the operation registers or resolves one —
// so the transitions before that happen before there is anywhere to send
// them. Dropping them would cost more than a thin trace:
//
//   - the console's progress would begin partway through, so the stages that
//     run before anything is touched would be invisible exactly while an
//     operator is deciding whether to walk away;
//   - and `started` is the ONLY signal that clears install_failed
//     (kubenest-backend 9c19e5e, deliberately sticky). A stage that never
//     emits `started` is a stage whose failure can never be cleared.
//
// So they are queued, with the timestamp of when they actually happened, and
// flushed IN ORDER the moment a cluster id exists. If one never appears — a
// preflight refusal — the queue is discarded, which is correct: nothing was
// written to any machine.
//
// Locking discipline: the mutex guards the queue slice ONLY, and is never
// held across a network call. Every critical section is lock, swap or append,
// unlock — deliberately without `defer`, because deferring to the end of send
// would hold the lock through every HTTP POST and make one slow control plane
// stall the next transition.
type ControlPlaneEmitter struct {
	Client *api.Client
	// ClusterID reports the cluster to attach events to, or "" while the
	// operation does not know it yet. A function rather than a value because
	// the id appears mid-run.
	ClusterID func() string

	mu      sync.Mutex
	pending []api.InstallJournalEntry
}

// NewControlPlaneEmitter builds the emitter. It is a pointer because it holds
// the queue.
func NewControlPlaneEmitter(client *api.Client, clusterID func() string) *ControlPlaneEmitter {
	return &ControlPlaneEmitter{Client: client, ClusterID: clusterID}
}

// Emit posts one transition, queueing it while there is no cluster id.
func (e *ControlPlaneEmitter) Emit(ctx context.Context, ev Event) error {
	if e == nil || e.Client == nil || e.ClusterID == nil {
		return nil
	}
	// Stamped when it HAPPENED, not when it is sent: a queued transition
	// flushed later must not claim to have occurred then.
	now := time.Now().UTC()
	entry := api.InstallJournalEntry{
		Stage:      ev.Stage,
		Component:  ev.Component,
		Status:     api.InstallStageStatus(ev.Status),
		At:         &now,
		ReasonCode: ev.ReasonCode,
		Detail:     Sanitize(ev.Message),
	}

	clusterID := e.ClusterID()
	if clusterID == "" {
		e.mu.Lock()
		e.pending = append(e.pending, entry)
		e.mu.Unlock()
		return nil
	}
	return e.send(ctx, clusterID, entry)
}

// send flushes anything queued, in order, then the new entry. A flush that
// fails part-way leaves the remainder AND the triggering entry queued so the
// next transition retries them — and returns the error, which the engine
// prints without failing the operation.
func (e *ControlPlaneEmitter) send(ctx context.Context, clusterID string, entry api.InstallJournalEntry) error {
	e.mu.Lock()
	queued := e.pending
	e.pending = nil
	e.mu.Unlock()

	requeue := func(from int, err error) error {
		e.mu.Lock()
		remainder := append([]api.InstallJournalEntry{}, queued[from:]...)
		remainder = append(remainder, entry)
		e.pending = append(remainder, e.pending...)
		held := len(e.pending)
		e.mu.Unlock()
		return fmt.Errorf("holding %d stage transition(s) for the next attempt: %w", held, err)
	}

	for i, q := range queued {
		if err := e.Client.ReportInstallStage(ctx, clusterID, q); err != nil {
			return requeue(i, err)
		}
	}
	if err := e.Client.ReportInstallStage(ctx, clusterID, entry); err != nil {
		return requeue(len(queued), err)
	}
	return nil
}

// Pending reports how many transitions are still waiting for a cluster id.
func (e *ControlPlaneEmitter) Pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending)
}
