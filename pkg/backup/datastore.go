package backup

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

const (
	// DatastoreSecretName is read by k3s at each snapshot operation. Keeping
	// credentials in a Secret means target rotation does not require another
	// k3s restart; only the non-secret config that enables the Secret does.
	DatastoreSecretName = "kubenest-etcd-snapshot-s3"
	datastoreConfigPath = "/etc/rancher/k3s/config.yaml.d/30-kubenest-backup.yaml"
)

// datastoreSecretManifest renders the S3 configuration format supported by
// k3s. Restore is the one exception: while the API server is down, the
// restore runbook supplies the same values through a temporary root-only
// config file because the Secret cannot be read then.
func (t Target) datastoreSecretManifest(keep int) ([]byte, error) {
	u, err := t.parsedS3URL()
	if err != nil {
		return nil, err
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("--endpoint %q contains a path; put object prefixes in --prefix so k3s and Velero share one endpoint", t.Endpoint)
	}

	folder := t.backupPrefix("datastore")
	values := map[string]string{
		"etcd-s3-endpoint":        u.Host,
		"etcd-s3-access-key":      t.AccessKeyID,
		"etcd-s3-secret-key":      t.SecretAccessKey,
		"etcd-s3-bucket":          t.Bucket,
		"etcd-s3-folder":          folder,
		"etcd-s3-region":          t.Region,
		"etcd-s3-insecure":        strconv.FormatBool(u.Scheme == "http"),
		"etcd-s3-skip-ssl-verify": "false",
		"etcd-s3-timeout":         "5m",
		"etcd-s3-retention":       strconv.Itoa(keep),
	}
	if !t.onAWS() {
		values["etcd-s3-bucket-lookup-type"] = "path"
	}

	return yaml.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      DatastoreSecretName,
			"namespace": "kube-system",
		},
		"type":       "etcd.k3s.cattle.io/s3-config-secret",
		"stringData": values,
	})
}

func (t Target) parsedS3URL() (*url.URL, error) {
	u, err := url.Parse(t.s3URL())
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("--endpoint %q is not a usable S3 endpoint", t.Endpoint)
	}
	return u, nil
}

// ConfigureDatastoreSnapshots enables the one datastore backup mechanism on
// a k3s server: embedded-etcd snapshots, hourly/24-kept in Platform 1.0.
// The cadence and retention come from the manifest. An on-demand snapshot is
// uploaded before returning, so set-target proves this path as well as Velero.
func ConfigureDatastoreSnapshots(
	ctx context.Context,
	r k3s.Runner,
	bundle *manifest.Manifest,
	t Target,
	rep converge.Reporter,
) error {
	if err := t.Validate(); err != nil {
		return err
	}
	schedule, err := bundle.Backup.Defaults.Datastore()
	if err != nil {
		return err
	}
	cron, err := Cron(schedule.Interval.Duration())
	if err != nil {
		return err
	}

	secret, err := t.datastoreSecretManifest(schedule.Keep)
	if err != nil {
		return err
	}
	if err := apply(ctx, r, "etcd snapshot S3 credentials", secret); err != nil {
		return err
	}

	config, err := yaml.Marshal(map[string]any{
		"etcd-snapshot-schedule-cron": cron,
		"etcd-snapshot-retention":     schedule.Keep,
		"etcd-s3":                     true,
		"etcd-s3-config-secret":       DatastoreSecretName,
	})
	if err != nil {
		return err
	}
	changed, err := writeDatastoreConfig(ctx, r, config)
	if err != nil {
		return err
	}
	if changed {
		res, err := r.Run(ctx, "sudo -n systemctl restart k3s")
		if err != nil {
			return fmt.Errorf("restart k3s for datastore snapshot configuration: %w", err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("restart k3s for datastore snapshot configuration: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
		}
	}

	deadline, err := bundle.Limits.Timeouts.For("node-ready")
	if err != nil {
		return err
	}
	ready, err := converge.Wait(ctx, func(ctx context.Context) (bool, converge.State, error) {
		res, runErr := r.Run(ctx, "sudo -n systemctl is-active k3s >/dev/null && sudo -n k3s kubectl get --raw=/readyz >/dev/null")
		if runErr != nil {
			return false, converge.State{Object: "k3s server", Status: "unobservable"}, runErr
		}
		if res.ExitCode == 0 {
			return true, converge.State{Object: "k3s server", Status: "Ready"}, nil
		}
		return false, converge.State{Object: "k3s server", Status: "restarting", Detail: firstLine(res.Stderr)}, nil
	}, converge.Options{Name: "k3s-ready-after-etcd-s3", Deadline: deadline, Reporter: rep})
	if err != nil {
		return err
	}
	if err := ready.Err(); err != nil {
		return err
	}

	proof := "kubenest-target-proof"
	res, err := r.Run(ctx, "sudo -n k3s etcd-snapshot save --name "+proof)
	if err != nil {
		return fmt.Errorf("prove datastore snapshot upload: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("prove datastore snapshot upload: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// writeDatastoreConfig atomically installs only the non-secret k3s settings.
// It reports whether k3s needs a restart; an identical set-target is a no-op.
func writeDatastoreConfig(ctx context.Context, r k3s.Runner, config []byte) (bool, error) {
	encoded := base64.StdEncoding.EncodeToString(config)
	const tmp = "/run/kubenest/etcd-s3-config.yaml"
	cmd := fmt.Sprintf(
		"sudo -n install -d -m 0700 /run/kubenest && "+
			"printf '%%s' %s | base64 -d | sudo -n tee %s >/dev/null && "+
			"sudo -n install -d -m 0755 /etc/rancher/k3s/config.yaml.d && "+
			"if sudo -n test -f %s && sudo -n cmp -s %s %s; then printf unchanged; "+
			"else sudo -n install -m 0600 %s %s && printf changed; fi; "+
			"status=$?; sudo -n rm -f %s; exit $status",
		encoded, tmp, datastoreConfigPath, tmp, datastoreConfigPath, tmp, datastoreConfigPath, tmp,
	)
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return false, fmt.Errorf("write datastore snapshot configuration: %w", err)
	}
	if res.ExitCode != 0 {
		return false, fmt.Errorf("write datastore snapshot configuration: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	switch strings.TrimSpace(res.Stdout) {
	case "changed":
		return true, nil
	case "unchanged":
		return false, nil
	default:
		return false, fmt.Errorf("write datastore snapshot configuration: unexpected result %q", strings.TrimSpace(res.Stdout))
	}
}
