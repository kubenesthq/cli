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
//	the common maintenance shape, not an input error.
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

	if strings.TrimSpace(spec.Timezone) == "" {
		return Window{}, fmt.Errorf("a maintenance window needs a timezone: an IANA name such as Asia/Kolkata or UTC, never an offset — offsets move twice a year and the window would silently shift with them")
	}
	loc, err := time.LoadLocation(spec.Timezone)
	if err != nil {
		return Window{}, fmt.Errorf("timezone %q is not an IANA name (Asia/Kolkata, Europe/Berlin, UTC): %w", spec.Timezone, err)
	}

	return Window{Days: days, Start: start, End: end, Location: loc}, nil
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
// A window that crosses midnight belongs to the day it OPENED: a Saturday
// 22:00-04:00 window is open at 02:00 on Sunday morning, and is not open at
// 02:00 on Saturday morning.
func (w Window) Contains(t time.Time) bool {
	if w.Location == nil || len(w.Days) == 0 {
		return false
	}
	local := t.In(w.Location)
	minutes := local.Hour()*60 + local.Minute()

	for _, day := range w.Days {
		if !w.CrossesMidnight() {
			if local.Weekday() == day && minutes >= w.Start && minutes < w.End {
				return true
			}
			continue
		}
		// Opened yesterday and still running, or opening today.
		if local.Weekday() == day && minutes >= w.Start {
			return true
		}
		if local.Weekday() == (day+1)%7 && minutes < w.End {
			return true
		}
	}
	return false
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
