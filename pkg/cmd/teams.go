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

func NewTeamsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "teams",
		Short: "List teams you are a part of",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			client, _ := api.NewClient()
			if cfg.Token != "" {
				client.SetToken(cfg.Token)
			}
			teams, err := client.ListTeams(context.Background())
			if err != nil {
				return fmt.Errorf("failed to list teams: %w", err)
			}
			if len(teams) == 0 {
				fmt.Println("No teams found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tUUID")
			for _, team := range teams {
				fmt.Fprintf(w, "%s\t%s\n", team.Name, team.UUID)
			}
			w.Flush()
			return nil
		},
	}
}
