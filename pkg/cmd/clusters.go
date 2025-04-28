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

func NewClustersCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "clusters",
		Short: "List clusters for the current team context",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := config.LoadConfig()
			if cfg.TeamUUID == "" {
				return fmt.Errorf("team context must be set. Use 'kubenest context set-team <team>' first.")
			}
			client, _ := api.NewClient()
			if cfg.Token != "" {
				client.SetToken(cfg.Token)
			}
			client.SetTeamUUID(cfg.TeamUUID)
			clusters, err := client.ListClusters(context.Background())
			if err != nil {
				return fmt.Errorf("failed to list clusters: %w", err)
			}
			if len(clusters) == 0 {
				fmt.Println("No clusters found.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tUUID")
			for _, cluster := range clusters {
				fmt.Fprintf(w, "%s\t%s\n", cluster.Name, cluster.UUID)
			}
			w.Flush()
			return nil
		},
	}
}
