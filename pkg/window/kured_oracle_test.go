package window_test

// kured's own window code as the oracle (bead kn-t34-window-semantics-kured-s-2rlu,
// plan 7.4 item 7).
//
// THE WINDOW IS INTERPRETED IN THREE PLACES — this package, the control plane's
// validator (kubenest-backend/app/services/maintenance_window.py) and kured —
// and a window they do not all represent identically means something other than
// what the operator set: they configure Saturday 22:00-04:00 and the cluster
// reboots somewhere else, silently. So every minute of a week, and every minute
// of the two local days a zone changes its clock, is put to kured's own code and
// to this package, and the two must answer the same. The specs the two cannot
// agree on are REFUSED at set-window (pkg/window/window.go::crossingRefusal),
// naming the supported alternatives rather than widening the window in silence.
//
// WHERE THE ORACLE COMES FROM — the vendored path, and why. The bead's first
// choice was to require the module, and it does not work in this repository:
//
//  1. `go get github.com/kubereboot/kured@v1.23.0` cannot be satisfied at all.
//     Upstream tags carry no `v` prefix: `git ls-remote --tags
//     https://github.com/kubereboot/kured` lists refs/tags/1.23.0 and no
//     refs/tags/v1.23.0, and the module proxy answers `not found:
//     github.com/kubereboot/kured@v1.23.0: invalid version: unknown revision
//     v1.23.0`. Even `@1.23.0` resolves to the pseudo-version
//     v0.0.0-20260630112732-ef3c90b2bd58 (commit ef3c90b2, tag 1.23.0), so the
//     pin would not be a tag.
//
//  2. Requiring the module rewrites this module's Go version. Measured in a
//     scratch module declared `go 1.25.0`:
//
//	$ go get github.com/kubereboot/kured@1.23.0
//	go: upgraded go 1.25.0 => 1.26.4
//	go: added github.com/kubereboot/kured v0.0.0-20260630112732-ef3c90b2bd58
//
//     kured's own go.mod at that tag declares `go 1.26.4` and kubenest-cli/go.mod
//     declares `go 1.25.0`, and this repository's CI installs Go FROM go.mod
//     (.github/workflows/ci.yml and build.yml: `go-version-file: go.mod`), so
//     requiring the module moves the toolchain of every build and every CI
//     image to 1.26.4+.
//
// So the oracle is kured's own code VENDORED at kubenest-cli/internal/kuredoracle/
// (module, tag, commit, file paths and digests in that package's provenance.go),
// with the upstream bytes unmodified — TestVendoredOracleMatchesUpstreamTag
// below hashes them against recorded digests, so the oracle cannot drift from
// kured 1.23.0 unnoticed. Reimplementing the semantics from memory is not an
// option: an oracle that is not kured's code proves nothing about kured.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	timewindow "kubenest.io/cli/internal/kuredoracle"
	"kubenest.io/cli/pkg/window"
)

// updateFixture regenerates the shared cross-language fixture. Go test binaries
// are the only thing that builds kured's code here, so the fixture is written
// from this side and consumed by kubenest-backend's test; see
// TestWindowSemanticsFixtureMatchesTheOracle.
var updateFixture = flag.Bool("update", false,
	"rewrite kubenest-contracts/testdata/window-semantics.json from kured's answers")

// The week is sampled from a Sunday 00:00 in the spec's own zone: the weekday
// the installer's default window (Sunday 06:00-09:00 UTC) opens on, and a week
// that contains no clock change in any zone in the table.
const (
	weekStart   = "2026-08-16"
	weekMinutes = 7 * 24 * 60
)

// Europe/Berlin changes its clock in 2026 on these two days: on the first,
// local 02:00-02:59 does not exist; on the second, local 02:00-02:59 happens
// twice. They are walked as INSTANTS (25 hours of consecutive minutes from
// local midnight), which is what samples the hour that is skipped or repeated —
// walking wall-clock times would miss both by construction.
const (
	dstZone    = "Europe/Berlin"
	dstSpring  = "2026-03-29"
	dstAutumn  = "2026-10-25"
	dstMinutes = 25 * 60
)

// everyDayCLI is the whole week in the spelling the flags and the control plane
// store, in the order the control plane canonicalises to (mon..sun).
var everyDayCLI = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// The refusals the three interpreters require. A row names the one it must be
// refused by, so the test can assert the refusal NAMES BOTH FIXES for each shape
// rather than only that something failed.
const (
	ruleCrossingWithoutSevenDays = "crossing-midnight-without-all-seven-days"
	ruleBoundaryInChangeoverHour = "boundary-in-the-clock-changeover-hour"
)

// oracleCase is one window put to both implementations.
type oracleCase struct {
	name string
	spec window.Spec
	// refusalRule is empty when the three interpreters agree on this window, and
	// otherwise names the rule that must refuse it.
	refusalRule string
	// witness is an instant in the window's zone where the CLI and the oracle
	// DISAGREE once that refusal is removed — the divergence the refusal exists
	// to prevent, named so the planted negative reports it rather than a
	// generic mismatch.
	witness string
	// dst samples the two Europe/Berlin change days in addition to the week.
	dst bool
}

func oracleCases() []oracleCase {
	return []oracleCase{
		{
			name: "the default window: sun 06:00-09:00 UTC",
			spec: window.Spec{Days: []string{"sun"}, Start: "06:00", End: "09:00", Timezone: "UTC"},
		},
		{
			name: "a single day in a zone with no DST: mon 02:00-06:00 Asia/Kolkata",
			spec: window.Spec{Days: []string{"mon"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata"},
		},
		{
			name: "a single day across both minute boundaries: sat 00:00-23:59 UTC",
			spec: window.Spec{Days: []string{"sat"}, Start: "00:00", End: "23:59", Timezone: "UTC"},
		},
		{
			name: "a crossing window, all seven days, no DST: 22:00-04:00 Asia/Kolkata",
			spec: window.Spec{Days: everyDayCLI, Start: "22:00", End: "04:00", Timezone: "Asia/Kolkata"},
		},
		{
			name: "a crossing window, all seven days, UTC: 22:00-04:00",
			spec: window.Spec{Days: everyDayCLI, Start: "22:00", End: "04:00", Timezone: "UTC"},
		},
		{
			// kured tests the weekday of the INSTANT, so it opens this window on
			// Saturday morning too ("it opened Friday"), and closes it Saturday
			// night; this package belongs it to the day it opened, so it is open
			// Sunday morning instead. Sunday 02:00 is where the two answers differ.
			name:        "a crossing window naming one day is refused: sat 22:00-04:00 UTC",
			spec:        window.Spec{Days: []string{"sat"}, Start: "22:00", End: "04:00", Timezone: "UTC"},
			refusalRule: ruleCrossingWithoutSevenDays,
			witness:     "2026-08-23 02:00",
		},
		{
			name:        "a crossing window naming six days is refused: mon-sat 22:00-04:00 UTC",
			spec:        window.Spec{Days: []string{"mon", "tue", "wed", "thu", "fri", "sat"}, Start: "22:00", End: "04:00", Timezone: "UTC"},
			refusalRule: ruleCrossingWithoutSevenDays,
			witness:     "2026-08-23 02:00",
		},
		{
			// Spans the spring change (which the window's start and end sit
			// outside) and the autumn one (whose repeated hour it contains).
			name: "Europe/Berlin, a window spanning both changes: sun 01:00-05:00",
			spec: window.Spec{Days: []string{"sun"}, Start: "01:00", End: "05:00", Timezone: dstZone},
			dst:  true,
		},
		{
			name: "Europe/Berlin, a crossing window over both changes: all seven days 22:00-04:00",
			spec: window.Spec{Days: everyDayCLI, Start: "22:00", End: "04:00", Timezone: dstZone},
			dst:  true,
		},
		{
			// The start falls in the local hour Berlin skips (2026-03-29) and
			// repeats (2026-10-25), so it does not name one instant: kured is
			// shut at 03:00-03:29 CEST on the spring day and at the CEST
			// occurrence of 02:30-02:59 on the autumn one, while this package's
			// wall-clock rule is open at both.
			name:        "Europe/Berlin, the START in the changeover hour is refused: sun 02:30-05:00",
			spec:        window.Spec{Days: []string{"sun"}, Start: "02:30", End: "05:00", Timezone: dstZone},
			refusalRule: ruleBoundaryInChangeoverHour,
			witness:     "2026-03-29 03:00",
			dst:         true,
		},
		{
			// The end falls in the same hour, the other way round: on the spring
			// day kured's normalised end keeps the window OPEN at 03:00-03:29
			// CEST, which this package's rule has already closed.
			name:        "Europe/Berlin, the END in the changeover hour is refused: sun 01:00-02:30",
			spec:        window.Spec{Days: []string{"sun"}, Start: "01:00", End: "02:30", Timezone: dstZone},
			refusalRule: ruleBoundaryInChangeoverHour,
			witness:     "2026-03-29 03:00",
			dst:         true,
		},
	}
}

// oracleFor builds kured's own TimeWindow. A spec the CLI accepted that kured
// refuses fails the test: the oracle is not allowed to be the lenient side.
//
// The seven-day rows are handed to kured in kured's own spelling of the whole
// week (EveryDay), so neither side is asked to parse the other's day names.
func oracleFor(t *testing.T, row oracleCase) *timewindow.TimeWindow {
	t.Helper()
	days := row.spec.Days
	if len(days) == 7 {
		days = timewindow.EveryDay
	}
	tw, err := timewindow.New(days, row.spec.Start, row.spec.End, row.spec.Timezone)
	if err != nil {
		t.Fatalf("kured refuses %+v: %v", row.spec, err)
	}
	return tw
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("timezone %q does not load: %v", name, err)
	}
	return loc
}

// minuteRun is `count` consecutive instants, one minute apart.
func minuteRun(start time.Time, count int) []time.Time {
	out := make([]time.Time, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, start.Add(time.Duration(i)*time.Minute))
	}
	return out
}

// weekInstants is every minute of the sampled week, from Sunday 00:00 in the
// spec's own zone to the next Sunday 00:00.
func weekInstants(t *testing.T, spec window.Spec) []time.Time {
	t.Helper()
	return minuteRun(at(t, mustLocation(t, spec.Timezone), weekStart+" 00:00"), weekMinutes)
}

// dayInstants is every minute of one local day, walked as instants from local
// midnight.
func dayInstants(t *testing.T, zone, day string) []time.Time {
	t.Helper()
	return minuteRun(at(t, mustLocation(t, zone), day+" 00:00"), dstMinutes)
}

// divergence is one instant the two implementations answer differently on.
type divergence struct {
	at         time.Time
	cli, kured bool
}

// firstDivergences returns up to three disagreements, so a failure names the
// instant, the spec and both answers rather than only that a count was wrong.
func firstDivergences(w window.Window, tw *timewindow.TimeWindow, instants []time.Time) []divergence {
	var found []divergence
	for _, instant := range instants {
		cli, kured := w.Contains(instant), tw.Contains(instant)
		if cli == kured {
			continue
		}
		found = append(found, divergence{at: instant, cli: cli, kured: kured})
		if len(found) == 3 {
			break
		}
	}
	return found
}

func renderDivergences(spec window.Spec, found []divergence) string {
	var b strings.Builder
	for _, d := range found {
		fmt.Fprintf(&b, "\n  %s: kured=%v, window.Contains=%v",
			d.at.In(mustLocationOrUTC(spec.Timezone)).Format("Mon 2006-01-02 15:04 MST"), d.kured, d.cli)
	}
	return b.String()
}

func mustLocationOrUTC(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// refusalNamesBothFixes is the rule this task requires of every refusal: the
// operator is told the two shapes that ARE representable, not only that theirs is
// not. Each refusal has its own pair of fixes, so each is asserted by name.
func refusalNamesBothFixes(err error, row oracleCase) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch row.refusalRule {
	case ruleCrossingWithoutSevenDays:
		return strings.Contains(msg, "all seven days") && strings.Contains(msg, "do not cross midnight")
	case ruleBoundaryInChangeoverHour:
		return strings.Contains(msg, "changeover hour") && strings.Contains(msg, "UTC or a zone without daylight saving")
	}
	return false
}

// TestAgreesWithKuredEveryMinuteOfTheWeek is the positive criterion of this
// task: for every accepted spec, kured's own window code and this package answer
// the same at every minute of a week. The rows that name fewer than all seven
// days of a crossing window must be REFUSED, and if the refusal is removed the
// failure names the instant the two diverge at.
func TestAgreesWithKuredEveryMinuteOfTheWeek(t *testing.T) {
	for _, row := range oracleCases() {
		t.Run(row.name, func(t *testing.T) {
			parsed, err := window.Parse(row.spec)
			if row.refusalRule != "" {
				if err == nil {
					reportAcceptedButDivergent(t, row, parsed)
					return
				}
				if !refusalNamesBothFixes(err, row) {
					t.Errorf("the refusal must name both supported shapes for %s:\n%v", row.refusalRule, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("this package must represent a window kured accepts: %v", err)
			}

			tw := oracleFor(t, row)
			instants := weekInstants(t, row.spec)
			if found := firstDivergences(parsed, tw, instants); len(found) > 0 {
				t.Errorf("kured and pkg/window disagree for %s %s-%s %s (kured=%q); the first disagreements are:%s",
					strings.Join(row.spec.Days, ","), row.spec.Start, row.spec.End, row.spec.Timezone,
					tw.String(), renderDivergences(row.spec, found))
			}

			// What set-window SENDS is the canonical form, and the validator on
			// the other side must accept it back unchanged.
			round, err := window.Parse(parsed.Spec())
			if err != nil {
				t.Errorf("the canonical stored form of %s is refused by this package: %v", row.name, err)
			} else if !sameSpec(round.Spec(), parsed.Spec()) {
				t.Errorf("the canonical form of %s is not stable: %+v then %+v", row.name, parsed.Spec(), round.Spec())
			}
		})
	}
}

// reportAcceptedButDivergent is where the planted negative of this task lands:
// the window that must be REFUSED was accepted, so the divergence the refusal
// exists to prevent is now observable. It names the witness instant first — and
// for a window boundary in a zone's changeover hour it also walks the two change
// days, so the failure names the instants measured against kured
// (2026-03-29 03:00 CEST and 2026-10-25 02:30 CEST for Europe/Berlin) rather
// than a generic mismatch.
func reportAcceptedButDivergent(t *testing.T, row oracleCase, parsed window.Window) {
	t.Helper()
	tw := oracleFor(t, row)
	witness := at(t, parsed.Location, row.witness)
	if cli, kured := parsed.Contains(witness), tw.Contains(witness); cli != kured {
		t.Errorf("pkg/window and kured disagree at %s (%s): kured says the window is %s, this package says it is %s — the divergence the %s refusal exists to prevent",
			witness.Format("Mon 2006-01-02 15:04 MST"), row.witness, insideWord(kured), insideWord(cli), row.refusalRule)
	} else {
		t.Errorf("Parse accepted %s, which must be refused (%s), but the divergence is not at %s: both answer %v there",
			row.name, row.refusalRule, row.witness, cli)
	}
	if found := firstDivergences(parsed, tw, weekInstants(t, row.spec)); len(found) > 0 {
		t.Errorf("Parse accepted %s, which must be refused (%s); over the sampled week the two first disagree at:%s",
			row.name, row.refusalRule, renderDivergences(row.spec, found))
	}
	if row.dst {
		for _, day := range []string{dstSpring, dstAutumn} {
			if found := firstDivergences(parsed, tw, dayInstants(t, row.spec.Timezone, day)); len(found) > 0 {
				t.Errorf("Parse accepted %s, which must be refused (%s); on %s the two first disagree at:%s",
					row.name, row.refusalRule, day, renderDivergences(row.spec, found))
			}
		}
	}
}

func insideWord(inside bool) string {
	if inside {
		return "OPEN (inside)"
	}
	return "SHUT (outside)"
}

// sameSpec compares two stored specs field by field (the day list is a slice,
// so the struct is not comparable with ==).
func sameSpec(a, b window.Spec) bool {
	if a.Start != b.Start || a.End != b.End || a.Timezone != b.Timezone || len(a.Days) != len(b.Days) {
		return false
	}
	for i := range a.Days {
		if a.Days[i] != b.Days[i] {
			return false
		}
	}
	return true
}

// TestAgreesWithKuredAcrossDSTChangeDays samples the two days Europe/Berlin
// changes its clock in 2026, minute by minute over each whole local day.
//
// Both implementations build a day's start and end with time.Date, which
// normalises a local time that does not exist forward by the offset change, so
// the skipped and repeated minutes have to be walked explicitly. The test
// asserts that the walk SAW the change (a local minute skipped on one day and
// repeated on the other) as well as that the two answer the same: a table with
// no change-day row would otherwise pass without looking at a change day at all.
func TestAgreesWithKuredAcrossDSTChangeDays(t *testing.T) {
	rows, skipped, repeated := 0, 0, 0
	for _, row := range oracleCases() {
		if !row.dst {
			continue
		}
		parsed, err := window.Parse(row.spec)
		if row.refusalRule != "" {
			// A boundary in the changeover hour is refused, and this is the test
			// that walks the very days the refusal exists for: if the rule goes,
			// this says so here rather than leaving the divergence to be found on
			// a cluster.
			if err == nil {
				t.Errorf("%s must be refused (%s): kured and this package disagree on the change days it names", row.name, row.refusalRule)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s must be representable: %v", row.name, err)
		}
		rows++
		tw := oracleFor(t, row)
		for _, day := range []string{dstSpring, dstAutumn} {
			instants := dayInstants(t, row.spec.Timezone, day)
			if found := firstDivergences(parsed, tw, instants); len(found) > 0 {
				t.Errorf("kured and pkg/window disagree on the %s day of %s (kured=%q); the first disagreements are:%s",
					day, row.name, tw.String(), renderDivergences(row.spec, found))
			}
			s, r := localClockChanges(instants)
			skipped, repeated = skipped+s, repeated+r
		}
	}
	if rows == 0 {
		t.Fatal("no row samples a DST change day: with the Europe/Berlin rows gone this test would pass without looking at one")
	}
	if skipped == 0 {
		t.Fatal("no local minute was SKIPPED in the sampled days: the spring change day was not walked as instants")
	}
	if repeated == 0 {
		t.Fatal("no local minute was REPEATED in the sampled days: the autumn change day was not walked as instants")
	}
}

// localClockChanges counts the zone-offset changes the sampled instants crossed:
// forward for the local minutes a zone skips, backward for the ones it repeats.
func localClockChanges(instants []time.Time) (skipped, repeated int) {
	for i := 1; i < len(instants); i++ {
		_, before := instants[i-1].Zone()
		_, after := instants[i].Zone()
		switch {
		case after > before:
			skipped++
		case after < before:
			repeated++
		}
	}
	return skipped, repeated
}

// TestCrossingWindowIsRefusedUnlessAllSevenDays is the refusal the operator
// meets at set-window. A crossing window is the common shape and stays
// supported; what is refused is the shape the three interpreters read
// differently — fewer than seven days — and the refusal names both fixes.
func TestCrossingWindowIsRefusedUnlessAllSevenDays(t *testing.T) {
	partial := map[string][]string{
		"one day":            {"sat"},
		"the weekend":        {"sat", "sun"},
		"six days":           {"mon", "tue", "wed", "thu", "fri", "sat"},
		"a day repeated":     {"sat", "sat", "sun"},
		"the same in Berlin": {"sun"},
	}
	for name, days := range partial {
		zone := "UTC"
		if name == "the same in Berlin" {
			zone = dstZone
		}
		_, err := window.Parse(window.Spec{Days: days, Start: "22:00", End: "04:00", Timezone: zone})
		if err == nil {
			t.Errorf("a crossing window naming %s (%v) must be refused: kured opens it on every day it lists and this package only on the day it opened", name, days)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "all seven days") || !strings.Contains(msg, "do not cross midnight") {
			t.Errorf("the refusal for %s must name both supported shapes:\n%v", name, msg)
		}
	}

	// The supported shapes stay supported: all seven days crossing midnight,
	// and every window that does not cross it.
	if _, err := window.Parse(window.Spec{Days: everyDayCLI, Start: "22:00", End: "04:00", Timezone: "UTC"}); err != nil {
		t.Errorf("a crossing window naming all seven days must be accepted: %v", err)
	}
	if _, err := window.Parse(window.Spec{Days: []string{"sat"}, Start: "22:00", End: "23:59", Timezone: "UTC"}); err != nil {
		t.Errorf("a window that does not cross midnight is unaffected by this refusal: %v", err)
	}
}

// TestAWindowStartingOrEndingInADSTChangeoverHourIsRefused is the second refusal
// the oracle comparison forces. A wall clock inside the hour a zone skips or
// repeats does not name one instant: kured normalises a skipped boundary forward
// and chooses one of the two instants a repeated one names, while this package
// reads wall-clock minutes. MEASURED against kured 1.23.0 for Europe/Berlin
// `sun 02:30-05:00`: shut at 03:00-03:29 CEST on 2026-03-29 and at the CEST
// occurrence of 02:30-02:59 on 2026-10-25 where this package is open; and for the
// other way round (`sun 01:00-02:30`, end in the skipped hour) kured is OPEN at
// 03:00-03:29 CEST where this package is shut.
//
// THE PLANTED NEGATIVE: remove the changeoverRefusal call from Parse and this
// test fails on the refusals, while TestAgreesWithKuredEveryMinuteOfTheWeek
// names the divergent instants above.
func TestAWindowStartingOrEndingInADSTChangeoverHourIsRefused(t *testing.T) {
	refused := []struct {
		name       string
		start, end string
		offending  string
	}{
		{"the start is inside the hour the zone skips and repeats", "02:30", "05:00", "02:30"},
		{"the start is exactly on it", "02:00", "05:00", "02:00"},
		{"the end is inside it", "01:00", "02:30", "02:30"},
		{"the end is exactly on it", "00:00", "02:00", "02:00"},
	}
	for _, c := range refused {
		_, err := window.Parse(window.Spec{Days: []string{"sun"}, Start: c.start, End: c.end, Timezone: dstZone})
		if err == nil {
			t.Errorf("a window whose boundary %s-%s falls in the hour %s changes its clock in must be refused", c.start, c.end, dstZone)
			continue
		}
		// The refusal names the boundary that cannot be honoured AND both shapes
		// that can, because the operator has to be able to write a window that
		// works.
		for _, want := range []string{"changeover hour", "UTC or a zone without daylight saving", c.offending} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal must name %q:\n%v", want, err)
			}
		}
	}

	// NARROW ON PURPOSE. A window whose own boundaries name one instant each is
	// accepted even when the change falls between them, and a zone that never
	// changes its clock is never refused.
	for _, spec := range []window.Spec{
		{Days: []string{"sun"}, Start: "01:00", End: "05:00", Timezone: dstZone},
		{Days: everyDayCLI, Start: "22:00", End: "04:00", Timezone: dstZone},
		{Days: []string{"sun"}, Start: "02:30", End: "05:00", Timezone: "UTC"},
		{Days: []string{"sun"}, Start: "02:30", End: "05:00", Timezone: "Asia/Kolkata"},
	} {
		if _, err := window.Parse(spec); err != nil {
			t.Errorf("%s %s-%s %s must stay accepted: %v", strings.Join(spec.Days, ","), spec.Start, spec.End, spec.Timezone, err)
		}
	}
}

// TestEndMinuteIsInsideTheWindow pins the boundary rule against kured's own
// code: kured builds the day's end as HH:MM:00.999999999 and tests
// `loctime.Before(end)`, so on the whole-minute instants a window is made of,
// the end minute is INSIDE and the minute after it is not.
//
// THE PLANTED NEGATIVE: restore `minutes < w.End` in Window.Contains and this
// test fails at the end minute, as does the every-minute agreement test (whose
// sampled week walks every end minute of every listed day).
func TestEndMinuteIsInsideTheWindow(t *testing.T) {
	cases := []struct {
		name      string
		spec      window.Spec
		start     string
		end       string
		afterEnd  string
		beforeBeg string
	}{
		{
			name:      "the default window",
			spec:      window.Spec{Days: []string{"sun"}, Start: "06:00", End: "09:00", Timezone: "UTC"},
			start:     "2026-08-23 06:00",
			end:       "2026-08-23 09:00",
			afterEnd:  "2026-08-23 09:01",
			beforeBeg: "2026-08-23 05:59",
		},
		{
			name:      "the morning end of a crossing window",
			spec:      window.Spec{Days: everyDayCLI, Start: "22:00", End: "04:00", Timezone: "UTC"},
			start:     "2026-08-23 22:00",
			end:       "2026-08-24 04:00",
			afterEnd:  "2026-08-24 04:01",
			beforeBeg: "2026-08-24 21:59",
		},
		{
			name:      "a single day in a zone with no DST",
			spec:      window.Spec{Days: []string{"mon"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata"},
			start:     "2026-08-17 02:00",
			end:       "2026-08-17 06:00",
			afterEnd:  "2026-08-17 06:01",
			beforeBeg: "2026-08-17 01:59",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := mustParse(t, c.spec)
			tw := oracleFor(t, oracleCase{name: c.name, spec: c.spec})
			loc := mustLocation(t, c.spec.Timezone)

			instantAt := func(value string) time.Time { return at(t, loc, value) }
			if !w.Contains(instantAt(c.start)) || !tw.Contains(instantAt(c.start)) {
				t.Errorf("the start minute %s must be inside: window=%v, kured=%v", c.start, w.Contains(instantAt(c.start)), tw.Contains(instantAt(c.start)))
			}
			if !w.Contains(instantAt(c.end)) {
				t.Errorf("the END MINUTE %s must be inside: kured answers %v and this package must answer the same", c.end, tw.Contains(instantAt(c.end)))
			}
			if !tw.Contains(instantAt(c.end)) {
				t.Fatalf("kured itself says %s is outside: the oracle and the expectation disagree", c.end)
			}
			if w.Contains(instantAt(c.afterEnd)) || tw.Contains(instantAt(c.afterEnd)) {
				t.Errorf("the minute after the end (%s) must be outside both: window=%v, kured=%v", c.afterEnd, w.Contains(instantAt(c.afterEnd)), tw.Contains(instantAt(c.afterEnd)))
			}
			if w.Contains(instantAt(c.beforeBeg)) || tw.Contains(instantAt(c.beforeBeg)) {
				t.Errorf("the minute before the start (%s) must be outside both: window=%v, kured=%v", c.beforeBeg, w.Contains(instantAt(c.beforeBeg)), tw.Contains(instantAt(c.beforeBeg)))
			}
		})
	}
}

// The vendored oracle, pinned by digest. These two files are upstream's bytes
// unmodified (internal/kuredoracle/provenance.go records module, tag, commit and
// paths), so the digest is the same one upstream's tag produces.
var vendoredOracle = map[string]string{
	filepath.Join("..", "..", "internal", "kuredoracle", "timewindow.go"): "08ee7e139c79d3500a06221d1fd227363e63fd295baffa812a8c0e293f892fb1",
	filepath.Join("..", "..", "internal", "kuredoracle", "days.go"):       "ab2392a00f574b888cc679a409ee644cd27e8f54a73dc9d3d027b7474ad24fa9",
}

// TestVendoredOracleMatchesUpstreamTag keeps the oracle from drifting: the
// digests are kured 1.23.0's own bytes, so a semantic "fix" made to the oracle
// instead of to pkg/window fails here. When the bundle's kured pin moves, these
// digests and provenance.go move with it — in one commit.
func TestVendoredOracleMatchesUpstreamTag(t *testing.T) {
	for path, want := range vendoredOracle {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the vendored oracle is missing: %v", err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s is not kured 1.23.0's file: sha256 %s, want %s — the oracle is upstream's code or it is not an oracle", path, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The shared cross-language fixture (plan 7.4 item 7, "one rule set, two
// languages"): the specs, their verdicts and kured's answer at every sampled
// instant, generated here because Go is the only side that can run kured's code.
// kubenest-backend/tests/api/test_maintenance_window.py consumes it, so the
// Python validator is compared against the same answers as pkg/window rather
// than against a paraphrase of them.

const fixtureRelativePath = "window-semantics.json"

type fixtureOracleFile struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

type fixtureOracle struct {
	Module  string              `json:"module"`
	Tag     string              `json:"tag"`
	Commit  string              `json:"commit"`
	Package string              `json:"package"`
	Files   []fixtureOracleFile `json:"files"`
}

type fixtureSample struct {
	Why string `json:"why"`
	// Start and Minutes define the sampled instants: `Minutes` consecutive
	// whole minutes from `start` (RFC3339 in the spec's zone).
	Start   string `json:"start"`
	Minutes int    `json:"minutes"`
	// OracleOpenRanges is kured's answer, run-length encoded: inclusive
	// [first, last] pairs of sampled-minute offsets from Start, ascending. An
	// offset in no range is an instant kured answers false for.
	OracleOpenRanges [][2]int `json:"oracle_open_ranges"`
}

type fixtureSpec struct {
	Name    string          `json:"name"`
	Spec    window.Spec     `json:"spec"`
	Verdict string          `json:"verdict"`
	Rule    string          `json:"refusal_rule,omitempty"`
	Samples []fixtureSample `json:"samples,omitempty"`
}

type windowSemanticsFixture struct {
	Comment []string      `json:"_comment"`
	Week    string        `json:"week"`
	Oracle  fixtureOracle `json:"oracle"`
	Specs   []fixtureSpec `json:"specs"`
}

func fixturePath() string {
	return filepath.Join("..", "..", "..", "kubenest-contracts", "testdata", fixtureRelativePath)
}

// openRanges is kured's answer at every sampled instant, run-length encoded as
// inclusive offsets from the sample's start. It refuses to produce a sample the
// two implementations disagree on: the fixture is the set of answers the three
// interpreters share, and a spec on which they differ belongs in the refusal
// table, not in the fixture.
//
// Ranges rather than one entry per minute because 10080 minutes per spec is
// 10080 numbers to say "Sundays 06:00-09:00": the runs are the same evidence in
// a readable size.
func openRanges(t *testing.T, row oracleCase, w window.Window, tw *timewindow.TimeWindow, instants []time.Time) [][2]int {
	t.Helper()
	if found := firstDivergences(w, tw, instants); len(found) > 0 {
		t.Errorf("refusing to write a fixture for %s: kured and pkg/window disagree at %s (kured=%v, window=%v)",
			row.name, found[0].at, found[0].kured, found[0].cli)
		return nil
	}
	var ranges [][2]int
	for i, instant := range instants {
		if !tw.Contains(instant) {
			continue
		}
		if n := len(ranges); n > 0 && ranges[n-1][1] == i-1 {
			ranges[n-1][1] = i
			continue
		}
		ranges = append(ranges, [2]int{i, i})
	}
	return ranges
}

// stamp is the RFC3339 form with an explicit offset, which every Python that
// reads this repository parses with datetime.fromisoformat.
func stamp(t time.Time) string { return t.Format("2006-01-02T15:04:05-07:00") }

func buildFixture(t *testing.T) windowSemanticsFixture {
	t.Helper()
	f := windowSemanticsFixture{
		Comment: []string{
			"kured's answers for the maintenance windows the CLI and the control plane must agree with it on.",
			"(kn-t34-window-semantics-kured-s-2rlu, plan 7.4 item 7. Do not edit by hand.)",
			"",
			"GENERATED by kubenest-cli/pkg/window/kured_oracle_test.go, because Go is the only side that can run",
			"kured's window code; regenerate with:",
			"    cd kubenest-cli && go test ./pkg/window -run TestWindowSemanticsFixtureMatchesTheOracle -count=1 -update",
			"",
			"CONSUMED by kubenest-backend/tests/api/test_maintenance_window.py, which puts the same specs to the",
			"Python validator and compares every sampled instant against the oracle's answer here.",
			"",
			"SHAPE. `specs[]` carries one window: its `spec` (the stored form), the `verdict` ('accepted' or",
			"'refused'), the `refusal_rule` when it is refused, and for accepted specs one entry per `samples[]`.",
			"A sample is `minutes` consecutive instants starting at `start` (RFC3339, in the spec's zone), and",
			"`oracle_open_ranges` is KURED'S OWN ANSWER run-length encoded: inclusive [first, last] pairs of",
			"sampled-minute offsets from `start`, ascending. An offset in no range is an instant kured answers",
			"false for. The sampled instants are whole minutes, which is the granularity a window is expressed",
			"in; sub-minute agreement is deliberately not sampled.",
		},
		Week: fmt.Sprintf("every minute of the week opening Sunday %s 00:00 in the spec's own zone (%d minutes)", weekStart, weekMinutes),
		Oracle: fixtureOracle{
			Module:  "github.com/kubereboot/kured",
			Tag:     "1.23.0",
			Commit:  "ef3c90b2bd58c0571e6eae7b893f0de3709cc98f",
			Package: "pkg/timewindow",
			Files: []fixtureOracleFile{
				{Path: "pkg/timewindow/timewindow.go", Sha256: "08ee7e139c79d3500a06221d1fd227363e63fd295baffa812a8c0e293f892fb1"},
				{Path: "pkg/timewindow/days.go", Sha256: "ab2392a00f574b888cc679a409ee644cd27e8f54a73dc9d3d027b7474ad24fa9"},
			},
		},
	}

	for _, row := range oracleCases() {
		entry := fixtureSpec{Name: row.name}
		parsed, err := window.Parse(row.spec)
		if row.refusalRule != "" {
			if err == nil {
				t.Errorf("the fixture cannot record a verdict for %s: it must be refused and Parse accepted it", row.name)
				continue
			}
			entry.Spec = row.spec
			entry.Verdict = "refused"
			entry.Rule = row.refusalRule
			f.Specs = append(f.Specs, entry)
			continue
		}
		if err != nil {
			t.Errorf("the fixture cannot record %s: %v", row.name, err)
			continue
		}
		entry.Spec = parsed.Spec()
		entry.Verdict = "accepted"
		tw := oracleFor(t, row)

		instants := weekInstants(t, row.spec)
		entry.Samples = append(entry.Samples, fixtureSample{
			Why:              fmt.Sprintf("every minute of the week opening Sunday %s 00:00 %s", weekStart, row.spec.Timezone),
			Start:            stamp(instants[0]),
			Minutes:          len(instants),
			OracleOpenRanges: openRanges(t, row, parsed, tw, instants),
		})
		if row.dst {
			for _, day := range []string{dstSpring, dstAutumn} {
				dayInstants := dayInstants(t, row.spec.Timezone, day)
				change := "local 02:00-02:59 does not exist"
				if day == dstAutumn {
					change = "local 02:00-02:59 happens twice"
				}
				entry.Samples = append(entry.Samples, fixtureSample{
					Why:              fmt.Sprintf("every minute of the local day of the %s %s clock change (%s)", day, row.spec.Timezone, change),
					Start:            stamp(dayInstants[0]),
					Minutes:          len(dayInstants),
					OracleOpenRanges: openRanges(t, row, parsed, tw, dayInstants),
				})
			}
		}
		f.Specs = append(f.Specs, entry)
	}
	return f
}

// TestWindowSemanticsFixtureMatchesTheOracle keeps the committed fixture equal
// to what the oracle answers today, and writes it under -update. The check is
// skipped — loudly — when kubenest-contracts is not checked out beside this
// repository, because a check that cannot run must say so rather than pass.
func TestWindowSemanticsFixtureMatchesTheOracle(t *testing.T) {
	generated := buildFixture(t)
	path := fixturePath()

	if *updateFixture {
		file, err := os.Create(path)
		if err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		encoder := json.NewEncoder(file)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(generated); err != nil {
			file.Close()
			t.Fatalf("writing %s: %v", path, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		t.Logf("wrote %s (%d specs)", path, len(generated.Specs))
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("kubenest-contracts is not checked out at %s, so the shared fixture cannot be checked here: %v", path, err)
	}
	var committed windowSemanticsFixture
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatalf("%s is not readable: %v", path, err)
	}
	if diff := fixtureDiff(committed, generated); diff != "" {
		t.Errorf("kubenest-contracts/testdata/%s is not what kured answers (%s); regenerate it with `go test ./pkg/window -run TestWindowSemanticsFixtureMatchesTheOracle -count=1 -update`", fixtureRelativePath, diff)
	}
}

// fixtureDiff names the first difference, so a stale fixture is a sentence
// rather than a wall of JSON.
func fixtureDiff(committed, generated windowSemanticsFixture) string {
	if committed.Oracle.Tag != generated.Oracle.Tag {
		return fmt.Sprintf("generated from kured %s, committed from %s", generated.Oracle.Tag, committed.Oracle.Tag)
	}
	if len(committed.Oracle.Files) != len(generated.Oracle.Files) {
		return "the oracle file list changed"
	}
	for i, file := range generated.Oracle.Files {
		if committed.Oracle.Files[i] != file {
			return fmt.Sprintf("%s: committed digest %s, generated %s", file.Path, committed.Oracle.Files[i].Sha256, file.Sha256)
		}
	}
	if len(committed.Specs) != len(generated.Specs) {
		return fmt.Sprintf("%d specs committed, %d generated", len(committed.Specs), len(generated.Specs))
	}
	for i, want := range generated.Specs {
		got := committed.Specs[i]
		if got.Name != want.Name {
			return fmt.Sprintf("spec %d is %q, generated %q", i, got.Name, want.Name)
		}
		if got.Verdict != want.Verdict {
			return fmt.Sprintf("spec %q: committed verdict %q, generated %q", want.Name, got.Verdict, want.Verdict)
		}
		if len(got.Samples) != len(want.Samples) {
			return fmt.Sprintf("spec %q: %d samples committed, %d generated", want.Name, len(got.Samples), len(want.Samples))
		}
		for j, wantSample := range want.Samples {
			gotSample := got.Samples[j]
			if gotSample.Start != wantSample.Start || gotSample.Minutes != wantSample.Minutes {
				return fmt.Sprintf("spec %q sample %d: committed %s+%dm, generated %s+%dm",
					want.Name, j, gotSample.Start, gotSample.Minutes, wantSample.Start, wantSample.Minutes)
			}
			if len(gotSample.OracleOpenRanges) != len(wantSample.OracleOpenRanges) {
				return fmt.Sprintf("spec %q sample %d: %d open ranges committed, %d generated",
					want.Name, j, len(gotSample.OracleOpenRanges), len(wantSample.OracleOpenRanges))
			}
			for k, span := range wantSample.OracleOpenRanges {
				if gotSample.OracleOpenRanges[k] != span {
					return fmt.Sprintf("spec %q sample %d: open range %d is %v committed, %v generated",
						want.Name, j, k, gotSample.OracleOpenRanges[k], span)
				}
			}
		}
	}
	return ""
}
