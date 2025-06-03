package cmd

import (
	"fmt"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"github.com/spf13/cobra"
)

func DeleteAppCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete",
		Short: "Delete an app by name and project",
		RunE: func(cmd *cobra.Command, args []string) error {
			var appName, projectName string
			cfg, _ := config.LoadConfig()
			if cfg.ProjectUUID != "" {
				projectName = cfg.ProjectUUID
			} else {
				fmt.Print("Project Name or UUID: ")
				fmt.Scanln(&projectName)
			}
			fmt.Print("App Name: ")
			fmt.Scanln(&appName)

			if cfg.TeamUUID == "" {
				return fmt.Errorf("team context must be set. Use 'kubenest context set-team <team>' first.")
			}
			client, err := api.NewClientFromConfig()
			if err != nil {
				return fmt.Errorf("failed to create client: %v", err)
			}
			client.SetTeamUUID(cfg.TeamUUID)
			apps, err := client.ListApps(cmd.Context())
			if err != nil {
				return fmt.Errorf("failed to list apps: %w", err)
			}
			var appUUID string
			for _, app := range apps {
				if app.Name == appName && (app.Project.UUID == projectName || app.Project.Name == projectName) {
					appUUID = app.UUID
					break
				}
			}
			if appUUID == "" {
				return fmt.Errorf("app '%s' not found in project '%s'", appName, projectName)
			}

			var confirm string
			fmt.Printf("Are you sure you want to delete app '%s' (UUID: %s) from project %s? (y/N): ", appName, appUUID, projectName)
			fmt.Scanln(&confirm)
			if confirm != "y" && confirm != "Y" {
				fmt.Println("Aborted.")
				return nil
			}

			err = client.DeleteApp(cmd.Context(), appUUID)
			if err != nil {
				return fmt.Errorf("failed to delete app: %w", err)
			}
			fmt.Println("App deleted successfully!")
			return nil
		},
	}
}
