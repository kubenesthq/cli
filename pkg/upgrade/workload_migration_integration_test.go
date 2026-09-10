//go:build integration

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

// dockerExecRunner is a real k3s-server transport for the local-profile
// integration test. The production CLI speaks SSH to a server host; k3d puts
// that same host in a Docker container. This adapter intentionally executes
// the exact command and stdin stream on the live k3s server instead of
// scripting responses.
type dockerExecRunner struct{ container string }

func (r dockerExecRunner) Run(ctx context.Context, command string) (sshx.Result, error) {
	return r.run(ctx, command, nil)
}

func (r dockerExecRunner) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	return r.run(ctx, command, stdin)
}

func (r dockerExecRunner) run(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	// k3d's server container runs as root, so sudo is unavailable and
	// unnecessary there. Its v1.35 image also has a broken `k3s kubectl`
	// wrapper (it errors with "unknown command kubectl for kubectl") while the
	// bundled kubectl binary talks to the same live API server successfully.
	// Keep this substitution in the k3d-only adapter; production still uses the
	// host's `k3s kubectl` command over SSH.
	command = strings.ReplaceAll(command, "sudo -n ", "")
	command = strings.Replace(command, "k3s kubectl ", "kubectl ", 1)
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", r.container, "sh", "-ceu", command)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := sshx.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, err
}

func integrationEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Skipf("set %s to run against a real local-profile cluster", key)
	}
	return value
}

// TestWorkloadApplicationOwnershipMigrationOnRealCluster is intentionally
// opt-in. It verifies the supported same-bundle carrier against a real
// backend, hub, k3s server, operator and running workload. It begins only
// after the operator's own ConfigMap reports closed; supplied Helm values are
// not evidence that the receiver observed them.
func TestWorkloadApplicationOwnershipMigrationOnRealCluster(t *testing.T) {
	apiURL := integrationEnv(t, "KUBENEST_E2E_API_URL")
	token := integrationEnv(t, "KUBENEST_E2E_TOKEN")
	clusterID := integrationEnv(t, "KUBENEST_E2E_CLUSTER_ID")
	container := integrationEnv(t, "KUBENEST_E2E_K3D_SERVER")
	namespace := integrationEnv(t, "KUBENEST_E2E_WORKLOAD_NAMESPACE")
	bundlePath := integrationEnv(t, "KUBENEST_E2E_BUNDLE_PATH")

	bundle, err := manifest.Load(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.New(apiURL, api.WithToken(token))
	if err != nil {
		t.Fatal(err)
	}
	runner := dockerExecRunner{container: container}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()

	state, err := workloadApplicationsGateState(ctx, runner)
	if err != nil {
		t.Fatalf("read receiver-recorded precondition: %v", err)
	}
	if state != "closed" {
		t.Fatalf("receiver gate before migration = %q, want closed", state)
	}
	applications, err := argoApplicationCount(ctx, runner)
	if err != nil {
		t.Fatalf("inspect Applications before migration: %v", err)
	}
	if applications == 0 {
		t.Fatal("pre-migration Application count is zero: this is an upstream application-creation observation, not evidence that workload ownership migration failed (kn-cqtb)")
	}
	ready, podState, err := k3s.CheckPodsReady(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read pre-migration workload pods: %v", err)
	}
	if !ready {
		t.Fatalf("pre-migration workload is not running and Ready: %s", podState)
	}
	uidBefore, err := readyWorkloadPodUID(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read pre-migration workload pod UID: %v", err)
	}

	session := &Session{
		Opts:     Options{MigrateWorkloadApplications: true},
		From:     bundle,
		To:       bundle,
		Jnl:      &stages.Journal{ClusterID: clusterID},
		API:      client,
		Reporter: converge.NewTextReporter(os.Stdout),
		Nodes:    []Node{{Address: container, Server: true, Runner: runner}},
	}
	if err := stageAgent(ctx, session); err != nil {
		t.Fatalf("run explicit ownership migration against live cluster: %v", err)
	}

	state, err = workloadApplicationsGateState(ctx, runner)
	if err != nil {
		t.Fatalf("read receiver-recorded result: %v", err)
	}
	if state != "open" {
		t.Fatalf("receiver gate after migration = %q, want open", state)
	}
	ready, podState, err = k3s.CheckPodsReady(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read post-migration workload pods: %v", err)
	}
	if !ready {
		t.Fatalf("post-migration workload is not running and Ready: %s", podState)
	}
	uidAfter, err := readyWorkloadPodUID(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read post-migration workload pod UID: %v", err)
	}
	if uidAfter != uidBefore {
		t.Fatalf("migration recreated the workload pod: before=%s after=%s", uidBefore, uidAfter)
	}

	values, err := k3s.Kubectl(ctx, runner, "get helmchart operator -n kube-system -o jsonpath='{.spec.valuesContent}'")
	if err != nil {
		t.Fatalf("read migrated HelmChart values: %v", err)
	}
	if !strings.Contains(values, "workloadApplications:") || !strings.Contains(values, "enabled: true") {
		t.Fatalf("the live HelmChart did not receive the explicit true that restarts the operator:\n%s", values)
	}

	t.Logf("receiver gate migrated closed→open and pods in %s remained Ready", namespace)
}

// TestWorkloadApplicationGateReopensAtTheReceiver proves the local half of
// the hand-off without inventing an API response. The ConfigMap is resolved
// only at manager startup, so a direct ConfigMap patch would be a false proof:
// it can say open while the old operator process remains closed. This test
// changes the live HelmChart through the production stdin transport and waits
// for the restarted operator to record open itself.
func TestWorkloadApplicationGateReopensAtTheReceiver(t *testing.T) {
	container := integrationEnv(t, "KUBENEST_E2E_K3D_SERVER")
	namespace := integrationEnv(t, "KUBENEST_E2E_WORKLOAD_NAMESPACE")
	bundlePath := integrationEnv(t, "KUBENEST_E2E_BUNDLE_PATH")

	bundle, err := manifest.Load(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	runner := dockerExecRunner{container: container}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()

	state, err := workloadApplicationsGateState(ctx, runner)
	if err != nil {
		t.Fatalf("read receiver-recorded precondition: %v", err)
	}
	if state != "closed" {
		t.Fatalf("receiver gate before migration = %q, want closed", state)
	}
	ready, podState, err := k3s.CheckPodsReady(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read pre-migration workload pods: %v", err)
	}
	if !ready {
		t.Fatalf("pre-migration workload is not running and Ready: %s", podState)
	}
	uidBefore, err := readyWorkloadPodUID(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read pre-restart workload pod UID: %v", err)
	}
	// The product always keeps the k3s auto-deploy source and live HelmChart
	// aligned. Make that precondition explicit in this local k3d test before
	// asking the production WriteManifest path to change the gate; otherwise a
	// stale fixture source can make an intended true write a no-op.
	alignmentID := fmt.Sprintf("%d", time.Now().UnixNano())
	current, err := liveHelmChartDocument(ctx, runner, alignmentID)
	if err != nil {
		t.Fatalf("read receiver HelmChart source document: %v", err)
	}
	if err := k3s.WriteManifest(ctx, runner, "kubenest-agent", current); err != nil {
		t.Fatalf("align receiver HelmChart source document: %v", err)
	}
	alignment, err := converge.Wait(ctx, helmChartSourceProbe(runner, alignmentID), converge.Options{
		Name: "kubenest-agent-source-aligned", Deadline: deadlineForTest(t, bundle), Reporter: converge.NewTextReporter(os.Stdout),
	})
	if err != nil {
		t.Fatalf("wait for k3s to apply the aligned source document: %v", err)
	}
	if err := alignment.Err(); err != nil {
		t.Fatal(err)
	}

	sourceChart, liveChart, err := migratedGateCharts(ctx, runner)
	if err != nil {
		t.Fatalf("prepare explicit receiver gate: %v", err)
	}
	if err := k3s.WriteManifest(ctx, runner, "kubenest-agent", sourceChart); err != nil {
		t.Fatalf("apply explicit receiver gate: %v", err)
	}
	if err := k3s.ReplaceManifest(ctx, runner, liveChart); err != nil {
		t.Fatalf("apply live explicit receiver gate: %v", err)
	}
	result, err := converge.Wait(ctx, workloadApplicationsGateProbe(runner), converge.Options{
		Name: "workload-applications-gate-open", Deadline: deadlineForTest(t, bundle), Reporter: converge.NewTextReporter(os.Stdout),
	})
	if err != nil {
		t.Fatalf("wait for the restarted operator to record open: %v", err)
	}
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}

	values, err := k3s.Kubectl(ctx, runner, "get helmchart operator -n kube-system -o jsonpath='{.spec.valuesContent}'")
	if err != nil {
		t.Fatalf("read receiver HelmChart values: %v", err)
	}
	if !strings.Contains(values, "workloadApplications:") || !strings.Contains(values, "enabled: true") {
		t.Fatalf("the HelmChart receiver has no explicit true:\n%s", values)
	}
	ready, podState, err = k3s.CheckPodsReady(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read post-restart workload pods: %v", err)
	}
	if !ready {
		t.Fatalf("existing workload did not remain running and Ready: %s", podState)
	}
	uidAfter, err := readyWorkloadPodUID(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read post-restart workload pod UID: %v", err)
	}
	if uidAfter != uidBefore {
		t.Fatalf("receiver gate change recreated the workload pod: before=%s after=%s", uidBefore, uidAfter)
	}
	t.Logf("operator-recorded gate opened only after the live HelmChart update and the existing workload stayed Ready")
}

// TestWorkloadApplicationOwnershipRefusesAnAlreadyOperatorCluster exercises
// the API response boundary on a live composition. A cluster whose backend
// already records operator ownership cannot be handed off again: the response
// is HTTP 200/state=operator, but that is not permission to reopen a closed
// receiver gate. The migration must refuse and leave both the receiver state
// and the existing workload intact.
func TestWorkloadApplicationOwnershipRefusesAnAlreadyOperatorCluster(t *testing.T) {
	apiURL := integrationEnv(t, "KUBENEST_E2E_API_URL")
	token := integrationEnv(t, "KUBENEST_E2E_TOKEN")
	clusterID := integrationEnv(t, "KUBENEST_E2E_CLUSTER_ID")
	container := integrationEnv(t, "KUBENEST_E2E_K3D_SERVER")
	namespace := integrationEnv(t, "KUBENEST_E2E_WORKLOAD_NAMESPACE")
	bundlePath := integrationEnv(t, "KUBENEST_E2E_BUNDLE_PATH")

	bundle, err := manifest.Load(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.New(apiURL, api.WithToken(token))
	if err != nil {
		t.Fatal(err)
	}
	runner := dockerExecRunner{container: container}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	state, err := workloadApplicationsGateState(ctx, runner)
	if err != nil {
		t.Fatalf("read receiver-recorded precondition: %v", err)
	}
	if state != "closed" {
		t.Fatalf("receiver gate before refusal = %q, want closed", state)
	}
	ready, podState, err := k3s.CheckPodsReady(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read pre-refusal workload pods: %v", err)
	}
	if !ready {
		t.Fatalf("pre-refusal workload is not running and Ready: %s", podState)
	}
	uidBefore, err := readyWorkloadPodUID(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read pre-refusal workload pod UID: %v", err)
	}

	err = stageAgent(ctx, &Session{
		Opts:     Options{MigrateWorkloadApplications: true},
		From:     bundle,
		To:       bundle,
		Jnl:      &stages.Journal{ClusterID: clusterID},
		API:      client,
		Reporter: converge.NewTextReporter(os.Stdout),
		Nodes:    []Node{{Address: container, Server: true, Runner: runner}},
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe state \"operator\"") {
		t.Fatalf("same-bundle migration error = %v, want the real backend operator-state refusal", err)
	}

	state, err = workloadApplicationsGateState(ctx, runner)
	if err != nil {
		t.Fatalf("read receiver state after refusal: %v", err)
	}
	if state != "closed" {
		t.Fatalf("refused migration opened the receiver gate: state=%q", state)
	}
	ready, podState, err = k3s.CheckPodsReady(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read post-refusal workload pods: %v", err)
	}
	if !ready {
		t.Fatalf("refused migration disrupted the existing workload: %s", podState)
	}
	uidAfter, err := readyWorkloadPodUID(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read post-refusal workload pod UID: %v", err)
	}
	if uidAfter != uidBefore {
		t.Fatalf("refused migration recreated the workload pod: before=%s after=%s", uidBefore, uidAfter)
	}
	t.Logf("live HTTP 200/operator response was refused; receiver gate stayed closed and the workload stayed Ready")
}

// TestWorkloadApplicationOwnershipRefusesOpenRecordWithExplicitFalse covers
// the converse of a closed ConfigMap paired with explicit true. It is a state
// older migration code could leave behind: the ConfigMap was patched open,
// while the HelmChart still tells a restarted operator to close. The CLI must
// not treat that open record as a completed hand-off or change either record.
func TestWorkloadApplicationOwnershipRefusesOpenRecordWithExplicitFalse(t *testing.T) {
	container := integrationEnv(t, "KUBENEST_E2E_K3D_SERVER")
	namespace := integrationEnv(t, "KUBENEST_E2E_WORKLOAD_NAMESPACE")
	bundlePath := integrationEnv(t, "KUBENEST_E2E_BUNDLE_PATH")

	bundle, err := manifest.Load(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	runner := dockerExecRunner{container: container}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	state, err := workloadApplicationsGateState(ctx, runner)
	if err != nil {
		t.Fatalf("read receiver-recorded precondition: %v", err)
	}
	if state != "closed" {
		t.Fatalf("receiver gate before inconsistency = %q, want closed", state)
	}
	if _, err := k3s.Kubectl(ctx, runner,
		"patch configmap kubenest-workload-applications-gate -n kubenest-system --type merge -p '{\"data\":{\"state\":\"open\",\"reason\":\"e2e-inconsistent-record\"}}'"); err != nil {
		t.Fatalf("create real false-plus-open inconsistent record: %v", err)
	}
	state, err = workloadApplicationsGateState(ctx, runner)
	if err != nil {
		t.Fatalf("read inconsistent receiver record: %v", err)
	}
	if state != "open" {
		t.Fatalf("receiver gate after setup = %q, want open", state)
	}
	values, err := k3s.Kubectl(ctx, runner, "get helmchart "+agent.ReleaseName()+" -n kube-system -o jsonpath='{.spec.valuesContent}'")
	if err != nil {
		t.Fatalf("read HelmChart false precondition: %v", err)
	}
	enabled, err := explicitWorkloadApplicationsValue(values)
	if err != nil {
		t.Fatalf("decode HelmChart false precondition: %v", err)
	}
	if enabled {
		t.Fatalf("the live HelmChart does not carry the explicit false precondition:\n%s", values)
	}
	valuesBefore := values
	ready, podState, err := k3s.CheckPodsReady(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read pre-refusal workload pods: %v", err)
	}
	if !ready {
		t.Fatalf("pre-refusal workload is not running and Ready: %s", podState)
	}
	uidBefore, err := readyWorkloadPodUID(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read pre-refusal workload pod UID: %v", err)
	}

	err = stageAgent(ctx, &Session{
		Opts:     Options{MigrateWorkloadApplications: true},
		From:     bundle,
		To:       bundle,
		Jnl:      &stages.Journal{ClusterID: "inconsistent-record"},
		Reporter: converge.NewTextReporter(os.Stdout),
		Nodes:    []Node{{Address: container, Server: true, Runner: runner}},
	})
	if err == nil || !strings.Contains(err.Error(), "explicitly sets kubenest.workloadApplications.enabled=false") {
		t.Fatalf("false-plus-open migration error = %v, want named inconsistent-record refusal", err)
	}

	state, err = workloadApplicationsGateState(ctx, runner)
	if err != nil {
		t.Fatalf("read receiver state after refusal: %v", err)
	}
	if state != "open" {
		t.Fatalf("refused migration changed the inconsistent receiver record: state=%q, want open", state)
	}
	valuesAfter, err := k3s.Kubectl(ctx, runner, "get helmchart "+agent.ReleaseName()+" -n kube-system -o jsonpath='{.spec.valuesContent}'")
	if err != nil {
		t.Fatalf("read HelmChart after refusal: %v", err)
	}
	if valuesAfter != valuesBefore {
		t.Fatal("refused migration changed the HelmChart valuesContent")
	}
	ready, podState, err = k3s.CheckPodsReady(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read post-refusal workload pods: %v", err)
	}
	if !ready {
		t.Fatalf("refused migration disrupted the existing workload: %s", podState)
	}
	uidAfter, err := readyWorkloadPodUID(ctx, runner, namespace)
	if err != nil {
		t.Fatalf("read post-refusal workload pod UID: %v", err)
	}
	if uidAfter != uidBefore {
		t.Fatalf("refused migration recreated the workload pod: before=%s after=%s", uidBefore, uidAfter)
	}
	t.Logf("explicit false plus open record was refused without mutating either record or the running workload")
}

func explicitWorkloadApplicationsValue(valuesContent string) (bool, error) {
	values := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesContent), &values); err != nil {
		return false, err
	}
	kubenestValues, ok := values["kubenest"].(map[string]any)
	if !ok {
		return false, fmt.Errorf("missing kubenest values")
	}
	workloadValues, ok := kubenestValues["workloadApplications"].(map[string]any)
	if !ok {
		return false, fmt.Errorf("missing explicit kubenest.workloadApplications values")
	}
	enabled, ok := workloadValues["enabled"].(bool)
	if !ok {
		return false, fmt.Errorf("missing boolean kubenest.workloadApplications.enabled")
	}
	return enabled, nil
}

// argoApplicationCount deliberately queries every namespace. A namespaced
// lookup could turn an Application placed outside the assumed namespace into a
// false absence, which would incorrectly attribute an upstream creation
// failure to the ownership migration.
func argoApplicationCount(ctx context.Context, runner k3s.Runner) (int, error) {
	out, err := k3s.Kubectl(ctx, runner, "get applications.argoproj.io -A -o json")
	if err != nil {
		return 0, err
	}
	var applications struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &applications); err != nil {
		return 0, err
	}
	return len(applications.Items), nil
}

func readyWorkloadPodUID(ctx context.Context, runner k3s.Runner, namespace string) (string, error) {
	out, err := k3s.Kubectl(ctx, runner, "get pods -n "+namespace+" -o json")
	if err != nil {
		return "", err
	}
	var pods struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &pods); err != nil {
		return "", err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != "Running" {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" && pod.Metadata.UID != "" {
				return pod.Metadata.UID, nil
			}
		}
	}
	return "", fmt.Errorf("no Running/Ready workload pod in namespace %s", namespace)
}

func liveHelmChartDocument(ctx context.Context, runner k3s.Runner, alignmentID string) ([]byte, error) {
	raw, err := k3s.Kubectl(ctx, runner, "get helmchart "+agent.ReleaseName()+" -n kube-system -o json")
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	if doc["apiVersion"] != "helm.cattle.io/v1" || doc["kind"] != "HelmChart" {
		return nil, fmt.Errorf("receiver HelmChart has unexpected GVK %v/%v", doc["apiVersion"], doc["kind"])
	}
	metadata, ok := doc["metadata"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("receiver HelmChart has no metadata")
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	// This makes the source alignment observable. A simple same-spec rewrite
	// is invisible to the deploy controller and would let a stale manifest
	// source make the following migration treatment a no-op.
	annotations["kubenest.io/e2e-gate-source"] = alignmentID
	delete(doc, "status")
	return json.Marshal(doc)
}

func helmChartSourceProbe(runner k3s.Runner, want string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, runner, "get helmchart "+agent.ReleaseName()+" -n kube-system -o json")
		if err != nil {
			return false, converge.State{Object: "agent HelmChart", Status: "unobservable"}, err
		}
		var chart struct {
			Metadata struct {
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(out), &chart); err != nil {
			return false, converge.State{Object: "agent HelmChart", Status: "unparsable"}, err
		}
		if chart.Metadata.Annotations["kubenest.io/e2e-gate-source"] != want {
			return false, converge.State{Object: "agent HelmChart", Status: "waiting for streamed source"}, nil
		}
		return true, converge.State{Object: "agent HelmChart", Status: "source applied"}, nil
	}
}

func deadlineForTest(t *testing.T, bundle *manifest.Manifest) time.Duration {
	t.Helper()
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		t.Fatal(err)
	}
	return deadline
}

var _ k3s.Runner = dockerExecRunner{}
