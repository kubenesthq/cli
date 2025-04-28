package cmd

import (
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
