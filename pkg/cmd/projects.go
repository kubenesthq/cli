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

func NewProjectsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "projects",
		Short: "List projects for the current team context",
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
			projects, err := client.ListProjects(context.Background())
			if err != nil {
				return fmt.Errorf("failed to list projects: %w", err)
			}
			if len(projects) == 0 {
				fmt.Println("No projects found.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tUUID\tCLUSTER")
			for _, project := range projects {
				fmt.Fprintf(w, "%s\t%s\t%s\n", project.Name, project.UUID, project.Cluster.Name)
			}
			w.Flush()
			return nil
		},
	}
}
