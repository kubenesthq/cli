package install

import (
	"os"

	"kubenest.io/cli/pkg/backup"
)

// parseBackupTarget reads --backup-target.
//
// The flag carries only non-secret coordinates:
//
//	s3://my-bucket/clusters/prod-1?endpoint=s3.ap-south-1.amazonaws.com&region=ap-south-1
//
// The access key and secret come from the environment
// (KUBENEST_BACKUP_ACCESS_KEY_ID / KUBENEST_BACKUP_SECRET_ACCESS_KEY, falling
// back to AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) — the same rule
// `kubenest backup configure` already follows, and for the same reason: a
// credential on a command line lands in shell history, in `ps`, and in the
// install transcript someone pastes into a support ticket.
//
// The parsing itself lives in pkg/backup, beside the Target it builds, so the
// install, `backup set-target` and `recovery-kit check` cannot drift apart
// about what a target string means.
func parseBackupTarget(raw string) (backup.Target, error) {
	return backup.ParseTarget(raw, envFirst("KUBENEST_BACKUP_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID"), envFirst("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY"))
}

func envFirst(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}
