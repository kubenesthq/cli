package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/kubenesthq/cli/pkg/api"
	"github.com/kubenesthq/cli/pkg/config"
	"github.com/manifoldco/promptui"
)

func login() error {
	client := api.NewClient()

	prompt := promptui.Prompt{
		Label: "Username",
	}
	username, err := prompt.Run()
	if err != nil {
		return err
	}

	prompt = promptui.Prompt{
		Label: "Password",
		Mask:  '*',
	}
	password, err := prompt.Run()
	if err != nil {
		return err
	}

	token, err := client.Login(username, password)
	if err != nil {
		return err
	}

	config.SetToken(token)
	color.Green("Successfully logged in!")
	return nil
}

func logout() error {
	config.ClearToken()
	color.Green("Successfully logged out!")
	return nil
}

func setContext() error {
	client := api.NewClient()
	client.SetToken(config.GetConfig().Token)

	// Get available clusters
	clusters, err := client.ListClusters()
	if err != nil {
		return err
	}

	clusterPrompt := promptui.Select{
		Label: "Select Cluster",
		Items: clusters,
	}
	_, cluster, err := clusterPrompt.Run()
	if err != nil {
		return err
	}

	// Get available projects
	projects, err := client.ListProjects(cluster)
	if err != nil {
		return err
	}

	projectPrompt := promptui.Select{
		Label: "Select Project",
		Items: projects,
	}
	_, project, err := projectPrompt.Run()
	if err != nil {
		return err
	}

	config.SetContext(cluster, project)
	color.Green("Context set to cluster %s, project %s", cluster, project)
	return nil
}

func listApps() error {
	client := api.NewClient()
	client.SetToken(config.GetConfig().Token)

	apps, err := client.ListApps()
	if err != nil {
		return err
	}

	if len(apps) == 0 {
		color.Yellow("No applications found")
		return nil
	}

	fmt.Println("\nDeployed Applications:")
	fmt.Println("---------------------")
	for _, app := range apps {
		fmt.Printf("Name: %s\n", app.Name)
		fmt.Printf("ID: %s\n", app.ID)
		fmt.Printf("Status: %s\n", app.Status)
		fmt.Println("---------------------")
	}

	return nil
}

func deployApp() error {
	client := api.NewClient()
	client.SetToken(config.GetConfig().Token)

	prompt := promptui.Prompt{
		Label: "Application Name",
	}
	name, err := prompt.Run()
	if err != nil {
		return err
	}

	prompt = promptui.Prompt{
		Label: "Application Configuration File",
	}
	configFile, err := prompt.Run()
	if err != nil {
		return err
	}

	configData, err := os.ReadFile(configFile)
	if err != nil {
		return err
	}

	var appConfig api.AppConfig
	if err := json.Unmarshal(configData, &appConfig); err != nil {
		return err
	}

	appConfig.Name = name
	if err := client.DeployApp(appConfig); err != nil {
		return err
	}

	color.Green("Application %s deployed successfully!", name)
	return nil
}

func getLogs() error {
	client := api.NewClient()
	client.SetToken(config.GetConfig().Token)

	apps, err := client.ListApps()
	if err != nil {
		return err
	}

	appPrompt := promptui.Select{
		Label: "Select Application",
		Items: apps,
	}
	_, app, err := appPrompt.Run()
	if err != nil {
		return err
	}

	logs, err := client.GetLogs(app.ID)
	if err != nil {
		return err
	}
	defer logs.Close()

	io.Copy(os.Stdout, logs)
	return nil
}

func execPod() error {
	client := api.NewClient()
	client.SetToken(config.GetConfig().Token)

	apps, err := client.ListApps()
	if err != nil {
		return err
	}

	appPrompt := promptui.Select{
		Label: "Select Application",
		Items: apps,
	}
	_, app, err := appPrompt.Run()
	if err != nil {
		return err
	}

	pods, err := client.ListPods(app.ID)
	if err != nil {
		return err
	}

	podPrompt := promptui.Select{
		Label: "Select Pod",
		Items: pods,
	}
	_, pod, err := podPrompt.Run()
	if err != nil {
		return err
	}

	prompt := promptui.Prompt{
		Label: "Command to execute",
	}
	command, err := prompt.Run()
	if err != nil {
		return err
	}

	output, err := client.ExecCommand(app.ID, pod.Name, command)
	if err != nil {
		return err
	}
	defer output.Close()

	io.Copy(os.Stdout, output)
	return nil
}

func copyFiles() error {
	client := api.NewClient()
	client.SetToken(config.GetConfig().Token)

	apps, err := client.ListApps()
	if err != nil {
		return err
	}

	appPrompt := promptui.Select{
		Label: "Select Application",
		Items: apps,
	}
	_, app, err := appPrompt.Run()
	if err != nil {
		return err
	}

	pods, err := client.ListPods(app.ID)
	if err != nil {
		return err
	}

	podPrompt := promptui.Select{
		Label: "Select Pod",
		Items: pods,
	}
	_, pod, err := podPrompt.Run()
	if err != nil {
		return err
	}

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
	if err := client.CopyFile(app.ID, pod.Name, srcPath, destPath, isUpload); err != nil {
		return err
	}

	color.Green("File %s successfully!", strings.ToLower(direction))
	return nil
}
