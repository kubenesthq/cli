package upgrade

import (
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/window"
)

func hoursAgo(h int) *time.Time {
	t := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC).Add(-time.Duration(h) * time.Hour)
	return &t
}

var now = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

// Only a FRESH PASS permits an upgrade. Rollback partly depends on restore, so
// an untested restore is not a rollback plan.
func TestOnlyAFreshPassedDrillPermitsAnUpgrade(t *testing.T) {
	const maxAge = 336 * time.Hour // the shipped 14 days

	cases := []struct {
		name   string
		drill  DrillStatus
		err    error
		passes bool
		says   string
	}{
		{
			name:   "fresh pass",
			drill:  DrillStatus{Status: "passed", CompletedAt: hoursAgo(48), Backup: "nightly-2026-08-19"},
			passes: true,
			says:   "nightly-2026-08-19",
		},
		{
			name:   "pass older than the manifest threshold",
			drill:  DrillStatus{Status: "passed", CompletedAt: hoursAgo(400), Backup: "old"},
			passes: false,
			says:   "older than",
		},
		{
			name:   "never run",
			drill:  DrillStatus{Status: "never_run"},
			passes: false,
			says:   "has ever completed",
		},
		{
			name:   "absent entirely",
			drill:  DrillStatus{},
			passes: false,
			says:   "has ever completed",
		},
		{
			name:   "failed",
			drill:  DrillStatus{Status: "failed", CompletedAt: hoursAgo(2)},
			passes: false,
			says:   "FAILED",
		},
		{
			name:   "unreadable",
			err:    errTest,
			passes: false,
			says:   "fails closed",
		},
		{
			name:   "an unrecognised status",
			drill:  DrillStatus{Status: "probably_fine", CompletedAt: hoursAgo(1)},
			passes: false,
			says:   "unrecognised",
		},
		{
			name:   "passed but undateable",
			drill:  DrillStatus{Status: "passed"},
			passes: false,
			says:   "age cannot be judged",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkDrill(c.drill, c.err, maxAge, now)
			if got.Passed != c.passes {
				t.Fatalf("passed = %v, want %v (%s)", got.Passed, c.passes, got.Detail)
			}
			haystack := got.Detail + " " + got.Fix
			if !strings.Contains(haystack, c.says) {
				t.Errorf("the verdict does not mention %q: %s", c.says, haystack)
			}
			if !got.Passed && got.Fix == "" {
				t.Error("a failed gate must name a fix")
			}
		})
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "the operator has not reported in" }

// The bundle path gate refuses transitions this cluster cannot make.
func TestBundlePathRefusals(t *testing.T) {
	from := parseManifest(t, `
bundle: "1.0"
ha-tiers: [single-server, ha]
limits: {timeouts: {node-ready: 5m}}
profiles: {observability: {}, ha: {}}
`)
	to := parseManifest(t, `
bundle: "1.1"
ha-tiers: [ha]
limits: {timeouts: {node-ready: 5m}}
profiles: {ha: {}}
`)
	t.Run("a tier the target does not offer", func(t *testing.T) {
		got := checkBundlePath(from, to, nil, "single-server")
		if got.Passed {
			t.Fatal("a bundle that drops this cluster's permanent tier cannot be a target")
		}
		if !strings.Contains(got.Detail, "single-server") {
			t.Errorf("the refusal must name the tier: %s", got.Detail)
		}
	})
	t.Run("a profile the target drops", func(t *testing.T) {
		got := checkBundlePath(from, to, []string{"observability"}, "ha")
		if got.Passed {
			t.Fatal("a bundle that drops an installed profile cannot be a target")
		}
		if !strings.Contains(got.Detail, "observability") {
			t.Errorf("the refusal must name the profile: %s", got.Detail)
		}
	})
	t.Run("already there", func(t *testing.T) {
		if got := checkBundlePath(from, from, nil, "ha"); got.Passed {
			t.Error("upgrading a cluster to the version it already runs is not an upgrade")
		}
	})
	t.Run("a supported transition", func(t *testing.T) {
		if got := checkBundlePath(from, to, nil, "ha"); !got.Passed {
			t.Errorf("want a pass: %s", got.Detail)
		}
	})
}

// Kubernetes does not downgrade, so a bundle whose Kubernetes pin is older
// than the running one cannot be reached by upgrading. Refusing it here is
// the difference between a clear message and system-upgrade-controller being
// asked to do something impossible after the point of no return — which is
// what happened on a real cluster before this gate existed.
func TestABackwardTransitionIsRefused(t *testing.T) {
	newer := parseManifest(t, "bundle: \"1.0\"\ncore: {k3s: v1.35.7+k3s1}\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n")
	older := parseManifest(t, "bundle: \"0.9\"\ncore: {k3s: v1.35.6+k3s1}\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n")

	got := checkBundlePath(newer, older, nil, "single-server")
	if got.Passed {
		t.Fatal("moving to an older Kubernetes version must be refused")
	}
	for _, want := range []string{"older", "does not support downgrading", "rollback"} {
		if !strings.Contains(got.Detail+" "+got.Fix, want) {
			t.Errorf("the refusal is missing %q: %s / %s", want, got.Detail, got.Fix)
		}
	}

	// Forward is fine, and equal Kubernetes with a newer bundle is fine —
	// a bundle may move only its charts.
	if got := checkBundlePath(older, newer, nil, "single-server"); !got.Passed {
		t.Errorf("a forward transition must pass: %s", got.Detail)
	}
	sameK8s := parseManifest(t, "bundle: \"1.1\"\ncore: {k3s: v1.35.7+k3s1}\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n")
	if got := checkBundlePath(newer, sameK8s, nil, "single-server"); !got.Passed {
		t.Errorf("a chart-only bundle move must pass: %s", got.Detail)
	}
}

// Versions are compared numerically: v1.35.10 is newer than v1.35.9, which a
// string comparison gets backwards.
func TestVersionsCompareNumerically(t *testing.T) {
	older := parseManifest(t, "bundle: \"a\"\ncore: {k3s: v1.35.9+k3s1}\nlimits: {timeouts: {node-ready: 5m}}\n")
	newer := parseManifest(t, "bundle: \"b\"\ncore: {k3s: v1.35.10+k3s1}\nlimits: {timeouts: {node-ready: 5m}}\n")
	back, err := movesBackwards(older, newer)
	if err != nil {
		t.Fatal(err)
	}
	if back {
		t.Error("v1.35.9 → v1.35.10 is forward")
	}
	back, err = movesBackwards(newer, older)
	if err != nil {
		t.Fatal(err)
	}
	if !back {
		t.Error("v1.35.10 → v1.35.9 is backward")
	}
}

// A CLUSTER WITH NO WINDOW IS REFUSED, NOT PASSED.
//
// This gate used to return Passed: true for a nil window with the detail "no
// maintenance window is configured for this cluster, so any time is inside it"
// (kn-nqj). One of seven documented gates therefore approved every cluster,
// every time, while an operator who had set a window believed upgrades and
// reboots were confined to it. A missing window has no time inside it, and the
// refusal has to name the command that fixes it.
//
// MUTATION THAT MUST FAIL THIS TEST: restore the nil-passes branch in
// checkWindow. It passes today with that behaviour, which is the whole reason
// this test exists.
func TestCheckWindowRefusesWhenNoWindowIsConfigured(t *testing.T) {
	s := &Session{Opts: Options{Cluster: "prod-1", Now: func() time.Time { return now }}}

	got := checkWindow(s)
	if got.Passed {
		t.Fatal("a cluster with no maintenance window has no time an upgrade may start")
	}
	if !strings.Contains(got.Fix, "kubenest cluster set-window") {
		t.Errorf("the refusal must name the fix, got: %s", got.Fix)
	}
	lowered := strings.ToLower(got.Detail)
	if strings.Contains(lowered, "any time") || strings.Contains(lowered, "inside it") {
		t.Errorf("a missing window must never read as permission: %s", got.Detail)
	}

	// An unreadable window is refused for the same reason and says WHICH of the
	// two it is: a read that failed must not arrive as a window that is absent.
	s.WindowErr = errTest
	unread := checkWindow(s)
	if unread.Passed {
		t.Fatal("a window that could not be read is not a passed check")
	}
	if !strings.Contains(unread.Detail, errTest.Error()) {
		t.Errorf("the refusal must carry the read's own failure: %s", unread.Detail)
	}
	if !strings.Contains(unread.Fix, "kubenest cluster set-window") {
		t.Errorf("the refusal must name the fix, got: %s", unread.Fix)
	}
}

// The refusal names the next opening in LOCAL TIME AND UTC.
//
// Both, because the operator set the window in their own zone while the log,
// the journal and the control plane's record are read in UTC. A refusal that
// names only one of them makes every reader convert in their head, and "the
// window opens at 02:00" and "it is 02:00 now" then mean different things to
// two people looking at the same cluster.
func TestCheckWindowNamesNextOpeningInLocalAndUTC(t *testing.T) {
	// IST is UTC+05:30, so a Saturday 02:00 opening in Asia/Kolkata is Friday
	// 20:30 UTC — the window is one thing and the instant is two renderings.
	w, err := window.Parse(window.Spec{Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{Window: &w, Opts: Options{Cluster: "prod-1", Now: func() time.Time { return now }}}

	got := checkWindow(s)
	if got.Passed {
		t.Fatalf("Friday midday UTC is outside a Saturday window: %s", got.Detail)
	}
	var report GateReport
	report.add(got)
	refusal := report.Err()
	if refusal == nil {
		t.Fatal("a failed gate must refuse the upgrade")
	}
	for _, want := range []string{"02:00 IST", "20:30 UTC"} {
		if !strings.Contains(refusal.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, refusal)
		}
	}
	if !strings.Contains(refusal.Error(), "kubenest cluster set-window") && !strings.Contains(refusal.Error(), "--wait") {
		t.Errorf("the refusal must name what to do about it:\n%s", refusal)
	}

	// Inside the window the same session passes, so the gate is answering about
	// the instant and not merely refusing everything.
	inside := time.Date(2026, 8, 21, 21, 0, 0, 0, time.UTC)
	s.Opts.Now = func() time.Time { return inside }
	if got := checkWindow(s); !got.Passed {
		t.Errorf("02:30 UTC on Saturday is inside a Saturday 02:00-06:00 IST window: %s", got.Detail)
	}
}

// --now bypasses the window and nothing else.
//
// The operator asking to act now has asked to act now; they have not asked to
// act on a degraded cluster mid-upgrade, or to make a transition the target
// bundle does not offer. So the window gate passes and every other gate still
// decides for itself.
func TestNowBypassesOnlyTheWindow(t *testing.T) {
	from := parseManifest(t, `
bundle: "1.0"
ha-tiers: [single-server, ha]
limits: {timeouts: {node-ready: 5m}}
profiles: {ha: {}}
`)
	to := parseManifest(t, `
bundle: "1.1"
ha-tiers: [ha]
limits: {timeouts: {node-ready: 5m}}
profiles: {ha: {}}
`)
	s := &Session{
		From: from, To: to,
		Opts: Options{Cluster: "prod-1", Now: func() time.Time { return now }, BypassWindow: true},
	}

	got := checkWindow(s)
	if !got.Passed {
		t.Fatalf("--now must bypass the window even with no window stored: %s / %s", got.Detail, got.Fix)
	}
	if !strings.Contains(got.Detail, "--now") {
		t.Errorf("the pass must say the window was bypassed by --now rather than that it was satisfied: %s", got.Detail)
	}

	// ONLY the window: the other gates are untouched.
	if other := checkBundlePath(s.From, s.To, nil, "single-server"); other.Passed {
		t.Error("--now must not bypass the bundle-path gate: a tier the target does not offer is still refused")
	}

	// And without --now the same session refuses, so the pass came from the
	// flag and not from something else about the session.
	s.Opts.BypassWindow = false
	if got := checkWindow(s); got.Passed {
		t.Fatal("without --now a cluster with no window must refuse")
	}
}
