package cmd

import (
	"encoding/json"
	"fmt"

	"kubenest.io/cli/pkg/models"
	"github.com/spf13/cobra"
)

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
			// TODO: Implement actual API call
			teams := models.TeamList{
				Items: []models.Team{
					{
						ID:          "team-1",
						Name:        "Team 1",
						Description: "First team",
					},
				},
				Total: 1,
			}

			output, err := json.MarshalIndent(teams, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal teams: %v", err)
			}

			fmt.Println(string(output))
			return nil
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
			// TODO: Implement actual API call
			clusters := models.ClusterList{
				Items: []models.Cluster{
					{
						ID:       "cluster-1",
						Name:     "Cluster 1",
						Provider: "aws",
						Region:   "us-west-2",
						Version:  "1.24",
						Status:   "running",
					},
				},
				Total: 1,
			}

			output, err := json.MarshalIndent(clusters, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal clusters: %v", err)
			}

			fmt.Println(string(output))
			return nil
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
			// TODO: Implement actual API call
			projects := models.ProjectList{
				Items: []models.Project{
					{
						ID:          "project-1",
						Name:        "Project 1",
						Description: "First project",
						TeamID:      "team-1",
					},
				},
				Total: 1,
			}

			output, err := json.MarshalIndent(projects, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal projects: %v", err)
			}

			fmt.Println(string(output))
			return nil
		},
	}
	return cmd
}

// NewStackDeploysCommand creates the stackdeploys command
func NewStackDeploysCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stack-deploys",
		Short: "List stack deployments",
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO: Implement actual API call
			stackDeploys := models.StackDeployList{
				Items: []models.StackDeploy{
					{
						ID:        "deploy-1",
						Name:      "Deploy 1",
						ProjectID: "project-1",
						ClusterID: "cluster-1",
						Status:    "running",
					},
				},
				Total: 1,
			}

			output, err := json.MarshalIndent(stackDeploys, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal stack deploys: %v", err)
			}

			fmt.Println(string(output))
			return nil
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
