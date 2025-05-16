package cmd

import (
	"github.com/spf13/cobra"
)

// NewStacksCommand creates the stacks command
func NewStacksCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stacks",
		Short: "Manage stacks and stack deployments",
		Long: `Manage stacks and stack deployments in Kubenest.

A stack is a collection of components and parameters that define your application.
Stack deployments are instances of stacks with specific parameter values and component configurations.

Examples:
  # Create a new stack from a YAML file
  kubenest stacks create my-stack.yaml

  # Update an existing stack
  kubenest stacks update <stack-uuid> updated-stack.yaml

  # Perform a dry run of a stack
  kubenest stacks dry-run <stack-uuid> params.json

  # Create a new stack deployment
  kubenest stacks create-deploy <stack-uuid> deploy.json

  # Update an existing stack deployment
  kubenest stacks update-deploy <stackdeploy-uuid> update.json

  # Delete a stack deployment
  kubenest stacks delete-deploy <stackdeploy-uuid>`,
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
		Long: `Create a new stack from a YAML file.

The YAML file should contain:
- name: Stack name
- description: Stack description
- components: List of components
- parameters: List of parameters

Example YAML:
  name: my-stack
  description: My application stack
  components:
    - name: frontend
      type: web
      description: Frontend application
    - name: backend
      type: api
      description: Backend API
  parameters:
    - name: environment
      type: string
      description: Deployment environment
      default: development
      required: true`,
		Args: cobra.ExactArgs(1),
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
		Long: `Update an existing stack with a new YAML file.

The YAML file should have the same structure as the create command.
Note: You cannot change the stack name during an update.`,
		Args: cobra.ExactArgs(2),
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
		Long: `Perform a dry run of a stack with the given parameters.

The params file should be a JSON file containing parameter values.
This will validate the stack configuration without actually deploying it.

Example params.json:
  {
    "environment": "staging",
    "replicas": 3,
    "memory": "512Mi"
  }`,
		Args: cobra.ExactArgs(2),
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
		Long: `Create a new stack deployment with the given configuration.

The deploy file should be a JSON file containing:
- name: Deployment name
- parameter_values: Map of parameter values
- components: List of component configurations

Example deploy.json:
  {
    "name": "my-deployment",
    "parameter_values": {
      "environment": "production",
      "replicas": 3
    },
    "components": [
      {
        "name": "frontend",
        "git_ref": "main",
        "image": "my-frontend:latest"
      }
    ]
  }`,
		Args: cobra.ExactArgs(2),
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
		Long: `Update an existing stack deployment with new configurations.

The update file should be a JSON file containing:
- components: List of component updates (optional)
- parameters: Map of parameter updates (optional)

Example update.json:
  {
    "components": [
      {
        "name": "frontend",
        "git_ref": "feature/new-ui"
      }
    ],
    "parameters": {
      "replicas": 5
    }
  }`,
		Args: cobra.ExactArgs(2),
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
		Long: `Delete a stack deployment by its UUID.

This will remove the deployment and all its associated resources.
Use with caution as this operation cannot be undone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return deleteStackDeploy(args[0])
		},
	}
	return cmd
}
