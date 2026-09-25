// Package window is the maintenance window — ONE definition, shared by every
// operation that must not start outside it.
//
// It exists as its own package because two things need it and they must agree:
// the bundle upgrade refuses to begin outside the window (kn-fuo), and kured's
// reboot orchestration reboots inside it (kn-nqj). Two definitions would mean
// a cluster that reboots at 02:00 and upgrades at 03:00 by different rules,
// which is exactly the surprise this product exists to remove.
//
// Two properties are easy to get wrong and are therefore the point of the
// package:
//
//	The timezone is an IANA NAME, never an offset. Offsets move twice a year
//	and a window that silently shifts by an hour is worse than no window —
//	the customer set 02:00-06:00 local, and local is what they meant.
//
//	A window whose end is before its start CROSSES MIDNIGHT. 22:00-04:00 is
//	the common maintenance shape, not an input error — but it is accepted
//	only when it names ALL SEVEN DAYS. kured opens such a window on every
//	day it lists, while this package belongs it to the day it OPENED, and
//	the two answer identically only for the whole week; with fewer days the
//	cluster would reboot four hours outside the window the operator set. See
//	crossingRefusal, and pkg/window/kured_oracle_test.go, which puts every
//	minute of a week to kured's own code.
package window

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is a recurring weekly maintenance window.
type Window struct {
	// Days are the weekdays the window opens on, by its START. A window that
	// crosses midnight belongs to the day it opened.
	Days []time.Weekday
	// Start and End are minutes since midnight, local to Location.
	Start, End int
	// Location is the IANA timezone the times are expressed in.
	Location *time.Location
}

// Spec is the serialized form: what is stored on the cluster record and what
// the CLI flags produce.
type Spec struct {
	Days     []string `json:"days"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Timezone string   `json:"timezone"`
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Name returns the three-letter form of a weekday, which is what the flags
// and the stored spec use.
func Name(d time.Weekday) string {
	return strings.ToLower(d.String()[:3])
}

// Parse turns the stored/flag form into a Window.
func Parse(spec Spec) (Window, error) {
	if len(spec.Days) == 0 {
		return Window{}, fmt.Errorf("a maintenance window needs at least one day (mon,tue,wed,thu,fri,sat,sun)")
	}
	var days []time.Weekday
	seen := map[time.Weekday]bool{}
	for _, raw := range spec.Days {
		key := strings.ToLower(strings.TrimSpace(raw))
		if len(key) > 3 {
			key = key[:3]
		}
		day, ok := weekdays[key]
		if !ok {
			return Window{}, fmt.Errorf("%q is not a day: use mon,tue,wed,thu,fri,sat,sun", raw)
		}
		if seen[day] {
			continue
		}
		seen[day] = true
		days = append(days, day)
	}

	start, err := parseClock(spec.Start)
	if err != nil {
		return Window{}, fmt.Errorf("--start: %w", err)
	}
	end, err := parseClock(spec.End)
	if err != nil {
		return Window{}, fmt.Errorf("--end: %w", err)
	}
	if start == end {
		return Window{}, fmt.Errorf("a maintenance window that starts and ends at %s is zero minutes long", spec.Start)
	}
	if end < start && len(days) != 7 {
		return Window{}, crossingRefusal(spec.Start, spec.End)
	}

	if strings.TrimSpace(spec.Timezone) == "" {
		return Window{}, fmt.Errorf("a maintenance window needs a timezone: an IANA name such as Asia/Kolkata or UTC, never an offset — offsets move twice a year and the window would silently shift with them")
	}
	loc, err := time.LoadLocation(spec.Timezone)
	if err != nil {
		return Window{}, fmt.Errorf("timezone %q is not an IANA name (Asia/Kolkata, Europe/Berlin, UTC): %w", spec.Timezone, err)
	}
	if err := changeoverRefusal(loc, start, end); err != nil {
		return Window{}, err
	}

	return Window{Days: days, Start: start, End: end, Location: loc}, nil
}

// The horizon the clock-change refusal below is asked over, and why it is a
// CONSTANT rather than time.Now.
//
// A local time is not skipped or repeated by itself — it is skipped or repeated
// on a date, because the zone's changeover falls on one. So "can this window be
// read on every day of the year" is a question about a horizon of days, and the
// horizon starts at a fixed date so that Parse is deterministic: the same spec
// is accepted or refused on every machine, on every run, whatever the wall clock
// says. That is also what lets the backend's Python apply the same rule to the
// same days (maintenance_window.DST_SCAN_START_YEAR) and be compared with this
// one at all.
//
// Five years is longer than the horizon any install plans over. WHEN IT STOPS
// COVERING THE FUTURE, move these two numbers in BOTH languages in one commit,
// the way internal/kuredoracle's digests move with the bundle's kured pin.
const (
	dstScanStartYear = 2026
	dstScanYears     = 5
)

// changeoverRefusal refuses a window whose start or end falls in a local hour
// the zone skips or repeats when it changes its clock.
//
// WHY REFUSE RATHER THAN PICK ONE ANSWER. Such a wall clock does not name one
// instant. This package reads a window as wall-clock minutes, and kured builds
// the day's boundaries with time.Date, which normalises a skipped time forward
// and chooses one of the two instants a repeated time names (a choice Go
// documents as unspecified). Measured against kured 1.23.0 for
// Europe/Berlin `sun 02:30-05:00`: on 2026-03-29 kured closes the window at
// 03:00-03:29 CEST while this package keeps it open, and on 2026-10-25 kured is
// shut at the CEST occurrence of 02:30-02:59 while this package is open — an
// hour of disagreement twice a year, for a window the operator set in good
// faith. The same shape the other way round (`sun 01:00-02:30`, end in the
// skipped hour) has kured OPEN for 03:00-03:29 CEST while this package is shut.
// So set-window refuses the shape and NAMES BOTH FIXES.
//
// NARROW ON PURPOSE: only the window's own two boundaries are asked about. A
// window that merely CONTAINS the changeover — `sun 01:00-05:00` in Berlin, or
// the all-seven-days 22:00-04:00 crossing — is accepted, because there the two
// readings produce the same instants (both boundaries name one instant each and
// the clock change happens between them).
func changeoverRefusal(loc *time.Location, start, end int) error {
	for _, boundary := range []struct {
		field   string
		minutes int
	}{{"start", start}, {"end", end}} {
		if _, skipped := changeoverDay(loc, boundary.minutes); skipped {
			return fmt.Errorf(
				"a maintenance window whose %s %s falls in the hour %s skips or repeats when it changes its clock is refused: that local time does not name one instant, so kured and this platform would disagree about when the window is open, for one hour twice a year. Either choose a start and end outside the zone's changeover hour, or use UTC or a zone without daylight saving",
				boundary.field, clock(boundary.minutes), loc)
		}
	}
	return nil
}

// changeoverDay returns the first day in the horizon on which this wall clock is
// skipped or repeated.
//
// Only days on which the zone's offset actually changes are examined in detail:
// the scan walks one probe a day and looks at the pair of days around each change
// of offset, which is the only place a wall clock can be skipped or repeated.
// That keeps Parse's cost at a walk over the horizon rather than a walk over the
// horizon times two boundaries.
func changeoverDay(loc *time.Location, minutes int) (time.Time, bool) {
	from := time.Date(dstScanStartYear, time.January, 1, 12, 0, 0, 0, loc)
	until := from.AddDate(dstScanYears, 0, 0)
	previous := from
	_, previousOffset := previous.Zone()

	for day := from.AddDate(0, 0, 1); day.Before(until); day = day.AddDate(0, 0, 1) {
		_, offset := day.Zone()
		if offset != previousOffset {
			for _, candidate := range [2]time.Time{previous, day} {
				if localTimeIsSkippedOrRepeated(loc, candidate, minutes) {
					return candidate, true
				}
			}
			previousOffset = offset
		}
		previous = day
	}
	return time.Time{}, false
}

// localTimeIsSkippedOrRepeated reports whether hh:mm on this day in this zone
// names zero instants (the hour was skipped) or two (it was repeated).
//
// It asks every instant that COULD display that wall clock — one per offset the
// zone uses on that day — which instants those are, and counts how many really
// do. Zero means the hour is skipped; two means it is repeated; one means the
// wall clock is ordinary and the two implementations cannot disagree about it.
func localTimeIsSkippedOrRepeated(loc *time.Location, day time.Time, minutes int) bool {
	year, month, date := day.Date()
	hours, mins := minutes/60, minutes%60

	// The offsets in play around this day. A zone moves its clock one transition
	// at a time, so the days either side of it bracket both offsets.
	noon := time.Date(year, month, date, 12, 0, 0, 0, loc)
	offsets := map[int]bool{}
	for _, probe := range [3]time.Time{noon.Add(-24 * time.Hour), noon, noon.Add(24 * time.Hour)} {
		_, offset := probe.Zone()
		offsets[offset] = true
	}

	// The wall clock as a UTC coordinate, so the instant that displays it under
	// an offset o is that coordinate minus o.
	naive := time.Date(year, month, date, hours, mins, 0, 0, time.UTC)
	instants := map[int64]bool{}
	for offset := range offsets {
		instant := naive.Add(-time.Duration(offset) * time.Second)
		local := instant.In(loc)
		if local.Year() != year || local.Month() != month || local.Day() != date {
			continue
		}
		if local.Hour()*60+local.Minute() != minutes {
			continue
		}
		instants[instant.Unix()] = true
	}
	return len(instants) != 1
}

// parseClock reads "HH:MM" into minutes since midnight.
func parseClock(s string) (int, error) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, fmt.Errorf("%q is not a time of day (want HH:MM, 24-hour)", s)
	}
	hours, err := strconv.Atoi(hh)
	if err != nil || hours < 0 || hours > 23 {
		return 0, fmt.Errorf("%q is not a time of day (want HH:MM, 24-hour)", s)
	}
	minutes, err := strconv.Atoi(mm)
	if err != nil || minutes < 0 || minutes > 59 {
		return 0, fmt.Errorf("%q is not a time of day (want HH:MM, 24-hour)", s)
	}
	return hours*60 + minutes, nil
}

// CrossesMidnight reports whether the window runs past 00:00 into the next
// day, which is the common shape for maintenance and not an error.
func (w Window) CrossesMidnight() bool { return w.End < w.Start }

// Contains reports whether an instant falls inside the window.
//
// A window that crosses midnight belongs to the day it OPENED: a window that
// names all seven days and runs 22:00-04:00 is open at 02:00 because the
// previous day's opening is still running, and it is open at 23:00 because
// that day's has begun. (A window that crosses midnight and names FEWER than
// seven days is refused by Parse, because here it would belong to the day it
// opened while kured opens it on every listed day — see crossingRefusal.)
//
// THE END MINUTE IS INSIDE, which is kured's own rule: it builds the day's end
// as HH:MM:00.999999999 and tests `loctime.Before(end)`, so on the whole-minute
// instants a window is made of, a window is [start, end] — both ends included.
// A half-open test here (minutes < w.End) would refuse an operator at 09:00
// while kured reboots at 09:00: one window, two answers, and the disagreement
// is silent. pkg/window/kured_oracle_test.go samples every minute of a week and
// of two DST change days against kured's code to keep that from happening.
func (w Window) Contains(t time.Time) bool {
	if w.Location == nil || len(w.Days) == 0 {
		return false
	}
	local := t.In(w.Location)
	minutes := local.Hour()*60 + local.Minute()

	for _, day := range w.Days {
		if !w.CrossesMidnight() {
			if local.Weekday() == day && minutes >= w.Start && minutes <= w.End {
				return true
			}
			continue
		}
		// Opened today and still running, or opened yesterday and not yet
		// closed: the end minute of the morning part is inside too.
		if local.Weekday() == day && minutes >= w.Start {
			return true
		}
		if local.Weekday() == (day+1)%7 && minutes <= w.End {
			return true
		}
	}
	return false
}

// crossingRefusal is the one refusal for a window that crosses midnight and
// does not name all seven days.
//
// The window is interpreted in three places — this package, the control
// plane's validator and kured — and the two weekday rules only meet when the
// day list is the whole week: kured tests the weekday of the INSTANT, so
// `sat 22:00-04:00` is open Saturday 00:00-04:00 ("it opened Friday") and
// Saturday 22:00-24:00, while this package's rule is that a crossing window
// belongs to the day it OPENED. The instant sets differ by four hours a week,
// and the operator who set Saturday night gets a reboot on Saturday MORNING.
//
// So set-window refuses the shape instead of picking one of the two answers —
// and NAMES BOTH FIXES, rather than widening the window silently.
func crossingRefusal(start, end string) error {
	return fmt.Errorf(
		"a maintenance window that crosses midnight (%s-%s) is refused unless it names all seven days: kured opens such a window on every day it lists, at 00:00, because it tests the day of the instant, while this window belongs to the day it opened — so with fewer than seven days the two disagree for four hours of every day listed. Either list all seven days, or do not cross midnight (make the end later than the start)",
		start, end)
}

// NextOpen returns when the window next opens at or after t, so a refusal can
// tell the operator when to come back rather than only that they are early.
func (w Window) NextOpen(t time.Time) (time.Time, bool) {
	if w.Location == nil || len(w.Days) == 0 {
		return time.Time{}, false
	}
	local := t.In(w.Location)
	for i := 0; i < 8; i++ {
		day := local.AddDate(0, 0, i)
		for _, wd := range w.Days {
			if day.Weekday() != wd {
				continue
			}
			open := time.Date(day.Year(), day.Month(), day.Day(), w.Start/60, w.Start%60, 0, 0, w.Location)
			if !open.Before(local) {
				return open, true
			}
		}
	}
	return time.Time{}, false
}

// Spec returns the window in its canonical stored form: the weekdays named as
// the flags name them, the times as HH:MM, and the zone as the IANA name that
// resolved. It is what a caller SENDS, so that everything the parser accepted
// in a lenient shape — "MONDAY", " 02:00" — goes out in the one shape the
// stored form has, rather than a form the CLI accepts and the control plane
// refuses.
func (w Window) Spec() Spec {
	days := make([]string, 0, len(w.Days))
	for _, d := range w.Days {
		days = append(days, Name(d))
	}
	zone := ""
	if w.Location != nil {
		zone = w.Location.String()
	}
	return Spec{Days: days, Start: clock(w.Start), End: clock(w.End), Timezone: zone}
}

// Moment renders one instant the way a window refusal must name it: in the
// window's own zone AND in UTC.
//
// Both, because the operator reasons in the zone they configured the window
// with, while a log line, a journal entry and the control plane's record are
// read in UTC. A refusal that names only one of them makes every reader
// convert in their head, which is how "the window opens at 02:00" and "it is
// 02:00 now" end up meaning different things to two people looking at the same
// cluster.
func (w Window) Moment(t time.Time) string {
	utc := t.UTC().Format("Mon 2 Jan 15:04 MST")
	if w.Location == nil {
		return utc
	}
	return t.In(w.Location).Format("Mon 2 Jan 15:04 MST") + " (" + utc + ")"
}

// The refusal every disruptive verb gives when the cluster has no window it may
// act inside, and the fix that goes with it.
//
// ONE SENTENCE, ONE FIX. The upgrade's window gate, node reboot (kn-nqj) and
// restore all print this, so an operator refused by one verb is not told
// something different by the next — and none of them can drift back to the
// kn-nqj behaviour of reading an absent window as "any time is inside it".
const (
	// NoWindow is the verdict: a cluster with no window has NO time inside it.
	NoWindow = "no maintenance window is configured for this cluster, so there is no time this operation is allowed to start"
	// NoWindowFix is what to do about it.
	NoWindowFix = "set the cluster's window with `kubenest cluster set-window --cluster <name> --days sat,sun --start 02:00 --end 06:00 --timezone Asia/Kolkata`"
	// OutsideFix is what to do when now is outside a window that exists.
	OutsideFix = "wait for it with --wait, or set a window that covers now with `kubenest cluster set-window`"
)

// Outside reports why a disruptive command must not act at instant now, or nil
// when now is inside the window.
//
// It is the ONE wording of the plan's rule (7.4 item 3): outside the window,
// refuse and name the next opening in LOCAL TIME AND UTC. Every verb that must
// not act outside the window calls this rather than writing its own sentence —
// a second wording drifts, and the operator then has to work out which verb is
// telling them the truth.
func (w Window) Outside(now time.Time) error {
	if len(w.Days) == 0 || w.Location == nil {
		return fmt.Errorf("the maintenance window %s cannot be read: it names no day or no zone", w)
	}
	if w.Contains(now) {
		return nil
	}
	next := "it has no opening within the next week"
	if at, ok := w.NextOpen(now); ok {
		next = "it next opens " + w.Moment(at)
	}
	return fmt.Errorf("now (%s) is outside the maintenance window %s, and %s", w.Moment(now), w, next)
}

// String renders the window the way the operator configured it.
func (w Window) String() string {
	days := make([]string, 0, len(w.Days))
	for _, d := range w.Days {
		days = append(days, Name(d))
	}
	return fmt.Sprintf("%s %s-%s %s", strings.Join(days, ","), clock(w.Start), clock(w.End), w.Location)
}

func clock(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}
