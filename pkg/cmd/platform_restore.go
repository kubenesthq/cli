package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/sshx"
)

func newPlatformRestoreCommand() *cobra.Command {
	var (
		conn     backupConn
		target   backup.Target
		snapshot string
		confirm  bool
	)
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore the cluster datastore from an S3 snapshot",
		Long: `Restore the embedded-etcd datastore from a k3s snapshot in the configured
S3-compatible target. This is a whole-cluster disaster operation with a
service interruption, not a workload restore.

Every control-plane server is stopped first. The first --server is restored
and becomes the new etcd member; additional servers preserve their stale
database under a root-only recovery path and rejoin serially. Persistent
volume data is deliberately untouched — restore the workload backup after
the Kubernetes datastore is healthy.

S3 credentials are read from KUBENEST_BACKUP_ACCESS_KEY_ID and
KUBENEST_BACKUP_SECRET_ACCESS_KEY (falling back to AWS_ACCESS_KEY_ID and
AWS_SECRET_ACCESS_KEY). They cross SSH only on stdin, live briefly in a
root-readable k3s config, and are removed before k3s restarts.`,
		Example: `  KUBENEST_BACKUP_ACCESS_KEY_ID=… KUBENEST_BACKUP_SECRET_ACCESS_KEY=… \
  kubenest platform restore --cluster prod-1 --confirm \
    --snapshot on-demand-prod-1-1787299200 \
    --endpoint s3.ap-south-1.amazonaws.com \
    --bucket kubenest-backups-prod-1 --region ap-south-1 \
    --server 10.0.1.10 --ssh-user ubuntu \
    --bundle-manifest bundles/platform-1.0.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("datastore restore stops every control-plane server and replaces cluster state: pass --confirm")
			}
			if snapshot == "" {
				return fmt.Errorf("--snapshot is required: use the exact name reported by `k3s etcd-snapshot ls --s3`")
			}
			if err := conn.validate(); err != nil {
				return err
			}
			target.AccessKeyID = envFirst("KUBENEST_BACKUP_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID")
			target.SecretAccessKey = envFirst("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY")
			if err := target.Validate(); err != nil {
				return err
			}

			bundle, primary, err := conn.dial(cmd)
			if err != nil {
				return err
			}
			clients := []*sshx.Client{primary}
			defer func() {
				for _, client := range clients {
					_ = client.Close()
				}
			}()
			servers := []backup.DatastoreServer{{Name: conn.Servers[0], Runner: primary}}
			for _, address := range conn.Servers[1:] {
				ep, err := sshx.Resolve(address, sshx.Options{User: conn.SSHUser, KeyPath: conn.SSHKey})
				if err != nil {
					return err
				}
				client, err := sshx.Dial(cmd.Context(), ep, sshx.Options{KeyPath: conn.SSHKey})
				if err != nil {
					return err
				}
				clients = append(clients, client)
				servers = append(servers, backup.DatastoreServer{Name: address, Runner: client})
			}

			reporter := converge.NewTextReporter(cmd.OutOrStdout())
			if err := backup.RestoreDatastoreSnapshotFromS3(
				cmd.Context(), servers, bundle, target, snapshot, reporter,
			); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "datastore snapshot %s restored on %s; restore workload volume data next\n", snapshot, conn.Cluster)
			return nil
		},
	}
	conn.register(cmd)
	fs := cmd.Flags()
	fs.StringVar(&snapshot, "snapshot", "", "exact k3s S3 snapshot name (required)")
	fs.StringVar(&target.Endpoint, "endpoint", "", "S3-compatible API endpoint (required; no scheme means https)")
	fs.StringVar(&target.Bucket, "bucket", "", "bucket containing the datastore snapshot (required)")
	fs.StringVar(&target.Region, "region", "", "bucket region (required)")
	fs.StringVar(&target.Prefix, "prefix", "", "directory within the bucket used by set-target")
	fs.BoolVar(&confirm, "confirm", false, "confirm service interruption and datastore replacement (required)")
	return cmd
}
