package operation

import (
	"context"
	"fmt"
	"time"

	"kubenest.io/cli/pkg/api"
)

// defaultMirrorTimeout bounds one mirror write.
//
// The mirror is display, so a control plane that accepts the connection and
// then does not answer must not hold the operation's write path open: the
// record in the cluster has already been written and the operation has work to
// do. It is generous for a control plane that is merely slow, and short enough
// that a dead one costs a pause rather than a stall.
const defaultMirrorTimeout = 15 * time.Second

// mirror posts the record to the control plane after a successful write to the
// cluster, and keeps the outcome on the handle for the caller to report.
//
// It can never fail the operation, and that is not leniency — it is the design.
// The record in the cluster is the lock and the authority: it is what refuses a
// second operator, and it keeps doing so while the control plane is
// unreachable, which is the case this whole mechanism exists for. The mirror is
// a copy for display and for `check_upgrade`, so a mirror write that fails is
// reported (Handle.MirrorError) and the operation carries on.
//
// Every write of the LIVE record is mirrored: the create or replacement that
// takes the lock, every action recorded before and after it is submitted, every
// heartbeat, and the terminal write. The history object is not, and does not
// need to be: it is the same revision and the same content as the terminal
// write that was just mirrored.
//
// It is called with the handle's lock held (every update) or on a handle no
// other goroutine has seen yet (the acquire), so the outcome is written where
// MirrorError reads it.
func (s *Store) mirror(ctx context.Context, h *Handle, rec *Record) {
	if s.Mirror == nil {
		return
	}
	h.mirrorErr = s.postMirror(ctx, rec)
}

// postMirror is one mirror write, bounded by MirrorTimeout.
func (s *Store) postMirror(ctx context.Context, rec *Record) error {
	if s.MirrorClusterID == "" {
		return fmt.Errorf("operation %s was not mirrored: this CLI does not know the control-plane cluster id, and the mirror is addressed by it (the record's own cluster name is not one)", rec.OperationID)
	}
	ctx, cancel := context.WithTimeout(ctx, s.mirrorTimeout())
	defer cancel()
	if err := s.Mirror.PutClusterOperation(ctx, s.MirrorClusterID, rec.mirrorState()); err != nil {
		return fmt.Errorf("mirroring operation %s to the control plane failed, and the record in the cluster is unaffected: %w", rec.OperationID, err)
	}
	return nil
}

func (s *Store) mirrorTimeout() time.Duration {
	if s.MirrorTimeout > 0 {
		return s.MirrorTimeout
	}
	return defaultMirrorTimeout
}

// mirrorState is one revision of the record as the control plane's mirror takes
// it: the identity, the request's digest rather than the request, the state and
// where it got to.
//
// There is no field for a command, an output or a credential. The record itself
// cannot hold one (Record.Validate), and this is the second place it would
// otherwise leave the cluster.
func (rec *Record) mirrorState() api.ClusterOperationState {
	state := api.ClusterOperationState{
		OperationID:   rec.OperationID,
		RequestDigest: rec.Request.Digest(),
		Terminal:      rec.Terminal,
		State:         string(rec.State()),
		Stage:         rec.Stage,
		ResumeCommand: rec.ResumeCommand(),
		Revision:      rec.Revision,
	}
	if !rec.Executor.Heartbeat.IsZero() {
		heartbeat := rec.Executor.Heartbeat.UTC()
		state.ExecutorHeartbeatAt = &heartbeat
	}
	return state
}

// MirrorError is the last mirror write's failure, or nil when the last one
// succeeded — or when this CLI has no mirror at all, which is not a failure:
// the record stays in the cluster either way.
//
// This is how a mirror that failed is REPORTED without the operation failing.
// A caller that only prints it at the end of an operation sees the last one;
// the mirror is a copy for display, and the next write to the record retries
// it with the revision that then exists.
func (h *Handle) MirrorError() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mirrorErr
}
