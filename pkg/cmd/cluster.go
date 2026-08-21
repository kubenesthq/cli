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
