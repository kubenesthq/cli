package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
)

func NewContextCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage CLI context (team, cluster, project)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := config.LoadConfig()
			client, _ := api.NewClient()
			if cfg.Token != "" {
				client.SetToken(cfg.Token)
			}

			// Print logged-in user info
			userEmail := "(unknown)"
			if cfg.UserEmail != "" {
				userEmail = cfg.UserEmail
				if cfg.UserFirstName != "" || cfg.UserLastName != "" {
					userEmail = fmt.Sprintf("%s %s <%s>", cfg.UserFirstName, cfg.UserLastName, cfg.UserEmail)
				}
			} else if cfg.Token != "" {
				userEmail = "(token present)"
			}
			fmt.Printf("Logged in as: %s\n", userEmail)

			// Print team context
			teamStr := "not set"
			if cfg.TeamUUID != "" {
				teamStr = cfg.TeamUUID
				// Try to resolve team name
				teams, err := client.ListTeams(context.Background())
				if err == nil {
					for _, t := range teams {
						if t.UUID == cfg.TeamUUID {
							teamStr = fmt.Sprintf("%s (%s)", t.Name, t.UUID)
							break
						}
					}
				}
			}
			fmt.Printf("Team:    %s\n", teamStr)

			// Print cluster context
			clusterStr := "not set"
			if cfg.ClusterUUID != "" && cfg.TeamUUID != "" {
				client.SetTeamUUID(cfg.TeamUUID)
				clusters, err := client.ListClusters(context.Background())
				if err == nil {
					for _, c := range clusters {
						if c.UUID == cfg.ClusterUUID {
							clusterStr = fmt.Sprintf("%s (%s)", c.Name, c.UUID)
							break
						}
					}
				}
				if clusterStr == "not set" {
					clusterStr = cfg.ClusterUUID
				}
			}
			fmt.Printf("Cluster: %s\n", clusterStr)

			// Print project context
			projectStr := "not set"
			if cfg.ProjectUUID != "" && cfg.TeamUUID != "" {
				client.SetTeamUUID(cfg.TeamUUID)
				projects, err := client.ListProjects(context.Background())
				if err == nil {
					for _, p := range projects {
						if p.UUID == cfg.ProjectUUID {
							projectStr = fmt.Sprintf("%s (%s)", p.Name, p.UUID)
							break
						}
					}
				}
				if projectStr == "not set" {
					projectStr = cfg.ProjectUUID
				}
			}
			fmt.Printf("Project: %s\n", projectStr)

			return nil
		},
	}
	cmd.AddCommand(newSetTeamCommand())
	cmd.AddCommand(newSetClusterCommand())
	cmd.AddCommand(newSetProjectCommand())
	return cmd
}

func newSetTeamCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set-team <team-name-or-uuid>",
		Short: "Set the current team context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			teamArg := args[0]
			cfg, _ := config.LoadConfig()
			client, _ := api.NewClient()
			if cfg.Token != "" {
				client.SetToken(cfg.Token)
			}
			teams, err := client.ListTeams(context.Background())
			if err != nil {
				return fmt.Errorf("failed to list teams: %w", err)
			}
			var foundUUID string
			for _, team := range teams {
				if team.UUID == teamArg || team.Name == teamArg {
					foundUUID = team.UUID
					break
				}
			}
			if foundUUID == "" {
				return errors.New("team not found")
			}
			cfg.TeamUUID = foundUUID
			config.SaveConfig(cfg)
			fmt.Printf("Team context set to %s\n", foundUUID)
			return nil
		},
	}
}

func newSetClusterCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set-cluster <cluster-name-or-uuid>",
		Short: "Set the current cluster context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clusterArg := args[0]
			cfg, _ := config.LoadConfig()
			if cfg.TeamUUID == "" {
				return errors.New("team context must be set first")
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
			var foundUUID string
			for _, cluster := range clusters {
				if cluster.UUID == clusterArg || cluster.Name == clusterArg {
					foundUUID = cluster.UUID
					break
				}
			}
			if foundUUID == "" {
				return errors.New("cluster not found")
			}
			cfg.ClusterUUID = foundUUID
			config.SaveConfig(cfg)
			fmt.Printf("Cluster context set to %s\n", foundUUID)
			return nil
		},
	}
}

func newSetProjectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set-project <project-name-or-uuid>",
		Short: "Set the current project context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectArg := args[0]
			cfg, _ := config.LoadConfig()
			if cfg.TeamUUID == "" {
				return errors.New("team context must be set first")
			}
			client, _ := api.NewClient()
			if cfg.Token != "" {
				client.SetToken(cfg.Token)
			}
			client.SetTeamUUID(cfg.TeamUUID)
			projects, err := client.ListProjects(context.Background())
			if err != nil {
				return fmt.Errorf("failed to list projects: %w", err)
			}
			var foundUUID string
			for _, project := range projects {
				if project.UUID == projectArg || project.Name == projectArg {
					foundUUID = project.UUID
					break
				}
			}
			if foundUUID == "" {
				return errors.New("project not found")
			}
			cfg.ProjectUUID = foundUUID
			config.SaveConfig(cfg)
			fmt.Printf("Project context set to %s\n", foundUUID)
			return nil
		},
	}
}
