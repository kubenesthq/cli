package cmd

import (
	"github.com/spf13/cobra"
)

// NewStacksCommand creates the stacks command
func NewStacksCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stacks",
		Short: "Manage stacks",
	}

	cmd.AddCommand(NewCreateStackCommand())
	cmd.AddCommand(NewUpdateStackCommand())
	cmd.AddCommand(NewDryRunStackCommand())
	cmd.AddCommand(NewDeployStackCommand())
	cmd.AddCommand(NewPatchStackDeployCommand())
	cmd.AddCommand(NewCreateStackDeployCommand())
	cmd.AddCommand(NewUpdateStackDeployCommand())
	cmd.AddCommand(NewDeleteStackDeployCommand())

	return cmd
}

// NewCreateStackCommand creates the create stack command
func NewCreateStackCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create [file]",
		Short: "Create a new stack",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createStack(args[0])
		},
	}
	return cmd
}

// NewUpdateStackCommand creates the update stack command
func NewUpdateStackCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update [stack-uuid] [file]",
		Short: "Update an existing stack",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return updateStack(args[0], args[1])
		},
	}
	return cmd
}

// NewDryRunStackCommand creates the dry run stack command
func NewDryRunStackCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dry-run [stack-uuid] [params-file]",
		Short: "Perform a dry run of a stack",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dryRunStack(args[0], args[1])
		},
	}
	return cmd
}

// NewDeployStackCommand creates the deploy stack command
func NewDeployStackCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy [stack-uuid] [deploy-file]",
		Short: "Deploy a stack",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deployStack(args[0], args[1])
		},
	}
	return cmd
}

// NewPatchStackDeployCommand creates the patch stack deploy command
func NewPatchStackDeployCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "patch-deploy [stackdeploy-uuid] [patch-file]",
		Short: "Patch a stack deployment (update components or parameters)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return patchStackDeploy(args[0], args[1])
		},
	}
	return cmd
}

// NewCreateStackDeployCommand creates the create stack deploy command
func NewCreateStackDeployCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create-deploy [stack-uuid] [deploy-file]",
		Short: "Create a new stack deployment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createStackDeploy(args[0], args[1])
		},
	}
	return cmd
}

// NewUpdateStackDeployCommand creates the update stack deploy command
func NewUpdateStackDeployCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update-deploy [stackdeploy-uuid] [update-file]",
		Short: "Update an existing stack deployment",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return updateStackDeploy(args[0], args[1])
		},
	}
	return cmd
}

// NewDeleteStackDeployCommand creates the delete stack deploy command
func NewDeleteStackDeployCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete-deploy [stackdeploy-uuid]",
		Short: "Delete a stack deployment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteStackDeploy(args[0])
		},
	}
	return cmd
}
