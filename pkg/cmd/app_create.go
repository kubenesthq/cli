package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
)

// CreateCommand creates the 'create' subcommand for apps
func CreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new app interactively",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := config.LoadConfig()
			if cfg.TeamUUID == "" {
				return fmt.Errorf("team context must be set. Use 'kubenest context set-team <team>' first.")
			}

			client, err := api.NewClientFromConfig()
			if err != nil {
				return fmt.Errorf("failed to create client: %v", err)
			}
			client.SetTeamUUID(cfg.TeamUUID)

			// 1. Get app name
			var appName string
			if err := huh.NewInput().
				Title("Enter app name").
				Value(&appName).
				Validate(func(s string) error {
					if s == "" {
						return fmt.Errorf("app name cannot be empty")
					}
					return nil
				}).Run(); err != nil {
				return fmt.Errorf("failed to get app name: %w", err)
			}

			// 2. Get stacks and let user select
			stacks, err := client.Get(context.Background(), "/api/v1/stacks")
			if err != nil {
				return fmt.Errorf("failed to list stacks: %w", err)
			}
			defer stacks.Body.Close()

			var stackList []struct {
				UUID        string `json:"uuid"`
				Name        string `json:"name"`
				DisplayName string `json:"display_name"`
				Description string `json:"description"`
				Version     string `json:"version"`
			}
			if err := json.NewDecoder(stacks.Body).Decode(&stackList); err != nil {
				return fmt.Errorf("failed to decode stacks: %w", err)
			}

			var selectedStackUUID string
			stackOptions := make([]huh.Option[string], len(stackList))
			for i, stack := range stackList {
				stackOptions[i] = huh.NewOption(
					fmt.Sprintf("%s (%s) - %s", stack.DisplayName, stack.Version, stack.Description),
					stack.UUID,
				)
			}
			if err := huh.NewSelect[string]().
				Title("Select a stack").
				Options(stackOptions...).
				Value(&selectedStackUUID).
				Run(); err != nil {
				return fmt.Errorf("failed to select stack: %w", err)
			}

			// 3. Get stack details
			stackDetail, err := client.Get(context.Background(), fmt.Sprintf("/api/v1/stacks/%s", selectedStackUUID))
			if err != nil {
				return fmt.Errorf("failed to get stack details: %w", err)
			}
			defer stackDetail.Body.Close()

			var stackInfo struct {
				Parameters []struct {
					Name         string      `json:"name"`
					Type         string      `json:"type"`
					DefaultValue interface{} `json:"default_value"`
				} `json:"parameters"`
				Components []struct {
					Name string `json:"name"`
					Kind string `json:"kind"`
				} `json:"components"`
			}
			if err := json.NewDecoder(stackDetail.Body).Decode(&stackInfo); err != nil {
				return fmt.Errorf("failed to decode stack details: %w", err)
			}

			// 4. Get parameter values
			parameters := make(map[string]interface{})
			var namespace string
			if cfg.ProjectUUID != "" {
				projects, err := client.ListProjects(context.Background())
				if err == nil {
					for _, p := range projects {
						if p.UUID == cfg.ProjectUUID {
							namespace = p.Namespace
							break
						}
					}
				}
			}
			if namespace == "" {
				return fmt.Errorf("namespace not found for project %s", cfg.ProjectUUID)
			}
			for _, param := range stackInfo.Parameters {
				var value interface{}
				promptTitle := fmt.Sprintf("Enter value for %s", param.Name)
				if param.DefaultValue != nil && param.DefaultValue != "" {
					promptTitle = fmt.Sprintf("%s (default: %v)", promptTitle, param.DefaultValue)
				}
				switch param.Type {
				case "string":
					var strValue string
					input := huh.NewInput().
						Title(promptTitle).
						Value(&strValue).
						Validate(func(s string) error {
							if s == "" && param.DefaultValue == nil {
								return fmt.Errorf("value cannot be empty")
							}
							return nil
						})
					if param.DefaultValue != nil && param.DefaultValue != "" {
						input.Placeholder(fmt.Sprintf("%v", param.DefaultValue))
					}
					if err := input.Run(); err != nil {
						return fmt.Errorf("failed to get parameter value: %w", err)
					}
					if strValue == "" && param.DefaultValue != nil {
						value = param.DefaultValue
					} else {
						value = strValue
					}
				case "secret":
					var secretValue string
					pwPrompt := huh.NewInput().EchoMode(huh.EchoModePassword).
						Title(promptTitle).
						Value(&secretValue).
						Validate(func(s string) error {
							if s == "" && param.DefaultValue == nil {
								return fmt.Errorf("value cannot be empty")
							}
							return nil
						})
					if param.DefaultValue != nil && param.DefaultValue != "" {
						pwPrompt.Placeholder(fmt.Sprintf("%v", param.DefaultValue))
					}
					if err := pwPrompt.Run(); err != nil {
						return fmt.Errorf("failed to get parameter value: %w", err)
					}
					if secretValue == "" && param.DefaultValue != nil {
						value = param.DefaultValue
					} else {
						value = secretValue
					}
				case "number":
					var numStr string
					input := huh.NewInput().
						Title(promptTitle).
						Value(&numStr).
						Validate(func(s string) error {
							if s == "" && param.DefaultValue == nil {
								return fmt.Errorf("value cannot be empty")
							}
							return nil
						})
					if param.DefaultValue != nil && param.DefaultValue != "" {
						input.Placeholder(fmt.Sprintf("%v", param.DefaultValue))
					}
					if err := input.Run(); err != nil {
						return fmt.Errorf("failed to get parameter value: %w", err)
					}
					if numStr == "" && param.DefaultValue != nil {
						value = param.DefaultValue
					} else {
						var numValue float64
						_, err := fmt.Sscanf(numStr, "%f", &numValue)
						if err != nil {
							return fmt.Errorf("invalid number for %s: %v", param.Name, err)
						}
						// If the default value is int, use int
						if _, ok := param.DefaultValue.(int); ok && numValue == float64(int(numValue)) {
							value = int(numValue)
						} else {
							value = numValue
						}
					}
				default:
					// Fallback: if type is secret or name contains 'password', use password prompt
					if param.Type == "secret" || (len(param.Name) >= 8 && (param.Name == "password" || param.Name[len(param.Name)-8:] == "password")) {
						var secretValue string
						pwPrompt := huh.NewInput().EchoMode(huh.EchoModePassword).
							Title(promptTitle).
							Value(&secretValue).
							Validate(func(s string) error {
								if s == "" && param.DefaultValue == nil {
									return fmt.Errorf("value cannot be empty")
								}
								return nil
							})
						if param.DefaultValue != nil && param.DefaultValue != "" {
							pwPrompt.Placeholder(fmt.Sprintf("%v", param.DefaultValue))
						}
						if err := pwPrompt.Run(); err != nil {
							return fmt.Errorf("failed to get parameter value: %w", err)
						}
						if secretValue == "" && param.DefaultValue != nil {
							value = param.DefaultValue
						} else {
							value = secretValue
						}
					} else {
						var strValue string
						input := huh.NewInput().
							Title(promptTitle).
							Value(&strValue).
							Validate(func(s string) error {
								if s == "" && param.DefaultValue == nil {
									return fmt.Errorf("value cannot be empty")
								}
								return nil
							})
						if param.DefaultValue != nil && param.DefaultValue != "" {
							input.Placeholder(fmt.Sprintf("%v", param.DefaultValue))
						}
						if err := input.Run(); err != nil {
							return fmt.Errorf("failed to get parameter value: %w", err)
						}
						if strValue == "" && param.DefaultValue != nil {
							value = param.DefaultValue
						} else {
							value = strValue
						}
					}
				}
				parameters[param.Name] = value
			}

			// 5. Get app spec details for each component
			components := make([]map[string]interface{}, 0)
			for _, comp := range stackInfo.Components {
				if comp.Kind == "app" {
					// Get build mode
					var buildMode string
					modeSelect := huh.NewSelect[string]().
						Title(fmt.Sprintf("Select build mode for %s", comp.Name)).
						Options(
							huh.NewOption("Image", "image"),
							huh.NewOption("Buildpack", "buildpack"),
							huh.NewOption("Dockerfile", "dockerfile"),
						).
						Value(&buildMode)

					if err := modeSelect.Run(); err != nil {
						return fmt.Errorf("failed to get build mode: %w", err)
					}

					// Get registry
					registries, err := client.ListRegistries(context.Background(), cfg.ProjectUUID)
					if err != nil {
						return fmt.Errorf("failed to list registries: %w", err)
					}

					var selectedRegistry string
					registryOptions := make([]huh.Option[string], len(registries))
					for i, reg := range registries {
						registryOptions[i] = huh.NewOption(reg.Name, reg.Name)
					}

					registrySelect := huh.NewSelect[string]().
						Title("Select registry").
						Options(registryOptions...).
						Value(&selectedRegistry)

					if err := registrySelect.Run(); err != nil {
						return fmt.Errorf("failed to get registry: %w", err)
					}

					// Get image details based on build mode
					var image, gitURL, gitRef string
					if buildMode == "image" {
						var imageInput string
						input := huh.NewInput().
							Title("Enter image URL").
							Value(&imageInput).
							Validate(func(s string) error {
								if s == "" {
									return fmt.Errorf("image URL cannot be empty")
								}
								return nil
							})
						if err := input.Run(); err != nil {
							return fmt.Errorf("failed to get image URL: %w", err)
						}
						image = imageInput
					} else {
						// Get git details for buildpack/dockerfile
						var gitURLInput string
						input := huh.NewInput().
							Title("Enter git repository URL").
							Value(&gitURLInput).
							Validate(func(s string) error {
								if s == "" {
									return fmt.Errorf("git URL cannot be empty")
								}
								return nil
							})
						if err := input.Run(); err != nil {
							return fmt.Errorf("failed to get git URL: %w", err)
						}
						gitURL = gitURLInput

						var gitRefInput string
						input = huh.NewInput().
							Title("Enter git reference (branch/tag/commit)").
							Value(&gitRefInput).
							Validate(func(s string) error {
								if s == "" {
									return fmt.Errorf("git reference cannot be empty")
								}
								return nil
							})
						if err := input.Run(); err != nil {
							return fmt.Errorf("failed to get git reference: %w", err)
						}
						gitRef = gitRefInput
					}

					// Get image tag
					var imageTag string
					tagInput := huh.NewInput().
						Title("Enter image tag").
						Value(&imageTag).
						Validate(func(s string) error {
							if s == "" {
								return fmt.Errorf("image tag cannot be empty")
							}
							return nil
						})
					if err := tagInput.Run(); err != nil {
						return fmt.Errorf("failed to get image tag: %w", err)
					}

					component := map[string]interface{}{
						"name": comp.Name,
						"type": "app",
						"appSpec": map[string]interface{}{
							"displayName":    fmt.Sprintf("%s %s", appName, comp.Name),
							"description":    fmt.Sprintf("%s %s component", appName, comp.Name),
							"registrySecret": selectedRegistry,
							"mode":          buildMode,
						},
						"buildSpec": map[string]interface{}{
							"imageTag": imageTag,
						},
					}

					if buildMode == "image" {
						component["appSpec"].(map[string]interface{})["image"] = image
					} else {
						component["appSpec"].(map[string]interface{})["gitUrl"] = gitURL
						component["appSpec"].(map[string]interface{})["gitRef"] = gitRef
					}

					components = append(components, component)
				}
			}

			// 6. Confirm values
			var confirm bool
			confirmInput := huh.NewConfirm().
				Title("Review and confirm").
				Affirmative("Yes, create app").
				Negative("No, cancel").
				Value(&confirm)

			// Print the payload for review
			payload := map[string]interface{}{
				"name":       appName,
				"parameters": parameters,
				"context": map[string]interface{}{
					"clusterUUID": cfg.ClusterUUID,
					"projectUUID": cfg.ProjectUUID,
					"namespace":   namespace,
				},
				"components": components,
			}
			if os.Getenv("DEBUG") == "1" {
				payloadJSON, _ := json.MarshalIndent(payload, "", "  ")
				fmt.Printf("\nApp creation payload:\n%s\n\n", string(payloadJSON))
			}

			if err := confirmInput.Run(); err != nil {
				return fmt.Errorf("failed to get confirmation: %w", err)
			}

			if !confirm {
				fmt.Println("App creation cancelled")
				return nil
			}

			endpoint := fmt.Sprintf("/api/v1/stacks/%s/deploy", selectedStackUUID)
			_, err = client.DoRequestWithMethod("POST", endpoint, payload)
			if err != nil {
				return fmt.Errorf("failed to create app: %w", err)
			}

			fmt.Printf("App %s created successfully!\n", appName)
			return nil
		},
	}

	return cmd
}
