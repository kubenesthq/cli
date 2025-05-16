package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewLogoutCommand creates the logout command
func NewLogoutCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Log out from Kubenest",
		Long: `Log out from your Kubenest account.

This will remove your stored credentials from the local machine.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return logout()
		},
	}
	return cmd
}

// NewExecCommand creates the exec command
func NewExecCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Execute command in application pod",
		Long: `Execute a command in a pod of your application.

This command will:
1. Show a list of available applications
2. Let you select an application
3. Show a list of pods in the selected application
4. Let you select a pod
5. Prompt for the command to execute
6. Show the command output

Example:
  $ kubenest exec
  Select Application:
  ▶ my-app
    other-app
  Select Pod:
  ▶ my-app-7d8f9g
    my-app-1a2b3c
  Command to execute: ls -la`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return execPod()
		},
	}
	return cmd
}

// NewCopyCommand creates the copy command
func NewCopyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "copy",
		Short: "Copy files to/from application pod",
		Long: `Copy files between your local machine and application pods.

This command will:
1. Show a list of available applications
2. Let you select an application
3. Show a list of pods in the selected application
4. Let you select a pod
5. Choose the copy direction (upload/download)
6. Specify source and destination paths

Example:
  $ kubenest copy
  Select Application:
  ▶ my-app
    other-app
  Select Pod:
  ▶ my-app-7d8f9g
    my-app-1a2b3c
  Direction:
  ▶ Upload
    Download
  Source Path: ./config.yaml
  Destination Path: /app/config.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return copyFiles()
		},
	}
	return cmd
}

// NewLogsCommand creates the logs command
func NewLogsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Get application logs",
		Long: `View logs from your application.

This command will:
1. Show a list of available applications
2. Let you select an application
3. Display the logs for the selected application

Example:
  $ kubenest logs
  Select Application:
  ▶ my-app
    other-app
  [logs will be displayed here]`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return getLogs()
		},
	}
	return cmd
}

func NewRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kubenest",
		Short: "A command-line interface for managing Kubenest resources",
		Long: `A command-line interface for managing Kubenest resources.

Authentication:
  login    - Log in to your Kubenest account
  logout   - Log out from your Kubenest account

Context Management:
  teams       - List teams you are a part of
  context     - Manage CLI context (team, cluster, project)
  projects    - List projects for the current team context
  clusters    - List clusters for the current team context

Stack Management:
  stacks create <file>              - Create a new stack from a YAML file
  stacks update <uuid> <file>       - Update an existing stack
  stacks dry-run <uuid> <file>      - Perform a dry run of a stack
  stacks deploy <uuid> <file>       - Deploy a stack with configuration
  stacks patch-deploy <uuid> <file> - Patch a stack deployment
  stacks create-deploy <uuid> <file> - Create a new stack deployment
  stacks update-deploy <uuid> <file> - Update an existing stack deployment
  stacks delete-deploy <uuid>       - Delete a stack deployment

Application Management:
  apps        - List apps (stackdeploys) for the current team context
  logs        - Get logs for an app

Examples:
  # Authentication
  kubenest login                    # Log in to your account
  kubenest logout                   # Log out from your account

  # Context Management
  kubenest teams                    # List available teams
  kubenest context                  # Show current context
  kubenest projects                 # List projects in current team
  kubenest clusters                 # List clusters in current team

  # Stack Management
  kubenest stacks create my-stack.yaml                    # Create a new stack
  kubenest stacks update <stack-uuid> updated-stack.yaml  # Update a stack
  kubenest stacks dry-run <stack-uuid> params.json       # Test stack configuration
  kubenest stacks deploy <stack-uuid> deploy.json        # Deploy a stack
  kubenest stacks patch-deploy <uuid> patch.json         # Update deployment
  kubenest stacks create-deploy <uuid> deploy.json       # Create deployment
  kubenest stacks update-deploy <uuid> update.json       # Update deployment
  kubenest stacks delete-deploy <uuid>                   # Delete deployment

  # Application Management
  kubenest apps                     # List applications
  kubenest logs <app-name>          # View application logs

For more information about a specific command, use:
  kubenest <command> --help`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("A command-line interface for managing Kubenest resources")
			fmt.Println("\nUsage:")
			fmt.Println("  kubenest [command]")
			fmt.Println("\nAvailable Commands:")
			fmt.Println("  apps        - List apps (stackdeploys) for the current team context")
			fmt.Println("  clusters    - List clusters for the current team context")
			fmt.Println("  completion  - Generate the autocompletion script for the specified shell")
			fmt.Println("  context     - Manage CLI context (team, cluster, project)")
			fmt.Println("  help        - Help about any command")
			fmt.Println("  login       - Log in to Kubenest")
			fmt.Println("  logout      - Log out from Kubenest")
			fmt.Println("  logs        - Get logs for an app")
			fmt.Println("  projects    - List projects for the current team context")
			fmt.Println("  stacks      - Manage stacks and stack deployments")
			fmt.Println("  teams       - List teams you are a part of")
			fmt.Println("\nFlags:")
			fmt.Println("  -h, --help   help for kubenest")
			fmt.Println("\nUse \"kubenest [command] --help\" for more information about a command.")
		},
	}

	// Add commands
	cmd.AddCommand(NewLoginCommand())
	cmd.AddCommand(NewLogoutCommand())
	cmd.AddCommand(NewLogsCommand())
	cmd.AddCommand(NewExecCommand())
	cmd.AddCommand(NewCopyCommand())
	cmd.AddCommand(NewStacksCommand())
	cmd.AddCommand(NewTeamsCommand())
	cmd.AddCommand(NewContextCommand())
	cmd.AddCommand(NewProjectsCommand())
	cmd.AddCommand(NewClustersCommand())
	cmd.AddCommand(NewAppsCommand())

	return cmd
}
