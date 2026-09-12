package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/window"
)

// NewClusterCommand groups the per-cluster settings that are not part of an
// install or an upgrade.
func NewClusterCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Settings that belong to one cluster",
	}
	cmd.AddCommand(newSetWindowCommand())
	cmd.AddCommand(newRotateTokenCommand())
	return cmd
}

// newSetWindowCommand sets the ONE maintenance window. Both the bundle
// upgrade and OS reboot orchestration read it: two definitions would mean a
// cluster that reboots at 02:00 and upgrades at 03:00 by different rules.
func newSetWindowCommand() *cobra.Command {
	var (
		cluster string
		spec    window.Spec
	)
	cmd := &cobra.Command{
		Use:   "set-window",
		Short: "Set the cluster's maintenance window",
		Long: `Set the recurring window inside which the platform may act on this cluster.

Upgrades never START outside it, and OS reboots are held for it. The rule when
the window closes mid-operation is that no new stage starts but the stage in
progress finishes — abandoning a half-completed stage to respect a clock
leaves the cluster in a worse state than the overrun does.

The timezone is an IANA name, never an offset: offsets move twice a year, and
a window that silently shifts by an hour is worse than no window at all.`,
		Example: `  kubenest cluster set-window --cluster prod-1 \
    --days sat,sun --start 02:00 --end 06:00 --timezone Asia/Kolkata`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			return runWindow(cmd.Context(), cmd.OutOrStdout(), cluster, spec)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&cluster, "cluster", "", "cluster to configure (required)")
	fs.StringSliceVar(&spec.Days, "days", nil, "days the window opens: mon,tue,wed,thu,fri,sat,sun (required)")
	fs.StringVar(&spec.Start, "start", "", "window start, HH:MM 24-hour (required)")
	fs.StringVar(&spec.End, "end", "", "window end, HH:MM 24-hour; earlier than start means it crosses midnight (required)")
	fs.StringVar(&spec.Timezone, "timezone", "", "IANA timezone name, e.g. Asia/Kolkata or UTC (required)")
	return cmd
}

// newRotateTokenCommand rotates a cluster's agent token AND delivers it.
//
// The endpoint alone is not a hygiene operation: it raises the revocation floor
// and drops the cluster's hub connection, and the new token only reaches the
// agent through chart values. Anyone scheduling a fleet-wide rotation against
// the endpoint would disconnect the fleet and learn about it from the alerts.
// This command owns the order so that rotation ends where an operator expects
// it to — with the cluster connected.
func newRotateTokenCommand() *cobra.Command {
	var f rotateFlags
	cmd := &cobra.Command{
		Use:   "rotate-token",
		Short: "Rotate this cluster's agent token and deliver it",
		Long: `Rotate the cluster's agent JWT, revoke every older one, and deliver the new
token to the cluster.

USE THIS WHEN YOU BELIEVE A TOKEN HAS LEAKED. It is not routine hygiene: the
cluster is disconnected from the moment the floor is raised until the new token
is delivered and the operator reconnects, and this command does not return
success before that happens.

If it fails after the rotation it says so and says the cluster is down. Re-run
the same command to rotate again and finish delivery — there is no half state
that needs a different repair.

The server node is read from this machine's install journal. Pass --server when
running from a machine that did not install the cluster.`,
		Example: `  kubenest cluster rotate-token --cluster prod-1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.Cluster == "" {
				return fmt.Errorf("--cluster is required")
			}
			return runRotate(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.Cluster, "cluster", "", "cluster whose token to rotate (required)")
	fs.StringArrayVar(&f.Servers, "server", nil, "server node address (only needed without a local install journal)")
	fs.StringVar(&f.SSHUser, "ssh-user", "", "SSH user on the server node")
	fs.StringVar(&f.SSHKey, "ssh-key", "", "SSH private key file; defaults to ssh-agent or ~/.ssh/config")
	return cmd
}
