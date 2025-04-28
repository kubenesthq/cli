package cmd

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/config"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func NewAppLogsCommand() *cobra.Command {
	var appUUID string
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs --app <uuid>",
		Short: "Get logs for an app",
		RunE: func(cmd *cobra.Command, args []string) error {
			if appUUID == "" {
				return fmt.Errorf("must specify --app <uuid>")
			}
			cfg, _ := config.LoadConfig()
			client, _ := api.NewClient()
			client.SetToken(cfg.Token)
			client.SetTeamUUID(cfg.TeamUUID)

			// 1. Fetch stackdeploy details
			detail, err := client.GetStackDeployDetail(context.Background(), appUUID)
			if err != nil {
				return fmt.Errorf("failed to get stackdeploy details: %w", err)
			}
			fmt.Printf("Stack name: %s\n", detail.Stack.Name)
			fmt.Printf("Project UUID: %s\n", detail.Project.UUID)

			// 2. Fetch kubeconfig and namespace
			kubeconfigB64, namespace, err := client.GetProjectKubeconfig(context.Background(), detail.Project.UUID)
			if err != nil {
				return fmt.Errorf("failed to get kubeconfig: %w", err)
			}
			kubeconfigBytes, err := base64.StdEncoding.DecodeString(kubeconfigB64)
			if err != nil {
				return fmt.Errorf("failed to decode kubeconfig: %w", err)
			}
			fmt.Printf("Using namespace: %s\n", namespace)

			label := fmt.Sprintf("release=%s", detail.Stack.Name)
			if err = streamLogsFromPods(label, namespace, string(kubeconfigBytes), follow); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to stream logs: %v\n", err)
				os.Exit(1)
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&appUUID, "app", "", "App UUID")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow the pod logs")
	return cmd
}

func streamLogsFromPods(label, namespace, kubeconfig string, stream bool) error {
	config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeconfig))
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: label,
	})
	if err != nil {
		return err
	}

	for _, pod := range pods.Items {
		if stream {
			fmt.Printf("Streaming logs for pod %s\n", pod.Name)
			err := streamPodLogs(clientset, pod, namespace)
			if err != nil {
				fmt.Printf("Error streaming logs for pod %s: %v\n", pod.Name, err)
			}
		} else {
			fmt.Printf("Getting last 100 lines of logs for pod %s\n", pod.Name)
			err := getLastPodLogs(clientset, pod, namespace)
			if err != nil {
				fmt.Printf("Error getting logs for pod %s: %v\n", pod.Name, err)
			}
		}
	}

	return nil
}

func streamPodLogs(clientset *kubernetes.Clientset, pod v1.Pod, namespace string) error {
	req := clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &v1.PodLogOptions{
		Follow: true,
	})

	podLogs, err := req.Stream(context.TODO())
	if err != nil {
		return err
	}
	defer podLogs.Close()

	scanner := bufio.NewScanner(podLogs)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func getLastPodLogs(clientset *kubernetes.Clientset, pod v1.Pod, namespace string) error {
	tail := int64(100)
	req := clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &v1.PodLogOptions{
		TailLines: &tail,
	})

	podLogs, err := req.Stream(context.TODO())
	if err != nil {
		return err
	}
	defer podLogs.Close()

	scanner := bufio.NewScanner(podLogs)
	for scanner.Scan() {
		fmt.Println(scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}
