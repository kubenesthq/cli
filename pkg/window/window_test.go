package window_test

import (
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
	if w.Contains(at(t, w.Location, "2026-08-22 06:00")) {
		t.Error("the window is half-open: 06:00 is the end, not inside")
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

// 22:00-04:00 is the common maintenance shape, not an input error. A window
// that crosses midnight belongs to the day it OPENED.
func TestAWindowThatCrossesMidnight(t *testing.T) {
	w := mustParse(t, window.Spec{
		Days: []string{"sat"}, Start: "22:00", End: "04:00", Timezone: "UTC",
	})
	if !w.CrossesMidnight() {
		t.Fatal("22:00-04:00 crosses midnight")
	}
	cases := map[string]bool{
		"2026-08-22 22:30": true,  // Saturday night, just opened
		"2026-08-23 01:00": true,  // Sunday morning, still the Saturday window
		"2026-08-23 04:00": false, // closed
		"2026-08-22 01:00": false, // Saturday morning is NOT the Saturday window
		"2026-08-23 23:00": false, // Sunday night is not a Saturday window
	}
	for value, want := range cases {
		if got := w.Contains(at(t, w.Location, value)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", value, got, want)
		}
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

// A window whose timezone observes DST still means local wall-clock time on
// both sides of the change. This is the whole reason the name is stored.
func TestTheWindowIsWallClockAcrossADSTChange(t *testing.T) {
	w := mustParse(t, window.Spec{
		Days: []string{"sun"}, Start: "02:30", End: "05:00", Timezone: "Europe/Berlin",
	})
	// Berlin left DST on 2026-10-25; both these Sundays are 03:00 local.
	before := at(t, w.Location, "2026-10-18 03:00")
	after := at(t, w.Location, "2026-11-01 03:00")
	if !w.Contains(before) || !w.Contains(after) {
		t.Error("03:00 local is inside a 02:30-05:00 local window on both sides of a DST change")
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
