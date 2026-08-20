package backup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// TargetSecretName is the Secret holding the target's credentials, in the
// velero namespace. The BackupStorageLocation references it per-location
// (spec.credential), which Velero reads through the API — no volume mount,
// so changing the target never restarts anything.
const TargetSecretName = "kubenest-backup-target"

// StorageLocationName is the one BackupStorageLocation kubenest manages.
// Velero treats the location named "default" as the implicit target for
// backups that name none, which is exactly the behaviour we want for the
// default schedule and for `kubenest backup now`.
const StorageLocationName = "default"

// Target is one cluster's S3-compatible backup destination, customer
// supplied: endpoint, bucket and credentials are theirs (backup-restore.mdx
// "Configuring a target").
type Target struct {
	// Endpoint is the S3 API endpoint, with or without a scheme —
	// "s3.ap-south-1.amazonaws.com" or "http://minio.internal:9000".
	// No scheme means https.
	Endpoint string
	Bucket   string
	Region   string
	// Prefix is an optional directory within the bucket.
	Prefix string

	AccessKeyID     string
	SecretAccessKey string
}

// Validate checks shape only; whether the credentials actually open the
// bucket is Velero's validation, surfaced by Configure's convergence wait.
func (t Target) Validate() error {
	switch {
	case t.Endpoint == "":
		return fmt.Errorf("a backup target needs --endpoint: the S3-compatible API endpoint of your bucket")
	case t.Bucket == "":
		return fmt.Errorf("a backup target needs --bucket")
	case t.Region == "":
		return fmt.Errorf("a backup target needs --region (S3-compatible stores that ignore it still want a value; \"main\" or \"us-east-1\" are common)")
	case t.AccessKeyID == "" || t.SecretAccessKey == "":
		return fmt.Errorf("a backup target needs credentials: set KUBENEST_BACKUP_ACCESS_KEY_ID and KUBENEST_BACKUP_SECRET_ACCESS_KEY (or the AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY pair)")
	}
	for _, v := range []string{t.AccessKeyID, t.SecretAccessKey} {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("backup credentials must not contain newlines")
		}
	}
	if _, err := url.Parse(t.s3URL()); err != nil {
		return fmt.Errorf("--endpoint %q does not parse as a URL: %v", t.Endpoint, err)
	}
	return nil
}

// s3URL is the endpoint with an explicit scheme (https unless given).
func (t Target) s3URL() string {
	if strings.Contains(t.Endpoint, "://") {
		return t.Endpoint
	}
	return "https://" + t.Endpoint
}

// onAWS reports whether the endpoint is AWS itself. Everything else — MinIO,
// Ceph, B2, … — gets path-style addressing (virtual-host style needs
// wildcard DNS most stores don't have) and checksumAlgorithm "" (the
// aws-sdk-go-v2 default checksums are rejected by several S3-compatibles;
// see the velero-plugin-for-aws known-issues list).
func (t Target) onAWS() bool {
	u, err := url.Parse(t.s3URL())
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com")
}

// secretManifest renders the credentials Secret. The single `cloud` key
// holds an AWS-style shared-credentials file, which is the format the
// object-store plugin reads regardless of the actual store vendor.
func (t Target) secretManifest() ([]byte, error) {
	credentials := fmt.Sprintf("[default]\naws_access_key_id=%s\naws_secret_access_key=%s\n", t.AccessKeyID, t.SecretAccessKey)
	return yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      TargetSecretName,
			"namespace": Namespace,
		},
		"type":       "Opaque",
		"stringData": map[string]any{"cloud": credentials},
	})
}

// storageLocationManifest renders the BackupStorageLocation.
func (t Target) storageLocationManifest() ([]byte, error) {
	config := map[string]any{
		"region": t.Region,
		"s3Url":  t.s3URL(),
	}
	if !t.onAWS() {
		config["s3ForcePathStyle"] = "true"
		config["checksumAlgorithm"] = ""
	}
	objectStorage := map[string]any{"bucket": t.Bucket}
	if t.Prefix != "" {
		objectStorage["prefix"] = t.Prefix
	}
	return yaml.Marshal(map[string]any{
		"apiVersion": "velero.io/v1",
		"kind":       "BackupStorageLocation",
		"metadata": map[string]any{
			"name":      StorageLocationName,
			"namespace": Namespace,
		},
		"spec": map[string]any{
			"provider":      "aws",
			"default":       true,
			"objectStorage": objectStorage,
			"credential": map[string]any{
				"name": TargetSecretName,
				"key":  "cloud",
			},
			"config": config,
		},
	})
}

// apply pipes one YAML document set into `kubectl apply -f -`. The content
// travels base64-encoded so nothing needs shell quoting. Deliberately NOT
// the k3s auto-deploy directory: a target is per-cluster configuration, not
// part of the bundle, and its Secret belongs in the datastore, not in a
// world-readable file under /var/lib/rancher. On failure only kubectl's
// stderr is reported, never the document or the command.
func apply(ctx context.Context, r k3s.Runner, what string, doc []byte) error {
	encoded := base64.StdEncoding.EncodeToString(doc)
	res, err := r.Run(ctx, fmt.Sprintf("printf '%%s' %s | base64 -d | sudo -n k3s kubectl apply -f -", encoded))
	if err != nil {
		return fmt.Errorf("apply %s: %w", what, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("apply %s: exit %d: %s", what, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// storageLocationProbe observes the BackupStorageLocation until Velero's own
// validation marks it Available — which it only does after actually reaching
// the bucket with the supplied credentials. status.message carries the
// store's error when it cannot, which is the part that names the fix
// (AccessDenied vs connection refused are different afternoons).
func storageLocationProbe(r k3s.Runner) converge.Probe {
	type bsl struct {
		Status struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"status"`
	}
	object := "backupstoragelocation " + StorageLocationName + " in " + Namespace
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get backupstoragelocation "+StorageLocationName+" -n "+Namespace+" -o json")
		if err != nil {
			return false, converge.State{Object: object, Status: "not found yet"}, err
		}
		var loc bsl
		if err := json.Unmarshal([]byte(out), &loc); err != nil {
			return false, converge.State{Object: object, Status: "unparsable"}, err
		}
		switch loc.Status.Phase {
		case "Available":
			return true, converge.State{Object: object, Status: "Available"}, nil
		case "":
			return false, converge.State{Object: object, Status: "not validated yet", Detail: "velero has not run its store validation cycle yet"}, nil
		default:
			return false, converge.State{Object: object, Status: loc.Status.Phase, Detail: loc.Status.Message}, nil
		}
	}
}

// Configure points the cluster at a backup target and proves it works:
// credentials Secret, BackupStorageLocation, convergence until Velero
// validates the location Available, then the default workload Schedule from
// the manifest. Idempotent — re-running with the same or a corrected target
// re-applies and re-validates.
func Configure(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, t Target, rep converge.Reporter) error {
	if err := t.Validate(); err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}

	secret, err := t.secretManifest()
	if err != nil {
		return err
	}
	if err := apply(ctx, r, "backup target credentials", secret); err != nil {
		return err
	}
	location, err := t.storageLocationManifest()
	if err != nil {
		return err
	}
	if err := apply(ctx, r, "backup storage location", location); err != nil {
		return err
	}

	res, err := converge.Wait(ctx, storageLocationProbe(r), converge.Options{
		Name:     "backup-target-available",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return err
	}

	return EnsureSchedule(ctx, r, bundle, rep)
}

// EnsureSchedule applies the default workload Schedule — cadence and
// retention straight from the manifest (decision E: daily, 14 kept) — and
// converges until Velero validates it Enabled. It exists separately from
// Configure so a future bundle bump can re-assert the schedule without
// re-supplying the target.
func EnsureSchedule(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	workload, err := bundle.Backup.Defaults.Workload()
	if err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	interval := workload.Interval.Duration()
	cron, err := Cron(interval)
	if err != nil {
		return err
	}
	ttl, err := TTL(interval, workload.Keep)
	if err != nil {
		return err
	}
	name := ScheduleName(interval)

	doc, err := yaml.Marshal(map[string]any{
		"apiVersion": "velero.io/v1",
		"kind":       "Schedule",
		"metadata": map[string]any{
			"name":      name,
			"namespace": Namespace,
		},
		"spec": map[string]any{
			"schedule": cron,
			"template": map[string]any{
				"storageLocation": StorageLocationName,
				"ttl":             ttl.String(),
			},
		},
	})
	if err != nil {
		return err
	}
	if err := apply(ctx, r, "workload backup schedule", doc); err != nil {
		return err
	}

	object := "schedule " + name + " in " + Namespace
	res, err := converge.Wait(ctx, func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get schedule "+name+" -n "+Namespace+" -o jsonpath='{.status.phase}'")
		if err != nil {
			return false, converge.State{Object: object, Status: "not found yet"}, err
		}
		phase := strings.TrimSpace(out)
		if phase == "Enabled" {
			return true, converge.State{Object: object, Status: "Enabled"}, nil
		}
		if phase == "" {
			phase = "not validated yet"
		}
		return false, converge.State{Object: object, Status: phase}, nil
	}, converge.Options{
		Name:     "backup-schedule-enabled",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// TakeBackup creates one immediate Backup (kubenest backup now) and waits —
// within the manifest's limits.timeouts.backup — for it to settle. The
// convergence condition is "settled" rather than "completed" because a
// Backup's phase is terminal: once Failed it can never become Completed, and
// waiting out the window would add nothing. A settled-but-not-Completed
// backup is reported as an error naming the phase and Velero's reason.
func TakeBackup(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, name string, rep converge.Reporter) error {
	workload, err := bundle.Backup.Defaults.Workload()
	if err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("backup")
	if err != nil {
		return err
	}
	ttl, err := TTL(workload.Interval.Duration(), workload.Keep)
	if err != nil {
		return err
	}

	doc, err := yaml.Marshal(map[string]any{
		"apiVersion": "velero.io/v1",
		"kind":       "Backup",
		"metadata": map[string]any{
			"name":      name,
			"namespace": Namespace,
		},
		"spec": map[string]any{
			"storageLocation": StorageLocationName,
			"ttl":             ttl.String(),
		},
	})
	if err != nil {
		return err
	}
	if err := apply(ctx, r, "backup "+name, doc); err != nil {
		return err
	}

	res, err := converge.Wait(ctx, backupSettledProbe(r, name), converge.Options{
		Name:     "backup-" + name + "-settled",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return err
	}
	if res.Last.Status != "Completed" {
		return fmt.Errorf("backup %s settled as %s: %s", name, res.Last.Status, res.Last.Detail)
	}
	return nil
}

// backupSettledProbe observes one Backup until it reaches a terminal phase.
func backupSettledProbe(r k3s.Runner, name string) converge.Probe {
	type backup struct {
		Status struct {
			Phase         string `json:"phase"`
			FailureReason string `json:"failureReason"`
			Progress      struct {
				ItemsBackedUp int `json:"itemsBackedUp"`
				TotalItems    int `json:"totalItems"`
			} `json:"progress"`
		} `json:"status"`
	}
	object := "backup " + name + " in " + Namespace
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get backup "+name+" -n "+Namespace+" -o json")
		if err != nil {
			return false, converge.State{Object: object, Status: "not found yet"}, err
		}
		var b backup
		if err := json.Unmarshal([]byte(out), &b); err != nil {
			return false, converge.State{Object: object, Status: "unparsable"}, err
		}
		switch b.Status.Phase {
		case "Completed", "Failed", "PartiallyFailed", "FailedValidation":
			return true, converge.State{Object: object, Status: b.Status.Phase, Detail: b.Status.FailureReason}, nil
		case "":
			return false, converge.State{Object: object, Status: "not started yet"}, nil
		default:
			detail := ""
			if p := b.Status.Progress; p.TotalItems > 0 {
				detail = fmt.Sprintf("%d/%d items", p.ItemsBackedUp, p.TotalItems)
			}
			return false, converge.State{Object: object, Status: b.Status.Phase, Detail: detail}, nil
		}
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
