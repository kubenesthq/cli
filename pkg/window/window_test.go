package window_test

import (
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/window"
)

func mustParse(t *testing.T, spec window.Spec) window.Window {
	t.Helper()
	w, err := window.Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func at(t *testing.T, loc *time.Location, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// The documented example, in the customer's own timezone.
func TestWindowInItsOwnTimezone(t *testing.T) {
	w := mustParse(t, window.Spec{
		Days: []string{"sat", "sun"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata",
	})
	// 2026-08-22 is a Saturday.
	if !w.Contains(at(t, w.Location, "2026-08-22 03:30")) {
		t.Error("03:30 on Saturday must be inside a Saturday 02:00-06:00 window")
	}
	// The END MINUTE IS INSIDE — kured's own rule, pinned against kured's code
	// by kured_oracle_test.go: it builds the day's end as HH:MM:00.999999999 and
	// tests Before(end). A half-open test here would refuse an operator at 06:00
	// while kured reboots at 06:00.
	if !w.Contains(at(t, w.Location, "2026-08-22 06:00")) {
		t.Error("the end minute is inside the window: kured's own code answers true at 06:00")
	}
	if w.Contains(at(t, w.Location, "2026-08-22 06:01")) {
		t.Error("the minute after the end is outside")
	}
	if w.Contains(at(t, w.Location, "2026-08-21 03:30")) {
		t.Error("Friday is not in a sat,sun window")
	}
	// The same instant expressed elsewhere is still inside: 03:30 IST is
	// 22:00 UTC the previous day.
	utc := at(t, w.Location, "2026-08-22 03:30").UTC()
	if !w.Contains(utc) {
		t.Error("the window is an instant test, not a string comparison — a UTC representation of the same moment is inside")
	}
}

// 22:00-04:00 is the common maintenance shape, not an input error — but only
// when it names all seven days, which is the one shape kured's weekday rule and
// this package's agree on (kured_oracle_test.go refuses the rest). The
// attribution below is this package's: a crossing window belongs to the day it
// OPENED, so Friday's opening is what is still running on Saturday morning.
func TestAWindowThatCrossesMidnight(t *testing.T) {
	w := mustParse(t, window.Spec{
		Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, Start: "22:00", End: "04:00", Timezone: "UTC",
	})
	if !w.CrossesMidnight() {
		t.Fatal("22:00-04:00 crosses midnight")
	}
	cases := map[string]bool{
		"2026-08-22 22:30": true, // Saturday night, just opened
		"2026-08-23 01:00": true, // Sunday morning, still the Saturday window
		"2026-08-23 04:00": true, // the end minute is inside
		"2026-08-23 04:01": false,
		"2026-08-22 01:00": true, // Saturday morning: Friday's window opened and is still running
		"2026-08-23 23:00": true, // Sunday night: Sunday's own opening
		"2026-08-23 13:00": false,
	}
	for value, want := range cases {
		if got := w.Contains(at(t, w.Location, value)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", value, got, want)
		}
	}

	// Fewer than seven days is the shape the three interpreters read
	// differently, so it is refused rather than answered twice.
	if _, err := window.Parse(window.Spec{
		Days: []string{"sat"}, Start: "22:00", End: "04:00", Timezone: "UTC",
	}); err == nil {
		t.Error("a crossing window that does not name all seven days must be refused")
	}
}

// An offset is refused. Offsets move twice a year, and a window that silently
// shifts with them is worse than no window: the customer said 02:00 local.
func TestATimezoneMustBeAnIANAName(t *testing.T) {
	for _, tz := range []string{"", "+05:30", "IST", "UTC+2"} {
		_, err := window.Parse(window.Spec{Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: tz})
		if err == nil {
			t.Errorf("timezone %q must be refused", tz)
		}
	}
	if _, err := window.Parse(window.Spec{Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: "UTC"}); err != nil {
		t.Errorf("UTC is an IANA name: %v", err)
	}
}

// A window whose zone observes DST still means local wall-clock time on both
// sides of the change. This is the whole reason the name is stored.
//
// The window starts at 03:00 rather than at 02:30 because 02:30 is inside the
// hour Berlin skips and repeats and is refused outright (see
// kured_oracle_test.go::TestAWindowStartingOrEndingInADSTChangeoverHourIsRefused):
// a boundary that names no single instant cannot mean one thing on the cluster.
func TestTheWindowIsWallClockAcrossADSTChange(t *testing.T) {
	w := mustParse(t, window.Spec{
		Days: []string{"sun"}, Start: "03:00", End: "05:00", Timezone: "Europe/Berlin",
	})
	// Berlin left DST on 2026-10-25; both these Sundays are 03:00 local.
	before := at(t, w.Location, "2026-10-18 03:00")
	after := at(t, w.Location, "2026-11-01 03:00")
	if !w.Contains(before) || !w.Contains(after) {
		t.Error("03:00 local is inside a 03:00-05:00 local window on both sides of a DST change")
	}
}

func TestMalformedSpecsAreRefused(t *testing.T) {
	cases := map[string]window.Spec{
		"no days":     {Days: nil, Start: "02:00", End: "06:00", Timezone: "UTC"},
		"unknown day": {Days: []string{"caturday"}, Start: "02:00", End: "06:00", Timezone: "UTC"},
		"bad start":   {Days: []string{"sat"}, Start: "2am", End: "06:00", Timezone: "UTC"},
		"bad end":     {Days: []string{"sat"}, Start: "02:00", End: "25:00", Timezone: "UTC"},
		"zero length": {Days: []string{"sat"}, Start: "02:00", End: "02:00", Timezone: "UTC"},
		"minutes out": {Days: []string{"sat"}, Start: "02:99", End: "06:00", Timezone: "UTC"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := window.Parse(spec); err == nil {
				t.Error("want a refusal")
			}
		})
	}
}

// A refusal should say when to come back, not only that the operator is early.
func TestNextOpenTellsTheOperatorWhenToReturn(t *testing.T) {
	w := mustParse(t, window.Spec{
		Days: []string{"sat", "sun"}, Start: "02:00", End: "06:00", Timezone: "UTC",
	})
	// Wednesday afternoon.
	next, ok := w.NextOpen(at(t, w.Location, "2026-08-19 15:00"))
	if !ok {
		t.Fatal("a weekly window always has a next opening")
	}
	if next.Weekday() != time.Saturday || next.Hour() != 2 {
		t.Errorf("next opening is %s, want Saturday 02:00", next)
	}

	// Inside the window, the next opening is now.
	inside := at(t, w.Location, "2026-08-22 03:00")
	if next, _ := w.NextOpen(inside); next.After(inside) && !w.Contains(inside) {
		t.Errorf("inside the window, NextOpen must not point at next week: %s", next)
	}
}

func TestStringRoundTripsWhatWasConfigured(t *testing.T) {
	w := mustParse(t, window.Spec{
		Days: []string{"sat", "sun"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata",
	})
	if got, want := w.String(), "sat,sun 02:00-06:00 Asia/Kolkata"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// THE ONE REFUSAL EVERY DISRUPTIVE VERB GIVES (plan 7.4 item 3).
//
// Outside the window, refuse and name the next opening in LOCAL TIME AND UTC.
// It lives here, once, because the upgrade's window gate, a node reboot and a
// restore all print it: two wordings would drift, and the operator would then
// have to work out which verb is telling them the truth.
//
// MUTATION THAT MUST FAIL THIS TEST: render the next opening in one zone only,
// or drop the opening from the message. The upgrade's gate would then refuse
// with a sentence that leaves the operator to convert the instant themselves,
// which is the failure this test is here to prevent.
func TestOutsideRefusesWithNowAndTheNextOpeningInLocalAndUTC(t *testing.T) {
	w := mustParse(t, window.Spec{
		Days: []string{"sat"}, Start: "02:00", End: "06:00", Timezone: "Asia/Kolkata",
	})
	// Friday midday UTC is outside a Saturday 02:00 IST window.
	now := at(t, time.UTC, "2026-08-21 12:00")
	err := w.Outside(now)
	if err == nil {
		t.Fatal("Friday midday is outside a Saturday window")
	}
	// The opening is Saturday 02:00 IST, which is Friday 20:30 UTC: the two
	// renderings name one instant, and both must be in the refusal.
	for _, want := range []string{"02:00 IST", "20:30 UTC", "12:00 UTC", w.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q:\n%v", want, err)
		}
	}

	// Inside the window there is nothing to refuse.
	if err := w.Outside(at(t, w.Location, "2026-08-22 03:00")); err != nil {
		t.Errorf("inside the window Outside must return nil, got %v", err)
	}

	// A window that cannot be read is refused too, and says so rather than
	// reading as "any time": no day list means there is no time the window
	// covers.
	if err := (window.Window{}).Outside(now); err == nil {
		t.Error("a window with no day and no zone cannot be acted inside")
	}

	// And the fix that goes with a missing window names the command, not a
	// shrug: this is the sentence a refused operator copies.
	if !strings.Contains(window.NoWindowFix, "kubenest cluster set-window") {
		t.Errorf("NoWindowFix = %q, want the command that sets one", window.NoWindowFix)
	}
	if !strings.Contains(window.OutsideFix, "--wait") {
		t.Errorf("OutsideFix = %q, want the flag that holds for the window", window.OutsideFix)
	}
}
