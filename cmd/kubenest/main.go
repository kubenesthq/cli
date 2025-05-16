package main

import (
	"fmt"
	"os"

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
	rootCmd := cmd.NewRootCommand()

	// Execute root command
	if err := rootCmd.Execute(); err != nil {
		fmt.Printf("Error executing command: %v\n", err)
		os.Exit(1)
	}
}
