package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
)

// DeployCommand creates the 'deploy' subcommand for apps
func DeployCommand() *cobra.Command {
	var (
		componentName string
		imageURL string
		imageTag string
		gitRef string
		mode string
		params []string
		clusterName string
		projectName string
		teamName string
		registrySecret string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "deploy <app-name>",
		Short: "Deploy a component of an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appName := args[0]
			if componentName == "" {
				return fmt.Errorf("--component is required")
			}

			if gitRef != "" && imageTag != "" {
				return fmt.Errorf("only one of --git-ref or --image-tag can be specified, not both")
			}

			cfg, _ := config.LoadConfig()
			// Team context
			if teamName != "" {
				client, err := api.NewClientFromConfig()
				if err != nil {
					return fmt.Errorf("failed to create client: %v", err)
				}
				teams, err := client.ListTeams(context.Background())
				if err != nil {
					return fmt.Errorf("failed to list teams: %w", err)
				}
				var foundTeamUUID string
				for _, team := range teams {
					if team.UUID == teamName || team.Name == teamName {
						foundTeamUUID = team.UUID
						break
					}
				}
				if foundTeamUUID == "" {
					return fmt.Errorf("team not found: %s", teamName)
				}
				cfg.TeamUUID = foundTeamUUID
			}
			if cfg.TeamUUID == "" {
				return fmt.Errorf("team context must be set. Use 'kubenest context set-team <team>' first or specify --team")
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

			// Validate component
			var foundComponent bool
			for _, comp := range stackdeploy.Components {
				if comp.Name == componentName {
					foundComponent = true
					break
				}
			}
			if !foundComponent {
				return fmt.Errorf("component '%s' not found in app", componentName)
			}

			// If registrySecret is set, validate it exists in the project
			if registrySecret != "" {
				registries, err := client.ListRegistries(context.Background(), targetApp.Project.UUID)
				if err != nil {
					return fmt.Errorf("failed to list registries for project: %w", err)
				}
				found := false
				for _, reg := range registries {
					if reg.Name == registrySecret {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("registry secret '%s' not found in project '%s'", registrySecret, targetApp.Project.Name)
				}
			}

			// Build a map of parameter name to type
			paramTypes := map[string]string{}
			for _, p := range stackdeploy.Parameters {
				paramTypes[p.Name] = p.Type
			}

			// Build PATCH payload
			var patch map[string]interface{}
			paramOnly := len(params) > 0 && gitRef == "" && imageTag == "" && imageURL == "" && mode == ""
			if paramOnly {
				paramMap := map[string]interface{}{}
				for _, p := range params {
					kv := strings.SplitN(p, "=", 2)
					if len(kv) != 2 {
						return fmt.Errorf("invalid --param format, expected key=value: %s", p)
					}
					key, val := kv[0], kv[1]
					typeStr := paramTypes[key]
					if typeStr == "number" {
						// Try int first, then float
						if i, err := strconv.Atoi(val); err == nil {
							paramMap[key] = i
						} else if f, err := strconv.ParseFloat(val, 64); err == nil {
							paramMap[key] = f
						} else {
							return fmt.Errorf("invalid number for %s: %v", key, err)
						}
					} else {
						paramMap[key] = val
					}
				}
				patch = map[string]interface{}{
					"parameters": paramMap,
				}
			} else {
				component := map[string]interface{}{
					"name": componentName,
				}
				// appSpec
				if imageURL != "" || mode != "" || registrySecret != "" {
					appSpec := map[string]interface{}{}
					if imageURL != "" {
						appSpec["image"] = imageURL
					}
					if mode != "" {
						appSpec["build_mode"] = mode
					}
					if registrySecret != "" {
						appSpec["registrySecret"] = registrySecret
					}
					// Handle params for appSpec
					if len(params) > 0 && gitRef == "" && imageTag == "" {
						paramMap := map[string]interface{}{}
						for _, p := range params {
							kv := strings.SplitN(p, "=", 2)
							if len(kv) != 2 {
								return fmt.Errorf("invalid --param format, expected key=value: %s", p)
							}
							key, val := kv[0], kv[1]
							typeStr := paramTypes[key]
							if typeStr == "number" {
								if i, err := strconv.Atoi(val); err == nil {
									paramMap[key] = i
								} else if f, err := strconv.ParseFloat(val, 64); err == nil {
									paramMap[key] = f
								} else {
									return fmt.Errorf("invalid number for %s: %v", key, err)
								}
							} else {
								paramMap[key] = val
							}
						}
						appSpec["params"] = paramMap
					}
					component["appSpec"] = appSpec
				}
				// buildSpec
				if gitRef != "" || imageTag != "" {
					buildSpec := map[string]interface{}{}
					if gitRef != "" {
						buildSpec["gitRef"] = gitRef
					}
					if imageTag != "" {
						buildSpec["imageTag"] = imageTag
					}
					// If params are also set, add them to buildSpec
					if len(params) > 0 {
						paramMap := map[string]interface{}{}
						for _, p := range params {
							kv := strings.SplitN(p, "=", 2)
							if len(kv) != 2 {
								return fmt.Errorf("invalid --param format, expected key=value: %s", p)
							}
							key, val := kv[0], kv[1]
							typeStr := paramTypes[key]
							if typeStr == "number" {
								if i, err := strconv.Atoi(val); err == nil {
									paramMap[key] = i
								} else if f, err := strconv.ParseFloat(val, 64); err == nil {
									paramMap[key] = f
								} else {
									return fmt.Errorf("invalid number for %s: %v", key, err)
								}
							} else {
								paramMap[key] = val
							}
						}
						buildSpec["params"] = paramMap
					}
					component["buildSpec"] = buildSpec
				}
				patch = map[string]interface{}{
					"components": []map[string]interface{}{component},
				}
			}

			// PATCH request
			if dryRun {
				patchJSON, _ := json.MarshalIndent(patch, "", "  ")
				fmt.Println("[DRY RUN] PATCH payload:")
				fmt.Println(string(patchJSON))
				return nil
			}
			endpoint := fmt.Sprintf("/api/v1/stackdeploys/%s", stackdeploy.UUID)
			_, err = client.DoRequestWithMethod("PATCH", endpoint, patch)
			if err != nil {
				// If err contains status code, print it, otherwise just print the error
				fmt.Printf("Deploy failed: %v\n", err)
				return err
			}
			fmt.Println("Deploy request sent successfully.")
			return nil
		},
	}

	cmd.Flags().StringVar(&componentName, "component", "", "Component name (required)")
	cmd.Flags().StringVar(&imageURL, "image", "", "Image URL")
	cmd.Flags().StringVar(&imageTag, "image-tag", "", "Image tag (cannot be used with --git-ref)")
	cmd.Flags().StringVar(&gitRef, "git-ref", "", "Git ref (cannot be used with --image-tag)")
	cmd.Flags().StringVar(&mode, "mode", "", "Build mode (image|dockerfile|buildpack)")
	cmd.Flags().StringArrayVar(&params, "param", nil, "Parameter key=value (repeatable)")
	cmd.Flags().StringVar(&clusterName, "cluster", "", "Cluster name or UUID (optional, overrides context)")
	cmd.Flags().StringVar(&projectName, "project", "", "Project name or UUID (optional, overrides context)")
	cmd.Flags().StringVar(&teamName, "team", "", "Team name or UUID (optional, overrides context)")
	cmd.Flags().StringVar(&registrySecret, "registry-secret", "", "Registry secret name to update in appSpec (optional)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the PATCH payload and do not make the API call")
	cmd.MarkFlagRequired("component")

	return cmd
}
