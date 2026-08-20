package converge

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// TextReporter prints progress for humans watching an install. While a check
// converges it prints every state CHANGE immediately and a heartbeat line
// otherwise, so long waits are visibly alive without scrolling identical
// lines. Pass and fail always print.
type TextReporter struct {
	W io.Writer
	// Heartbeat is the minimum gap between identical converging lines.
	// Zero means 15s.
	Heartbeat time.Duration

	mu        sync.Mutex
	lastState map[string]State
	lastPrint map[string]time.Time
}

func NewTextReporter(w io.Writer) *TextReporter {
	return &TextReporter{W: w}
}

func (r *TextReporter) Report(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastState == nil {
		r.lastState = map[string]State{}
		r.lastPrint = map[string]time.Time{}
	}
	heartbeat := r.Heartbeat
	if heartbeat == 0 {
		heartbeat = 15 * time.Second
	}

	switch e.Outcome {
	case Pass:
		fmt.Fprintf(r.W, "✓ %s: pass (%s)\n", e.Check, e.Elapsed.Round(time.Second))
	case Fail:
		fmt.Fprintf(r.W, "✗ %s: fail after %s: %s\n", e.Check, e.Elapsed.Round(time.Second), e.State)
	case Converging:
		changed := r.lastState[e.Check] != e.State
		due := time.Since(r.lastPrint[e.Check]) >= heartbeat
		if !changed && !due {
			return
		}
		remaining := (e.Deadline - e.Elapsed).Round(time.Second)
		if remaining < 0 {
			remaining = 0
		}
		fmt.Fprintf(r.W, "… %s: converging — %s (%s left)\n", e.Check, e.State, remaining)
		r.lastPrint[e.Check] = time.Now()
	}
	r.lastState[e.Check] = e.State
}
