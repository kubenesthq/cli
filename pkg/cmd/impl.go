package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/fatih/color"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
		if project.Description != "" {
			fmt.Printf("Description: %s\n", project.Description)
		}
		fmt.Printf("Cluster ID: %s\n", project.ClusterID)
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

	apps, err := client.ListApps()
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
	logs, err := client.GetLogs(selectedApp.ID)
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

	apps, err := client.ListApps()
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
	pods, err := client.ListPods(selectedApp.ID)
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

	output, err := client.ExecCommand(selectedApp.ID, selectedPod.Name, command)
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

	apps, err := client.ListApps()
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
	pods, err := client.ListPods(selectedApp.ID)
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
	if err := client.CopyFile(selectedApp.ID, selectedPod.Name, srcPath, destPath, isUpload); err != nil {
		return err
	}

	color.Green("File %s successfully!", strings.ToLower(direction))
	return nil
}
