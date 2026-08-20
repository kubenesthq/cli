package converge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// A condition that settles late — transient Error states first, exactly the
// helm-install-traefik shape — must still pass. This is the reason the
// package exists.
func TestLateSettlingConditionPasses(t *testing.T) {
	observations := 0
	probe := func(ctx context.Context) (bool, State, error) {
		observations++
		if observations < 4 {
			return false, State{
				Object: "job helm-install-traefik in kube-system",
				Status: "Error",
				Detail: "back-off restarting failed container",
			}, nil
		}
		return true, State{Object: "job helm-install-traefik in kube-system", Status: "Completed"}, nil
	}

	res, err := Wait(context.Background(), probe, Options{
		Name: "traefik-ready", Deadline: 2 * time.Second, Interval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Outcome != Pass {
		t.Fatalf("outcome = %s, want pass: a condition that settles inside the window passed", res.Outcome)
	}
	if res.Observations < 4 {
		t.Errorf("observations = %d, want at least 4 — a snapshot check would have failed this install", res.Observations)
	}
	if res.Err() != nil {
		t.Errorf("Err() on pass = %v, want nil", res.Err())
	}
}

// A condition that never settles fails AT THE DEADLINE and names the stuck
// object, its state, and the fix-shaped detail.
func TestNeverSettlingConditionFailsAtDeadlineNamingTheObject(t *testing.T) {
	probe := func(ctx context.Context) (bool, State, error) {
		return false, State{
			Object: "pod traefik-7c9f in kube-system",
			Status: "Pending",
			Detail: "no node matches its node selector",
		}, nil
	}

	deadline := 300 * time.Millisecond
	start := time.Now()
	res, err := Wait(context.Background(), probe, Options{
		Name: "traefik-ready", Deadline: deadline, Interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Outcome != Fail {
		t.Fatalf("outcome = %s, want fail", res.Outcome)
	}
	if elapsed := time.Since(start); elapsed < deadline {
		t.Errorf("failed after %s, before the %s deadline — only the deadline may fail a check", elapsed, deadline)
	}

	msg := res.Err().Error()
	for _, want := range []string{"traefik-7c9f", "Pending", "no node matches its node selector"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure %q does not name %q — a failure must name the object, its state and the fix", msg, want)
		}
	}
}

// Progress is printed, not silence: converging events flow to the reporter
// on every observation while the check runs.
func TestProgressIsEmittedThroughoutConverging(t *testing.T) {
	var mu sync.Mutex
	var events []Event
	rep := ReporterFunc(func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	observations := 0
	probe := func(ctx context.Context) (bool, State, error) {
		observations++
		return observations >= 5, State{Object: "node worker-1", Status: "NotReady"}, nil
	}

	_, err := Wait(context.Background(), probe, Options{
		Name: "nodes-ready", Deadline: 2 * time.Second, Interval: time.Millisecond, Reporter: rep,
	})
	if err != nil {
		t.Fatal(err)
	}

	converging := 0
	for _, e := range events {
		if e.Outcome == Converging {
			converging++
		}
	}
	if converging < 4 {
		t.Errorf("saw %d converging events for 4 not-yet observations — converging must be reported, not silent", converging)
	}
	last := events[len(events)-1]
	if last.Outcome != Pass {
		t.Errorf("final event = %s, want the settled outcome", last.Outcome)
	}
}

// A probe error — unreachable API server, refused SSH — is an observation,
// not a verdict. The check keeps converging and can still pass.
func TestProbeErrorsAreTransientNotFatal(t *testing.T) {
	observations := 0
	probe := func(ctx context.Context) (bool, State, error) {
		observations++
		if observations < 3 {
			return false, State{}, errors.New("connection refused: the API server is restarting")
		}
		return true, State{Object: "apiserver", Status: "Ready"}, nil
	}

	res, err := Wait(context.Background(), probe, Options{
		Name: "apiserver-up", Deadline: 2 * time.Second, Interval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.Outcome != Pass {
		t.Fatalf("outcome = %s: a probe error during convergence must not fail a check the deadline would have passed", res.Outcome)
	}
}

// If the probe is erroring when the deadline passes, the failure carries that
// error as the last observed state.
func TestFailureReportsLastErrorObservation(t *testing.T) {
	probe := func(ctx context.Context) (bool, State, error) {
		return false, State{}, errors.New("dial tcp 10.0.1.10:6443: connection refused")
	}
	res, err := Wait(context.Background(), probe, Options{
		Name: "apiserver-up", Deadline: 50 * time.Millisecond, Interval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Fail {
		t.Fatalf("outcome = %s, want fail", res.Outcome)
	}
	if !strings.Contains(res.Err().Error(), "connection refused") {
		t.Errorf("failure %q should carry the last probe error", res.Err())
	}
}

// Aborting is not a verdict: cancellation returns the context error and no
// pretended outcome.
func TestCancellationIsNotAVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	probe := func(ctx context.Context) (bool, State, error) {
		cancel()
		return false, State{Object: "pod x", Status: "Pending"}, nil
	}
	res, err := Wait(ctx, probe, Options{
		Name: "x", Deadline: time.Hour, Interval: time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Outcome == Pass || res.Outcome == Fail {
		t.Errorf("outcome = %s: an aborted check must not pretend a verdict", res.Outcome)
	}
}

// No deadline, no check. Deadlines come from the manifest; a zero value means
// someone skipped it, and that must be loud.
func TestMissingDeadlineRefusesToRun(t *testing.T) {
	_, err := Wait(context.Background(),
		func(ctx context.Context) (bool, State, error) { return true, State{}, nil },
		Options{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "limits.timeouts") {
		t.Errorf("running without a deadline must refuse and point at the manifest, got: %v", err)
	}
}
