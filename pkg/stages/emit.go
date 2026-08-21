package stages

import (
	"context"
	"fmt"
	"io"
)

// Event is one stage transition on the wire: the install_stage_update
// payload (kn-w051), emitted by the CLI, carried by the backend and hub, and
// rendered by the console.
//
// Only started, completed and failed cross the wire. `converging` stays
// inside the stage's converge.Wait loop and is printed locally — a progress
// event per poll is noise, and the UI has StageIndex/StageTotal for a
// progress bar without it.
type Event struct {
	// RunID identifies one installer PROCESS, constant across its stages and
	// new on every resume. It is what separates "resumed after a fix" from
	// "retry storm" without inferring from timestamps.
	RunID         string
	Stage         string
	StageIndex    int
	StageTotal    int
	Component     string
	Status        Status
	BundleVersion string
	// ReasonCode and Message are REQUIRED when Status is failed. Message
	// carries the last observed convergence state verbatim — "pod traefik-x
	// in kube-system is Pending — 0/1 nodes available: insufficient memory".
	// Summarising it turns a fix back into "install failed".
	ReasonCode string
	Message    string
}

// Emitter publishes stage events to the control plane.
//
// An emitter error NEVER fails the install. An install that succeeded on the
// machines but could not POST its telemetry has still succeeded, and failing
// it would make the observability path able to break the thing it observes.
// The engine prints the emitter's error and carries on.
type Emitter interface {
	Emit(ctx context.Context, e Event) error
}

// EmitterFunc adapts a function to Emitter.
type EmitterFunc func(ctx context.Context, e Event) error

// Emit calls f.
func (f EmitterFunc) Emit(ctx context.Context, e Event) error { return f(ctx, e) }

// NopEmitter drops events. It is what an install with no reachable control
// plane would use — which today is no install at all, since stage 2 requires
// one, so this exists for tests.
type NopEmitter struct{}

// Emit does nothing.
func (NopEmitter) Emit(context.Context, Event) error { return nil }

// TextEmitter prints stage transitions for a human watching the terminal.
// It is composed WITH the control-plane emitter, never instead of it: the
// operator running the install and the console watching it see the same
// thirteen transitions.
type TextEmitter struct{ W io.Writer }

// Emit prints one line per transition.
func (t TextEmitter) Emit(_ context.Context, e Event) error {
	if t.W == nil {
		return nil
	}
	prefix := fmt.Sprintf("[%2d/%d] %-21s", e.StageIndex, e.StageTotal, e.Stage)
	switch e.Status {
	case StatusStarted:
		fmt.Fprintf(t.W, "%s ...\n", prefix)
	case StatusCompleted:
		if e.Message != "" {
			fmt.Fprintf(t.W, "%s ok   (%s)\n", prefix, e.Message)
			return nil
		}
		fmt.Fprintf(t.W, "%s ok\n", prefix)
	case StatusFailed:
		fmt.Fprintf(t.W, "%s FAILED: %s\n", prefix, e.Message)
	}
	return nil
}

// Emitters fans one event out to several emitters and returns the first
// error, having called them all. Used to print locally and publish remotely
// from the same transition.
type Emitters []Emitter

// Emit calls every emitter.
func (es Emitters) Emit(ctx context.Context, e Event) error {
	var firstErr error
	for _, em := range es {
		if em == nil {
			continue
		}
		if err := em.Emit(ctx, e); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
