package cmd

import (
	"github.com/spf13/cobra"
)

// NewLoginCommand creates the login command
func NewLoginCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Login to Kubenest",
		RunE: func(cmd *cobra.Command, args []string) error {
			return login()
		},
	}
	return cmd
}

// NewLogoutCommand creates the logout command
func NewLogoutCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Logout from Kubenest",
		RunE: func(cmd *cobra.Command, args []string) error {
			return logout()
		},
	}
	return cmd
}

// NewContextCommand creates the context command
func NewContextCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Set or view current context",
		RunE: func(cmd *cobra.Command, args []string) error {
			return setContext()
		},
	}
	return cmd
}

// NewAppsCommand creates the apps command
func NewAppsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "List deployed applications",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listApps()
		},
	}
	return cmd
}

// NewDeployCommand creates the deploy command
func NewDeployCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy an application",
		RunE: func(cmd *cobra.Command, args []string) error {
			return deployApp()
		},
	}
	return cmd
}

// NewLogsCommand creates the logs command
func NewLogsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "View application logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return getLogs()
		},
	}
	return cmd
}

// NewExecCommand creates the exec command
func NewExecCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "exec",
		Short: "Execute command in application pod",
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return copyFiles()
		},
	}
	return cmd
}
