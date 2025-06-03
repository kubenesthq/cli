package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
)

func NewAppsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "apps",
		Aliases: []string{"app"},
		Short:   "Manage apps for the current team context",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := config.LoadConfig()
			if cfg.TeamUUID == "" {
				return fmt.Errorf("team context must be set. Use 'kubenest context set-team <team>' first.")
			}
			client, err := api.NewClientFromConfig()
			if err != nil {
				return fmt.Errorf("failed to create client: %v", err)
			}
			client.SetTeamUUID(cfg.TeamUUID)
			apps, err := client.ListApps(context.Background())
			if err != nil {
				return fmt.Errorf("failed to list apps: %w", err)
			}
			if len(apps) == 0 {
				fmt.Println("No apps found.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "UUID\tNAME\tPROJECT\tCLUSTER\tSTATUS")
			for _, app := range apps {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", app.UUID, app.Name, app.Project.Name, app.Cluster.Name, app.Status)
			}
			w.Flush()
			return nil
		},
	}

	cmd.AddCommand(NewAppExecCommand())
	cmd.AddCommand(DeployCommand())
	cmd.AddCommand(InfoCommand())
	cmd.AddCommand(CreateCommand())
	cmd.AddCommand(DeleteAppCommand())
	return cmd
}
