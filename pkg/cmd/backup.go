package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// The backup command surface follows docs.kubenest.io/platform/backup-restore:
// set-target, now, drill, restore. set-target and now are implemented (the
// kn-mzn install half); drill and restore land with the wave-3 half of the
// bead and say so plainly until then.

// backupConn is how a backup command reaches its cluster in wave 1: the same
// SSH transport as platform install. Once the installer's per-cluster record
// lands (kn-7k8), --cluster alone will resolve the server address and the
// bundle, and the transport flags become optional overrides.
type backupConn struct {
	Cluster    string
	Server     string
	SSHUser    string
	SSHKey     string
	BundlePath string
}

func (c *backupConn) register(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&c.Cluster, "cluster", "", "cluster name (required)")
	fs.StringVar(&c.Server, "server", "", "the cluster's server node address (required until the cluster record resolves it)")
	fs.StringVar(&c.SSHUser, "ssh-user", "", "SSH user on the server node")
	fs.StringVar(&c.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	fs.StringVar(&c.BundlePath, "bundle-manifest", "", "path to the cluster's bundle manifest (required until the cluster record resolves it)")
}

func (c *backupConn) validate() error {
	if c.Cluster == "" {
		return fmt.Errorf("--cluster is required")
	}
	if c.Server == "" {
		return fmt.Errorf("--server is required: the cluster record that will resolve --cluster to an address is not built yet")
	}
	if c.BundlePath == "" {
		return fmt.Errorf("--bundle-manifest is required: schedules, retention and deadlines all come from the bundle manifest, never from defaults in this binary")
	}
	return nil
}

// dial loads the manifest and opens the SSH connection. The caller closes.
func (c *backupConn) dial(cmd *cobra.Command) (*manifest.Manifest, *sshx.Client, error) {
	bundle, err := manifest.Load(c.BundlePath)
	if err != nil {
		return nil, nil, err
	}
	ep, err := sshx.Resolve(c.Server, sshx.Options{User: c.SSHUser, KeyPath: c.SSHKey})
	if err != nil {
		return nil, nil, err
	}
	client, err := sshx.Dial(cmd.Context(), ep, sshx.Options{KeyPath: c.SSHKey})
	if err != nil {
		return nil, nil, err
	}
	return bundle, client, nil
}

// NewBackupCommand groups backup operations per the backup and restore page.
func NewBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up, drill and restore a platform cluster",
	}
	cmd.AddCommand(
		newBackupSetTargetCommand(),
		newBackupNowCommand(),
		newBackupSkeletonCommand("drill", "Run a verified restore drill", "kubenest backup drill"),
		newBackupSkeletonCommand("restore", "Restore from a backup", "kubenest backup restore"),
	)
	return cmd
}

func newBackupSetTargetCommand() *cobra.Command {
	var (
		conn   backupConn
		target backup.Target
	)
	cmd := &cobra.Command{
		Use:   "set-target",
		Short: "Configure the S3-compatible backup target",
		Long: `Point the cluster's backups at an S3-compatible bucket you supply and
control — AWS S3, MinIO, Ceph, anything speaking the S3 API.

Credentials are read from KUBENEST_BACKUP_ACCESS_KEY_ID and
KUBENEST_BACKUP_SECRET_ACCESS_KEY (falling back to AWS_ACCESS_KEY_ID and
AWS_SECRET_ACCESS_KEY), never from flags, so they cannot land in shell
history. They travel only over the SSH connection to your own server node.

The target is proven, not assumed: the command waits until Velero has
reached the bucket with those credentials and marked the location
Available, then installs the default backup schedule from the bundle
manifest. Until a target is set the cluster reports backup: unconfigured —
loud, but never blocking.`,
		Example: `  KUBENEST_BACKUP_ACCESS_KEY_ID=… KUBENEST_BACKUP_SECRET_ACCESS_KEY=… \
  kubenest backup set-target --cluster prod-1 \
    --endpoint s3.ap-south-1.amazonaws.com \
    --bucket kubenest-backups-prod-1 \
    --region ap-south-1 \
    --server 10.0.1.10 --ssh-user ubuntu \
    --bundle-manifest bundles/platform-1.0.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := conn.validate(); err != nil {
				return err
			}
			target.AccessKeyID = envFirst("KUBENEST_BACKUP_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID")
			target.SecretAccessKey = envFirst("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY")
			if err := target.Validate(); err != nil {
				return err
			}
			bundle, client, err := conn.dial(cmd)
			if err != nil {
				return err
			}
			defer client.Close()
			rep := converge.NewTextReporter(cmd.OutOrStdout())
			if err := backup.Configure(cmd.Context(), client, bundle, target, rep); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backup target for %s configured and verified: bucket %s via %s\n", conn.Cluster, target.Bucket, target.Endpoint)
			return nil
		},
	}
	conn.register(cmd)
	fs := cmd.Flags()
	fs.StringVar(&target.Endpoint, "endpoint", "", "S3-compatible API endpoint (required; no scheme means https)")
	fs.StringVar(&target.Bucket, "bucket", "", "bucket to store backups in (required)")
	fs.StringVar(&target.Region, "region", "", "bucket region (required)")
	fs.StringVar(&target.Prefix, "prefix", "", "directory within the bucket (optional)")
	return cmd
}

func newBackupNowCommand() *cobra.Command {
	var conn backupConn
	cmd := &cobra.Command{
		Use:   "now",
		Short: "Take a backup immediately",
		Long: `Take one workload backup right now, outside the schedule, and wait for it
to complete — within the bundle manifest's limits.timeouts.backup. A backup
that settles as anything but Completed is an error naming Velero's reason,
not a silent log line.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := conn.validate(); err != nil {
				return err
			}
			bundle, client, err := conn.dial(cmd)
			if err != nil {
				return err
			}
			defer client.Close()
			name := "manual-" + time.Now().UTC().Format("20060102-150405")
			rep := converge.NewTextReporter(cmd.OutOrStdout())
			if err := backup.TakeBackup(cmd.Context(), client, bundle, name, rep); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backup %s completed on %s\n", name, conn.Cluster)
			return nil
		},
	}
	conn.register(cmd)
	return cmd
}

// newBackupSkeletonCommand marks the wave-3 half of kn-mzn: the scheduled
// verified restore drill and restores need a running cluster with workloads
// and land with that wave. Non-zero exit, per the skeleton rule.
func newBackupSkeletonCommand(use, short, what string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			return errNotYetImplemented(what)
		},
	}
	cmd.Flags().String("cluster", "", "cluster name")
	return cmd
}

// envFirst returns the first set environment variable of the names given.
func envFirst(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}
