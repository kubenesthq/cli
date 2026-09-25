package upgrade

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/window"
)

// fakeClock is a clock the session's sleeper moves. A --wait test cannot use
// the real one: the wait is up to a week long, and a test that slept through it
// would not be run at all.
type fakeClock struct {
	at    time.Time
	slept time.Duration
	// steps records every sleep the wait asked for, so a test can tell one long
	// sleep from the bounded steps the loop actually takes.
	steps []time.Duration
}

func (c *fakeClock) now() time.Time { return c.at }

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.slept += d
	c.steps = append(c.steps, d)
	c.at = c.at.Add(d)
	return nil
}

// waitingSession is a session an hour before its Saturday 02:00-06:00 UTC
// window opens, with a fake clock and a recording runner.
func waitingSession(t *testing.T) (*Session, *fakeClock, *componenttest.FakeRunner) {
	t.Helper()
	// Saturday 2026-08-22, 01:00 UTC: the window opens at 02:00.
	clock := &fakeClock{at: time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC)}
	w, err := window.Parse(window.Spec{Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &componenttest.FakeRunner{}
	s := &Session{
		Opts:   Options{Cluster: "prod-1", To: "1.1", Now: clock.now, Sleep: clock.sleep},
		Window: &w,
		Nodes:  []Node{{Address: "10.0.0.1", Server: true, Runner: runner}},
		// 1.0 -> 1.1 is the transition the catalog serves; never invent one.
		From: parseManifest(t, "bundle: \"1.0\"\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n"),
		To:   parseManifest(t, "bundle: \"1.1\"\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n"),
	}
	return s, clock, runner
}

// --wait HOLDS NOTHING WHILE IT WAITS (PLAN 7.2).
//
// "Nothing" is measurable: no command reaches the cluster, so no node
// connection is used for anything and no operation record exists. That is what
// makes an interrupted wait simply re-runnable, and what leaves a second
// operator free to work while this one waits.
//
// MUTATION THAT MUST FAIL THIS TEST: acquire the operation record (or run any
// cluster command) before the wait. The runner's command list is then non-empty
// at the moment the window opens.
func TestWaitForWindowHoldsNothingUntilTheWindowOpens(t *testing.T) {
	s, clock, runner := waitingSession(t)

	var out bytes.Buffer
	if err := s.WaitForWindow(context.Background(), &out); err != nil {
		t.Fatal(err)
	}

	if cmds := runner.Commands(); len(cmds) != 0 {
		t.Errorf("the wait held the cluster: it ran %v", cmds)
	}
	if !s.Window.Contains(clock.now()) {
		t.Fatalf("the wait returned at %s, still outside the window", clock.now())
	}
	if clock.slept <= 0 {
		t.Fatal("the wait returned without waiting through a closed window")
	}
	for _, want := range []string{"inside", "UTC", "next opens"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the wait's narrative is missing %q:\n%s", want, out.String())
		}
	}

	// And every gate can now decide for itself, which is what re-running them at
	// the moment of action is for.
	if got := checkWindow(s); !got.Passed {
		t.Errorf("inside the window the gate must pass: %s / %s", got.Detail, got.Fix)
	}
}

// A cluster with no window has no opening to wait for, and waiting for one that
// does not exist is how "any time" comes back in through the --wait path.
func TestWaitForWindowRefusesWithoutAWindow(t *testing.T) {
	s := &Session{Opts: Options{Cluster: "prod-1"}}
	err := s.WaitForWindow(context.Background(), nil)
	if err == nil {
		t.Fatal("--wait with no stored window must refuse rather than wait for nothing")
	}
	if !strings.Contains(err.Error(), "kubenest cluster set-window") {
		t.Errorf("the refusal must name the fix, got: %v", err)
	}

	s.WindowErr = errTest
	if err := s.WaitForWindow(context.Background(), nil); err == nil || !strings.Contains(err.Error(), errTest.Error()) {
		t.Errorf("an unreadable window must refuse naming the read's failure, got: %v", err)
	}
}

// THE LOCK IS TAKEN AFTER THE WINDOW OPENS, NEVER BEFORE.
//
// The record being the lock means a run that took it and then waited would hold
// the whole cluster for hours while it held nothing but a clock — the exact
// opposite of what --wait is for. So the first command that reaches the cluster
// must be issued at or after the opening, and the record it writes must name
// this upgrade.
func TestTheOperationLockIsTakenOnlyAfterTheWindowOpens(t *testing.T) {
	s, clock, runner := waitingSession(t)
	opensAt := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)

	var firstCommandAt time.Time
	runner.Respond = func(command string) (sshx.Result, error) {
		if firstCommandAt.IsZero() {
			firstCommandAt = clock.now()
		}
		if strings.Contains(command, "get configmap") {
			// The lock is its absence: no record exists yet.
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "kubenest-operation" not found`}, nil
		}
		return sshx.Result{Stdout: `{"metadata":{"resourceVersion":"7"}}`}, nil
	}

	var out bytes.Buffer
	if err := s.WaitForWindow(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if cmds := runner.Commands(); len(cmds) != 0 {
		t.Fatalf("the wait touched the cluster before the window opened: %v", cmds)
	}

	_, handle, err := s.LockOperation(context.Background())
	if err != nil {
		t.Fatalf("taking the operation lock: %v", err)
	}
	if firstCommandAt.IsZero() {
		t.Fatal("taking the lock issued no command, so the test proved nothing")
	}
	if firstCommandAt.Before(opensAt) {
		t.Errorf("the record was written at %s, before the window opened at %s: --wait held the cluster while it waited",
			firstCommandAt.Format(time.RFC3339), opensAt.Format(time.RFC3339))
	}
	rec := handle.Record()
	if rec.Request.Kind != operation.KindUpgrade {
		t.Errorf("the record's kind is %q, want the upgrade kind: an upgrade record must not be adoptable by a restore", rec.Request.Kind)
	}
	if rec.Request.Cluster != "prod-1" {
		t.Errorf("the record names cluster %q, want the cluster being upgraded", rec.Request.Cluster)
	}
	if got := rec.Request.Versions["bundle"]; got != "1.0 -> 1.1" {
		t.Errorf("the record does not name the bundle transition: %q", got)
	}
}
