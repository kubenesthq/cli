package operation

import (
	"context"
	"fmt"
	"strings"
)

// Complete ends an operation: the live record is marked terminal, and its
// history is copied under the operation's own ID.
//
// The live object is NEVER renamed. In Kubernetes a rename is a delete plus a
// create, and between those two instants the cluster holds no record at all —
// precisely the window in which two laptops each conclude that nothing is
// running and both take the same node down. So the terminal record stays where
// it is, and the copy is a second object, kubenest-operation-<id>.
//
// Marking terminal is what lets the next operation acquire: Acquire replaces a
// terminal record, and refuses a live one.
func (s *Store) Complete(ctx context.Context, h *Handle, result Result) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := s.update(ctx, h, func(r *Record) error {
		r.Terminal = true
		r.Result = string(result)
		// A write the operation still owes is not a failure — the host work is
		// done and only the inventory or recovery-metadata write is missing —
		// but completion must not read as "nothing outstanding" either, so it
		// stays in the record and in the result.
		if out := r.Outstanding(); len(out) > 0 {
			r.Result += "; pending: " + describePending(out)
		}
		return nil
	}); err != nil {
		return err
	}
	return s.copyHistory(ctx, h.rec)
}

// copyHistory writes the finished record's copy. A repeated Complete replaces
// its own copy rather than failing: completion may be retried after a lost
// connection, and the second attempt is the same operation finishing.
func (s *Store) copyHistory(ctx context.Context, rec *Record) error {
	name := HistoryPrefix + rec.OperationID
	existing, err := s.read(ctx, name)
	if err != nil {
		return err
	}
	if existing != nil {
		_, err := s.replace(ctx, name, rec, existing.ResourceVersion)
		return err
	}
	_, err = s.create(ctx, name, rec)
	return err
}

// PendingWrite records a write the operation owes and has not discharged yet.
//
// It exists so that a host that joined but whose inventory write was
// interrupted is identifiable from the record, rather than discovered by the
// next operation when it finds a host nothing knows about.
func (h *Handle) PendingWrite(ctx context.Context, kind, target, detail string) error {
	return h.store.Update(ctx, h, func(r *Record) error {
		for i := range r.Pending {
			if r.Pending[i].Kind == kind && r.Pending[i].Target == target {
				return nil
			}
		}
		r.Pending = append(r.Pending, PendingWrite{
			Kind:   kind,
			Target: target,
			Detail: detail,
			Status: WritePending,
			At:     h.store.now(),
		})
		return nil
	})
}

// PendingWriteDone discharges one. A write the record does not owe is refused
// rather than silently added as done: a ledger that can be satisfied by
// inventing an entry records nothing.
func (h *Handle) PendingWriteDone(ctx context.Context, kind, target string) error {
	return h.store.Update(ctx, h, func(r *Record) error {
		for i := range r.Pending {
			w := &r.Pending[i]
			if w.Kind == kind && w.Target == target {
				if w.Status == WriteDone {
					return nil
				}
				now := h.store.now()
				w.Status = WriteDone
				w.DoneAt = &now
				return nil
			}
		}
		return fmt.Errorf("operation %s owes no %s write for %s", r.OperationID, kind, target)
	})
}

func describePending(writes []PendingWrite) string {
	parts := make([]string, 0, len(writes))
	for _, w := range writes {
		detail := w.Kind + " write for " + w.Target
		if w.Detail != "" {
			detail += " (" + w.Detail + ")"
		}
		parts = append(parts, detail)
	}
	return strings.Join(parts, ", ")
}
