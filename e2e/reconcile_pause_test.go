//go:build e2e

// T4.2's acceptance on real hardware: the reconcile pause holds a namespace a
// restore deleted, and recovery mode holds every project until activation.
//
// This is not a unit test with a fake API server. It needs an INSTALLED lab
// cluster — the operator running from the candidate chart, a control plane
// that created the project, and k3s on the node — because the whole claim is
// about what happens when the operator is restarted and a real namespace is
// really deleted:
//
//	the pause is on the Project CR in kubenest-system, so it survives the
//	project namespace's deletion and an operator restart;
//	deleting the namespace while the pause is set is not undone by the
//	reconcilers;
//	the Project's conditions are the in-cluster acknowledgement;
//	recovery mode holds every project until it carries the activation
//	annotation, and removing the pause alone releases nothing.
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000
//	export KUBENEST_CLI_TOKEN=knp_...
//	cd kubenest-cli && go test -tags e2e -v -timeout 30m ./e2e/ -run 'TestReconcilePauseHoldsNamespace|TestRecoveryModeHoldsProjects'
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/uninstall"
)

// operatorRequeueInterval is op3's utils.DefaultRequeueDelay: the interval the
// Project reconciler requeues a HELD project at, which sets how long a settling
// window has to be to see a release or an unwanted re-create. It is a constant
// here rather than a build dependency because kubenest-cli does not import op3;
// it must move with that value.
const operatorRequeueInterval = 30 * time.Second

const (
	pauseAnnotation     = "kubenest.io/reconcile-paused"
	activateAnnotation  = "kubenest.io/reconcile-activated"
	projectCRNamespace  = "kubenest-system"
	operatorTokenSecret = "kubenest-operator-token"
)

// settlingPeriod is how long "still absent" has to be observed for. Two
// reconcile requeue intervals is the shortest window in which a reconciler that
// meant to recreate the namespace would have done it at least twice; the
// bundle's component-ready deadline is the ceiling, because a wait longer than
// the platform's own convergence budget would fail a healthy cluster.
func settlingPeriod(t *testing.T, deadline time.Duration) time.Duration {
	t.Helper()
	settle := 2 * operatorRequeueInterval
	if deadline < settle {
		t.Fatalf("limits.timeouts.component-ready is %s, shorter than the %s a settling window needs (two reconcile requeue intervals)", deadline, settle)
	}
	return settle
}

// TestReconcilePauseHoldsNamespace is the pause's acceptance: with the hub
// connection cut and the operator restarted, deleting a paused project's
// namespace is not undone.
func TestReconcilePauseHoldsNamespace(t *testing.T) {
	env := gateEnvironment(t)
	nodes := connectNodes(t, env)

	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	bundle := fetchBundle(t, client, env.bundle)
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		t.Fatal(err)
	}
	settle := settlingPeriod(t, deadline)

	// A project the control plane created. The Project CR carries the backend's
	// ownership identity, which is what makes the operator's namespace its own
	// to manage (the bearer guard in project_controller.go checks the same
	// labels).
	project := envOr("KUBENEST_E2E_PROJECT", "e2e-reconcile-pause")
	t.Cleanup(func() {
		labTry(nodes, fmt.Sprintf("delete project %s -n %s --ignore-not-found", project, projectCRNamespace))
		labTry(nodes, fmt.Sprintf("delete namespace %s --ignore-not-found --wait=false", project))
	})

	// 1. The project's namespace, reconciled by the operator.
	labApply(t, nodes, projectManifest(project))
	waitForNamespace(t, nodes, project, namespacePresent, deadline)
	t.Logf("project %s reconciled into namespace %s", project, project)

	// 2. The pause the CLI writes before its first destructive step. The value
	//    is the operation id, and the Project CR is in kubenest-system, so the
	//    hold does not live in the namespace about to be deleted.
	labKubectl(t, nodes, fmt.Sprintf("annotate project %s -n %s %s=%s --overwrite",
		project, projectCRNamespace, pauseAnnotation, "e2e-restore-op-1"))

	// 3. The operator's acknowledgement — the in-cluster report the restore
	//    waits for before it deletes anything.
	message := waitForProjectCondition(t, nodes, project, "ReconcilePaused", deadline)
	if !strings.Contains(message, "e2e-restore-op-1") {
		t.Errorf("the ReconcilePaused message does not name the operation: %q", message)
	}

	// 4. Cut the operator's hub connection and restart it. The pause has to be
	//    in the cluster, not in the operator's memory: an operator restart
	//    during a restore must not release the project, and no release event
	//    can arrive over a hub it cannot reach.
	//
	//    The hub credential is REPLACED with one the hub rejects rather than
	//    deleted: the operator's identity refs are deliberately non-optional
	//    (kn-z6e4), so a missing Secret leaves the pod in
	//    CreateContainerConfigError instead of running and disconnected.
	cutHubCredential(t, nodes, projectCRNamespace)
	labKubectl(t, nodes, fmt.Sprintf("-n %s rollout restart deployment/%s",
		projectCRNamespace, agent.DeploymentName))
	waitForOperatorRolledOut(t, nodes, projectCRNamespace, deadline)
	waitForProjectCondition(t, nodes, project, "ReconcilePaused", deadline)

	// 5. The restore's destructive step.
	labKubectl(t, nodes, fmt.Sprintf("delete namespace %s --wait=false", project))
	waitForNamespace(t, nodes, project, namespaceAbsent, deadline)

	// 6. It stays gone, for longer than two reconcile requeue intervals, and
	//    the cluster still records the operation.
	assertNamespaceStaysAbsent(t, nodes, project, settle)
	annotations := projectAnnotations(t, nodes, project)
	if annotations[pauseAnnotation] != "e2e-restore-op-1" {
		t.Errorf("the pause annotation is %q after the restart, want the operation id", annotations[pauseAnnotation])
	}

	// 7. Clearing the annotation releases the project. This is what proves step
	//    6 was the hold and not a reconciler that had stopped working.
	labKubectl(t, nodes, fmt.Sprintf("annotate project %s -n %s %s-", project, projectCRNamespace, pauseAnnotation))
	waitForNamespace(t, nodes, project, namespacePresent, deadline)
}

// TestRecoveryModeHoldsProjects is recovery mode's acceptance: an operator
// installed for a recovery syncs nothing until an operator activates each
// project.
func TestRecoveryModeHoldsProjects(t *testing.T) {
	env := gateEnvironment(t)
	nodes := connectNodes(t, env)

	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	bundle := fetchBundle(t, client, env.bundle)
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		t.Fatal(err)
	}
	settle := settlingPeriod(t, deadline)

	project := envOr("KUBENEST_E2E_RECOVERY_PROJECT", "e2e-recovery-hold")
	t.Cleanup(func() {
		labTry(nodes, fmt.Sprintf("delete project %s -n %s --ignore-not-found", project, projectCRNamespace))
		labTry(nodes, fmt.Sprintf("delete namespace %s --ignore-not-found --wait=false", project))
	})

	// 1. Recovery mode, set the way T4.8/T4.9 set it: the chart value on the
	//    operator's HelmChart, not an environment variable poked into a live
	//    Deployment. The lab cluster is already installed, so this is an
	//    upgrade of the same release.
	operatorNamespace, err := applyRecoveryMode(nodes, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The lab cluster is shared with the other gates; leave recovery mode
		// off when this test is done with it.
		if _, err := applyRecoveryMode(nodes, false); err != nil {
			t.Logf("cleanup: could not turn recovery mode back off: %v", err)
		}
	})
	waitForOperatorRolledOut(t, nodes, operatorNamespace, deadline)
	t.Logf("operator upgraded with kubenest.recoveryMode=true in %s", operatorNamespace)

	// 2. A project that asks for nothing at all.
	labApply(t, nodes, projectManifest(project))

	// 3. Held anyway, and the Project says why. No namespace is created: the
	//    recovered data is not the desired state yet, so a reconcile would
	//    overwrite the restore with what the control plane last knew.
	condition := waitForProjectCondition(t, nodes, project, "RecoveryMode", deadline)
	if !strings.Contains(condition, "recovery mode") && !strings.Contains(condition, activateAnnotation) {
		t.Errorf("the RecoveryMode message does not explain the release: %q", condition)
	}
	assertNamespaceStaysAbsent(t, nodes, project, settle)

	// 4. PLANTED NEGATIVE: a pause that is then removed releases nothing. If
	//    this ever releases the project, "nothing syncs until activation" is
	//    untrue for every project nobody had paused.
	labKubectl(t, nodes, fmt.Sprintf("annotate project %s -n %s %s=e2e-recovery-op-2 --overwrite",
		project, projectCRNamespace, pauseAnnotation))
	waitForProjectCondition(t, nodes, project, "ReconcilePaused", deadline)
	assertNamespaceStaysAbsent(t, nodes, project, settle)

	labKubectl(t, nodes, fmt.Sprintf("annotate project %s -n %s %s-", project, projectCRNamespace, pauseAnnotation))
	assertNamespaceStaysAbsent(t, nodes, project, settle)

	// 5. Activation is the only release.
	labKubectl(t, nodes, fmt.Sprintf("annotate project %s -n %s %s=e2e-recovery-op-2",
		project, projectCRNamespace, activateAnnotation))
	waitForNamespace(t, nodes, project, namespacePresent, deadline)
}

const (
	namespacePresent = "present"
	namespaceAbsent  = "absent"
)

// projectManifest renders the Project CR the control plane's project_create
// intent writes: in kubenest-system, with the backend ownership identity the
// operator's bearer guard verifies.
func projectManifest(name string) string {
	return fmt.Sprintf(`apiVersion: apps.kubenest.io/v1
kind: Project
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubenest.io/managed-by: backend
    app.kubenest.io/project-id: e2e-%s
spec:
  displayName: "e2e %s"
`, name, projectCRNamespace, name, name)
}

// labApply writes a manifest the way the installers do: over stdin, never as
// an argument of the shell sshd spawns.
func labApply(t *testing.T, nodes []uninstall.Node, manifest string) {
	t.Helper()
	res, err := nodes[0].Runner.RunInput(context.Background(),
		"sudo -n k3s kubectl apply -f -", strings.NewReader(manifest))
	if err != nil {
		t.Fatalf("apply manifest: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("apply manifest: exit %d: %s", res.ExitCode, res.Stderr)
	}
}

func labKubectl(t *testing.T, nodes []uninstall.Node, args string) string {
	t.Helper()
	out, err := k3s.Kubectl(context.Background(), nodes[0].Runner, args)
	if err != nil {
		t.Fatalf("kubectl %s: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// labTry is labKubectl for cleanup, where a failure is a log line rather than
// a test verdict.
func labTry(nodes []uninstall.Node, args string) {
	_, _ = k3s.Kubectl(context.Background(), nodes[0].Runner, args)
}

// namespaceProbe observes whether a namespace exists, using kubectl's own exit
// status rather than parsing a listing.
func namespaceProbe(nodes []uninstall.Node, name, want string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		_, err := k3s.Kubectl(ctx, nodes[0].Runner, "get namespace "+name)
		if err == nil {
			if want == namespacePresent {
				return true, converge.State{Object: "namespace/" + name, Status: namespacePresent}, nil
			}
			return false, converge.State{Object: "namespace/" + name, Status: namespacePresent}, nil
		}
		if !strings.Contains(err.Error(), "NotFound") {
			return false, converge.State{Object: "namespace/" + name, Status: "unobservable"}, err
		}
		if want == namespaceAbsent {
			return true, converge.State{Object: "namespace/" + name, Status: namespaceAbsent}, nil
		}
		return false, converge.State{Object: "namespace/" + name, Status: "absent"}, nil
	}
}

func waitForNamespace(t *testing.T, nodes []uninstall.Node, name, want string, deadline time.Duration) {
	t.Helper()
	result, err := converge.Wait(context.Background(), namespaceProbe(nodes, name, want), converge.Options{
		Name:     "namespace-" + name + "-" + want,
		Deadline: deadline,
		Reporter: reporterTo(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
}

// assertNamespaceStaysAbsent samples for a whole settling window rather than
// once: a reconciler that recreates the namespace would do it on its own
// requeue, not instantly.
func assertNamespaceStaysAbsent(t *testing.T, nodes []uninstall.Node, name string, settle time.Duration) {
	t.Helper()
	deadline := time.Now().Add(settle)
	for {
		if _, err := k3s.Kubectl(context.Background(), nodes[0].Runner, "get namespace "+name); err == nil {
			t.Fatalf("namespace %s came back while the reconcile hold was in force", name)
		} else if !strings.Contains(err.Error(), "NotFound") {
			t.Fatalf("could not observe namespace %s: %v", name, err)
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(operatorRequeueInterval)
	}
}

// waitForProjectCondition waits for a Project condition to be True and returns
// its message, which is the operator's acknowledgement of the hold.
func waitForProjectCondition(t *testing.T, nodes []uninstall.Node, project, conditionType string, deadline time.Duration) string {
	t.Helper()
	var message string
	probe := func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, nodes[0].Runner,
			fmt.Sprintf("get project %s -n %s -o json", project, projectCRNamespace))
		if err != nil {
			return false, converge.State{Object: "project/" + project, Status: "unreadable"}, err
		}
		var doc struct {
			Status struct {
				Conditions []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			return false, converge.State{Object: "project/" + project, Status: "unparsable"}, err
		}
		for _, condition := range doc.Status.Conditions {
			if condition.Type == conditionType && condition.Status == "True" {
				message = condition.Message
				return true, converge.State{Object: "project/" + project, Status: condition.Reason, Detail: condition.Message}, nil
			}
		}
		return false, converge.State{Object: "project/" + project, Status: "no " + conditionType + " condition yet"}, nil
	}
	result, err := converge.Wait(context.Background(), probe, converge.Options{
		Name:     "project-" + project + "-" + conditionType,
		Deadline: deadline,
		Reporter: reporterTo(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	return message
}

// waitForOperatorRolledOut waits for the restarted (or upgraded) operator pod
// to have replaced the old one. It deliberately does NOT wait for the
// Deployment's Available condition: with the hub credential rejected the
// operator's readiness is EXPECTED to fail (kn-m6wk — a started-but-unconnected
// agent is READY 0/1), and waiting for Available would hang on exactly the
// state this test creates.
func waitForOperatorRolledOut(t *testing.T, nodes []uninstall.Node, namespace string, deadline time.Duration) {
	t.Helper()
	probe := func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, nodes[0].Runner,
			fmt.Sprintf("get deployment %s -n %s -o json", agent.DeploymentName, namespace))
		if err != nil {
			return false, converge.State{Object: "deployment/" + agent.DeploymentName, Status: "unreadable"}, err
		}
		var deployment struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Spec struct {
				Replicas int32 `json:"replicas"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				Replicas           int32 `json:"replicas"`
				UpdatedReplicas    int32 `json:"updatedReplicas"`
			} `json:"status"`
		}
		if err := json.Unmarshal([]byte(out), &deployment); err != nil {
			return false, converge.State{Object: "deployment/" + agent.DeploymentName, Status: "unparsable"}, err
		}
		want := deployment.Spec.Replicas
		state := converge.State{
			Object: "deployment/" + agent.DeploymentName,
			Status: fmt.Sprintf("generation %d/%d, %d/%d updated, %d running",
				deployment.Status.ObservedGeneration, deployment.Metadata.Generation,
				deployment.Status.UpdatedReplicas, want, deployment.Status.Replicas),
		}
		if deployment.Status.ObservedGeneration != deployment.Metadata.Generation {
			return false, state, nil
		}
		if deployment.Status.UpdatedReplicas != want || deployment.Status.Replicas != want {
			return false, state, nil
		}
		return true, state, nil
	}
	result, err := converge.Wait(context.Background(), probe, converge.Options{
		Name: "operator-rolled-out", Deadline: deadline, Reporter: reporterTo(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
}

// cutHubCredential replaces the operator's hub token with one the hub rejects,
// keeping the rest of the identity Secret intact: the pod has to be able to
// start, because a started-but-unconnected agent is the state this gate needs.
// The original token is restored when the test ends.
func cutHubCredential(t *testing.T, nodes []uninstall.Node, namespace string) {
	t.Helper()
	out := labKubectl(t, nodes, fmt.Sprintf("get secret %s -n %s -o json", operatorTokenSecret, namespace))
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &secret); err != nil {
		t.Fatalf("read the operator identity Secret: %v", err)
	}
	if secret.Data["cluster-id"] == "" || secret.Data["token"] == "" {
		t.Fatalf("the operator identity Secret has no hub credential (cluster-id=%v token=%v): this gate needs a managed cluster whose hub dial can be cut",
			secret.Data["cluster-id"] != "", secret.Data["token"] != "")
	}
	original := secret.Data
	t.Cleanup(func() {
		if err := writeOperatorIdentity(nodes, namespace, original); err != nil {
			t.Logf("cleanup: could not restore the operator hub credential: %v", err)
		}
	})

	rejected := make(map[string]string, len(original))
	for key, value := range original {
		rejected[key] = value
	}
	rejected["token"] = base64.StdEncoding.EncodeToString([]byte("e2e-rejected-token"))
	if err := writeOperatorIdentity(nodes, namespace, rejected); err != nil {
		t.Fatal(err)
	}
}

// writeOperatorIdentity writes the operator's identity Secret verbatim, with
// every value already base64-encoded.
func writeOperatorIdentity(nodes []uninstall.Node, namespace string, data map[string]string) error {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	body := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\ndata:\n", operatorTokenSecret, namespace)
	for _, key := range keys {
		body += fmt.Sprintf("  %s: %s\n", key, data[key])
	}
	res, err := nodes[0].Runner.RunInput(context.Background(),
		"sudo -n k3s kubectl apply -f -", strings.NewReader(body))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("apply the operator identity Secret: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// projectAnnotations reads a Project's annotations through the JSON the API
// returns, rather than a jsonpath expression: the annotation keys contain dots,
// which jsonpath needs escaped, and a wrong escape reads as "absent".
func projectAnnotations(t *testing.T, nodes []uninstall.Node, project string) map[string]string {
	t.Helper()
	out := labKubectl(t, nodes, fmt.Sprintf("get project %s -n %s -o json", project, projectCRNamespace))
	var doc struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("read Project %s annotations: %v", project, err)
	}
	return doc.Metadata.Annotations
}

// applyRecoveryMode sets kubenest.recoveryMode on the operator's HelmChart —
// the same chart value T4.8/T4.9 pass through agent.ValuesOptions — and
// returns the operator's namespace. Editing the release's values is how the
// lab cluster's operator is upgraded without a control plane mint.
func applyRecoveryMode(nodes []uninstall.Node, enabled bool) (string, error) {
	ctx := context.Background()
	out, err := k3s.Kubectl(ctx, nodes[0].Runner,
		fmt.Sprintf("get helmchart %s -n kube-system -o json", agent.ReleaseName()))
	if err != nil {
		return "", err
	}
	var chart struct {
		Spec struct {
			TargetNamespace string `json:"targetNamespace"`
			ValuesContent   string `json:"valuesContent"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &chart); err != nil {
		return "", fmt.Errorf("read HelmChart %s: %w", agent.ReleaseName(), err)
	}
	if chart.Spec.TargetNamespace == "" {
		return "", fmt.Errorf("HelmChart %s names no target namespace", agent.ReleaseName())
	}

	values := map[string]any{}
	if chart.Spec.ValuesContent != "" {
		if err := yaml.Unmarshal([]byte(chart.Spec.ValuesContent), &values); err != nil {
			return "", fmt.Errorf("read HelmChart %s values: %w", agent.ReleaseName(), err)
		}
	}
	kubenest, ok := values["kubenest"].(map[string]any)
	if !ok {
		kubenest = map[string]any{}
		values["kubenest"] = kubenest
	}
	kubenest["recoveryMode"] = enabled
	rendered, err := yaml.Marshal(values)
	if err != nil {
		return "", err
	}
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"valuesContent": string(rendered)},
	})
	if err != nil {
		return "", err
	}

	res, err := nodes[0].Runner.RunInput(ctx,
		fmt.Sprintf("sudo -n k3s kubectl patch helmchart %s -n kube-system --type=merge --patch-file=/dev/stdin", agent.ReleaseName()),
		bytes.NewReader(patch))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("patch HelmChart %s: exit %d: %s", agent.ReleaseName(), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return chart.Spec.TargetNamespace, nil
}
