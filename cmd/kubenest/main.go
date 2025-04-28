package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/config"
)

func main() {
	// Initialize configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Printf("Error initializing configuration: %v\n", err)
		os.Exit(1)
	}

	client, _ := api.NewClient()
	if cfg.Token != "" {
		client.SetToken(cfg.Token)
	}
	if cfg.TeamUUID != "" {
		client.SetTeamUUID(cfg.TeamUUID)
	}

	// Create root command
	rootCmd := &cobra.Command{
		Use:   "kubenest",
		Short: "Kubenest CLI",
		Long:  "A command-line interface for managing Kubenest resources",
	}

	// Add commands
	rootCmd.AddCommand(cmd.NewLoginCommand())
	rootCmd.AddCommand(cmd.NewLogoutCommand())
	rootCmd.AddCommand(cmd.NewTeamsCommand())
	rootCmd.AddCommand(cmd.NewClustersCommand())
	rootCmd.AddCommand(cmd.NewProjectsCommand())
	rootCmd.AddCommand(cmd.NewAppsCommand())
	rootCmd.AddCommand(cmd.NewAppLogsCommand())
	rootCmd.AddCommand(cmd.NewContextCommand())

	// Execute root command
	if err := rootCmd.Execute(); err != nil {
		fmt.Printf("Error executing command: %v\n", err)
		os.Exit(1)
	}
}
