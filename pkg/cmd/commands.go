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

// NewTeamsCommand creates the teams command
func NewTeamsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teams",
		Short: "List teams",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listTeams()
		},
	}
	return cmd
}

// NewClustersCommand creates the clusters command
func NewClustersCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clusters",
		Short: "List clusters",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listClusters()
		},
	}
	return cmd
}

// NewProjectsCommand creates the projects command
func NewProjectsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "projects",
		Short: "List projects",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listProjects()
		},
	}
	return cmd
}

// NewStackDeploysCommand creates the stackdeploys command
func NewStackDeploysCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stackdeploys",
		Short: "List stackdeploys",
		RunE: func(cmd *cobra.Command, args []string) error {
			return listStackDeploys()
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
