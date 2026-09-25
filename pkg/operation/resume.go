package operation

import (
	"context"
	"fmt"
	"strings"
)

// Decision is what a resume does about one action.
type Decision string

const (
	// DecisionSkip: the action is proven done, so it is not submitted again.
	DecisionSkip Decision = "skip"
	// DecisionRepeat: the action is safe to submit again — it is recorded as
	// never submitted, or its recorded attempt failed.
	DecisionRepeat Decision = "repeat"
)

// Step is the resume's decision about one action, with the reason, because a
// decision an operator cannot read is a decision they cannot check.
type Step struct {
	ActionID string
	Stage    string
	Decision Decision
	Reason   string
}

// Observation is one read-only probe the resume ran to establish an action's
// outcome, kept so the operator can see what was consulted.
type Observation struct {
	ActionID string
	Command  string
	ExitCode int
	Err      error
}

// Blocked is an action whose outcome cannot be established. It names the
// reconciliation step rather than leaving the operator to guess.
type Blocked struct {
	ActionID      string
	Stage         string
	Postcondition string
	Observe       string
	// Reconciliation is the step to perform before this action is repeated.
	Reconciliation string
}

// BlockedError is a resume that stopped. It is returned WITH the plan, so the
// operator sees everything the resume established and the one thing it could
// not.
type BlockedError struct {
	OperationID string
	Blocked
}

func (e *BlockedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: operation %s stopped instead of re-submitting action %s (stage %s).\n",
		ErrReconcile, e.OperationID, e.ActionID, e.Stage)
	fmt.Fprintf(&b, "  The record says it was submitted and its postcondition was not established:\n    %s\n", e.Postcondition)
	fmt.Fprintf(&b, "  Reconcile it before continuing — %s", e.Reconciliation)
	return b.String()
}

// Is makes errors.Is(err, ErrReconcile) true.
func (e *BlockedError) Is(target error) bool { return target == ErrReconcile }

// Plan is a resume: the record read, plus what to do about each action.
//
// Resume itself submits NOTHING. It reads the record and runs read-only
// probes, and everything that changes the cluster is left to the re-run that
// follows, with Guarded.Skip set from this plan. That ordering is the whole
// point: "reconcile actual state before repeating any step" (7.2).
type Plan struct {
	OperationID string
	Record      *Record
	// Revision is the resourceVersion the record was read at.
	Revision string
	// Request is the immutable request this resume continues.
	Request  Request
	Terminal bool
	Steps    []Step
	// Observations are the probes the resume ran, in order.
	Observations []Observation
	// Blocked is set when the resume stopped. The error returned alongside is
	// a *BlockedError carrying the same thing.
	Blocked *Blocked
}

// Verify refuses a resume that would continue a DIFFERENT request. Targets and
// versions cannot change: a resume into a half-finished cluster with changed
// arguments is how a cluster ends up not matching its own record.
func (p *Plan) Verify(want Request) error {
	diffs := p.Request.Differences(want)
	if len(diffs) == 0 {
		return nil
	}
	return fmt.Errorf("a resume continues the same immutable request, and this one differs:\n  %s", strings.Join(diffs, "\n  "))
}

// Skip is the set of action identities a re-run must not submit, in the shape
// Guarded.Skip wants.
func (p *Plan) Skip() map[string]bool {
	out := map[string]bool{}
	for _, s := range p.Steps {
		if s.Decision == DecisionSkip {
			out[s.ActionID] = true
		}
	}
	return out
}

// Resume reconciles an interrupted operation and decides what may be repeated.
//
// It finds the record by ID — live or historical, so an operation finished by a
// later one is still resumable-readable — prints nothing itself, and returns
// the plan. Knowing an operation ID does not confer ownership: this only reads.
func Resume(ctx context.Context, s *Store, opID string) (*Plan, error) {
	stored, err := s.Find(ctx, opID)
	if err != nil {
		return nil, err
	}
	rec := stored.Record
	plan := &Plan{
		OperationID: opID,
		Record:      rec,
		Revision:    stored.ResourceVersion,
		Request:     rec.Request,
		Terminal:    rec.Terminal,
	}
	if rec.Terminal {
		// Nothing to repeat. A terminal record is the answer, not a step.
		return plan, nil
	}

	for _, a := range rec.Actions {
		switch a.Status {
		case ActionSucceeded:
			plan.Steps = append(plan.Steps, Step{
				ActionID: a.ID, Stage: a.Stage, Decision: DecisionSkip,
				Reason: "the record says this action succeeded",
			})
		case ActionFailed:
			plan.Steps = append(plan.Steps, Step{
				ActionID: a.ID, Stage: a.Stage, Decision: DecisionRepeat,
				Reason: "the recorded attempt failed; fix what it names and the action is repeated",
			})
		case ActionRecorded:
			plan.Steps = append(plan.Steps, Step{
				ActionID: a.ID, Stage: a.Stage, Decision: DecisionRepeat,
				Reason: "recorded before submission: it was never sent, so repeating it cannot double-apply anything",
			})
		case ActionSubmitted:
			step, blocked, err := plan.reconcile(ctx, s, a)
			if err != nil {
				return plan, err
			}
			if blocked != nil {
				plan.Blocked = blocked
				return plan, &BlockedError{OperationID: opID, Blocked: *blocked}
			}
			plan.Steps = append(plan.Steps, step)
		default:
			// A status this build does not understand — a record written by a
			// newer CLI, or a hand-edited one. Whether the action was
			// submitted is exactly what is unknown, so this stops like any
			// other unestablished outcome rather than guessing.
			blocked := &Blocked{
				ActionID: a.ID, Stage: a.Stage, Postcondition: a.Postcondition, Observe: a.Observe,
				Reconciliation: fmt.Sprintf(
					"stage %s: the record holds the status %q for this action, which this build does not understand, so establish %q by hand before repeating anything",
					a.Stage, a.Status, a.Postcondition),
			}
			plan.Blocked = blocked
			return plan, &BlockedError{OperationID: opID, Blocked: *blocked}
		}
	}
	return plan, nil
}

// reconcile establishes the outcome of an action that was submitted and never
// marked. It never re-submits and it never guesses: the postcondition is
// checked by observation, and anything short of a clean "it holds" stops the
// resume with the reconciliation step named.
func (p *Plan) reconcile(ctx context.Context, s *Store, a Action) (Step, *Blocked, error) {
	step := Step{ActionID: a.ID, Stage: a.Stage}
	if a.Observe == "" {
		return step, &Blocked{
			ActionID: a.ID, Stage: a.Stage, Postcondition: a.Postcondition,
			Reconciliation: fmt.Sprintf(
				"the record holds no read-only command that can establish this, so inspect the cluster state for stage %s and establish %q before repeating anything",
				a.Stage, a.Postcondition),
		}, nil
	}
	res, err := s.Runner.Run(ctx, a.Observe)
	p.Observations = append(p.Observations, Observation{
		ActionID: a.ID, Command: a.Observe, ExitCode: res.ExitCode, Err: err,
	})
	if err == nil && res.ExitCode == 0 {
		step.Decision = DecisionSkip
		step.Reason = fmt.Sprintf("submitted, and the recorded postcondition holds (%q)", a.Postcondition)
		return step, nil, nil
	}
	// A non-zero exit is deliberately not read as "it did not happen": a probe
	// that could not reach the cluster and a probe that looked and found
	// nothing look identical from here, and re-submitting an action that did
	// happen is the mistake this whole path exists to avoid.
	why := fmt.Sprintf("exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	if err != nil {
		why = err.Error()
	}
	return step, &Blocked{
		ActionID: a.ID, Stage: a.Stage, Postcondition: a.Postcondition, Observe: a.Observe,
		Reconciliation: fmt.Sprintf(
			"stage %s: establish %q by hand — `%s` exiting 0 — because the recorded probe did not establish it (%s) and re-submitting the action would be a guess",
			a.Stage, a.Postcondition, a.Observe, why),
	}, nil
}

// TakeOver hands an interrupted operation to this executor.
//
// It is deliberately the narrowest thing that can work. The record's ownership
// does not fence an SSH session or a Plan the old executor already submitted,
// so a take-over requires the previous executor to be STOPPED and its
// outstanding actions to be reconciled — both asserted in the record before
// anything is written. A stale heartbeat is not either of those things: the CLI
// can see a heartbeat stop but it cannot see a laptop that is asleep, and a
// network partition is a reason to do nothing rather than a reason to proceed.
// Elapsed time never releases ownership.
func TakeOver(ctx context.Context, s *Store, opID string) (*Handle, error) {
	if !validOperationID(opID) {
		return nil, fmt.Errorf("%w: operation id %q is not a usable identity", ErrTakeOverRefused, opID)
	}
	stored, err := s.Find(ctx, opID)
	if err != nil {
		// A record that cannot be read is a record that cannot be reasoned
		// about, so this is a refusal and not a retry.
		return nil, fmt.Errorf("%w: the record for %s could not be read (%v), and a record this executor cannot see never permits a take-over", ErrTakeOverRefused, opID, err)
	}
	rec := stored.Record
	if rec.Terminal {
		return nil, fmt.Errorf("%w: operation %s is terminal (%s), so nothing is left to take over", ErrTakeOverRefused, opID, rec.Result)
	}
	live, err := s.Current(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: the live record could not be read (%v)", ErrTakeOverRefused, err)
	}
	if live == nil || live.Record.OperationID != opID {
		return nil, fmt.Errorf("%w: operation %s has no live record — it was finished or a later operation replaced it", ErrTakeOverRefused, opID)
	}
	if rec.Executor.State != ExecutorStopped {
		return nil, fmt.Errorf(
			"%w: operation %s is still %s (held by %s, last heartbeat %s). A take-over needs the previous executor stopped and its actions reconciled; a stale record or a network partition alone never permits one",
			ErrTakeOverRefused, opID, rec.Executor.State, holder(rec), rec.Executor.Heartbeat.UTC().Format("2006-01-02T15:04:05Z"))
	}
	for _, a := range rec.Actions {
		if a.Status == ActionRecorded || a.Status == ActionSubmitted {
			return nil, fmt.Errorf(
				"%w: action %s in stage %s is %s and its outcome has not been reconciled. A take-over does not fence a Plan or an SSH session the old executor already submitted, so reconcile it first (--resume %s names the step)",
				ErrTakeOverRefused, a.ID, a.Stage, a.Status, opID)
		}
	}

	now := s.now()
	next := *rec
	next.Executor = Executor{
		Token:     NewToken(),
		Operator:  s.Operator,
		State:     ExecutorRunning,
		Heartbeat: now,
	}
	next.UpdatedAt = now
	rv, err := s.replace(ctx, Name, &next, live.ResourceVersion)
	if err != nil {
		return nil, fmt.Errorf("%w: claiming the record: %v", ErrTakeOverRefused, err)
	}
	next.Revision = rv
	return &Handle{store: s, rec: &next, rv: rv, token: next.Executor.Token}, nil
}
