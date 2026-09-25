package operation

import (
	"context"
	"fmt"
	"io"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

// Spec is what a successor needs in order to tell whether one remote action
// happened. It is written down BEFORE the action is submitted, because after
// the submission the only thing left is a guess.
type Spec struct {
	// Kind is the shape of the action: an SSH step, a Plan, a restore.
	Kind ActionKind
	// Postcondition is the sentence a successor reads. It is not decoration:
	// it is what a resume reports when the outcome cannot be established, and
	// what tells an operator which reconciliation step to perform.
	Postcondition string
	// Observe is a read-only command that exits zero iff the postcondition
	// holds. Empty means the outcome cannot be established by observation,
	// which makes a resume stop and name the reconciliation step instead of
	// re-submitting.
	Observe string
}

// Specs names the actions among the commands an operation submits.
//
// It returns false for everything that is not an action. Reads are not
// actions: their postcondition is the read itself, and recording every
// observation would fill the record with things no successor needs and could
// not safely skip.
type Specs func(stage, command string) (Spec, bool)

// Guarded is a k3s.Runner decorator: every action it submits is written to the
// operation record first, with a stable identity and an observable
// postcondition, and marked when it returns.
//
// This is how an SSH step and the upgrade's Plan apply are covered without
// every call site remembering to journal (PLAN 7.2). It is a decorator rather
// than a journal call at each site for the same reason the SSH transport is an
// interface and not an optional capability: a guarantee that depends on every
// call site remembering is not a guarantee.
type Guarded struct {
	// Inner is the real transport.
	Inner k3s.Runner
	// Op is the operation this run belongs to.
	Op *Handle
	// Stage names the stage the actions belong to, in the operation's words.
	Stage string
	// Specs decides what is an action and what its postcondition is.
	Specs Specs
	// Skip names the action identities this process must not submit because a
	// predecessor already did.
	//
	// It comes from a resume plan and NEVER from the record's own history. The
	// same command may legitimately run twice inside one operation — a
	// per-node step is the obvious case — and skipping on history alone would
	// silently drop the second one.
	Skip map[string]bool
}

// Run submits one remote command.
func (g *Guarded) Run(ctx context.Context, command string) (sshx.Result, error) {
	return g.submit(ctx, command, nil)
}

// RunInput submits one remote command with stdin streamed.
func (g *Guarded) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	return g.submit(ctx, command, stdin)
}

func (g *Guarded) submit(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	call := func() (sshx.Result, error) {
		if stdin != nil {
			return g.Inner.RunInput(ctx, command, stdin)
		}
		return g.Inner.Run(ctx, command)
	}

	if g.Specs == nil {
		return call()
	}
	spec, isAction := g.Specs(g.Stage, command)
	if !isAction {
		return call()
	}
	if g.Op == nil {
		return sshx.Result{}, fmt.Errorf("refusing to submit an action with no operation record: a cluster change nothing can resume must not happen")
	}

	id := ActionID(g.Stage, command)
	if g.Skip[id] {
		// The predecessor already carried this action out, so it is not
		// submitted again and there is no output to return: what the record
		// promises is the action's EFFECT, not its output.
		return sshx.Result{}, nil
	}
	if spec.Postcondition == "" {
		return sshx.Result{}, fmt.Errorf("refusing to submit an action with no postcondition: a successor could not tell whether it happened")
	}

	if err := g.Op.RecordAction(ctx, id, g.Stage, spec); err != nil {
		// Fail closed. The record is both the lock and the resume path; an
		// action that could not be written down first must not reach the
		// cluster, because nothing after this process dies would know it was
		// sent.
		return sshx.Result{}, fmt.Errorf("this action was not submitted: it could not be recorded first: %w", err)
	}
	if err := g.Op.SubmitAction(ctx, id); err != nil {
		return sshx.Result{}, fmt.Errorf("this action was not submitted: the record could not be told it was about to be: %w", err)
	}

	res, runErr := call()
	if err := g.Op.FinishAction(ctx, id, res, runErr); err != nil {
		// The action WAS submitted and the outcome could not be written. The
		// record still says "submitted", which is exactly the uncertain state
		// a resume reconciles — so this is reported alongside the action's own
		// result rather than replacing it.
		outcome := "the action returned"
		if runErr != nil {
			outcome = runErr.Error()
		} else if res.ExitCode != 0 {
			outcome = fmt.Sprintf("the action exited %d", res.ExitCode)
		}
		return res, fmt.Errorf("%s; the outcome could not be written to the operation record, which still says it was submitted: %w", outcome, err)
	}
	return res, runErr
}

// RecordAction writes one action down before it is submitted.
//
// An action of the same identity that is already in the record is kept, not
// duplicated: the record is the progress of ONE operation, and a resumed
// repeat of an action is the same action tried again, so its status restarts
// at recorded.
func (h *Handle) RecordAction(ctx context.Context, id, stage string, spec Spec) error {
	if spec.Postcondition == "" {
		return fmt.Errorf("an action with no postcondition cannot be resumed, so it is not recorded")
	}
	return h.store.Update(ctx, h, func(r *Record) error {
		now := h.store.now()
		for i := range r.Actions {
			a := &r.Actions[i]
			if a.ID != id {
				continue
			}
			a.Stage = stage
			a.Kind = spec.Kind
			a.Postcondition = spec.Postcondition
			a.Observe = spec.Observe
			a.Status = ActionRecorded
			a.SubmittedAt = nil
			a.FinishedAt = nil
			a.Detail = ""
			a.At = now
			return nil
		}
		r.Actions = append(r.Actions, Action{
			ID:            id,
			Stage:         stage,
			Kind:          spec.Kind,
			Postcondition: spec.Postcondition,
			Observe:       spec.Observe,
			Status:        ActionRecorded,
			At:            now,
		})
		return nil
	})
}

// SubmitAction marks the instant an action is handed to the cluster. The write
// that means "it may have happened" is separate from the write that means "I am
// about to make it happen", because a successor must be able to tell those two
// apart: the first is safe to repeat, the second is not.
func (h *Handle) SubmitAction(ctx context.Context, id string) error {
	return h.store.Update(ctx, h, func(r *Record) error {
		if _, ok := r.ActionByID(id); !ok {
			return fmt.Errorf("refusing to submit action %s: it is not in the record", id)
		}
		now := h.store.now()
		for i := range r.Actions {
			if r.Actions[i].ID != id {
				continue
			}
			r.Actions[i].Status = ActionSubmitted
			r.Actions[i].SubmittedAt = &now
		}
		return nil
	})
}

// FinishAction marks an action's outcome. Detail is sanitized: it comes from a
// remote shell, and the record is copied off-cluster and mirrored to the
// control plane.
func (h *Handle) FinishAction(ctx context.Context, id string, res sshx.Result, runErr error) error {
	return h.store.Update(ctx, h, func(r *Record) error {
		found := false
		for i := range r.Actions {
			a := &r.Actions[i]
			if a.ID != id {
				continue
			}
			found = true
			now := h.store.now()
			a.FinishedAt = &now
			if runErr != nil {
				a.Status = ActionFailed
				a.Detail = stages.Sanitize(runErr.Error())
				continue
			}
			if res.ExitCode != 0 {
				a.Status = ActionFailed
				a.Detail = stages.Sanitize(fmt.Sprintf("exit %d: %s", res.ExitCode, firstLine(res.Stderr)))
				continue
			}
			a.Status = ActionSucceeded
		}
		if !found {
			return fmt.Errorf("refusing to record the outcome of action %s: it is not in the record", id)
		}
		return nil
	})
}
