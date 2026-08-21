package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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

// upgradeAgentChart moves the KubeNest agent to a new chart version WITHOUT
// re-minting its identity.
//
// The agent's HelmChart resource already carries its cluster id and JWT in
// valuesContent. Rewriting the whole resource would need those credentials
// again, and an upgrade has no business re-minting a live cluster's identity
// — so only the version field is patched, in place, and the existing values
// are left exactly as they are.
func upgradeAgentChart(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, version string, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"spec":{"version":%q}}`, version)
	if _, err := k3s.Kubectl(ctx, r,
		fmt.Sprintf("patch helmchart kubenest-agent -n kube-system --type merge -p %s", shellQuote(patch))); err != nil {
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

// agentUpgradedProbe waits for the chart resource to report the new version
// and the deployment to be Available again.
func agentUpgradedProbe(r k3s.Runner, version string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, r, "get helmchart kubenest-agent -n kube-system -o jsonpath='{.spec.version}'")
		if err != nil {
			return false, converge.State{Object: "helmchart kubenest-agent", Status: "unobservable"}, err
		}
		if got := strings.Trim(strings.TrimSpace(out), "'"); got != version {
			return false, converge.State{
				Object: "helmchart kubenest-agent",
				Status: "still at " + got,
			}, nil
		}
		out, err = k3s.Kubectl(ctx, r,
			"get deployment -n kubenest-system -l app.kubernetes.io/name=kubenest-operator-2 -o jsonpath='{.items[*].status.conditions[?(@.type==\"Available\")].status}'")
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
