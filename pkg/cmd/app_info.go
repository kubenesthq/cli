package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
)

// InfoCommand creates the 'info' subcommand for apps
func InfoCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info <app-name>",
		Short: "Show information about an app and its components",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appName := args[0]
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

			var targetApp *api.StackDeployApp
			for _, app := range apps {
				if app.Name == appName {
					if cfg.ClusterUUID != "" && app.Cluster.UUID != cfg.ClusterUUID {
						continue
					}
					if cfg.ProjectUUID != "" && app.Project.UUID != cfg.ProjectUUID {
						continue
					}
					targetApp = &app
					break
				}
			}
			if targetApp == nil {
				return fmt.Errorf("app not found: %s", appName)
			}

			// Fetch stackdeploy details (with components)
			stackdeploy, err := client.GetStackDeployDetailWithComponents(context.Background(), targetApp.UUID)
			if err != nil {
				return fmt.Errorf("failed to get stackdeploy details: %w", err)
			}

			fmt.Printf("App: %s\n", stackdeploy.Name)
			if targetApp != nil {
				fmt.Printf("UUID: %s\n", targetApp.UUID)
				fmt.Printf("Project: %s\n", targetApp.Project.Name)
			}

			// Print created at and last deployed at (updated at) as humanized times
			if targetApp != nil {
				createdAt, err1 := time.Parse(time.RFC3339, targetApp.CreatedAt)
				updatedAt, err2 := time.Parse(time.RFC3339, targetApp.UpdatedAt)
				if err1 == nil {
					fmt.Printf("Created: %s\n", humanize.Time(createdAt))
				}
				if err2 == nil {
					fmt.Printf("Last Deployed: %s\n", humanize.Time(updatedAt))
				}
			}

			fmt.Println("Components:")
			for _, comp := range stackdeploy.Components {
				fmt.Printf("  - Name: %s\n", comp.Name)
				fmt.Printf("    Status: %s\n", comp.Status)
				if comp.AppSpec != nil && comp.AppSpec.RegistrySecret != "" {
					fmt.Printf("    Registry Secret: %s\n", comp.AppSpec.RegistrySecret)
				}
				if comp.GitRef != "" {
					fmt.Printf("    GitRef: %s\n", comp.GitRef)
				}
				if comp.Message != "" {
					fmt.Printf("    Message: %s\n", comp.Message)
				}
				if comp.BuildMode != "" {
					if comp.BuildMode != "" {
						fmt.Printf("    Build Mode: %s\n", comp.BuildMode)
					}
					if comp.Image != "" {
						fmt.Printf("    Image: %s\n", comp.Image)
					}
					if comp.ImageTag != "" {
						fmt.Printf("    Image Tag: %s\n", comp.ImageTag)
					}
					if comp.GitRef != "" {
						fmt.Printf("    Git Ref: %s\n", comp.GitRef)
					}
					if comp.GitURL != "" {
						fmt.Printf("    Git Repo: %s\n", comp.GitURL)
					}
				}
			}
			// Show parameters
			if len(stackdeploy.Parameters) > 0 {
				fmt.Println("Parameters:")
				for _, param := range stackdeploy.Parameters {
					value := "<hidden>"
					if param.Type != "secret" {
						value = fmt.Sprintf("%v", param.Value)
					}
					fmt.Printf("  - %s: %s\n", param.Name, value)
				}
			}
			return nil
		},
	}
	return cmd
}
