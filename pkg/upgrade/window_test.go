package upgrade

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/window"
)

func sessionInWindow(t *testing.T, at time.Time) *Session {
	t.Helper()
	w, err := window.Parse(window.Spec{Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		Opts:   Options{Cluster: "prod-1", To: "1.1", Now: func() time.Time { return at }},
		Window: &w,
	}
}

// THE RULE: no new stage starts once the window has closed, but the stage in
// progress finishes. Abandoning a half-completed stage to respect a clock
// leaves the cluster worse than the overrun does.
func TestNoNewStageStartsOutsideTheWindow(t *testing.T) {
	// 2026-08-22 is a Saturday; 07:00 is after the 02:00-06:00 window.
	closed := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	s := sessionInWindow(t, closed)

	err := s.windowStillOpen(StageComponents)
	if err == nil {
		t.Fatal("a stage must not start once the window has closed")
	}
	if !errors.Is(err, stages.ErrPaused) {
		t.Fatalf("a closed window is a PAUSE, not a failure: %v", err)
	}
	for _, want := range []string{"no new stage is starting", "mid-upgrade", "resumes at"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the pause message is missing %q: %v", want, err)
		}
	}
}

// Inside the window everything proceeds.
func TestStagesRunInsideTheWindow(t *testing.T) {
	open := time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC)
	s := sessionInWindow(t, open)
	for _, stage := range StageNames {
		if err := s.windowStillOpen(stage); err != nil {
			t.Errorf("%s must run inside the window: %v", stage, err)
		}
	}
}

// Two stages are exempt by design, and one class is exempt in effect.
func TestWhichStagesAreExemptFromTheWindow(t *testing.T) {
	closed := time.Date(2026, 8, 22, 7, 0, 0, 0, time.UTC)
	s := sessionInWindow(t, closed)

	// The backup is what a rollback depends on; it is never skipped for a
	// clock. Preflight is where the window is checked as a gate at all.
	for _, stage := range []string{StagePreflight, StageBackup} {
		if err := s.windowStillOpen(stage); err != nil {
			t.Errorf("%s must be exempt: %v", stage, err)
		}
	}
	// Pausing between the kubernetes stage and the verification of it would
	// leave a cluster upgraded and unverified until the next window.
	for _, stage := range []string{StageVerify, StageRecord} {
		if err := s.windowStillOpen(stage); err != nil {
			t.Errorf("%s must be exempt: %v", stage, err)
		}
	}
	// The expensive middle is not exempt.
	for _, stage := range []string{StageComponents, StageProfiles, StageAgent, StageKubernetes} {
		if err := s.windowStillOpen(stage); err == nil {
			t.Errorf("%s must not start outside the window", stage)
		}
	}
}

// A cluster with no window configured is always inside it.
func TestNoWindowMeansAnyTime(t *testing.T) {
	s := &Session{Opts: Options{Cluster: "prod-1"}}
	for _, stage := range StageNames {
		if err := s.windowStillOpen(stage); err != nil {
			t.Errorf("%s: %v", stage, err)
		}
	}
}

// A pause ends the sequence without failing it: no terminal journal entry, no
// failed event, and the stage's `started` entry stands as "in progress".
func TestAPauseIsNotAFailure(t *testing.T) {
	j, err := stages.OpenJournal(t.TempDir()+"/u.json", stages.Identity{Kind: Kind, Cluster: "prod-1"})
	if err != nil {
		t.Fatal(err)
	}
	ctl := &pauseController{journal: j}
	sequence := []stages.Stage{
		{Name: StageComponents, Run: func(context.Context) error { return nil }},
		{Name: StageAgent, Run: func(context.Context) error {
			return stages.Paused("the maintenance window has closed")
		}},
		{Name: StageKubernetes, Run: func(context.Context) error {
			t.Error("no stage after the pause may run")
			return nil
		}},
	}

	result, err := stages.Execute(context.Background(), ctl, sequence)
	if err == nil {
		t.Fatal("a pause is reported to the caller")
	}
	if !errors.Is(err, stages.ErrPaused) {
		t.Fatalf("want a pause, got %v", err)
	}
	if result.Paused != StageAgent {
		t.Errorf("paused at %q, want %s", result.Paused, StageAgent)
	}

	var terminal, started int
	for _, e := range j.Entries {
		if e.Stage != StageAgent {
			continue
		}
		switch e.Status {
		case stages.StatusStarted:
			started++
		default:
			terminal++
		}
	}
	if started != 1 {
		t.Errorf("the paused stage has %d started entries, want 1", started)
	}
	if terminal != 0 {
		t.Errorf("the paused stage has %d terminal entries, want 0 — a pause is not a failure and must not mark the cluster failed", terminal)
	}
	for _, e := range ctl.emitted {
		if e.Stage == StageAgent && e.Status == stages.StatusFailed {
			t.Error("a pause must not emit a failed transition: the fleet would see a healthy cluster as install_failed")
		}
	}
	// And it resumes: the stage never completed, so it runs again.
	if _, done := j.Completed(StageAgent); done {
		t.Error("a paused stage must not read as completed")
	}
}

type pauseController struct {
	journal *stages.Journal
	emitted []stages.Event
}

func (c *pauseController) RunID() string            { return "run-1" }
func (c *pauseController) Journal() *stages.Journal { return c.journal }
func (c *pauseController) Emitter() stages.Emitter {
	return stages.EmitterFunc(func(_ context.Context, e stages.Event) error {
		c.emitted = append(c.emitted, e)
		return nil
	})
}
func (c *pauseController) BundleVersion() string { return "1.1" }
func (c *pauseController) ResumeAdvice() string  { return "re-run inside the window" }
func (c *pauseController) Exits() []string       { return exits }
func (c *pauseController) Logf(string, ...any)   {}
func (c *pauseController) TotalDeadline() (time.Duration, error) {
	return time.Hour, nil
}
