package stages

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Controller is what the engine needs from whichever operation it is
// driving. Both install.Session and upgrade.Session satisfy it, which is how
// one engine runs two sequences without knowing anything about either.
type Controller interface {
	// RunID identifies one process across its stages, new on every resume.
	RunID() string
	// Journal is the durable record. The engine writes every transition to
	// it before and after running a stage.
	Journal() *Journal
	// Emitter publishes transitions. Never nil — use NopEmitter.
	Emitter() Emitter
	// BundleVersion is carried on every event.
	BundleVersion() string
	// TotalDeadline bounds the whole sequence, and comes from the bundle
	// manifest's limits.timeouts rather than from a constant.
	TotalDeadline() (time.Duration, error)
	// Logf writes narrative that is not a stage transition.
	Logf(format string, args ...any)
	// Exits are the supported ways on from a failure, in the operation's own
	// words — an install offers resume or uninstall, an upgrade offers
	// resume or rollback. Every failure message prints them, because a
	// failure that does not say what to do next is the thing this whole
	// engine exists to avoid.
	Exits() []string
}

// Result is what a completed run reports.
type Result struct {
	// Elapsed is wall-clock for the whole run. install.mdx budgets fifteen
	// minutes for both reference shapes; the budget is asserted by the
	// release tests, not enforced here, because a run that takes twenty
	// minutes has failed its budget and succeeded at its job.
	Elapsed time.Duration
	// Skipped names the stages resume did not re-run.
	Skipped []string
	// Ran names the stages this process executed.
	Ran []string
}

// StageError is a failed stage: which stage, which component, what to do
// next. Those three things are the entire difference between an installer and
// a thing that prints "error".
type StageError struct {
	Stage       string
	Component   string
	ReasonCode  string
	Err         error
	JournalPath string
	// PreflightOnly marks the cheap failure — the first stage, which by
	// convention in both operations writes nothing anywhere, so the
	// two-exit advice does not apply.
	PreflightOnly bool
	// Exits are the supported ways on from a failure, printed in order.
	// They differ per operation: an install offers resume or uninstall, an
	// upgrade offers resume or rollback.
	Exits []string
}

func (e *StageError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stage %s failed", e.Stage)
	if e.Component != "" {
		fmt.Fprintf(&b, " installing %s", e.Component)
	}
	fmt.Fprintf(&b, ":\n  %s\n", e.Err)

	if e.PreflightOnly {
		b.WriteString("\nNothing was written to any node and no cluster record was created.\n")
		b.WriteString("Fix the condition above and run the identical command again.\n")
		return b.String()
	}

	// The supported exits are printed, always. Completed stages are
	// deliberately left in place: automatic teardown would destroy the
	// evidence needed to diagnose the failure and can itself fail.
	fmt.Fprintf(&b, "\nCompleted stages were left in place. The journal is at %s.\n", e.JournalPath)
	if len(e.Exits) > 0 {
		b.WriteString("There are two ways on from here:\n")
		for _, exit := range e.Exits {
			fmt.Fprintf(&b, "  %s\n", exit)
		}
	}
	return b.String()
}

// Unwrap exposes the underlying failure to errors.Is / errors.As.
func (e *StageError) Unwrap() error { return e.Err }

// Execute runs the stages in order, journalling every transition, skipping
// what a previous run completed, and stopping at the first failure.
//
// The whole run is bounded by the bundle's limits.timeouts.install-total.
// That deadline is NOT the fifteen-minute budget: the budget is a target the
// release tests assert and an overrun is a defect to fix, while the deadline
// is when an install that is going nowhere gives up and says which stage was
// still running. Collapsing them would mean either a target that asserts
// nothing or an install that aborts on a slow image pull.
func Execute(ctx context.Context, c Controller, sequence []Stage) (Result, error) {
	journal := c.Journal()
	if journal == nil {
		return Result{}, errors.New("this operation has no journal: resume is deterministic because of the journal, so running without one is not supported")
	}
	emitter := c.Emitter()
	if emitter == nil {
		emitter = NopEmitter{}
	}
	started := time.Now()

	total, err := c.TotalDeadline()
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	var result Result
	bundleVersion := c.BundleVersion()

	for _, stage := range sequence {
		index := Index(sequence, stage.Name)
		event := Event{
			RunID:         c.RunID(),
			Stage:         stage.Name,
			StageIndex:    index,
			StageTotal:    len(sequence),
			Component:     stage.Component,
			BundleVersion: bundleVersion,
		}

		if at, done := journal.Completed(stage.Name); done && !stage.AlwaysRun {
			// Skipped stages still emit both transitions so the console's
			// progress bar is not a mystery on a resumed run. They are not
			// re-journalled: the stage's completion is already recorded, and
			// a journal that grows an entry per resume records the resuming,
			// not the install.
			skipped := "skipped: completed " + at.Format(time.RFC3339)
			emit(ctx, c, emitter, withStatus(event, StatusStarted, "", skipped))
			emit(ctx, c, emitter, withStatus(event, StatusCompleted, "", skipped))
			result.Skipped = append(result.Skipped, stage.Name)
			continue
		}

		if stage.Run == nil {
			return result, &StageError{
				Stage:       stage.Name,
				Component:   stage.Component,
				ReasonCode:  ReasonCode(stage.Name),
				Err:         errors.New("this stage is not implemented in this build of the CLI"),
				JournalPath: journal.Path(),
			}
		}

		emit(ctx, c, emitter, withStatus(event, StatusStarted, "", ""))
		if err := journal.Append(Entry{
			Stage: stage.Name, Status: StatusStarted, Component: stage.Component, RunID: c.RunID(),
		}); err != nil {
			return result, fmt.Errorf("write install journal: %w", err)
		}

		runErr := stage.Run(ctx)
		if runErr != nil {
			runErr = annotateDeadline(ctx, stage.Name, total, runErr)
			reason := ReasonCode(stage.Name)
			// WHICH component failed, not which one the stage happens to
			// list first. platform-networking installs the Gateway API CRDs
			// and Traefik; platform-day2 installs system-upgrade-controller
			// and kured. The failing call tags its own error, and that tag
			// wins over the stage's declared component for the failed
			// event and the failed journal entry alike.
			if actual := ComponentOf(runErr); actual != "" {
				event.Component = actual
				stage.Component = actual
			}
			// Everything that leaves this process — the wire event and the
			// journal — is sanitized. The text comes from component
			// installers and remote shells, so "no secrets, no raw command
			// output" (install_journal_entry.json) has to be enforced at the
			// sink, not trusted at the source.
			detail := Sanitize(runErr.Error())
			emit(ctx, c, emitter, withStatus(event, StatusFailed, reason, detail))
			// The journal write must happen even though the install is
			// failing: the record of WHERE it failed is the whole resume
			// path. A journal error here is reported alongside, not instead.
			if jErr := journal.Append(Entry{
				Stage: stage.Name, Status: StatusFailed, Component: stage.Component,
				Detail: detail, RunID: c.RunID(),
			}); jErr != nil {
				c.Logf("warning: could not write the journal: %v", jErr)
			}
			result.Elapsed = time.Since(started)
			return result, &StageError{
				Stage:         stage.Name,
				Component:     stage.Component,
				ReasonCode:    reason,
				Err:           runErr,
				JournalPath:   journal.Path(),
				PreflightOnly: index == 1,
				Exits:         c.Exits(),
			}
		}

		emit(ctx, c, emitter, withStatus(event, StatusCompleted, "", ""))
		if err := journal.Append(Entry{
			Stage: stage.Name, Status: StatusCompleted, Component: stage.Component, RunID: c.RunID(),
		}); err != nil {
			return result, fmt.Errorf("write install journal: %w", err)
		}
		result.Ran = append(result.Ran, stage.Name)
	}

	result.Elapsed = time.Since(started)
	return result, nil
}

// emit publishes an event, printing but never propagating an emitter error:
// telemetry must not be able to fail the thing it observes.
func emit(ctx context.Context, c Controller, e Emitter, ev Event) {
	if err := e.Emit(ctx, ev); err != nil {
		c.Logf("warning: could not report stage %q as %s to the control plane: %v", ev.Stage, ev.Status, err)
	}
}

func withStatus(e Event, status Status, reason, message string) Event {
	e.Status = status
	e.ReasonCode = reason
	e.Message = message
	return e
}

// annotateDeadline turns the generic context error into the sentence the
// deadline exists to produce: which stage was still running when the whole
// install ran out of time.
func annotateDeadline(ctx context.Context, stage string, total time.Duration, err error) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("the install-total deadline of %s expired while stage %s was still running (last state: %w)", total, stage, err)
}

// NewRunID mints the identifier for one process: constant across
// its stages, new on every resume. It is what separates "resumed after a fix"
// from "retry storm" in the record without inferring from timestamps.
//
// Random hex rather than a UUID library: the CLI cross-compiles for six
// platforms and a dependency for sixteen bytes of randomness is not worth it.
func NewRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a time-based fallback keeps
		// an install from dying over a run label.
		return "run-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}
