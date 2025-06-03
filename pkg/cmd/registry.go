package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"golang.org/x/term"
)

// Registry subcommand and its children
func newRegistryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage registries for a project",
	}

	cmd.AddCommand(newRegistryListCommand())
	cmd.AddCommand(newRegistryAddCommand())
	cmd.AddCommand(newRegistryDeleteCommand())
	return cmd
}

func newRegistryListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registries for a project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := config.LoadConfig()
			if cfg.TeamUUID == "" {
				return fmt.Errorf("team context must be set. Use 'kubenest context set-team <team>' first.")
			}
			if cfg.ProjectUUID == "" {
				return fmt.Errorf("project context must be set. Use 'kubenest context set-project <project>' first.")
			}
			projectUUID := cfg.ProjectUUID
			client, err := api.NewClientFromConfig()
			if err != nil {
				return fmt.Errorf("failed to create client: %v", err)
			}
			client.SetTeamUUID(cfg.TeamUUID)
			registries, err := client.ListRegistries(cmd.Context(), projectUUID)
			if err != nil {
				return fmt.Errorf("failed to list registries: %w", err)
			}
			if len(registries) == 0 {
				fmt.Println("No registries found.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tUUID\tURL\tUSERNAME\tCREATED_AT")
			for _, reg := range registries {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", reg.Name, reg.UUID, reg.URL, reg.Username, reg.CreatedAt)
			}
			w.Flush()
			return nil
		},
	}
}

func newRegistryAddCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "add",
		Short: "Add a registry to a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			var name, url, username, password string
			cfg, _ := config.LoadConfig()
			if cfg.TeamUUID == "" {
				return fmt.Errorf("team context must be set. Use 'kubenest context set-team <team>' first.")
			}
			if cfg.ProjectUUID == "" {
				return fmt.Errorf("project context must be set. Use 'kubenest context set-project <project>' first.")
			}
			projectUUID := cfg.ProjectUUID
			fmt.Print("Registry Name: ")
			fmt.Scanln(&name)
			fmt.Print("Registry URL: ")
			fmt.Scanln(&url)
			fmt.Print("Username: ")
			fmt.Scanln(&username)
			fmt.Print("Password: ")
			bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				return fmt.Errorf("failed to read password: %w", err)
			}
			password = string(bytePassword)

			client, err := api.NewClientFromConfig()
			if err != nil {
				return fmt.Errorf("failed to create client: %v", err)
			}
			client.SetTeamUUID(cfg.TeamUUID)
			err = client.AddRegistry(cmd.Context(), projectUUID, name, url, username, password)
			if err != nil {
				return fmt.Errorf("failed to add registry: %w", err)
			}
			fmt.Println("Registry added successfully!")
			return nil
		},
	}
}

func newRegistryDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete",
		Short: "Delete a registry from a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			var projectUUID, registryName string
			cfg, _ := config.LoadConfig()
			if cfg.ProjectUUID != "" {
				projectUUID = cfg.ProjectUUID
			} else {
				fmt.Print("Project UUID: ")
				fmt.Scanln(&projectUUID)
			}
			fmt.Print("Registry Name: ")
			fmt.Scanln(&registryName)

			if cfg.TeamUUID == "" {
				return fmt.Errorf("team context must be set. Use 'kubenest context set-team <team>' first.")
			}
			client, err := api.NewClientFromConfig()
			if err != nil {
				return fmt.Errorf("failed to create client: %v", err)
			}
			client.SetTeamUUID(cfg.TeamUUID)
			registries, err := client.ListRegistries(cmd.Context(), projectUUID)
			if err != nil {
				return fmt.Errorf("failed to list registries: %w", err)
			}
			var registryUUID string
			for _, reg := range registries {
				if reg.Name == registryName {
					registryUUID = reg.UUID
					break
				}
			}
			if registryUUID == "" {
				return fmt.Errorf("registry with name '%s' not found in project %s", registryName, projectUUID)
			}

			var confirm string
			fmt.Printf("Are you sure you want to delete registry '%s' (UUID: %s) from project %s? (y/N): ", registryName, registryUUID, projectUUID)
			fmt.Scanln(&confirm)
			if confirm != "y" && confirm != "Y" {
				fmt.Println("Aborted.")
				return nil
			}

			err = client.DeleteRegistry(cmd.Context(), projectUUID, registryUUID)
			if err != nil {
				return fmt.Errorf("failed to delete registry: %w", err)
			}
			fmt.Println("Registry deleted successfully!")
			return nil
		},
	}
}
