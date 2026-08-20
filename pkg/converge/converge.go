// Package converge is the one way the CLI waits for cluster state.
//
// Every acceptance check and every component installer polls a condition
// until a deadline and reports one of exactly three outcomes:
//
//	pass        — the condition held within its window
//	converging  — not there yet, deadline not reached; progress is printed
//	fail        — the deadline passed; the last observed state and the stuck
//	              object are reported
//
// Never a single sample. This exists because of a real observation (kn-bkwa):
// a clean k3s install puts helm-install-traefik into Error before it retries
// and completes. A snapshot check fails a healthy install and sends the
// operator to debug an installer that was about to succeed. So transient
// states — CrashLoopBackOff, Pending, Error, even a probe that cannot reach
// the API server while it restarts — are just observations until the
// deadline. Only the deadline fails a check.
//
// Deadlines come from the bundle manifest's limits.timeouts (pkg/manifest),
// never from constants in this package. There is no default deadline.
package converge

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Outcome is one of exactly three check results. There is no fourth.
type Outcome string

const (
	// Pass: the condition held within its window.
	Pass Outcome = "pass"
	// Converging: not there yet, deadline not reached. Only ever seen in
	// progress events — a finished check is Pass or Fail.
	Converging Outcome = "converging"
	// Fail: the deadline passed without the condition holding.
	Fail Outcome = "fail"
)

// State is one observation of the thing being waited on. Probes fill it so a
// failure can name the object, its state, and what to do — "traefik is
// Pending, no node matches its node selector" is a fix, "install failed"
// is not.
type State struct {
	// Object names what is being observed, e.g. "pod traefik-7c9f in kube-system".
	Object string
	// Status is its current condition, e.g. "Pending", "CrashLoopBackOff", "2/3 Ready".
	Status string
	// Detail carries the last event or reason — the part that names the fix.
	Detail string
}

func (s State) String() string {
	var b strings.Builder
	if s.Object != "" {
		b.WriteString(s.Object)
	}
	if s.Status != "" {
		if b.Len() > 0 {
			b.WriteString(" is ")
		}
		b.WriteString(s.Status)
	}
	if s.Detail != "" {
		if b.Len() > 0 {
			b.WriteString(" — ")
		}
		b.WriteString(s.Detail)
	}
	if b.Len() == 0 {
		return "no state observed yet"
	}
	return b.String()
}

// Probe observes the condition once. done reports whether the condition
// currently holds; state describes what was seen either way.
//
// A returned error is an OBSERVATION, not a verdict: an unreachable API
// server or a refused SSH connection during an install is transient like any
// other state, and polling continues until the deadline. Probes must not
// try to classify their own failures as fatal — the deadline does that.
type Probe func(ctx context.Context) (done bool, state State, err error)

// Event is one progress report while a check converges, and the final report
// when it settles. Progress is printed, not silence: printing what is still
// converging is what stops an operator killing an install that was going to
// succeed.
type Event struct {
	Check    string
	Outcome  Outcome
	State    State
	Elapsed  time.Duration
	Deadline time.Duration
}

// Reporter receives events. Implementations must be cheap; they are called
// on every poll.
type Reporter interface {
	Report(Event)
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(Event)

func (f ReporterFunc) Report(e Event) { f(e) }

// Options configures one check.
type Options struct {
	// Name identifies the check in progress output, e.g. "nodes-ready".
	Name string
	// Deadline is REQUIRED and comes from the bundle manifest's
	// limits.timeouts via manifest.Timeouts.For. Wait refuses to run
	// without one — a zero deadline would either fail instantly or mean a
	// constant crept in somewhere.
	Deadline time.Duration
	// Interval between observations. Default 5s.
	Interval time.Duration
	// Reporter receives progress. Nil means events are dropped (tests);
	// the CLI passes a printer.
	Reporter Reporter
}

// Result is the settled verdict of one check: Pass or Fail, never Converging.
type Result struct {
	Check   string
	Outcome Outcome
	// Last is the final observation — on Fail, the stuck object and its
	// state.
	Last    State
	Elapsed time.Duration
	// Observations counts how many times the probe ran.
	Observations int
}

// Err returns nil for Pass and a descriptive error for Fail, naming the
// object, its last state, and the deadline that expired.
func (r Result) Err() error {
	if r.Outcome == Pass {
		return nil
	}
	return fmt.Errorf("%s: fail after %s: %s", r.Check, r.Elapsed.Round(time.Second), r.Last)
}

// Wait polls probe until it holds or the deadline passes.
//
// Wait only returns early for context cancellation — the operator aborting —
// in which case it returns the context's error and a Result carrying the last
// observation with no verdict pretended. Everything else, including probe
// errors, waits for the deadline.
func Wait(ctx context.Context, probe Probe, opts Options) (Result, error) {
	if opts.Deadline <= 0 {
		return Result{}, fmt.Errorf("check %q has no deadline: deadlines come from the bundle manifest's limits.timeouts, pass one via manifest.Timeouts.For", opts.Name)
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	start := time.Now()
	deadline := start.Add(opts.Deadline)
	res := Result{Check: opts.Name}

	for {
		done, state, err := probe(ctx)
		res.Observations++
		if err != nil {
			if ctx.Err() != nil {
				// The operator aborted; the probe error is just the abort.
				res.Last = state
				res.Elapsed = time.Since(start)
				return res, ctx.Err()
			}
			// A probe error is an observation like any other: record it and
			// keep converging. The API server restarting mid-install must
			// not fail a check the deadline would have passed.
			if state == (State{}) {
				state = State{Status: "unobservable", Detail: err.Error()}
			} else if state.Detail == "" {
				state.Detail = err.Error()
			}
		}
		res.Last = state
		res.Elapsed = time.Since(start)

		if done {
			res.Outcome = Pass
			report(opts, Event{Check: opts.Name, Outcome: Pass, State: state, Elapsed: res.Elapsed, Deadline: opts.Deadline})
			return res, nil
		}

		if time.Now().After(deadline) {
			res.Outcome = Fail
			report(opts, Event{Check: opts.Name, Outcome: Fail, State: state, Elapsed: res.Elapsed, Deadline: opts.Deadline})
			return res, nil
		}

		report(opts, Event{Check: opts.Name, Outcome: Converging, State: state, Elapsed: res.Elapsed, Deadline: opts.Deadline})

		select {
		case <-ctx.Done():
			res.Elapsed = time.Since(start)
			return res, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func report(opts Options, e Event) {
	if opts.Reporter != nil {
		opts.Reporter.Report(e)
	}
}
