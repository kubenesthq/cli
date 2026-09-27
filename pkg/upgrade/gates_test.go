package upgrade

import (
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/manifest"
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
		got := checkBundlePath(from, to, nil, "single-server", testSequence(t))
		if got.Passed {
			t.Fatal("a bundle that drops this cluster's permanent tier cannot be a target")
		}
		if !strings.Contains(got.Detail, "single-server") {
			t.Errorf("the refusal must name the tier: %s", got.Detail)
		}
	})
	t.Run("a profile the target drops", func(t *testing.T) {
		got := checkBundlePath(from, to, []string{"observability"}, "ha", testSequence(t))
		if got.Passed {
			t.Fatal("a bundle that drops an installed profile cannot be a target")
		}
		if !strings.Contains(got.Detail, "observability") {
			t.Errorf("the refusal must name the profile: %s", got.Detail)
		}
	})
	t.Run("already there", func(t *testing.T) {
		if got := checkBundlePath(from, from, nil, "ha", testSequence(t)); got.Passed {
			t.Error("upgrading a cluster to the version it already runs is not an upgrade")
		}
	})
	t.Run("a supported transition", func(t *testing.T) {
		if got := checkBundlePath(from, to, nil, "ha", testSequence(t)); !got.Passed {
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

	got := checkBundlePath(newer, older, nil, "single-server", testSequence(t))
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
	if got := checkBundlePath(older, newer, nil, "single-server", testSequence(t)); !got.Passed {
		t.Errorf("a forward transition must pass: %s", got.Detail)
	}
	sameK8s := parseManifest(t, "bundle: \"1.1\"\ncore: {k3s: v1.35.7+k3s1}\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n")
	if got := checkBundlePath(newer, sameK8s, nil, "single-server", testSequence(t)); !got.Passed {
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
	if other := checkBundlePath(s.From, s.To, nil, "single-server", testSequence(t)); other.Passed {
		t.Error("--now must not bypass the bundle-path gate: a tier the target does not offer is still refused")
	}

	// And without --now the same session refuses, so the pass came from the
	// flag and not from something else about the session.
	s.Opts.BypassWindow = false
	if got := checkWindow(s); got.Passed {
		t.Fatal("without --now a cluster with no window must refuse")
	}
}

// testSequence is the catalog's published sequence, in the shape the gate reads
// it. Tests that pass nil here would exercise the unmeasurable-hop refusal
// rather than the adjacency rule.
func testSequence(t *testing.T) []bundles.Release {
	t.Helper()
	sequence, err := bundles.Sequence()
	if err != nil {
		t.Fatal(err)
	}
	return sequence
}

// syntheticBundle is a manifest with only what a path gate reads: the version,
// a tier it offers, and (when given) the sources it declares.
func syntheticBundle(t *testing.T, body string) *manifest.Manifest {
	t.Helper()
	return parseManifest(t, body)
}

// TWO BUNDLES AT A TIME (decision K). `--to 1.2` from 1.0 is refused because
// 1.1 is a bundle you must step through: the deprecation scan runs against the
// Kubernetes version the named target pins and only that one, so a bundle you
// jump over is never scanned for an API removal it carries. This is S2's first
// planted negative, and it goes through the REAL shipped manifests so it is the
// transition the product offers that is being tested.
func TestCheckBundlePathRefusesATwoBundleHop(t *testing.T) {
	sequence := testSequence(t)
	from, err := bundles.Manifest("1.0")
	if err != nil {
		t.Fatal(err)
	}
	to, err := bundles.Manifest("1.2")
	if err != nil {
		t.Fatal(err)
	}

	got := checkBundlePath(from, to, nil, "single-server", sequence)
	if got.Passed {
		t.Fatal("1.0 -> 1.2 skips 1.1 and must be refused")
	}
	// The Fix names the intermediate bundle AND the command that takes it: a
	// refusal that says "you skipped a bundle" without saying which one is a
	// refusal an operator cannot act on.
	if !strings.Contains(got.Detail, "1.1") {
		t.Errorf("the refusal must name the skipped bundle: %s", got.Detail)
	}
	if !strings.Contains(got.Fix, "1.1") || !strings.Contains(got.Fix, "upgrade --to 1.1") {
		t.Errorf("the fix must name the step and the command that takes it: %s", got.Fix)
	}

	// The positive control. Without it, a gate that refused every hop would pass
	// the assertions above.
	oneHop, err := bundles.Manifest("1.1")
	if err != nil {
		t.Fatal(err)
	}
	if passed := checkBundlePath(oneHop, to, nil, "single-server", sequence); !passed.Passed {
		t.Errorf("1.1 -> 1.2 is one hop and must pass: %s / %s", passed.Detail, passed.Fix)
	}

	// And the UNDECLARED edge, which 1.2 declares and its predecessors do not:
	// 0.9 is not one of the sources 1.2 declares, so this is refused for that
	// reason rather than for distance, and the fix names the first declared
	// source above it.
	oldest, err := bundles.Manifest("0.9")
	if err != nil {
		t.Fatal(err)
	}
	undeclared := checkBundlePath(oldest, to, nil, "single-server", sequence)
	if undeclared.Passed {
		t.Fatal("0.9 is not a declared source for 1.2 and the transition must be refused")
	}
	if !strings.Contains(undeclared.Detail, "declares no upgrade edge from 0.9") {
		t.Errorf("the refusal must be the declared-edge one, not a distance: %s", undeclared.Detail)
	}
	if !strings.Contains(undeclared.Fix, "1.0") {
		t.Errorf("the fix must name the first declared source above 0.9: %s", undeclared.Fix)
	}
}

// THE COUNT COMES FROM THE CATALOG'S ORDER, NOT FROM THE VERSION STRINGS.
// "1.10" is newer than "1.9" and sorts before it as text, so an adjacency check
// built on string order computes the distance backwards: it would pass the
// two-hop 1.9 -> 1.11 below, because in string order the target looks like it
// comes first.
func TestTheAdjacencyCheckUsesCatalogOrderNotStringOrder(t *testing.T) {
	sequence := []bundles.Release{{Version: "1.9"}, {Version: "1.10"}, {Version: "1.11"}}
	manifest := func(version string) *manifest.Manifest {
		return syntheticBundle(t, "bundle: \""+version+"\"\ncore: {k3s: v1.35.8+k3s1}\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n")
	}
	nine, ten, eleven := manifest("1.9"), manifest("1.10"), manifest("1.11")

	// One hop in the catalog, and the hop a string comparison calls backward.
	if got := checkBundlePath(nine, ten, nil, "single-server", sequence); !got.Passed {
		t.Errorf("1.9 -> 1.10 is adjacent in the catalog and must pass: %s / %s", got.Detail, got.Fix)
	}
	// Two hops in the catalog, and a hop a string comparison passes.
	got := checkBundlePath(nine, eleven, nil, "single-server", sequence)
	if got.Passed {
		t.Fatal("1.9 -> 1.11 skips 1.10 and must be refused")
	}
	if !strings.Contains(got.Fix, "1.10") {
		t.Errorf("the fix must name the intermediate bundle: %s", got.Fix)
	}
	// The same two bundles with the sequence the strings would give: the check
	// reads what it is handed, so this is the order the catalog has and not a
	// re-derivation of it.
	stringOrder := []bundles.Release{{Version: "1.10"}, {Version: "1.11"}, {Version: "1.9"}}
	if got := checkBundlePath(nine, ten, nil, "single-server", stringOrder); !got.Passed {
		t.Errorf("the check must walk the sequence it is given: %s", got.Detail)
	}
}

// A SECURITY-ONLY RELEASE DOES NOT CONSUME THE HOP (decision K). A release that
// exists only because a component needed patching was not skipped by the
// operator — we chose to issue it — so 1.0 -> 1.2 is one hop when 1.1 was
// security-only, and two when it was not.
func TestASecurityOnlyReleaseDoesNotConsumeTheHop(t *testing.T) {
	manifest := func(version string) *manifest.Manifest {
		return syntheticBundle(t, "bundle: \""+version+"\"\ncore: {k3s: v1.35.8+k3s1}\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n")
	}
	one, two := manifest("1.0"), manifest("1.2")

	ordinary := []bundles.Release{{Version: "1.0"}, {Version: "1.1"}, {Version: "1.2"}}
	if got := checkBundlePath(one, two, nil, "single-server", ordinary); got.Passed {
		t.Fatal("with 1.1 an ordinary release, 1.0 -> 1.2 is two hops and must be refused")
	}

	securityOnly := []bundles.Release{{Version: "1.0"}, {Version: "1.1", SecurityOnly: true}, {Version: "1.2"}}
	if got := checkBundlePath(one, two, nil, "single-server", securityOnly); !got.Passed {
		t.Errorf("with 1.1 security-only, 1.0 -> 1.2 is one hop and must pass: %s / %s", got.Detail, got.Fix)
	}

	// And the shipped catalog agrees: 1.1 is NOT security-only, so the real hop
	// is the refused one.
	sequence := testSequence(t)
	for _, release := range sequence {
		if release.Version == "1.1" && release.SecurityOnly {
			t.Error("the shipped catalog marks 1.1 security-only: that is a claim about the release, not about the hop")
		}
	}
}

// An unmeasurable hop is refused rather than passed: "I could not tell" is not
// "they are adjacent", the same rule the window gate follows.
func TestAHopTheCatalogCannotMeasureIsRefused(t *testing.T) {
	from := syntheticBundle(t, "bundle: \"1.0\"\ncore: {k3s: v1.35.8+k3s1}\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n")
	to := syntheticBundle(t, "bundle: \"9.9\"\ncore: {k3s: v1.35.8+k3s1}\nha-tiers: [single-server]\nlimits: {timeouts: {node-ready: 5m}}\n")

	got := checkBundlePath(from, to, nil, "single-server", testSequence(t))
	if got.Passed {
		t.Fatal("a target the catalog does not carry cannot be measured and must not pass")
	}
	if !strings.Contains(got.Detail, "9.9") {
		t.Errorf("the refusal must name the bundle it could not place: %s", got.Detail)
	}
}
