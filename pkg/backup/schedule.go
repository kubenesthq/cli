package backup

import (
	"fmt"
	"time"
)

// The manifest records the backup CADENCE (backup.defaults.workload-backup:
// interval + keep); Velero wants a cron expression and a TTL. The functions
// here are that translation and nothing else — the numbers themselves come
// from the manifest, never from this file.

// anchorHourUTC is when daily-or-slower backups fire. The hour is
// presentation, not policy — the manifest owns how OFTEN, this only picks a
// quiet time of day — and it matches the backup-restore page's examples
// (daily-…-0200).
const anchorHourUTC = 2

// Cron renders a backup interval as a Velero schedule expression. Velero's
// cron parser also accepts @every, which is the exact meaning of an interval
// the calendar forms below cannot express.
func Cron(interval time.Duration) (string, error) {
	if interval <= 0 {
		return "", fmt.Errorf("backup interval must be positive, got %s", interval)
	}
	day := 24 * time.Hour
	switch {
	case interval == day:
		return fmt.Sprintf("0 %d * * *", anchorHourUTC), nil
	case interval < day && interval%time.Hour == 0 && (24%int(interval/time.Hour)) == 0:
		return fmt.Sprintf("0 */%d * * *", int(interval/time.Hour)), nil
	case interval < time.Hour && interval%time.Minute == 0 && (60%int(interval/time.Minute)) == 0:
		return fmt.Sprintf("*/%d * * * *", int(interval/time.Minute)), nil
	default:
		return "@every " + interval.String(), nil
	}
}

// ScheduleName names the Schedule for its cadence — backups are then named
// <schedule>-<timestamp>, so a drill result reads "daily-20260813-0200" the
// way the backup-restore page shows it. Cadences without a household name
// fall back to "workload".
func ScheduleName(interval time.Duration) string {
	switch interval {
	case time.Hour:
		return "hourly"
	case 24 * time.Hour:
		return "daily"
	case 7 * 24 * time.Hour:
		return "weekly"
	default:
		return "workload"
	}
}

// TTL is the retention window: keep × interval. Velero expires a backup when
// its TTL passes, so "14 kept" at a daily cadence is written as 336h.
func TTL(interval time.Duration, keep int) (time.Duration, error) {
	if interval <= 0 || keep <= 0 {
		return 0, fmt.Errorf("retention needs a positive interval and keep, got %s × %d", interval, keep)
	}
	return time.Duration(keep) * interval, nil
}
