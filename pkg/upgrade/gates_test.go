package upgrade

import (
	"strings"
	"testing"
	"time"
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
