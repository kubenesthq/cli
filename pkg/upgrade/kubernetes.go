package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

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
	} else {
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
	}
	// THE CLUSTER-DNS GUARANTEE GOES LAST IN THIS STAGE, and it goes here
	// rather than in the installer alone because the k3s upgrade is the one
	// thing that re-applies every packaged manifest: each server rewrites them
	// at start-up and the deploy controller's first pass applies them all with
	// the checksum comparison off (pkg/k3s/coredns.go, EnsureCoreDNSReplicas,
	// for why that still cannot revert the two fields this sets — and for the
	// one k3s release that could). Whatever a k3s upgrade does to the CoreDNS
	// replica count, the upgrade that moved the Kubernetes version ends by
	// putting it back, so the cluster is never left with DNS on one node.
	//
	// It runs even when the Kubernetes version does not move: this stage is
	// where an upgrade asserts what it moved, and a run that skipped the pin
	// change would otherwise be the one upgrade that never checks.
	return s.ensureCoreDNS(ctx, server)
}

// ensureCoreDNS re-asserts the cluster-DNS layout the cluster's node count
// calls for after the Kubernetes stage has restarted every node
// (pkg/k3s.EnsureCoreDNSReplicas). A single-node cluster keeps k3s's one
// replica; a cluster of two or more nodes runs two, one per node, so a name
// lookup survives the loss of any one of them.
func (s *Session) ensureCoreDNS(ctx context.Context, server k3s.Runner) error {
	if len(s.Nodes) < 2 {
		s.Logf("  one node: k3s's single CoreDNS replica stays — a second one has no other node to be scheduled on")
		return nil
	}
	deadline, err := s.To.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	if err := k3s.EnsureCoreDNSReplicas(ctx, server, len(s.Nodes), deadline, s.Reporter); err != nil {
		return stages.NewComponentError("k3s", fmt.Errorf("cluster DNS across %d nodes: %w", len(s.Nodes), err))
	}
	s.Logf("  CoreDNS: %d replicas across %d nodes, one per node, so a lookup survives the loss of any one of them",
		k3s.CoreDNSReplicas(len(s.Nodes)), len(s.Nodes))
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

// upgradeAgentChart moves the KubeNest agent to the target bundle's chart
// without re-minting its identity. An ordinary bundle upgrade changes only the
// chart pin: valuesContent can contain credentials and customer choices, and is
// deliberately left exactly as it was.
//
// THE PATCH MOVES THE SOURCE AS WELL AS THE VERSION. A bundle pins its operator
// as <sources.kubenest-agent>:<core.kubenest-agent>, and the two can move
// independently: bundle 1.2's chart lives in the candidate channel while every
// released bundle's lives on the stable path. Patching only spec.version would
// leave spec.chart pointing at the old registry and ask it for a chart version
// that is not there — the upgrade would fail to pull, or worse, pull a same-named
// chart from a registry the target bundle never named. A manifest that declares
// no source leaves the chart alone, because then the bundle says nothing about
// where its chart lives.
func upgradeAgentChart(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, version string, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	patch, err := agentChartPatch(bundle, version)
	if err != nil {
		return err
	}
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

// agentChartPatch is the merge patch that moves the agent's HelmChart to a
// bundle's operator: the version always, and the chart SOURCE when the bundle
// declares one.
//
// ONE PIN, ONE PLACE. The version is the bundle's core.kubenest-agent pin, never
// a second copy of it here, and the source is the bundle's own
// sources.kubenest-agent — the same two values the control plane composes the
// install-time chart_ref from (kn-z6e4), so an install and an upgrade of the
// same bundle cannot disagree about where its chart lives.
func agentChartPatch(bundle *manifest.Manifest, version string) (string, error) {
	spec := map[string]string{"version": version}
	if source := bundle.Sources["kubenest-agent"]; source != "" {
		spec["chart"] = source
	}
	body, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return "", fmt.Errorf("building the agent chart patch: %w", err)
	}
	return string(body), nil
}

// agentUpgradedProbe waits for the operator Deployment to be running the new
// chart: the helm.sh/chart label it carries names the version helm actually
// applied, its pod template is observed, every replica is from it and
// available, and none of the previous chart's replicas are left.
//
// The HelmChart's spec.version is deliberately not consulted. It is the field
// this stage patched, so it reads back the target the instant the patch lands —
// long before k3s's helm controller has rendered anything, and it would still
// read the target if the controller never applied the chart at all. The
// evidence that the new chart reached the cluster is the label the chart
// stamps on its own objects (agent.ChartName): only helm rendering the new
// version changes it.
//
// "Available" alone is not enough either, which is the defect this probe was
// rewritten for. During a rolling update the previous ReplicaSet keeps the
// Deployment Available, and before helm applies at all the old ReplicaSet is
// the only one there is — so an upgrade whose only change is the agent
// (bundle 1.0 → 1.1) could be recorded complete while the old operator was
// still the one running. The replica counts say which ReplicaSet's pods are
// actually up; pkg/controlplane's rolledOut is the same shape for the same
// measured reason.
func agentUpgradedProbe(r k3s.Runner, version string) converge.Probe {
	object := "deployment " + agent.DeploymentName + " -n kubenest-system"
	want := agent.ChartName + "-" + version
	return func(ctx context.Context) (bool, converge.State, error) {
		var d struct {
			Metadata struct {
				Labels     map[string]string `json:"labels"`
				Generation int64             `json:"generation"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int32 `json:"replicas"`
			} `json:"spec"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				Replicas           int32 `json:"replicas"`
				UpdatedReplicas    int32 `json:"updatedReplicas"`
				AvailableReplicas  int32 `json:"availableReplicas"`
			} `json:"status"`
		}
		out, err := k3s.Kubectl(ctx, r,
			"get deployment "+agent.DeploymentName+" -n kubenest-system -o json")
		if err != nil {
			return false, converge.State{Object: object, Status: "not found yet"}, err
		}
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			return false, converge.State{Object: object, Status: "unparsable"}, err
		}
		replicas := int32(1)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		st := d.Status
		switch label := d.Metadata.Labels["helm.sh/chart"]; {
		case label == "":
			return false, converge.State{Object: object, Status: "no helm.sh/chart label yet"}, nil
		case label != want:
			return false, converge.State{
				Object: object,
				Status: "still on chart " + label,
				Detail: "the helm controller has not applied " + want + " yet",
			}, nil
		case d.Metadata.Generation > st.ObservedGeneration:
			return false, converge.State{Object: object, Status: "the new pod template is not observed yet"}, nil
		case st.Replicas > st.UpdatedReplicas:
			return false, converge.State{
				Object: object,
				Status: fmt.Sprintf("%d replica(s) of the previous chart still running", st.Replicas-st.UpdatedReplicas),
			}, nil
		case st.UpdatedReplicas < replicas || st.AvailableReplicas < replicas:
			return false, converge.State{
				Object: object,
				Status: fmt.Sprintf("%d/%d replicas updated, %d available", st.UpdatedReplicas, replicas, st.AvailableReplicas),
			}, nil
		}
		return true, converge.State{Object: object, Status: fmt.Sprintf("%d/%d on chart %s", st.AvailableReplicas, replicas, want)}, nil
	}
}
