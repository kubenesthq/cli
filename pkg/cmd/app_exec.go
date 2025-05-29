package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

func NewAppExecCommand() *cobra.Command {
	var (
		component    string
		command      string
		container    string
		podName      string
		clusterName  string
		projectName  string
		teamName     string
	)

	cmd := &cobra.Command{
		Use:   "exec <app-name>",
		Short: "Execute a command in an app component",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appName := args[0]
			if component == "" {
				return fmt.Errorf("--component is required")
			}
			if command == "" {
				return fmt.Errorf("--command is required")
			}

			cfg, _ := config.LoadConfig()
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
			} else if cfg.TeamUUID == "" {
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
					targetApp = &app
					break
				}
			}
			if targetApp == nil {
				return fmt.Errorf("app not found: %s", appName)
			}

			if projectName != "" {
				if projectName != targetApp.Project.Name && projectName != targetApp.Project.UUID {
					return fmt.Errorf("app %s is not in project %s", appName, projectName)
				}
			}

			if clusterName != "" {
				if clusterName != targetApp.Cluster.Name && clusterName != targetApp.Cluster.UUID {
					return fmt.Errorf("app %s is not in cluster %s", appName, clusterName)
				}
			}

			kubeconfigB64, namespace, err := client.GetProjectKubeconfig(context.Background(), targetApp.Project.UUID)
			if err != nil {
				return fmt.Errorf("failed to get kubeconfig: %w", err)
			}

			kubeconfigBytes, err := base64.StdEncoding.DecodeString(kubeconfigB64)
			if err != nil {
				return fmt.Errorf("failed to decode kubeconfig: %w", err)
			}

			// Build k8s client config
			restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
			if err != nil {
				return fmt.Errorf("failed to build rest config: %w", err)
			}

			clientset, err := kubernetes.NewForConfig(restConfig)
			if err != nil {
				return fmt.Errorf("failed to create k8s client: %w", err)
			}

			// Find pod if not specified
			actualPodName := podName
			if actualPodName == "" {
				pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					return fmt.Errorf("failed to list pods: %w", err)
				}
				if len(pods.Items) == 0 {
					return fmt.Errorf("no pods found in namespace %s", namespace)
				}
				actualPodName = pods.Items[0].Name
			}

			// Find container if not specified
			actualContainer := container
			if actualContainer == "" {
				pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), actualPodName, metav1.GetOptions{})
				if err != nil {
					return fmt.Errorf("failed to get pod: %w", err)
				}
				if len(pod.Spec.Containers) == 0 {
					return fmt.Errorf("no containers found in pod %s", actualPodName)
				}
				actualContainer = pod.Spec.Containers[0].Name
			}

			// Prepare exec request
			req := clientset.CoreV1().RESTClient().
				Post().
				Resource("pods").
				Name(actualPodName).
				Namespace(namespace).
				SubResource("exec").
				Param("container", actualContainer).
				Param("stdin", "true").
				Param("stdout", "true").
				Param("stderr", "true").
				Param("tty", "true")

			// Split command string into args
			cmdArgs := []string{"sh", "-c", command}
			for _, c := range cmdArgs {
				req = req.Param("command", c)
			}

			exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
			if err != nil {
				return fmt.Errorf("failed to create SPDY executor: %w", err)
			}

			// Stream options
			streamOpts := remotecommand.StreamOptions{
				Stdin:  os.Stdin,
				Stdout: os.Stdout,
				Stderr: os.Stderr,
				Tty:    true,
			}

			if err := exec.StreamWithContext(context.Background(), streamOpts); err != nil {
				return fmt.Errorf("exec stream failed: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&component, "component", "", "Component name (required)")
	cmd.Flags().StringVar(&command, "command", "", "Command to execute (required)")
	cmd.Flags().StringVar(&container, "container", "", "Container name (optional)")
	cmd.Flags().StringVar(&podName, "pod", "", "Pod name (optional)")
	cmd.Flags().StringVar(&clusterName, "cluster", "", "Cluster name or UUID (optional)")
	cmd.Flags().StringVar(&projectName, "project", "", "Project name or UUID (optional)")
	cmd.Flags().StringVar(&teamName, "team", "", "Team name or UUID (optional)")

	cmd.MarkFlagRequired("component")
	cmd.MarkFlagRequired("command")

	return cmd
}
