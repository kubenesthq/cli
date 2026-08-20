package backup

import (
	"testing"
	"time"
)

func TestCronSpeaksTheManifestCadences(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     string
	}{
		// Decision E's shipped default, matching the page's daily-…-0200.
		{24 * time.Hour, "0 2 * * *"},
		{time.Hour, "0 */1 * * *"},
		{6 * time.Hour, "0 */6 * * *"},
		{30 * time.Minute, "*/30 * * * *"},
		// Cadences the calendar forms cannot express fall back to @every,
		// which Velero's cron parser accepts.
		{36 * time.Hour, "@every 36h0m0s"},
		{7 * time.Hour, "@every 7h0m0s"},
	}
	for _, c := range cases {
		got, err := Cron(c.interval)
		if err != nil {
			t.Errorf("Cron(%s): %v", c.interval, err)
			continue
		}
		if got != c.want {
			t.Errorf("Cron(%s) = %q, want %q", c.interval, got, c.want)
		}
	}
	if _, err := Cron(0); err == nil {
		t.Error("Cron(0) must be an error")
	}
}

func TestScheduleNameMatchesTheCadence(t *testing.T) {
	cases := map[time.Duration]string{
		time.Hour:          "hourly",
		24 * time.Hour:     "daily",
		7 * 24 * time.Hour: "weekly",
		36 * time.Hour:     "workload",
		90 * time.Minute:   "workload",
	}
	for interval, want := range cases {
		if got := ScheduleName(interval); got != want {
			t.Errorf("ScheduleName(%s) = %q, want %q", interval, got, want)
		}
	}
}

func TestTTLIsKeepTimesInterval(t *testing.T) {
	got, err := TTL(24*time.Hour, 14)
	if err != nil {
		t.Fatal(err)
	}
	if got != 336*time.Hour {
		t.Errorf("TTL(24h, 14) = %s, want 336h", got)
	}
	if got.String() != "336h0m0s" {
		t.Errorf("TTL string = %q, want the form velero accepts (336h0m0s)", got.String())
	}
	if _, err := TTL(24*time.Hour, 0); err == nil {
		t.Error("TTL with keep=0 must be an error")
	}
	if _, err := TTL(0, 14); err == nil {
		t.Error("TTL with a zero interval must be an error")
	}
}
