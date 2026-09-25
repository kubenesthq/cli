package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/s3"
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
	Servers    []string
	SSHUser    string
	SSHKey     string
	BundlePath string
}

func (c *backupConn) register(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&c.Cluster, "cluster", "", "cluster name (required)")
	fs.StringArrayVar(&c.Servers, "server", nil, "control-plane node address (repeat for every server; required until the cluster record resolves it)")
	fs.StringVar(&c.SSHUser, "ssh-user", "", "SSH user on the server node")
	fs.StringVar(&c.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	fs.StringVar(&c.BundlePath, "bundle-manifest", "", "path to the cluster's bundle manifest (required until the cluster record resolves it)")
}

func (c *backupConn) validate() error {
	if c.Cluster == "" {
		return fmt.Errorf("--cluster is required")
	}
	if len(c.Servers) == 0 {
		return fmt.Errorf("at least one --server is required: the cluster record that will resolve --cluster to addresses is not built yet")
	}
	if c.BundlePath == "" {
		return fmt.Errorf("--bundle-manifest is required: schedules, retention and deadlines all come from the bundle manifest, never from defaults in this binary")
	}
	return nil
}

// validateControlPlane is the control-plane run's own required set, and it is
// deliberately not validate() with a case relaxed.
//
// --cluster is REFUSED rather than optional, because the subject of
// `backup now --control-plane` is not a workload cluster at all: the control
// plane runs in the management cluster that --server already addresses, and a
// cluster name would only ever be used to select a workload cluster's backups
// (and its recovery set). Accepting one and ignoring it would leave an operator
// believing the command had been scoped to a cluster the run never looked at.
//
// What remains is what the run needs: the host to reach and the bundle manifest
// to bound the wait with.
func (c *backupConn) validateControlPlane() error {
	if c.Cluster != "" {
		return fmt.Errorf("--cluster does not apply to --control-plane: the control plane runs in the management cluster that --server addresses, and this command backs up the control plane rather than a workload cluster's backups. Drop --cluster")
	}
	if len(c.Servers) == 0 {
		return fmt.Errorf("at least one --server is required: it addresses the management cluster the control plane runs in (the cluster record that will resolve it is not built yet)")
	}
	if c.BundlePath == "" {
		return fmt.Errorf("--bundle-manifest is required: the wait for the checkpoint is bounded by the bundle's component-ready timeout, and deadlines come from the bundle manifest, never from defaults in this binary")
	}
	return nil
}

// dial loads the manifest and opens the SSH connection. The caller closes.
func (c *backupConn) dial(cmd *cobra.Command) (*manifest.Manifest, *sshx.Client, error) {
	bundle, err := manifest.Load(c.BundlePath)
	if err != nil {
		return nil, nil, err
	}
	ep, err := sshx.Resolve(c.Servers[0], sshx.Options{User: c.SSHUser, KeyPath: c.SSHKey})
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
		newBackupDrillCommand(),
		newBackupSkeletonCommand("restore", "Restore from a backup", "kubenest backup restore",
			"workload restores need a running cluster with workloads; they land with the wave-3 half of kn-mzn"),
	)
	return cmd
}

func newBackupDrillCommand() *cobra.Command {
	var conn backupConn
	cmd := &cobra.Command{
		Use:   "drill",
		Short: "Run the verified latest-backup restore drill now",
		Long: `Request the same real restore path the weekly scheduler uses: the newest
completed Velero backup is restored into an isolated scratch namespace, the
proof objects and PVC bytes are compared, cleanup is awaited, and the shared
fleet result is updated. A failed drill exits non-zero with its actionable
stage and reason code.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := conn.validate(); err != nil {
				return err
			}
			bundle, client, err := conn.dial(cmd)
			if err != nil {
				return err
			}
			defer client.Close()
			result, err := backup.RequestDrill(cmd.Context(), client, bundle, converge.NewTextReporter(cmd.OutOrStdout()))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restore drill passed for backup %s at %s\n", result.Backup, result.CompletedAt)
			return nil
		},
	}
	conn.register(cmd)
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
manifest. It also configures every supplied k3s server for scheduled embedded-
etcd snapshots, restarts changed servers serially, and proves an S3 snapshot
upload before returning. Until a target is set the cluster reports backup:
unconfigured — loud, but never blocking.`,
		Example: `  KUBENEST_BACKUP_ACCESS_KEY_ID=… KUBENEST_BACKUP_SECRET_ACCESS_KEY=… \
  kubenest backup set-target --cluster prod-1 \
    --endpoint s3.ap-south-1.amazonaws.com \
    --bucket kubenest-backups-prod-1 \
    --region ap-south-1 \
    --server 10.0.1.10 --ssh-user ubuntu \
    --bundle-manifest bundles/platform-1.1.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := conn.validate(); err != nil {
				return err
			}
			target.AccessKeyID = envFirst("KUBENEST_BACKUP_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID")
			target.SecretAccessKey = envFirst("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY")
			if err := target.Validate(); err != nil {
				return err
			}
			// The client the scope check and the pre-flight read use. Without
			// it, Configure would skip the scope check entirely — and the
			// check is the thing that keeps one cluster's credential from
			// being read access to every cluster's backups and to the control
			// plane's recovery material.
			client, err := target.S3Client()
			if err != nil {
				return err
			}
			target.Client = client

			out := cmd.OutOrStdout()
			bundle, ssh, err := conn.dial(cmd)
			if err != nil {
				return err
			}
			defer ssh.Close()
			rep := converge.NewTextReporter(out)

			// What the bucket does and does not protect, before anything
			// depends on it. Warnings, never refusals: an unknown storage
			// choice must not block an operator.
			for _, warning := range target.Preflight(cmd.Context(), target.Client) {
				fmt.Fprintf(out, "warning: %s\n", warning)
			}

			if err := backup.Configure(cmd.Context(), ssh, bundle, target, rep); err != nil {
				return err
			}
			if err := backup.ConfigureDatastoreSnapshots(cmd.Context(), ssh, bundle, target, rep); err != nil {
				return fmt.Errorf("configure datastore snapshots on %s: %w", conn.Servers[0], err)
			}
			for _, address := range conn.Servers[1:] {
				ep, err := sshx.Resolve(address, sshx.Options{User: conn.SSHUser, KeyPath: conn.SSHKey})
				if err != nil {
					return err
				}
				node, err := sshx.Dial(cmd.Context(), ep, sshx.Options{KeyPath: conn.SSHKey})
				if err != nil {
					return err
				}
				configureErr := backup.ConfigureDatastoreSnapshots(cmd.Context(), node, bundle, target, rep)
				closeErr := node.Close()
				if configureErr != nil {
					return fmt.Errorf("configure datastore snapshots on %s: %w", address, configureErr)
				}
				if closeErr != nil {
					return fmt.Errorf("close SSH connection to %s: %w", address, closeErr)
				}
			}
			fmt.Fprintf(out, "backup target for %s configured and verified: bucket %s via %s\n", conn.Cluster, target.Bucket, target.Endpoint)
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
	var (
		conn         backupConn
		controlPlane bool
	)
	cmd := &cobra.Command{
		Use:   "now",
		Short: "Take a backup immediately",
		Long: `Take one workload backup right now, outside the schedule, and wait for it
to complete — within the bundle manifest's limits.timeouts.backup. A backup
that settles as anything but Completed is an error naming Velero's reason,
not a silent log line.

With --control-plane the subject is the control plane's OWN recovery point.
A Job is created from the checkpoint CronJob the control-plane chart installs,
and the command waits until the checkpoint it produced is ELIGIBLE: the runner
uploads the sealed dump and its manifest, reads both back, and only then
publishes the marker, so a Job that finished without a new eligible checkpoint
is reported as the interrupted run it is rather than as a backup. The wait is
bounded by the bundle's component-ready timeout, and --cluster does not apply —
the control plane runs in the management cluster that --server addresses.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if controlPlane {
				return runControlPlaneNow(cmd, conn)
			}
			if err := conn.validate(); err != nil {
				return err
			}
			bundle, client, err := conn.dial(cmd)
			if err != nil {
				return err
			}
			defer client.Close()
			name := "manual-" + time.Now().UTC().Format("20060102-150405")
			out := cmd.OutOrStdout()
			rep := converge.NewTextReporter(out)
			if err := backup.TakeBackup(cmd.Context(), client, bundle, name, rep); err != nil {
				return err
			}
			// The backup is in the bucket; now it has to be FINDABLE. A
			// completed backup that no recovery set names is a backup nobody
			// can select on the day it matters.
			if err := recordRecoverySet(cmd.Context(), out, conn.Cluster, name); err != nil {
				return err
			}
			fmt.Fprintf(out, "backup %s completed on %s\n", name, conn.Cluster)
			return nil
		},
	}
	conn.register(cmd)
	cmd.Flags().BoolVar(&controlPlane, "control-plane", false, "back up the control plane itself: create a Job from the chart's checkpoint CronJob and wait until the checkpoint is eligible (no --cluster)")
	return cmd
}

// runControlPlaneNow takes one control-plane checkpoint on demand.
//
// THE SAME TRANSPORT AS EVERY OTHER BACKUP COMMAND: the Job is created on the
// management cluster over SSH through the same `k3s kubectl` the install uses,
// so no kubeconfig and no cluster-admin credential has to exist on the
// operator's machine.
//
// The Job is derived from the chart's CronJob rather than rendered here — see
// pkg/controlplane.OnDemandCheckpoint — so the image digest, the scratch volume
// and the fleet recipient are the chart's, and an on-demand checkpoint cannot
// differ from a scheduled one.
func runControlPlaneNow(cmd *cobra.Command, conn backupConn) error {
	if err := conn.validateControlPlane(); err != nil {
		return err
	}
	bundle, client, err := conn.dial(cmd)
	if err != nil {
		return err
	}
	defer client.Close()
	// THE CONTROL PLANE'S OWN IDENTITY IS CHECKED HERE, after the transport is
	// up and before the checkpoint Job is created. This command's subject is
	// the control plane rather than a workload cluster, so it is the backup
	// entry point that needs the control plane — and the check is what stops an
	// operator taking a checkpoint of a control plane this CLI cannot describe.
	if err := checkControlPlaneNow(cmd.Context(), "backup"); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	run, err := controlplane.OnDemandCheckpoint(cmd.Context(), client, bundle, converge.NewTextReporter(out))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "control-plane checkpoint %s is eligible: job %s, %d bytes sealed at %s\n",
		run.Checkpoint.Key, run.Job, run.Checkpoint.SizeBytes, run.Checkpoint.At)
	return nil
}

// newBackupSkeletonCommand marks the wave-3 half of kn-mzn: the scheduled
// verified restore drill and restores need a running cluster with workloads
// and land with that wave. Non-zero exit, per the skeleton rule, and
// AnnotationUnavailable so the command metadata reports the path as a stub.
func newBackupSkeletonCommand(use, short, what, reason string) *cobra.Command {
	cmd := &cobra.Command{
		Use:         use,
		Short:       short,
		Annotations: map[string]string{AnnotationUnavailable: reason},
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

// recordRecoverySet adds one completed backup to the cluster's recovery set,
// and writes the set back.
//
// It needs no new flags. The install journal names the cluster's immutable id,
// and the local recovery kit carries the artifact id, the S3 location and the
// scope — so the CLI can write a set for a backup it just took without being
// told any of it again.
//
// When there is no journal or no local kit it says so and continues rather
// than failing: the backup itself succeeded, and the honest report is that
// nothing will be able to find it. That report is loud, because a bucket full
// of unselectable backups is the failure this is meant to prevent.
func recordRecoverySet(ctx context.Context, out io.Writer, clusterName, backupName string) error {
	journalPath, err := install.JournalPath(clusterName)
	if err != nil {
		return err
	}
	journal, err := install.ReadJournal(journalPath)
	if err != nil || journal == nil || journal.ClusterID == "" {
		fmt.Fprintf(out, "warning: no install journal for %q on this machine, so no recovery set was written for %s. Run `kubenest recovery-kit check` and record it from wherever the journal is\n", clusterName, backupName)
		return nil
	}
	kit, err := recoverykit.NewestLocal(journal.ClusterID, recoverykit.KindCluster)
	if err != nil {
		fmt.Fprintf(out, "warning: %v; no recovery set was written for %s, so nothing will be able to select it\n", err, backupName)
		return nil
	}
	doc, err := kit.Document()
	if err != nil {
		return err
	}
	scope := strings.Trim(kit.S3Location.Prefix, "/")
	accessKeyID := envFirst("KUBENEST_BACKUP_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID")
	secretAccessKey := envFirst("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY")
	if accessKeyID == "" || secretAccessKey == "" {
		// The backup itself succeeded. Failing here would turn a good backup
		// into a failed command over a set nobody asked for, so this is a
		// warning that names exactly what will not be findable.
		fmt.Fprintf(out, "warning: no bucket credentials in the environment (KUBENEST_BACKUP_ACCESS_KEY_ID / KUBENEST_BACKUP_SECRET_ACCESS_KEY), so no recovery set was written for %s and nothing will be able to select it\n", backupName)
		return nil
	}
	client, err := s3.New(s3.Config{
		Endpoint:        kit.S3Location.Endpoint,
		Bucket:          kit.S3Location.Bucket,
		Region:          kit.S3Location.Region,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	})
	if err != nil {
		fmt.Fprintf(out, "warning: %v; no recovery set was written for %s, so nothing will be able to select it\n", err, backupName)
		return nil
	}

	key := recoverykit.SetKey(scope, journal.ClusterID, recoverykit.KindCluster, kit.ArtifactID)
	var set *recoverykit.Set
	switch raw, err := client.Get(ctx, key); {
	case err == nil:
		set, err = recoverykit.LoadSet(raw)
		if err != nil {
			return err
		}
	case errors.Is(err, s3.ErrNotFound):
		set, err = recoverykit.NewSet(kit, versionsOf(journal), recoverykit.Digest(doc), time.Now().UTC())
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("reading the recovery set at %s: %w", key, err)
	}
	set, err = set.WithBackup(recoverykit.Backup{Name: backupName, Status: "Completed", CompletedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	set.Complete = true
	body, err := set.Document()
	if err != nil {
		return err
	}
	if err := client.Put(ctx, key, body); err != nil {
		return fmt.Errorf("writing the recovery set at %s: %w", key, err)
	}
	fmt.Fprintf(out, "recovery set updated: %s now names %d completed backup(s)\n", key, len(set.Backups))
	return nil
}

// versionsOf reads the versions a recovery needs out of the install journal's
// identity, and nothing else: a version is not a credential.
func versionsOf(journal *install.Journal) map[string]string {
	versions := map[string]string{}
	for _, key := range []string{"bundle"} {
		if v := journal.Identity.Fields[key]; v != "" {
			versions[key] = v
		}
	}
	return versions
}
