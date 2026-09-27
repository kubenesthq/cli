//go:build e2e

// The gate for kn-t52 and kn-t53 (`kubenest node add`, `kubenest node remove`),
// on REAL Ubuntu hosts. k3d cannot run the installer, LVM or a reboot, so this
// is the only place these verbs are accepted (AGENTS.md: "a day-2 verb is
// accepted only by a scenario on real Ubuntu hosts").
//
// WHAT A HARDWARE RUN NEEDS, exactly:
//
//	source lab/hetzner/.lab-env.sh
//	./scripts/ephemeral-env.sh up --profile host --nodes 4     # S7's fixture
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=...
//	export KUBENEST_LAB_SPARE_IP=<the 4th host>                # the machine added and removed
//	export KUBENEST_LAB_SPARE_STORAGE_DEVICE=/dev/disk/by-id/scsi-...  # a BLANK device on it
//	cd kubenest-cli && go test -tags e2e -run TestNodeLifecycleGate -v -timeout 90m ./e2e/
//
// The cluster must already be installed and REGISTERED (its inventory written),
// with its maintenance window free for the gate to open and restore. `remove`'s
// planted negative needs an agent that still holds two bound local PVCs — S5's
// fixture; without one, that subtest SKIPS rather than passing vacuously.
//
// This file is T5.2/T5.3's half of S7 (its add and remove steps). The upgrade's
// use of the added node (T5.5) and the patch-night gate (T3.7) live in their own
// files.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/k3s"
)

// nodeVerb runs the REAL command tree, so the flags, the refusals and the
// output are the ones an operator gets.
func nodeVerb(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := cmd.NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// spareHost is the machine this gate adds and then removes: S7's 4th host.
func spareHost(t *testing.T) (string, string) {
	t.Helper()
	spare := os.Getenv("KUBENEST_LAB_SPARE_IP")
	if spare == "" {
		spare = os.Getenv("KUBENEST_LAB_NODE4_IP")
	}
	if spare == "" {
		t.Skip("KUBENEST_LAB_SPARE_IP is not set: this gate needs a 4th host to add and remove (./scripts/ephemeral-env.sh up --profile host --nodes 4)")
	}
	return spare, os.Getenv("KUBENEST_LAB_SPARE_STORAGE_DEVICE")
}

// nodeView is one Node object, as much of it as this gate reads.
type nodeView struct {
	Name          string   `json:"name"`
	UID           string   `json:"uid"`
	Addresses     []string `json:"addresses"`
	Ready         bool     `json:"ready"`
	Unschedulable bool     `json:"unschedulable"`
	Hold          string   `json:"hold"`
}

// clusterNodes reads the cluster's Node objects from a server.
func clusterNodes(t *testing.T, ctx context.Context, server k3s.Runner) []nodeView {
	t.Helper()
	out, err := k3s.Kubectl(ctx, server, "get nodes -o json")
	if err != nil {
		t.Fatalf("reading the cluster's nodes: %v", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				UID    string            `json:"uid"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Addresses []struct {
					Address string `json:"address"`
				} `json:"addresses"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("parsing `kubectl get nodes -o json`: %v", err)
	}
	nodes := make([]nodeView, 0, len(list.Items))
	for _, item := range list.Items {
		n := nodeView{Name: item.Metadata.Name, UID: item.Metadata.UID, Unschedulable: item.Spec.Unschedulable,
			Hold: item.Metadata.Labels["kubenest.io/auto-reboot"]}
		for _, a := range item.Status.Addresses {
			n.Addresses = append(n.Addresses, a.Address)
		}
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// nodeByAddress finds the Node object that answers on an address.
func nodeByAddress(t *testing.T, ctx context.Context, server k3s.Runner, address string) (nodeView, bool) {
	t.Helper()
	for _, n := range clusterNodes(t, ctx, server) {
		for _, a := range n.Addresses {
			if a == address {
				return n, true
			}
		}
	}
	return nodeView{}, false
}

func nodeHoldLabel(t *testing.T, ctx context.Context, server k3s.Runner, address string) string {
	t.Helper()
	n, ok := nodeByAddress(t, ctx, server, address)
	if !ok {
		return ""
	}
	return n.Hold
}

func spareIsSchedulable(t *testing.T, ctx context.Context, server k3s.Runner, address string) bool {
	t.Helper()
	n, ok := nodeByAddress(t, ctx, server, address)
	return ok && !n.Unschedulable && n.Ready
}

func nodeExists(t *testing.T, ctx context.Context, server k3s.Runner, address string) bool {
	t.Helper()
	_, ok := nodeByAddress(t, ctx, server, address)
	return ok
}

// agentHoldingLocalVolumes finds an agent of this cluster that holds at least
// one BOUND local volume, and returns its SSH address and its claims.
//
// It is S5's fixture, discovered rather than assumed: a cluster whose agents
// hold no local volume cannot exercise the refusal, and the subtest says so
// instead of passing on nothing.
func agentHoldingLocalVolumes(t *testing.T, ctx context.Context, server k3s.Runner, hosts []api.HostRecord) (string, []string) {
	t.Helper()
	out, err := k3s.Kubectl(ctx, server, "get pv -o json")
	if err != nil {
		t.Fatalf("reading the cluster's persistent volumes: %v", err)
	}
	var list struct {
		Items []struct {
			Spec struct {
				ClaimRef *struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				} `json:"claimRef"`
				NodeAffinity *struct {
					Required *struct {
						NodeSelectorTerms []struct {
							MatchExpressions []struct {
								Key      string   `json:"key"`
								Operator string   `json:"operator"`
								Values   []string `json:"values"`
							} `json:"matchExpressions"`
						} `json:"nodeSelectorTerms"`
					} `json:"required"`
				} `json:"nodeAffinity"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("parsing `kubectl get pv -o json`: %v", err)
	}
	for _, item := range list.Items {
		if item.Status.Phase != "Bound" || item.Spec.ClaimRef == nil || item.Spec.NodeAffinity == nil || item.Spec.NodeAffinity.Required == nil {
			continue
		}
		var nodeNames []string
		for _, term := range item.Spec.NodeAffinity.Required.NodeSelectorTerms {
			for _, expr := range term.MatchExpressions {
				if expr.Key == "kubernetes.io/hostname" {
					nodeNames = append(nodeNames, expr.Values...)
				}
			}
		}
		if len(nodeNames) == 0 {
			continue
		}
		for _, h := range hosts {
			if h.Role != "agent" || h.LifecycleState != "active" {
				continue
			}
			for _, name := range nodeNames {
				for _, n := range clusterNodes(t, ctx, server) {
					if n.Name != name {
						continue
					}
					if h.NodeUID != "" && h.NodeUID != n.UID {
						continue
					}
					return h.SSHAddress, []string{item.Spec.ClaimRef.Name}
				}
			}
		}
	}
	return "", nil
}

func TestNodeLifecycleGate(t *testing.T) {
	env := gateEnvironment(t)
	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()
	clusterID := clusterIDFor(t, ctx, client, env.cluster)
	bundle := fetchBundle(t, client, env.bundle)
	nodeReady, err := bundle.Limits.Timeouts.For("node-ready")
	if err != nil {
		t.Fatal(err)
	}
	spare, spareDevice := spareHost(t)
	server := connectNodes(t, env)[0].Runner

	record, err := client.BundleRecord(ctx, clusterID)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Hosts) == 0 {
		t.Fatal("this cluster's record carries no host inventory: run the install's record stage before this gate")
	}

	// A window that is open at any instant the gate runs, restored afterwards
	// so a later gate starts where an operator would.
	storeWindow(t, ctx, client, clusterID, api.MaintenanceWindow{
		Days: weekdays(), Start: "00:00", End: "23:59", Timezone: "UTC"})
	t.Cleanup(func() {
		storeWindow(t, context.Background(), client, clusterID, api.MaintenanceWindow{
			Days: []string{"sun"}, Start: "06:00", End: "09:00", Timezone: "UTC"})
	})
	t.Logf("cluster %s: adding then removing spare host %s (node-ready %s)", env.cluster, spare, nodeReady)

	t.Run("add-capacity", func(t *testing.T) {
		// FROM A SECOND LAPTOP: an empty HOME has no install journal and no
		// local kit, which is the case the inventory (T5.0) exists for.
		t.Setenv("HOME", t.TempDir())

		args := []string{"node", "add", "--cluster", env.cluster, "--agent", spare}
		if spareDevice != "" {
			args = append(args, "--storage-device", spareDevice)
		}
		args = append(args, "--now")
		out, err := nodeVerb(t, args...)
		if err != nil {
			t.Fatalf("node add failed from a second laptop:\n%s\n%v", out, err)
		}
		if !strings.Contains(out, "inventory") {
			t.Errorf("the run does not report the inventory write:\n%s", out)
		}

		// The node is in the INVENTORY and in the cluster, and marked active.
		record, err := client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		var found *api.HostRecord
		for i := range record.Hosts {
			if record.Hosts[i].SSHAddress == spare {
				found = &record.Hosts[i]
			}
		}
		if found == nil {
			t.Fatalf("the spare host is not in the cluster's inventory: %+v", record.Hosts)
		}
		if found.LifecycleState != "active" {
			t.Errorf("the host is recorded as %q, want active", found.LifecycleState)
		}
		if found.NodeUID == "" {
			t.Error("the host's entry carries no Node UID, so a later verb cannot tell which Node object is its")
		}
		if hold := nodeHoldLabel(t, ctx, server, spare); hold == "false" {
			t.Error("the new node still carries kubenest.io/auto-reboot=false, so the cluster's patching will never reboot it")
		}
		// The capacity it was added for is really there: the node is Ready and
		// schedulable.
		nodes := clusterNodes(t, ctx, server)
		var target nodeView
		for _, n := range nodes {
			for _, a := range n.Addresses {
				if a == spare {
					target = n
				}
			}
		}
		if target.Name == "" {
			t.Fatalf("the spare host is not a node of this cluster: %+v", nodes)
		}
		if !target.Ready || target.Unschedulable {
			t.Errorf("the new node is Ready=%t unschedulable=%t: the capacity it was added for is not usable", target.Ready, target.Unschedulable)
		}
	})

	t.Run("remove-of-a-server-is-refused", func(t *testing.T) {
		out, err := nodeVerb(t, "node", "remove", "--cluster", env.cluster, "--node", env.server, "--now")
		if err == nil {
			t.Fatalf("removing a server was accepted:\n%s", out)
		}
		for _, want := range []string{"S6", "promotion"} {
			if !strings.Contains(err.Error(), want) && !strings.Contains(out, want) {
				t.Errorf("the refusal does not name %q:\n%s\n%v", want, out, err)
			}
		}
	})

	t.Run("remove-refuses-a-node-holding-local-volumes", func(t *testing.T) {
		holder, pvcs := agentHoldingLocalVolumes(t, ctx, server, record.Hosts)
		if holder == "" {
			t.Skip("no agent of this cluster holds a bound local volume: S5's fixture is not present, so there is nothing for this planted negative to refuse")
		}
		out, err := nodeVerb(t, "node", "remove", "--cluster", env.cluster, "--node", holder, "--now")
		if err == nil {
			t.Fatalf("a node holding bound local volumes was removed without --abandon-volumes:\n%s", out)
		}
		if !strings.Contains(out+err.Error(), "kubenest backup restore") {
			t.Errorf("the refusal carries no usable restore path:\n%s\n%v", out, err)
		}
		for _, pvc := range pvcs {
			if !strings.Contains(out+err.Error(), pvc) {
				t.Errorf("the refusal does not name the stranded claim %s:\n%s", pvc, out)
			}
		}
	})

	t.Run("remove-spare", func(t *testing.T) {
		out, err := nodeVerb(t, "node", "remove", "--cluster", env.cluster, "--node", spare, "--now")
		if err != nil {
			t.Fatalf("node remove failed:\n%s\n%v", out, err)
		}
		record, err := client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		var found *api.HostRecord
		for i := range record.Hosts {
			if record.Hosts[i].SSHAddress == spare {
				found = &record.Hosts[i]
			}
		}
		if found == nil {
			t.Fatal("the removed host's entry was deleted; it is kept as the record that the machine was here")
		}
		if found.LifecycleState != "removed" {
			t.Errorf("the host is recorded as %q, want removed", found.LifecycleState)
		}
		if found.HostID == "" {
			t.Error("the removed host lost its host ID")
		}
		if nodeExists(t, ctx, server, spare) {
			t.Error("the Node object still exists after the removal")
		}
	})
}
