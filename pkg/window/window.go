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
