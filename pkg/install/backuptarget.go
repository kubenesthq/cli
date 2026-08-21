package install

import (
	"fmt"
	"net/url"
	"os"
	"strings"

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
func parseBackupTarget(raw string) (backup.Target, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return backup.Target{}, fmt.Errorf(
			"--backup-target %q is not an S3 target: use s3://<bucket>[/<prefix>]?endpoint=<host>&region=<region> (credentials come from KUBENEST_BACKUP_ACCESS_KEY_ID and KUBENEST_BACKUP_SECRET_ACCESS_KEY, never from a flag)", raw)
	}
	query := u.Query()
	target := backup.Target{
		Bucket:          u.Host,
		Prefix:          strings.Trim(u.Path, "/"),
		Endpoint:        query.Get("endpoint"),
		Region:          query.Get("region"),
		AccessKeyID:     envFirst("KUBENEST_BACKUP_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID"),
		SecretAccessKey: envFirst("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY"),
	}
	if target.AccessKeyID == "" || target.SecretAccessKey == "" {
		return backup.Target{}, fmt.Errorf(
			"--backup-target was given but no credentials are in the environment: set KUBENEST_BACKUP_ACCESS_KEY_ID and KUBENEST_BACKUP_SECRET_ACCESS_KEY (or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY). Omit --backup-target to install Velero unconfigured and set a target later")
	}
	if err := target.Validate(); err != nil {
		return backup.Target{}, err
	}
	return target, nil
}

func envFirst(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}
