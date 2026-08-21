package backup

import (
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

const datastoreRestoreConfigPath = "/etc/rancher/k3s/config.yaml.d/40-kubenest-restore.yaml"

var snapshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// DatastoreServer is one embedded-etcd member participating in a restore.
// The first server passed to RestoreDatastoreSnapshotFromS3 is the member
// whose database is restored; peers discard their stale membership and join
// it again after the reset.
type DatastoreServer struct {
	Name   string
	Runner k3s.Runner
}

type inputRunner interface {
	k3s.Runner
	RunInput(context.Context, string, io.Reader) (sshx.Result, error)
}

// RestoreDatastoreSnapshotFromS3 performs the documented whole-cluster
// disaster restore. Credentials cross SSH only on stdin and exist on the
// server only in a root-readable temporary k3s drop-in. The Kubernetes
// Secret cannot be used here because the API server is the thing being
// restored.
func RestoreDatastoreSnapshotFromS3(
	ctx context.Context,
	servers []DatastoreServer,
	bundle *manifest.Manifest,
	target Target,
	snapshot string,
	rep converge.Reporter,
) (returnErr error) {
	if len(servers) == 0 || servers[0].Runner == nil {
		return fmt.Errorf("datastore restore needs at least one control-plane server")
	}
	if !snapshotNamePattern.MatchString(snapshot) {
		return fmt.Errorf("snapshot %q is not a safe k3s snapshot name", snapshot)
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if _, err := bundle.Limits.Timeouts.For("node-ready"); err != nil {
		return err
	}

	config, err := target.datastoreRestoreConfig()
	if err != nil {
		return err
	}
	primary := servers[0]
	if err := writeDatastoreRestoreConfig(ctx, primary.Runner, config); err != nil {
		return err
	}
	configPresent := true
	defer func() {
		if !configPresent {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := removeDatastoreRestoreConfig(cleanupCtx, primary.Runner); err != nil && returnErr == nil {
			returnErr = err
		}
	}()

	stopped := make([]DatastoreServer, 0, len(servers))
	for _, server := range servers {
		if server.Runner == nil {
			restartServers(ctx, stopped)
			return fmt.Errorf("datastore restore server %q has no SSH runner", server.Name)
		}
		if err := runServiceCommand(ctx, server, "stop"); err != nil {
			restartServers(ctx, stopped)
			return err
		}
		stopped = append(stopped, server)
	}

	res, err := primary.Runner.Run(ctx,
		"sudo -n k3s server --cluster-reset --cluster-reset-restore-path="+snapshot)
	if err != nil {
		return fmt.Errorf("restore datastore snapshot on %s: %w; all k3s servers remain stopped", primary.Name, err)
	}
	if res.ExitCode != 0 && !strings.Contains(res.Stdout+res.Stderr, "has been reset") {
		return fmt.Errorf("restore datastore snapshot on %s: exit %d; all k3s servers remain stopped", primary.Name, res.ExitCode)
	}

	if err := removeDatastoreRestoreConfig(ctx, primary.Runner); err != nil {
		return fmt.Errorf("remove temporary datastore restore credentials before restart: %w; all k3s servers remain stopped", err)
	}
	configPresent = false

	// The restored primary has reset membership to itself. Preserve each
	// peer database under an exact root-only path before starting it, so the
	// operation is recoverable until the operator verifies the new quorum.
	recoverySuffix := time.Now().UTC().Format("20060102T150405Z")
	for _, peer := range servers[1:] {
		recoveryPath := "/var/lib/rancher/k3s/server/db.kubenest-before-restore-" + recoverySuffix
		command := "sudo -n test -d /var/lib/rancher/k3s/server/db && " +
			"sudo -n test ! -e " + recoveryPath + " && " +
			"sudo -n mv /var/lib/rancher/k3s/server/db " + recoveryPath
		res, err := peer.Runner.Run(ctx, command)
		if err != nil || res.ExitCode != 0 {
			// The primary can still return as a one-member control plane. Do not
			// start a peer with stale membership.
			primaryErr := startAndWait(ctx, primary, bundle, rep)
			if primaryErr != nil {
				return fmt.Errorf("preserve stale etcd database on %s and restart restored primary: %v; peers remain stopped", peer.Name, primaryErr)
			}
			if err != nil {
				return fmt.Errorf("preserve stale etcd database on %s: %w; peer remains stopped", peer.Name, err)
			}
			return fmt.Errorf("preserve stale etcd database on %s: exit %d; peer remains stopped", peer.Name, res.ExitCode)
		}
	}

	if err := startAndWait(ctx, primary, bundle, rep); err != nil {
		return err
	}
	for _, peer := range servers[1:] {
		if err := startAndWait(ctx, peer, bundle, rep); err != nil {
			return err
		}
	}
	return nil
}

func (t Target) datastoreRestoreConfig() ([]byte, error) {
	u, err := t.parsedS3URL()
	if err != nil {
		return nil, err
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("--endpoint %q contains a path; put object prefixes in --prefix", t.Endpoint)
	}
	values := map[string]any{
		"etcd-s3":                 true,
		"etcd-s3-endpoint":        u.Host,
		"etcd-s3-access-key":      t.AccessKeyID,
		"etcd-s3-secret-key":      t.SecretAccessKey,
		"etcd-s3-bucket":          t.Bucket,
		"etcd-s3-folder":          path.Join(strings.Trim(t.Prefix, "/"), "datastore"),
		"etcd-s3-region":          t.Region,
		"etcd-s3-insecure":        u.Scheme == "http",
		"etcd-s3-skip-ssl-verify": false,
		"etcd-s3-timeout":         "5m",
	}
	if !t.onAWS() {
		values["etcd-s3-bucket-lookup-type"] = "path"
	}
	return yaml.Marshal(values)
}

func writeDatastoreRestoreConfig(ctx context.Context, r k3s.Runner, config []byte) error {
	secure, ok := r.(inputRunner)
	if !ok {
		return fmt.Errorf("datastore restore refuses to put S3 credentials in a command line: the SSH runner must support stdin")
	}
	command := "sudo -n install -d -m 0755 /etc/rancher/k3s/config.yaml.d && " +
		"sudo -n tee " + datastoreRestoreConfigPath + " >/dev/null && " +
		"sudo -n chmod 0600 " + datastoreRestoreConfigPath
	res, err := secure.RunInput(ctx, command, strings.NewReader(string(config)))
	if err != nil {
		return fmt.Errorf("write temporary datastore restore configuration: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write temporary datastore restore configuration: exit %d", res.ExitCode)
	}
	return nil
}

func removeDatastoreRestoreConfig(ctx context.Context, r k3s.Runner) error {
	res, err := r.Run(ctx, "sudo -n rm -f "+datastoreRestoreConfigPath)
	if err != nil {
		return fmt.Errorf("remove temporary datastore restore configuration: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("remove temporary datastore restore configuration: exit %d", res.ExitCode)
	}
	return nil
}

func runServiceCommand(ctx context.Context, server DatastoreServer, action string) error {
	res, err := server.Runner.Run(ctx, "sudo -n systemctl "+action+" k3s")
	if err != nil {
		return fmt.Errorf("%s k3s on %s: %w", action, server.Name, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s k3s on %s: exit %d", action, server.Name, res.ExitCode)
	}
	return nil
}

func restartServers(ctx context.Context, servers []DatastoreServer) {
	restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	for _, server := range servers {
		_, _ = server.Runner.Run(restartCtx, "sudo -n systemctl start k3s")
	}
}

func startAndWait(ctx context.Context, server DatastoreServer, bundle *manifest.Manifest, rep converge.Reporter) error {
	if err := runServiceCommand(ctx, server, "start"); err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("node-ready")
	if err != nil {
		return err
	}
	result, err := converge.Wait(ctx, func(ctx context.Context) (bool, converge.State, error) {
		res, runErr := server.Runner.Run(ctx,
			"sudo -n systemctl is-active k3s >/dev/null && sudo -n k3s kubectl get --raw=/readyz >/dev/null")
		if runErr != nil {
			return false, converge.State{Object: "k3s server " + server.Name, Status: "unobservable"}, runErr
		}
		if res.ExitCode == 0 {
			return true, converge.State{Object: "k3s server " + server.Name, Status: "Ready"}, nil
		}
		return false, converge.State{Object: "k3s server " + server.Name, Status: "starting"}, nil
	}, converge.Options{Name: "k3s-ready-after-datastore-restore-" + server.Name, Deadline: deadline, Reporter: rep})
	if err != nil {
		return err
	}
	return result.Err()
}
