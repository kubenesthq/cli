package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/models"
)

func login(cmd *cobra.Command, args []string) error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Enter API URL (default: https://api.kubenest.io): ")
	apiURL, _ := reader.ReadString('\n')
	apiURL = strings.TrimSpace(apiURL)
	if apiURL == "" {
		apiURL = "https://api.kubenest.io"
	}
	cfg, _ := config.LoadConfig()
	cfg.APIURL = apiURL
	config.SaveConfig(cfg)

	fmt.Print("Enter email: ")
	email, _ := reader.ReadString('\n')
	email = strings.TrimSpace(email)

	fmt.Print("Enter password: ")
	bytePassword, err := term.ReadPassword(int(syscall.Stdin))
	if err != nil {
		return err
	}
	password := string(bytePassword)
	fmt.Println()

	client, _ := api.NewClient()

	loginResp, err := client.Login(email, password)
	if err != nil {
		return err
	}

	cfg.Token = loginResp.Token
	// If loginResp.User.TeamUUID exists, set it. Otherwise, skip.
	if loginResp.User.TeamUUID != "" {
		cfg.TeamUUID = loginResp.User.TeamUUID
	}
	config.SaveConfig(cfg)
	color.Green("Successfully logged in!")
	return nil
}

func logout() error {
	cfg, _ := config.LoadConfig()
	cfg.Token = ""
	cfg.TeamUUID = ""
	config.SaveConfig(cfg)
	color.Green("Successfully logged out!")
	return nil
}

func listTeams() error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)

	teams, err := client.ListTeams(context.Background())
	if err != nil {
		return err
	}

	if len(teams) == 0 {
		color.Yellow("No teams found")
		return nil
	}

	fmt.Println("\nTeams:")
	fmt.Println("------")
	for _, team := range teams {
		fmt.Printf("Name: %s\n", team.Name)
		fmt.Printf("UUID: %s\n", team.UUID)
		if team.Description != "" {
			fmt.Printf("Description: %s\n", team.Description)
		}
		fmt.Println("------")
	}

	return nil
}

func listClusters() error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	clusters, err := client.ListClusters(context.Background())
	if err != nil {
		return err
	}

	if len(clusters) == 0 {
		color.Yellow("No clusters found")
		return nil
	}

	fmt.Println("\nClusters:")
	fmt.Println("---------")
	for _, cluster := range clusters {
		fmt.Printf("Name: %s\n", cluster.Name)
		fmt.Printf("UUID: %s\n", cluster.UUID)
		fmt.Printf("Type: %s\n", cluster.Type)
		fmt.Println("---------")
	}

	return nil
}

func listProjects() error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	projects, err := client.ListProjects(context.Background())
	if err != nil {
		return err
	}

	if len(projects) == 0 {
		color.Yellow("No projects found")
		return nil
	}

	fmt.Println("\nProjects:")
	fmt.Println("---------")
	for _, project := range projects {
		fmt.Printf("Name: %s\n", project.Name)
		fmt.Printf("UUID: %s\n", project.UUID)
		fmt.Printf("Cluster: %s\n", project.Cluster.Name)
		fmt.Println("---------")
	}

	return nil
}

func listStackDeploys() error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	stackdeploys, err := client.ListStackDeploys(context.Background())
	if err != nil {
		return err
	}

	if len(stackdeploys) == 0 {
		color.Yellow("No stackdeploys found")
		return nil
	}

	fmt.Println("\nStack Deploys:")
	fmt.Println("-------------")
	for _, sd := range stackdeploys {
		fmt.Printf("Name: %s\n", sd.Name)
		fmt.Printf("UUID: %s\n", sd.UUID)
		fmt.Printf("Status: %s\n", sd.Status)

		if len(sd.Components) > 0 {
			fmt.Println("\nComponents:")
			for _, comp := range sd.Components {
				fmt.Printf("  - %s (%s)\n", comp.Name, comp.Status)
				if comp.Message != "" {
					fmt.Printf("    Message: %s\n", comp.Message)
				}
			}
		}

		if len(sd.ParameterValues) > 0 {
			fmt.Println("\nParameters:")
			for k, v := range sd.ParameterValues {
				fmt.Printf("  - %s: %v\n", k, v)
			}
		}
		fmt.Println("-------------")
	}

	return nil
}

func getLogs() error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)

	apps, err := client.ListApps(context.Background())
	if err != nil {
		return err
	}

	appPrompt := promptui.Select{
		Label: "Select Application",
		Items: apps,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}?",
			Active:   "▶ {{ .Name | cyan }}",
			Inactive: "  {{ .Name | cyan }}",
			Selected: "▶ {{ .Name | red | cyan }}",
		},
	}
	index, _, err := appPrompt.Run()
	if err != nil {
		return err
	}

	selectedApp := apps[index]
	logs, err := client.GetLogs(selectedApp.UUID)
	if err != nil {
		return err
	}
	defer logs.Close()

	io.Copy(os.Stdout, logs)
	return nil
}

func execPod() error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)

	apps, err := client.ListApps(context.Background())
	if err != nil {
		return err
	}

	appPrompt := promptui.Select{
		Label: "Select Application",
		Items: apps,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}?",
			Active:   "▶ {{ .Name | cyan }}",
			Inactive: "  {{ .Name | cyan }}",
			Selected: "▶ {{ .Name | red | cyan }}",
		},
	}
	index, _, err := appPrompt.Run()
	if err != nil {
		return err
	}

	selectedApp := apps[index]
	pods, err := client.ListPods(selectedApp.UUID)
	if err != nil {
		return err
	}

	podPrompt := promptui.Select{
		Label: "Select Pod",
		Items: pods,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}?",
			Active:   "▶ {{ .Name | cyan }}",
			Inactive: "  {{ .Name | cyan }}",
			Selected: "▶ {{ .Name | red | cyan }}",
		},
	}
	podIndex, _, err := podPrompt.Run()
	if err != nil {
		return err
	}

	selectedPod := pods[podIndex]
	prompt := promptui.Prompt{
		Label: "Command to execute",
	}
	command, err := prompt.Run()
	if err != nil {
		return err
	}

	output, err := client.ExecCommand(selectedApp.UUID, selectedPod.Name, command)
	if err != nil {
		return err
	}
	defer output.Close()

	io.Copy(os.Stdout, output)
	return nil
}

func copyFiles() error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)

	apps, err := client.ListApps(context.Background())
	if err != nil {
		return err
	}

	appPrompt := promptui.Select{
		Label: "Select Application",
		Items: apps,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}?",
			Active:   "▶ {{ .Name | cyan }}",
			Inactive: "  {{ .Name | cyan }}",
			Selected: "▶ {{ .Name | red | cyan }}",
		},
	}
	index, _, err := appPrompt.Run()
	if err != nil {
		return err
	}

	selectedApp := apps[index]
	pods, err := client.ListPods(selectedApp.UUID)
	if err != nil {
		return err
	}

	podPrompt := promptui.Select{
		Label: "Select Pod",
		Items: pods,
		Templates: &promptui.SelectTemplates{
			Label:    "{{ . }}?",
			Active:   "▶ {{ .Name | cyan }}",
			Inactive: "  {{ .Name | cyan }}",
			Selected: "▶ {{ .Name | red | cyan }}",
		},
	}
	podIndex, _, err := podPrompt.Run()
	if err != nil {
		return err
	}

	selectedPod := pods[podIndex]
	directionPrompt := promptui.Select{
		Label: "Direction",
		Items: []string{"Upload", "Download"},
	}
	_, direction, err := directionPrompt.Run()
	if err != nil {
		return err
	}

	prompt := promptui.Prompt{
		Label: "Source Path",
	}
	srcPath, err := prompt.Run()
	if err != nil {
		return err
	}

	prompt = promptui.Prompt{
		Label: "Destination Path",
	}
	destPath, err := prompt.Run()
	if err != nil {
		return err
	}

	isUpload := direction == "Upload"
	if err := client.CopyFile(selectedApp.UUID, selectedPod.Name, srcPath, destPath, isUpload); err != nil {
		return err
	}

	color.Green("File %s successfully!", strings.ToLower(direction))
	return nil
}

func createStack(filePath string) error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	// Read the stack file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read stack file: %v", err)
	}

	// Create the stack
	stack, err := client.CreateStack(context.Background(), content)
	if err != nil {
		return fmt.Errorf("failed to create stack: %v", err)
	}

	fmt.Printf("Stack created with UUID: %s\n", stack.UUID)
	fmt.Println("Waiting for deployment to complete...")

	// Wait for deployment to complete
	for {
		status, err := client.GetStackDeploymentStatus(context.Background(), stack.UUID)
		if err != nil {
			return fmt.Errorf("failed to get deployment status: %v", err)
		}

		if status.Completed {
			if status.Status == "failed" {
				return fmt.Errorf("deployment failed: %s", status.Message)
			}
			fmt.Printf("Deployment completed successfully: %s\n", status.Message)
			return nil
		}

		time.Sleep(5 * time.Second) // Check status every 5 seconds
	}
}

func updateStack(stackUUID, filePath string) error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	// Get current stack details
	currentStack, err := client.GetStack(context.Background(), stackUUID)
	if err != nil {
		return fmt.Errorf("failed to get current stack: %v", err)
	}

	// Read the stack file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read stack file: %v", err)
	}

	// Parse the new stack content to get the name
	var newStack models.Stack
	if err := yaml.Unmarshal(content, &newStack); err != nil {
		return fmt.Errorf("failed to parse stack file: %v", err)
	}

	// Check if name is being changed
	if newStack.Name != currentStack.Name {
		return fmt.Errorf("cannot update stack: changing stack name is not allowed (current: %s, new: %s)", currentStack.Name, newStack.Name)
	}

	// Update the stack
	stack, err := client.UpdateStack(context.Background(), stackUUID, content)
	if err != nil {
		return fmt.Errorf("failed to update stack: %v", err)
	}

	fmt.Printf("Stack updated successfully: %s\n", stack.UUID)
	return nil
}

func dryRunStack(stackUUID, paramsFilePath string) error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	// Read the params file
	content, err := os.ReadFile(paramsFilePath)
	if err != nil {
		return fmt.Errorf("failed to read params file: %v", err)
	}

	// Perform dry run
	result, err := client.DryRunStack(context.Background(), stackUUID, content)
	if err != nil {
		return fmt.Errorf("failed to perform dry run: %v", err)
	}

	fmt.Printf("Dry run completed successfully\n")
	if result.Message != "" {
		fmt.Printf("Message: %s\n", result.Message)
	}
	return nil
}

func deployStack(stackUUID, deployFilePath string) error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	// Read the deploy file
	content, err := os.ReadFile(deployFilePath)
	if err != nil {
		return fmt.Errorf("failed to read deploy file: %v", err)
	}

	// Deploy the stack
	deploy, err := client.DeployStack(context.Background(), stackUUID, content)
	if err != nil {
		return fmt.Errorf("failed to deploy stack: %v", err)
	}

	fmt.Printf("Stack deployment created with UUID: %s\n", deploy.UUID)
	fmt.Println("Waiting for deployment to complete...")

	// Wait for deployment to complete
	for {
		status, err := client.GetStackDeployStatus(context.Background(), deploy.UUID)
		if err != nil {
			return fmt.Errorf("failed to get deployment status: %v", err)
		}

		if status.Completed {
			if status.Status == "failed" {
				return fmt.Errorf("deployment failed: %s", status.Message)
			}
			fmt.Printf("Deployment completed successfully: %s\n", status.Message)
			return nil
		}

		time.Sleep(5 * time.Second) // Check status every 5 seconds
	}
}

func patchStackDeploy(stackDeployUUID, patchFilePath string) error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	// Read the patch file
	content, err := os.ReadFile(patchFilePath)
	if err != nil {
		return fmt.Errorf("failed to read patch file: %v", err)
	}

	// Parse the patch content to validate it
	var patch struct {
		Components []struct {
			Name      string `json:"name"`
			GitRef    string `json:"git_ref,omitempty"`
			Image     string `json:"image,omitempty"`
			BuildSpec struct {
				GitRef   string `json:"gitRef,omitempty"`
				ImageTag string `json:"imageTag,omitempty"`
			} `json:"buildSpec,omitempty"`
		} `json:"components,omitempty"`
		Parameters map[string]interface{} `json:"parameters,omitempty"`
	}

	if err := json.Unmarshal(content, &patch); err != nil {
		return fmt.Errorf("failed to parse patch file: %v", err)
	}

	// Validate that at least one of components or parameters is provided
	if len(patch.Components) == 0 && len(patch.Parameters) == 0 {
		return fmt.Errorf("patch file must contain either components or parameters")
	}

	// Apply the patch
	deploy, err := client.PatchStackDeploy(context.Background(), stackDeployUUID, content)
	if err != nil {
		return fmt.Errorf("failed to patch stack deployment: %v", err)
	}

	fmt.Printf("Stack deployment patched successfully: %s\n", deploy.UUID)
	fmt.Println("Waiting for deployment to complete...")

	// Wait for deployment to complete
	for {
		status, err := client.GetStackDeployStatus(context.Background(), deploy.UUID)
		if err != nil {
			return fmt.Errorf("failed to get deployment status: %v", err)
		}

		if status.Completed {
			if status.Status == "failed" {
				return fmt.Errorf("deployment failed: %s", status.Message)
			}
			fmt.Printf("Deployment completed successfully: %s\n", status.Message)
			return nil
		}

		time.Sleep(5 * time.Second) // Check status every 5 seconds
	}
}

func createStackDeploy(stackUUID, deployFilePath string) error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	// Read the deploy file
	content, err := os.ReadFile(deployFilePath)
	if err != nil {
		return fmt.Errorf("failed to read deploy file: %v", err)
	}

	// Create the stack deployment
	deploy, err := client.CreateStackDeploy(context.Background(), stackUUID, content)
	if err != nil {
		return fmt.Errorf("failed to create stack deployment: %v", err)
	}

	fmt.Printf("Stack deployment created with UUID: %s\n", deploy.UUID)
	fmt.Println("Waiting for deployment to complete...")

	// Wait for deployment to complete
	for {
		status, err := client.GetStackDeployStatus(context.Background(), deploy.UUID)
		if err != nil {
			return fmt.Errorf("failed to get deployment status: %v", err)
		}

		if status.Completed {
			if status.Status == "failed" {
				return fmt.Errorf("deployment failed: %s", status.Message)
			}
			fmt.Printf("Deployment completed successfully: %s\n", status.Message)
			return nil
		}

		time.Sleep(5 * time.Second) // Check status every 5 seconds
	}
}

func updateStackDeploy(stackDeployUUID, updateFilePath string) error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	// Read the update file
	content, err := os.ReadFile(updateFilePath)
	if err != nil {
		return fmt.Errorf("failed to read update file: %v", err)
	}

	// Update the stack deployment
	deploy, err := client.UpdateStackDeploy(context.Background(), stackDeployUUID, content)
	if err != nil {
		return fmt.Errorf("failed to update stack deployment: %v", err)
	}

	fmt.Printf("Stack deployment updated successfully: %s\n", deploy.UUID)
	fmt.Println("Waiting for deployment to complete...")

	// Wait for deployment to complete
	for {
		status, err := client.GetStackDeployStatus(context.Background(), deploy.UUID)
		if err != nil {
			return fmt.Errorf("failed to get deployment status: %v", err)
		}

		if status.Completed {
			if status.Status == "failed" {
				return fmt.Errorf("deployment failed: %s", status.Message)
			}
			fmt.Printf("Deployment completed successfully: %s\n", status.Message)
			return nil
		}

		time.Sleep(5 * time.Second) // Check status every 5 seconds
	}
}

func deleteStackDeploy(stackDeployUUID string) error {
	client, _ := api.NewClient()
	cfg, _ := config.LoadConfig()
	client.SetToken(cfg.Token)
	client.SetTeamUUID(cfg.TeamUUID)

	// Delete the stack deployment
	if err := client.DeleteStackDeploy(context.Background(), stackDeployUUID); err != nil {
		return fmt.Errorf("failed to delete stack deployment: %v", err)
	}

	fmt.Printf("Stack deployment %s deleted successfully\n", stackDeployUUID)
	return nil
}
