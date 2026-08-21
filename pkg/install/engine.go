package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// Options is the install request — `kubenest platform install`'s flag surface
// resolved into the engine's terms.
type Options struct {
	Bundle        string
	Name          string
	Org           string
	Servers       []string
	Agents        []string
	HATier        string
	Profiles      []string
	SSHUser       string
	SSHKey        string
	StorageDevice string
	BackupTarget  string
}

// Identity is the part of the request a resume must match exactly.
func (o Options) Identity() Identity {
	return Identity{
		Cluster:       o.Name,
		Bundle:        o.Bundle,
		HATier:        o.HATier,
		Servers:       o.Servers,
		Agents:        o.Agents,
		Profiles:      o.Profiles,
		StorageDevice: o.StorageDevice,
	}.Normalized()
}

// NodeRole is what a node is for. It decides which stage touches it and, at
// uninstall, which script removes k3s from it.
type NodeRole string

const (
	RoleServer NodeRole = "server"
	RoleAgent  NodeRole = "agent"
)

// Node is one target host with an open connection to it.
type Node struct {
	Address string
	Role    NodeRole
	Runner  k3s.Runner
}

// Credentials is stage 2's output, opaque to the engine.
//
// The engine can hold it and hand it to stage 10 and to nothing else. It
// cannot inspect it, print it or write it, and the journal has no field that
// could accept it. Keeping it `any` here is the type system carrying the rule
// that key material never leaves this process except onto the target hosts.
type Credentials any

// Session is one install run's state: what the flags asked for, what the
// bundle pins, what the journal already knows, and what earlier stages have
// produced for later ones.
type Session struct {
	// RunID identifies this process across its stages.
	RunID string
	Opts  Options
	// Bundle is the manifest fetched from the control plane. Every version
	// and every deadline comes from here.
	Bundle   *manifest.Manifest
	Journal  *Journal
	Emitter  Emitter
	Reporter converge.Reporter
	// Out receives human-facing narrative that is not a stage transition.
	Out io.Writer

	// Nodes is filled by stage 1 (preflight), which is why preflight always
	// runs: every later stage needs these connections.
	Nodes []Node
	// Creds is filled by stage 2 (register) and consumed by stage 10. In
	// memory only, for the life of this process.
	Creds Credentials

	// Started is when the run began, for the elapsed-time report.
	Started time.Time
}

// Server returns the primary control-plane node — the one that runs kubectl
// and holds the k3s auto-deploy directory.
func (s *Session) Server() (k3s.Runner, error) {
	for _, n := range s.Nodes {
		if n.Role == RoleServer {
			return n.Runner, nil
		}
	}
	return nil, errors.New("no server node connection: stage 1 (preflight) opens these, so this is an engine bug, not a host problem")
}

// NodesWithRole returns every node of one role, in the order given on the
// command line.
func (s *Session) NodesWithRole(role NodeRole) []Node {
	var out []Node
	for _, n := range s.Nodes {
		if n.Role == role {
			out = append(out, n)
		}
	}
	return out
}

// Logf writes narrative to the session's output.
func (s *Session) Logf(format string, args ...any) {
	if s.Out == nil {
		return
	}
	fmt.Fprintf(s.Out, format+"\n", args...)
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
	// PreflightOnly marks the cheap failure: nothing was written to any node
	// and no cluster record exists, so the two-exit advice does not apply.
	PreflightOnly bool
}

func (e *StageError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stage %d/%d %s failed", Index(e.Stage), len(StageNames), e.Stage)
	if e.Component != "" {
		fmt.Fprintf(&b, " installing %s", e.Component)
	}
	fmt.Fprintf(&b, ":\n  %s\n", e.Err)

	if e.PreflightOnly {
		b.WriteString("\nNothing was written to any node and no cluster record was created.\n")
		b.WriteString("Fix the condition above and run the identical command again.\n")
		return b.String()
	}

	// Exactly two supported exits, and the failure message prints both
	// (install.mdx). Completed stages are deliberately left in place:
	// automatic teardown would destroy the evidence.
	fmt.Fprintf(&b, "\nCompleted stages were left in place. The journal is at %s.\n", e.JournalPath)
	b.WriteString("There are two ways on from here:\n")
	b.WriteString("  resume     fix what the error names, then run the identical command again\n")
	b.WriteString("             (completed stages are skipped)\n")
	b.WriteString("  start over kubenest platform uninstall --confirm\n")
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
func Execute(ctx context.Context, s *Session, stages []Stage) (Result, error) {
	if s.Journal == nil {
		return Result{}, errors.New("install session has no journal: resume is deterministic because of the journal, so an install without one is not supported")
	}
	if s.Emitter == nil {
		s.Emitter = NopEmitter{}
	}
	if s.Started.IsZero() {
		s.Started = time.Now()
	}

	total, err := s.Bundle.Limits.Timeouts.For("install-total")
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	var result Result
	bundleVersion := s.Bundle.Bundle

	for _, stage := range stages {
		index := Index(stage.Name)
		event := Event{
			RunID:         s.RunID,
			Stage:         stage.Name,
			StageIndex:    index,
			StageTotal:    len(stages),
			Component:     stage.Component,
			BundleVersion: bundleVersion,
		}

		if at, done := s.Journal.Completed(stage.Name); done && !stage.AlwaysRun {
			// Skipped stages still emit both transitions so the console's
			// progress bar is not a mystery on a resumed run. They are not
			// re-journalled: the stage's completion is already recorded, and
			// a journal that grows an entry per resume records the resuming,
			// not the install.
			skipped := "skipped: completed " + at.Format(time.RFC3339)
			s.emit(ctx, withStatus(event, StatusStarted, "", skipped))
			s.emit(ctx, withStatus(event, StatusCompleted, "", skipped))
			result.Skipped = append(result.Skipped, stage.Name)
			continue
		}

		if stage.Run == nil {
			return result, &StageError{
				Stage:       stage.Name,
				Component:   stage.Component,
				ReasonCode:  ReasonCode(stage.Name),
				Err:         errors.New("this stage is not implemented in this build of the CLI"),
				JournalPath: s.Journal.Path(),
			}
		}

		s.emit(ctx, withStatus(event, StatusStarted, "", ""))
		if err := s.Journal.Append(Entry{
			Stage: stage.Name, Status: StatusStarted, Component: stage.Component, RunID: s.RunID,
		}); err != nil {
			return result, fmt.Errorf("write install journal: %w", err)
		}

		runErr := stage.Run(ctx, s)
		if runErr != nil {
			runErr = annotateDeadline(ctx, stage.Name, total, runErr)
			reason := ReasonCode(stage.Name)
			s.emit(ctx, withStatus(event, StatusFailed, reason, runErr.Error()))
			// The journal write must happen even though the install is
			// failing: the record of WHERE it failed is the whole resume
			// path. A journal error here is reported alongside, not instead.
			if jErr := s.Journal.Append(Entry{
				Stage: stage.Name, Status: StatusFailed, Component: stage.Component,
				Detail: runErr.Error(), RunID: s.RunID,
			}); jErr != nil {
				s.Logf("warning: could not write the install journal: %v", jErr)
			}
			result.Elapsed = time.Since(s.Started)
			return result, &StageError{
				Stage:         stage.Name,
				Component:     stage.Component,
				ReasonCode:    reason,
				Err:           runErr,
				JournalPath:   s.Journal.Path(),
				PreflightOnly: stage.Name == StagePreflight,
			}
		}

		s.emit(ctx, withStatus(event, StatusCompleted, "", ""))
		if err := s.Journal.Append(Entry{
			Stage: stage.Name, Status: StatusCompleted, Component: stage.Component, RunID: s.RunID,
		}); err != nil {
			return result, fmt.Errorf("write install journal: %w", err)
		}
		result.Ran = append(result.Ran, stage.Name)
	}

	result.Elapsed = time.Since(s.Started)
	return result, nil
}

// emit publishes an event, printing but never propagating an emitter error:
// telemetry must not be able to fail the thing it observes.
func (s *Session) emit(ctx context.Context, e Event) {
	if err := s.Emitter.Emit(ctx, e); err != nil {
		s.Logf("warning: could not report stage %q as %s to the control plane: %v", e.Stage, e.Status, err)
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
