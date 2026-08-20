package cmd

import (
	"github.com/spf13/cobra"
)

// NewBackupCommand groups backup operations per the backup and restore page:
// set-target, now, drill, restore. Skeleton surface; implementations land
// with the Velero bead (kn-mzn).
func NewBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up, drill and restore a platform cluster",
	}

	var cluster string
	sub := func(use, short, what string) *cobra.Command {
		c := &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(cmd *cobra.Command, args []string) error {
				return errNotYetImplemented(what)
			},
		}
		c.Flags().StringVar(&cluster, "cluster", "", "cluster name")
		return c
	}

	cmd.AddCommand(
		sub("set-target", "Configure the S3-compatible backup target", "kubenest backup set-target"),
		sub("now", "Take a backup immediately", "kubenest backup now"),
		sub("drill", "Run a verified restore drill", "kubenest backup drill"),
		sub("restore", "Restore from a backup", "kubenest backup restore"),
	)
	return cmd
}
