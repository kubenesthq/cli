package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/agent"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/stages"
)

// Stage 6: Kubernetes. THE POINT OF NO RETURN.
//
// Kubernetes does not support downgrading and neither does k3s, so once the
// API server has upgraded and the datastore has written data in the new
// schema, going back means restoring the stage-2 snapshot with a service
// interruption. Everything before this stage reverts in seconds; this stage
// does not revert at all. That is why it is last.
//
// The mechanism is Rancher's system-upgrade-controller — the same tool RKE2
// users call a killer feature — which the installer already placed. We do not
// drain, cordon or replace binaries ourselves: we declare a Plan and watch it.
// Servers upgrade before agents, one node at a time, each cordoned, drained,
// upgraded and Ready again before the next is touched.

const (
	// planNamespace is where system-upgrade-controller watches for Plans.
	planNamespace = "system-upgrade"
	// serverPlan and agentPlan are the two Plans. They are separate because
	// servers must complete before agents start: an agent joining a
	// control plane it is newer than is the one skew Kubernetes does not
	// promise to tolerate.
	serverPlan = "kubenest-k3s-server"
	agentPlan  = "kubenest-k3s-agent"
)

// planDoc renders one system-upgrade-controller Plan.
//
// concurrency is 1 always: nodes upgrade one at a time so a failure leaves a
// cluster with some nodes new and some old — a supported, survivable state —
// rather than every node mid-flight at once.
func planDoc(name, version string, servers bool) ([]byte, error) {
	selector := map[string]any{
		"matchExpressions": []any{
			map[string]any{
				"key":      "node-role.kubernetes.io/control-plane",
				"operator": map[bool]string{true: "In", false: "NotIn"}[servers],
				"values":   []any{"true"},
			},
		},
	}
	spec := map[string]any{
		"concurrency":        1,
		"version":            version,
		"nodeSelector":       selector,
		"serviceAccountName": "system-upgrade",
		"cordon":             true,
		"drain": map[string]any{
			// Pods with local storage are evicted like any other: OpenEBS
			// volumes are node-local, so their pods return to the same node
			// after the upgrade. Refusing to evict them would stall every
			// drain on a cluster that uses the platform's own storage.
			"force":                    false,
			"deleteEmptydirData":       true,
			"ignoreDaemonSets":         true,
			"disableEviction":          false,
			"skipWaitForDeleteTimeout": 60,
		},
		"upgrade": map[string]any{"image": "rancher/k3s-upgrade"},
	}
	if !servers {
		// Agents wait for the servers, by the controller's own dependency
		// mechanism rather than by us polling and hoping.
		spec["prepare"] = map[string]any{
			"image": "rancher/k3s-upgrade",
			"args":  []any{"prepare", serverPlan},
		}
	}
	doc := map[string]any{
		"apiVersion": "upgrade.cattle.io/v1",
		"kind":       "Plan",
		"metadata":   map[string]any{"name": name, "namespace": planNamespace},
		"spec":       spec,
	}
	return yaml.Marshal(doc)
}

// stageKubernetes moves every node to the target Kubernetes version.
func stageKubernetes(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}
	from, _ := s.From.Core.Version("k3s")
	target, err := s.To.Core.Version("k3s")
	if err != nil {
		return err
	}
	if from == target {
		s.Logf("  Kubernetes unchanged at %s", target)
		return nil
	}

	s.Logf("  Kubernetes %s → %s. This is the point of no return: from here, going back means", from, target)
	s.Logf("  restoring the datastore snapshot %s, with a service interruption.", s.Record.Snapshot)

	perNode, err := s.To.Limits.Timeouts.For("upgrade-per-node")
	if err != nil {
		return err
	}

	plans := []struct {
		name    string
		servers bool
		count   int
	}{
		{serverPlan, true, s.count(true)},
		{agentPlan, false, s.count(false)},
	}
	for _, p := range plans {
		if p.count == 0 {
			continue
		}
		doc, err := planDoc(p.name, target, p.servers)
		if err != nil {
			return stages.NewComponentError("k3s", err)
		}
		if err := k3s.WriteManifest(ctx, server, p.name, doc); err != nil {
			return stages.NewComponentError("k3s", err)
		}
		// One node, end to end, has its own deadline, so a slow loop cannot
		// run indefinitely; the whole plan gets that per node.
		deadline := perNode * time.Duration(p.count)
		if err := waitForPlan(ctx, server, p.name, target, p.count, deadline, s.Reporter); err != nil {
			return stages.NewComponentError("k3s", err)
		}
	}
	return nil
}

func (s *Session) count(servers bool) int {
	n := 0
	for _, node := range s.Nodes {
		if node.Server == servers {
			n++
		}
	}
	return n
}

// waitForPlan converges on every targeted node reporting the new version.
//
// The Plan's own status is not the condition — a Plan can be Complete while a
// node is still coming back — so the check is the thing that actually
// matters: the nodes report the version, and they are Ready.
func waitForPlan(ctx context.Context, r k3s.Runner, plan, target string, want int, deadline time.Duration, rep converge.Reporter) error {
	res, err := converge.Wait(ctx, planProbe(r, plan, target, want), converge.Options{
		Name:     plan,
		Deadline: deadline,
		Interval: 15 * time.Second,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

type nodeVersions struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			NodeInfo struct {
				KubeletVersion string `json:"kubeletVersion"`
			} `json:"nodeInfo"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// planProbe reports how many targeted nodes are on the new version AND Ready.
//
// A node mid-upgrade is cordoned, drained and restarting: every one of those
// is an observation, not a verdict, and only the deadline decides. A node
// left cordoned at the deadline is named as such, because "upgraded but
// still cordoned" is a different problem from "did not upgrade".
func planProbe(r k3s.Runner, plan, target string, want int) converge.Probe {
	servers := plan == serverPlan
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get nodes -o json")
		if err != nil {
			return false, converge.State{
				Object: "nodes",
				Status: "the API server is not answering",
				Detail: "expected while a control-plane node restarts",
			}, err
		}
		var nodes nodeVersions
		if err := json.Unmarshal([]byte(out), &nodes); err != nil {
			return false, converge.State{Object: "nodes", Status: "unparsable"}, err
		}

		done := 0
		var pending converge.State
		for _, n := range nodes.Items {
			_, isServer := n.Metadata.Labels["node-role.kubernetes.io/control-plane"]
			if isServer != servers {
				continue
			}
			ready := false
			var detail string
			for _, c := range n.Status.Conditions {
				if c.Type != "Ready" {
					continue
				}
				ready = c.Status == "True"
				if !ready {
					detail = c.Reason + ": " + c.Message
				}
			}
			switch {
			case n.Status.NodeInfo.KubeletVersion != target:
				pending = converge.State{
					Object: "node " + n.Metadata.Name,
					Status: "on " + n.Status.NodeInfo.KubeletVersion + ", waiting for " + target,
					Detail: detail,
				}
			case !ready:
				pending = converge.State{Object: "node " + n.Metadata.Name, Status: "upgraded but not Ready", Detail: detail}
			case n.Spec.Unschedulable:
				pending = converge.State{
					Object: "node " + n.Metadata.Name,
					Status: "upgraded and Ready but still cordoned",
					Detail: "system-upgrade-controller uncordons after its job completes",
				}
			default:
				done++
			}
		}
		if done >= want {
			return true, converge.State{
				Object: strings.TrimPrefix(plan, "kubenest-"),
				Status: fmt.Sprintf("%d/%d node(s) on %s", done, want, target),
			}, nil
		}
		if pending.Object == "" {
			pending = converge.State{
				Object: strings.TrimPrefix(plan, "kubenest-"),
				Status: fmt.Sprintf("%d/%d node(s) on %s", done, want, target),
			}
		}
		return false, pending, nil
	}
}

// upgradeAgentChart moves the KubeNest agent to a new chart version without
// re-minting its identity. An ordinary bundle upgrade changes only the chart
// pin: valuesContent can contain credentials and customer choices, and is
// deliberately left exactly as it was.
func upgradeAgentChart(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, version string, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"spec":{"version":%q}}`, version)
	if _, err := k3s.Kubectl(ctx, r,
		fmt.Sprintf("patch helmchart %s -n kube-system --type merge -p %s", agent.ReleaseName(), shellQuote(patch))); err != nil {
		return fmt.Errorf("moving the agent chart to %s: %w", version, err)
	}

	res, err := converge.Wait(ctx, agentUpgradedProbe(r, version), converge.Options{
		Name: "kubenest-agent", Deadline: deadline, Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// migrateWorkloadApplicationOwnership is the explicit, one-way hand-off from
// backend-owned workload Applications to the in-cluster operator. This is not
// implicit in a chart version: a caller must opt in because an explicit false
// could be a deliberate user choice. The ConfigMap is only a startup record:
// changing it under an already-running operator does not change the resolved
// in-memory gate. This one deliberate hand-off therefore renders an explicit
// true into the existing HelmChart and waits for the restarted operator to
// record open itself. It preserves the cluster identity, deploy key, and every
// other Helm value.
func migrateWorkloadApplicationOwnership(ctx context.Context, r k3s.Runner, apiClient *api.Client, clusterID string, bundle *manifest.Manifest, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	state, err := workloadApplicationsGateState(ctx, r)
	if err != nil {
		return fmt.Errorf("reading the operator's workload-application gate before migration: %w", err)
	}
	if state != "closed" {
		if state == "open" {
			explicitlyDisabled, err := workloadApplicationsExplicitlyDisabled(ctx, r)
			if err != nil {
				return fmt.Errorf("reading the HelmChart workload-application value before migration: %w", err)
			}
			if explicitlyDisabled {
				return fmt.Errorf("cannot migrate workload-application ownership: the operator gate records state=open while the HelmChart explicitly sets kubenest.workloadApplications.enabled=false; do not edit the operator-managed ConfigMap, reconcile the HelmChart so a restarted operator records closed, then re-run the migration")
			}
		}
		return fmt.Errorf("cannot migrate workload-application ownership: the operator gate records state=%q, want closed; inspect kubenest-workload-applications-gate before retrying", state)
	}
	sourceChart, liveChart, err := migratedGateCharts(ctx, r)
	if err != nil {
		return err
	}

	// Prepare the local update before changing backend ownership. A malformed
	// HelmChart must not strand the backend in migrating before it has a way to
	// open the operator side of the hand-off.
	if apiClient == nil || clusterID == "" {
		return fmt.Errorf("cannot migrate workload-application ownership: this upgrade has no registered control-plane cluster")
	}
	detach, err := apiClient.DetachWorkloadApplications(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("detaching host workload Applications before opening the operator gate: %w", err)
	}
	if detach == nil || detach.State != "migrating" {
		return fmt.Errorf("detaching host workload Applications returned unsafe state %q (%s); refusing to open the operator gate", detachState(detach), detachMessage(detach))
	}
	for _, result := range detach.Results {
		if result.Result != "detached" && result.Result != "already_absent" {
			return fmt.Errorf("workload application %s was not detached (%s); refusing to open the operator gate", result.Application, result.Result)
		}
	}
	// valuesContent can hold the agent JWT and Git deploy key. Stream the
	// complete typed HelmChart through stdin rather than placing either in a
	// kubectl patch command line. The new explicit value makes Helm roll the
	// controller; that startup is what resolves (and records) the new gate.
	// Never patch the ConfigMap ourselves: it is a report of the receiver's
	// resolved state, not a dynamic control plane for an already-running pod.
	if err := k3s.WriteManifest(ctx, r, "kubenest-agent", sourceChart); err != nil {
		return fmt.Errorf("recording the explicit workload-application hand-off: %w", err)
	}
	if err := k3s.ReplaceManifest(ctx, r, liveChart); err != nil {
		return fmt.Errorf("applying the explicit workload-application hand-off: %w", err)
	}
	gate, err := converge.Wait(ctx, workloadApplicationsGateProbe(r), converge.Options{
		Name: "workload-applications-gate-open", Deadline: deadline, Reporter: rep,
	})
	if err != nil {
		return err
	}
	return gate.Err()
}

// migratedGateCharts returns two forms of the same changed HelmChart. source
// persists without a resourceVersion for k3s's next boot; live carries the
// observed version for an optimistic replace now. Both differ from the
// existing resource only at kubenest.workloadApplications.enabled=true. An
// unset value is deliberately made explicit: the receiver ConfigMap records a
// startup decision, so merely changing that record cannot reopen the running
// operator.
func migratedGateCharts(ctx context.Context, r k3s.Runner) (source, live []byte, err error) {
	var chart struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name            string            `json:"name"`
			Namespace       string            `json:"namespace"`
			ResourceVersion string            `json:"resourceVersion,omitempty"`
			Labels          map[string]string `json:"labels,omitempty"`
			Annotations     map[string]string `json:"annotations,omitempty"`
			Finalizers      []string          `json:"finalizers,omitempty"`
		} `json:"metadata"`
		Spec map[string]json.RawMessage `json:"spec"`
	}
	raw, err := k3s.Kubectl(ctx, r, "get helmchart "+agent.ReleaseName()+" -n kube-system -o json")
	if err != nil {
		return nil, nil, fmt.Errorf("reading the existing agent values: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &chart); err != nil {
		return nil, nil, fmt.Errorf("decoding the existing agent values: %w", err)
	}
	if chart.APIVersion != "helm.cattle.io/v1" || chart.Kind != "HelmChart" ||
		chart.Metadata.Name != agent.ReleaseName() || chart.Metadata.Namespace != "kube-system" {
		return nil, nil, fmt.Errorf("the existing agent HelmChart is not the expected kube-system/%s helm.cattle.io/v1 object", agent.ReleaseName())
	}
	if chart.Metadata.ResourceVersion == "" {
		return nil, nil, fmt.Errorf("the existing agent HelmChart has no resourceVersion; refusing a blind ownership update")
	}
	valuesRaw, ok := chart.Spec["valuesContent"]
	if !ok {
		return nil, nil, fmt.Errorf("the existing agent HelmChart has no valuesContent; refusing to change workload ownership")
	}
	var valuesContent string
	if err := json.Unmarshal(valuesRaw, &valuesContent); err != nil {
		return nil, nil, fmt.Errorf("decoding the existing agent valuesContent: %w", err)
	}
	values := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesContent), &values); err != nil {
		return nil, nil, fmt.Errorf("decoding the existing agent valuesContent: %w", err)
	}
	kubenestValues, ok := values["kubenest"].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("the existing agent valuesContent has no kubenest identity; refusing to change workload ownership")
	}
	workloadRaw, hasWorkload := kubenestValues["workloadApplications"]
	if !hasWorkload {
		workloadRaw = map[string]any{}
		kubenestValues["workloadApplications"] = workloadRaw
	}
	workloadValues, ok := workloadRaw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("the existing agent valuesContent has kubenest.workloadApplications as %T, not a mapping; refusing to replace a user value", workloadRaw)
	}
	enabledRaw, hasEnabled := workloadValues["enabled"]
	if hasEnabled {
		enabled, ok := enabledRaw.(bool)
		if !ok {
			return nil, nil, fmt.Errorf("the existing agent valuesContent has kubenest.workloadApplications.enabled as %T, not a boolean; refusing to replace a user value", enabledRaw)
		}
		if enabled {
			return nil, nil, fmt.Errorf("the operator gate is recorded closed while the HelmChart explicitly enables workload Applications; resolve that inconsistent state before migrating")
		}
	}
	workloadValues["enabled"] = true
	updatedValues, err := yaml.Marshal(values)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding migrated agent values: %w", err)
	}
	updatedValuesRaw, err := json.Marshal(string(updatedValues))
	if err != nil {
		return nil, nil, err
	}
	chart.Spec["valuesContent"] = updatedValuesRaw
	live, err = json.Marshal(chart)
	if err != nil {
		return nil, nil, fmt.Errorf("rendering the live migrated agent HelmChart: %w", err)
	}
	chart.Metadata.ResourceVersion = ""
	source, err = json.Marshal(chart)
	if err != nil {
		return nil, nil, fmt.Errorf("rendering the persisted migrated agent HelmChart: %w", err)
	}
	return source, live, nil
}

// workloadApplicationsExplicitlyDisabled reports whether the agent HelmChart
// has an explicit false. It deliberately reads the source of truth rather than
// inferring it from the gate ConfigMap: an older CLI could have patched that
// ConfigMap open while the chart still rendered false, and treating the open
// record as a completed hand-off would create two Application owners.
func workloadApplicationsExplicitlyDisabled(ctx context.Context, r k3s.Runner) (bool, error) {
	var chart struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec map[string]json.RawMessage `json:"spec"`
	}
	raw, err := k3s.Kubectl(ctx, r, "get helmchart "+agent.ReleaseName()+" -n kube-system -o json")
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(raw), &chart); err != nil {
		return false, fmt.Errorf("decoding the existing agent HelmChart: %w", err)
	}
	if chart.APIVersion != "helm.cattle.io/v1" || chart.Kind != "HelmChart" ||
		chart.Metadata.Name != agent.ReleaseName() || chart.Metadata.Namespace != "kube-system" {
		return false, fmt.Errorf("the existing agent HelmChart is not the expected kube-system/%s helm.cattle.io/v1 object", agent.ReleaseName())
	}
	valuesRaw, ok := chart.Spec["valuesContent"]
	if !ok {
		return false, fmt.Errorf("the existing agent HelmChart has no valuesContent")
	}
	var valuesContent string
	if err := json.Unmarshal(valuesRaw, &valuesContent); err != nil {
		return false, fmt.Errorf("decoding the existing agent valuesContent: %w", err)
	}
	values := map[string]any{}
	if err := yaml.Unmarshal([]byte(valuesContent), &values); err != nil {
		return false, fmt.Errorf("decoding the existing agent valuesContent: %w", err)
	}
	kubenestValues, ok := values["kubenest"].(map[string]any)
	if !ok {
		return false, fmt.Errorf("the existing agent valuesContent has no kubenest identity")
	}
	workloadValues, ok := kubenestValues["workloadApplications"].(map[string]any)
	if !ok {
		return false, nil
	}
	enabledRaw, ok := workloadValues["enabled"]
	if !ok {
		return false, nil
	}
	enabled, ok := enabledRaw.(bool)
	if !ok {
		return false, fmt.Errorf("the existing agent valuesContent has kubenest.workloadApplications.enabled as %T, not a boolean", enabledRaw)
	}
	return !enabled, nil
}

func detachState(r *api.WorkloadApplicationsDetachResponse) string {
	if r == nil {
		return "<nil>"
	}
	return r.State
}

func detachMessage(r *api.WorkloadApplicationsDetachResponse) string {
	if r == nil || r.Message == "" {
		return "no backend message"
	}
	return r.Message
}

func workloadApplicationsGateState(ctx context.Context, r k3s.Runner) (string, error) {
	out, err := k3s.Kubectl(ctx, r,
		"get configmap kubenest-workload-applications-gate -n kubenest-system -o jsonpath='{.data.state}'")
	if err != nil {
		return "", err
	}
	return strings.Trim(strings.TrimSpace(out), "'"), nil
}

// workloadApplicationsGateProbe reads the operator's own recorded decision,
// not the Helm value the CLI intended to set. That receiver-side assertion is
// what catches an ignored/misnested value before the upgrade is recorded.
func workloadApplicationsGateProbe(r k3s.Runner) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		state, err := workloadApplicationsGateState(ctx, r)
		if err != nil {
			return false, converge.State{Object: "the workload-application gate", Status: "unobservable"}, err
		}
		if state != "open" {
			return false, converge.State{Object: "the workload-application gate", Status: "state=" + state}, nil
		}
		return true, converge.State{Object: "the workload-application gate", Status: "open"}, nil
	}
}

// agentUpgradedProbe waits for the chart resource to report the new version
// and the deployment to be Available again.
func agentUpgradedProbe(r k3s.Runner, version string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get helmchart "+agent.ReleaseName()+" -n kube-system -o jsonpath='{.spec.version}'")
		if err != nil {
			return false, converge.State{Object: "helmchart " + agent.ReleaseName(), Status: "unobservable"}, err
		}
		if got := strings.Trim(strings.TrimSpace(out), "'"); got != version {
			return false, converge.State{
				Object: "helmchart kubenest-agent",
				Status: "still at " + got,
			}, nil
		}
		out, err = k3s.Kubectl(ctx, r,
			"get deployment "+agent.DeploymentName+" -n kubenest-system -o jsonpath='{.status.conditions[?(@.type==\"Available\")].status}'")
		if err != nil {
			return false, converge.State{Object: "the agent deployment", Status: "unobservable"}, err
		}
		statuses := strings.Fields(strings.Trim(out, "'"))
		if len(statuses) == 0 {
			return false, converge.State{Object: "the agent deployment", Status: "not found yet"}, nil
		}
		for _, s := range statuses {
			if s != "True" {
				return false, converge.State{Object: "the agent deployment", Status: "Available=" + s}, nil
			}
		}
		return true, converge.State{Object: "the agent", Status: "Available at chart " + version}, nil
	}
}
