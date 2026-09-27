package operation

import (
	"context"
	"fmt"
	"slices"
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
//
// THIS IS THE CLAIM A --resume MAKES, and it is the reason a record whose
// executor died without recording its stop is not resumable: the record says
// `running`, and no observation can turn that into `stopped`. An operator who
// can see the machine is the only one who can say so, and that is
// TakeOverAsserted.
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
	switch rec.Executor.State {
	case ExecutorStopped:
		// What this claim needs, and the only state it continues.
	case ExecutorPaused:
		// A paused executor is between stages BY ITS OWN RECORD: it is alive
		// and coming back, so there is no ambiguity for an assertion to
		// resolve and neither command takes its record (PLAN 7.2, T2.3).
		return nil, fmt.Errorf(
			"%w: operation %s is %s (held by %s, last heartbeat %s). A paused executor is between stages by its own record, so it is alive and coming back and its record is not taken from it; wait for it, or stop it from the machine that started it so its record says stopped",
			ErrTakeOverRefused, opID, rec.Executor.State, holder(rec), rec.Executor.Heartbeat.UTC().Format("2006-01-02T15:04:05Z"))
	default:
		// `running` is ambiguous in a way only a person can settle: a laptop
		// that is asleep and one that is off look identical from here. So the
		// refusal names the one command that lets the operator settle it.
		return nil, fmt.Errorf(
			"%w: operation %s is still %s (held by %s, last heartbeat %s), and this command continues an operation whose record says its executor stopped. An executor that died could not record that, and this CLI cannot see a laptop that is off: if you know the previous executor and its outstanding actions have stopped, assert it — --take-over %s --confirm — which reconciles the recorded actions and continues the operation",
			ErrTakeOverRefused, opID, rec.Executor.State, holder(rec), rec.Executor.Heartbeat.UTC().Format("2006-01-02T15:04:05Z"), opID)
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

// TakeOverAsserted hands an interrupted operation to this executor on the
// operator's explicit assertion that the previous executor and its outstanding
// actions have stopped.
//
// It is what `--take-over <operation-id> --confirm` does, and it exists because
// a record cannot always say what a resume needs. An executor that died — a
// laptop that lost power, a connection that dropped, a reboot that took the
// session with it — never wrote `stopped`, and the only writer of that state is
// `Stop`, which needs the dead executor's own ownership token. Requiring the
// record to say `stopped` therefore locked such a record for good: on hardware
// (2026-09-27, lab w3) a `node reboot` that lost its SSH session left the node
// cordoned and every later command on the cluster refused with "another
// operation holds the record" (kn-yzuv).
//
// THE OPERATOR'S ASSERTION IS THE FACT (PLAN 7.2). The CLI can see a heartbeat
// go stale and it cannot see the difference between a laptop that is asleep and
// one that is off, so it never guesses: the person who can see the machine says
// so, and the record says who said it. A stale record or a network partition
// alone still permits nothing.
//
// WHAT IS ASSERTED IS THE EXECUTOR AND ITS OUTSTANDING ACTIONS, NOT THEIR
// OUTCOMES. The outcomes are the reconciliation's business, and the caller runs
// one (Resume) before this: an action whose postcondition cannot be established
// stops the resume with the reconciliation step named, and so stops the
// take-over before it claims anything. This is why the outstanding actions are
// not re-checked here — the reconcile that just ran is what established them.
//
// A record whose executor is already `stopped` needs no assertion, so it is
// refused and names --resume as the verb that continues it; a paused executor
// is alive by its own record (T2.3); and a terminal record has nothing left to
// take over.
func TakeOverAsserted(ctx context.Context, s *Store, opID string) (*Handle, error) {
	if !validOperationID(opID) {
		return nil, fmt.Errorf("%w: operation id %q is not a usable identity", ErrTakeOverRefused, opID)
	}
	stored, err := s.Find(ctx, opID)
	if err != nil {
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
	switch rec.Executor.State {
	case ExecutorStopped:
		return nil, fmt.Errorf(
			"%w: operation %s records its executor stopped already (held by %s), so there is nothing to assert: --resume %s continues it",
			ErrTakeOverRefused, opID, holder(rec), opID)
	case ExecutorPaused:
		return nil, fmt.Errorf(
			"%w: operation %s is %s (held by %s, last heartbeat %s). A paused executor is between stages by its own record, so it is alive and coming back and its record is not taken from it; wait for it, or stop it from the machine that started it",
			ErrTakeOverRefused, opID, rec.Executor.State, holder(rec), rec.Executor.Heartbeat.UTC().Format("2006-01-02T15:04:05Z"))
	}

	now := s.now()
	next := *rec
	// THE ASSERTION IS WRITTEN WITH THE CLAIM, in the one compare-and-swap that
	// changes hands: a record that changed ownership without saying who said the
	// previous executor was gone would be indistinguishable from theft, and a
	// second write could fail on its own and leave that state behind.
	next.TakeOvers = append(slices.Clone(rec.TakeOvers), TakeOverRecord{
		Operator: s.Operator,
		At:       now,
		Replaced: rec.Executor,
	})
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

// Recovery is how an operator takes up an interrupted operation: `--resume`, or
// `--take-over` with the confirmation that only a person can give.
//
// It exists so that the two flags, their mutual exclusion and the confirmation
// `--take-over` requires live in ONE place rather than in every verb — the
// verbs' flag surfaces differ, and a rule restated per verb drifts (PLAN 7.2:
// "a take-over requires the previous executor and its outstanding actions to be
// stopped").
type Recovery struct {
	// Resume is the `--resume` operation id, or "".
	Resume string
	// TakeOver is the `--take-over` operation id, or "".
	TakeOver string
	// Confirm is `--confirm` alongside `--take-over`: the operator's assertion
	// that the previous executor and its outstanding actions have stopped.
	Confirm bool
}

// Validate refuses the flag combinations before anything is read.
func (r Recovery) Validate() error {
	if r.Resume != "" && r.TakeOver != "" {
		return fmt.Errorf("%w: --resume and --take-over are two ways to take up the same operation and cannot both be given: --resume continues an operation whose record says the previous executor stopped, --take-over asserts that it has", ErrTakeOverRefused)
	}
	if r.TakeOver != "" && !r.Confirm {
		return fmt.Errorf("%w: --take-over requires --confirm. The take-over IS your assertion that the previous executor and its outstanding actions have stopped — the CLI cannot see a laptop that is off or a partition that is still there, so it never makes that assertion for you. Pass --confirm with --take-over %s if that is what you found", ErrTakeOverRefused, r.TakeOver)
	}
	return nil
}

// ID is the operation this recovery takes up, or "" for a new operation.
func (r Recovery) ID() string {
	if r.TakeOver != "" {
		return r.TakeOver
	}
	return r.Resume
}

// IsTakeOver reports whether this is the operator's assertion rather than a
// resume.
func (r Recovery) IsTakeOver() bool { return r.TakeOver != "" }

// Progress is what the verbs print about the reconciliation that ran, in the
// CLI's own words, with the operation named.
func (r Recovery) Progress(plan *Plan) string {
	verb := "Resuming"
	if r.IsTakeOver() {
		verb = "Taking over"
	}
	return fmt.Sprintf("%s operation %s, stopped at %s: %d step(s) established, %d to repeat.",
		verb, r.ID(), plan.Record.Stage, len(plan.Skip()), len(plan.Steps)-len(plan.Skip()))
}

// Claim claims the record for this executor, after the caller has reconciled it
// with Resume.
//
// The assertion is the whole difference: a resume claims a record that already
// says its executor stopped, and a take-over claims one that cannot say it
// because the operator has said it instead.
func (r Recovery) Claim(ctx context.Context, s *Store) (*Handle, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.TakeOver != "" {
		return TakeOverAsserted(ctx, s, r.TakeOver)
	}
	return TakeOver(ctx, s, r.Resume)
}
