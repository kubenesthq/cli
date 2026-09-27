//go:build e2e

// The gate for kn-t52 and kn-t53 (`kubenest node add`, `kubenest node remove`),
// on REAL Ubuntu hosts. k3d cannot run the installer, LVM or a reboot, so this
// is the only place these verbs are accepted (AGENTS.md: "a day-2 verb is
// accepted only by a scenario on real Ubuntu hosts").
//
// WHAT A HARDWARE RUN NEEDS, exactly:
//
//	source lab/hetzner/.lab-env.sh
//	./scripts/ephemeral-env.sh up --profile host --nodes 4     # S5's and S7's fixture
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=...
//	export KUBENEST_LAB_SPARE_IP=<the 4th host>                # the machine added, removed, or used as a replacement
//	export KUBENEST_LAB_SPARE_STORAGE_DEVICE=/dev/disk/by-id/scsi-...  # a BLANK device on it
//	cd kubenest-cli && go test -tags e2e -run TestNodeLifecycleGate -v -timeout 90m ./e2e/
//
// The cluster must already be installed and REGISTERED (its inventory written),
// with its maintenance window free for the gate to open and restore. `remove`'s
// planted negative needs an agent that still holds two bound local PVCs — S5's
// fixture; without one, that subtest SKIPS rather than passing vacuously.
//
// S5 AND S7 SHARE THE LAB BUT NOT THE SPARE. The add/remove half above spends the
// 4th host as S7's added capacity and then removes it; S5's `replace-dead-node`
// needs that host as a REPLACEMENT, and a replacement is a node of the cluster
// afterwards. Run the replace half on its own, on a fresh four-node fixture:
//
//	go test -tags e2e -run 'TestNodeLifecycleGate/replace-dead-node' -v -timeout 90m ./e2e/
//
// That subtest skips, naming this command, when the spare is already a host of
// the cluster. It stops the dying agent's k3s rather than powering the machine
// off through the provider API — the Node goes NotReady either way, so the verb
// takes its remove-first branch — and it then runs the restore command the
// replace printed, followed by activation.
//
// This file is T5.2/T5.3's half of S7 (its add and remove steps) and T5.4's half
// of S5 (the dead node's replacement). The upgrade's use of the added node
// (T5.5) and the patch-night gate (T3.7) live in their own files.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
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

// withSSH appends the gate's SSH user and key to a node verb, the way an
// operator names them on the command line. The gate's HOME has no SSH config,
// so without them the CLI dials as the local user through ssh-agent (hardware,
// 2026-09-27: "Too many authentication failures").
func withSSH(env gateEnv, args ...string) []string {
	if env.sshUser != "" {
		args = append(args, "--ssh-user", env.sshUser)
	}
	if env.sshKey != "" {
		args = append(args, "--ssh-key", env.sshKey)
	}
	return args
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
		gateLogin(t, env)

		args := []string{"node", "add", "--cluster", env.cluster, "--agent", spare}
		if spareDevice != "" {
			args = append(args, "--storage-device", spareDevice)
		}
		args = append(args, "--now")
		out, err := nodeVerb(t, withSSH(env, args...)...)
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
		out, err := nodeVerb(t, withSSH(env, "node", "remove", "--cluster", env.cluster, "--node", env.server, "--now")...)
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
		out, err := nodeVerb(t, withSSH(env, "node", "remove", "--cluster", env.cluster, "--node", holder, "--now")...)
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
		out, err := nodeVerb(t, withSSH(env, "node", "remove", "--cluster", env.cluster, "--node", spare, "--now")...)
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

	t.Run("replace-dead-node", func(t *testing.T) {
		// S5's OWN PASS, not a continuation of the add/remove half above: a
		// spare spent here is a node afterwards, and one spare cannot be both
		// S5's replacement and S7's added capacity. This subtest therefore
		// SKIPS when the spare is already a host of this cluster, naming the
		// command that runs it on a fresh fixture:
		//
		//	go test -tags e2e -run 'TestNodeLifecycleGate/replace-dead-node' -v -timeout 90m ./e2e/
		//
		// The agent that dies is stopped through SSH (its k3s-agent unit goes
		// down), which is what an unattended gate can do: a machine powered off
		// at the provider stays off until somebody powers it back on. From every
		// client that matters the two are the same — the Node goes NotReady and
		// its volumes are unreachable — and `node replace` takes its remove-first
		// branch either way. A run that powers the machine off at the provider
		// additionally exercises the unreachable-host path on the host itself;
		// the unit tests cover that path with a host that does not answer.
		spare, spareDevice := spareHost(t)
		lab := s5Open(t, env)
		inventory, err := client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range inventory.Hosts {
			if h.SSHAddress == spare {
				t.Skipf("the machine that replaces is already a host of this cluster (recorded %s), so S5's spare is spent: run this subtest alone on a fresh four-node fixture with -run 'TestNodeLifecycleGate/replace-dead-node'", h.LifecycleState)
			}
		}
		addresses := lab.nodeNames()
		agents := lab.agents(t)
		if len(agents) < 2 {
			t.Skipf("S5 needs two agents — the one that dies and the one that must keep serving — and this cluster reports %d", len(agents))
		}
		dead, keep := agents[0], agents[1]
		deadAddress := addresses[dead]
		if deadAddress == "" {
			t.Fatalf("the cluster reports no address for agent %s, so the gate cannot say which machine is about to die: %v", dead, addresses)
		}

		lab.apply(s5NamespaceDocument())
		lab.kubectl("delete configmap kubenest-operation -n kube-system --ignore-not-found")
		workload, keeper := "replace-dead", "replace-keep"
		tag := fmt.Sprintf("%d", time.Now().UnixNano())
		// Both claims of the workload that will be stranded are on the agent
		// that dies, by pinning the workload there; the workload that must keep
		// its data is pinned to the other agent.
		lab.apply(s5WorkloadDocument(workload, map[string]string{"kubernetes.io/hostname": dead}, true))
		lab.apply(s5WorkloadDocument(keeper, map[string]string{"kubernetes.io/hostname": keep}, true))
		lab.waitFor(5*time.Minute, "both fixture workloads running", func() (bool, string) {
			return lab.podRunning(workload) && lab.podRunning(keeper), lab.podState(workload) + " / " + lab.podState(keeper)
		})
		lab.writeClaim(workload, "/a", "replace-a-"+tag)
		lab.writeClaim(workload, "/b", "replace-b-"+tag)
		lab.backupNow()
		// The workload that must survive gets NEWER data than the backup, so
		// what the gate checks afterwards is data this cluster never copied.
		lab.writeClaim(keeper, "/a", "keep-newer-a-"+tag)
		lab.writeClaim(keeper, "/b", "keep-newer-b-"+tag)

		// The node loss. The workload is stopped first so that the drain has
		// nothing to evict, and its CLAIMS STAY: they are the stranded volumes
		// the printed restore command has to cover.
		lab.kubectl("-n " + s5Namespace + " scale deployment " + workload + " --replicas=0")
		lab.stopAgent(dead)
		lab.waitFor(5*time.Minute, "the agent to go NotReady", func() (bool, string) {
			out, err := k3s.Kubectl(context.Background(), lab.nodes["node1"], "get node "+dead+" -o jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
			if err != nil {
				return false, err.Error()
			}
			return strings.TrimSpace(out) != "True", strings.TrimSpace(out)
		})
		lab.kubectl("-n " + s5Namespace + " delete pod -l app=" + workload + " --grace-period=0 --force --ignore-not-found")

		// The isolation confirmation is the operator's step: without it the
		// command refuses and names the machine, and nothing is removed.
		out, err := nodeVerb(t, withSSH(env, "node", "replace", "--cluster", env.cluster, "--node", dead, "--with", spare, "--now")...)
		if err == nil {
			t.Fatalf("a replace of a machine that does not answer was accepted without --confirm-isolated:\n%s", out)
		}
		if !strings.Contains(out+err.Error(), deadAddress) {
			t.Errorf("the refusal does not name the machine by its address (%s):\n%s\n%v", deadAddress, out, err)
		}
		if !strings.Contains(out+err.Error(), "--confirm-isolated") {
			t.Errorf("the refusal does not say how to confirm:\n%s\n%v", out, err)
		}
		if !nodeExists(t, ctx, server, deadAddress) {
			t.Fatal("the refusal removed the Node object it refused to remove")
		}

		args := []string{"node", "replace", "--cluster", env.cluster, "--node", dead, "--with", spare, "--confirm-isolated", "--now"}
		if spareDevice != "" {
			args = append(args, "--storage-device", spareDevice)
		}
		out, err = nodeVerb(t, withSSH(env, args...)...)
		if err != nil {
			t.Fatalf("node replace failed:\n%s\n%v", out, err)
		}

		// ONE restore command per affected workload, with every stranded claim
		// of that workload in it: restoring one claim of a pod while another
		// stays stranded leaves the pod unable to start.
		commands := restoreCommandLines(out)
		if len(commands) != 1 {
			t.Fatalf("%d restore command(s) printed for the one workload whose claims were stranded:\n%s", len(commands), out)
		}
		for _, claim := range []string{workload + "-a", workload + "-b"} {
			if !strings.Contains(commands[0], "--pvc "+claim) {
				t.Errorf("the printed restore command does not carry the stranded claim %s: %s", claim, commands[0])
			}
		}

		// The replacement joined at the bundle's pinned k3s version, with its
		// volume group: a replacement that joined at another version or without
		// storage would be capacity the cluster cannot use.
		replacement := lab.nodeNames()[spare]
		if replacement == "" {
			t.Fatalf("the replacement host %s is not a node of this cluster: %v", spare, lab.nodeNames())
		}
		pin, err := bundle.Core.Version("k3s")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(lab.kubectl("get node " + replacement + " -o jsonpath={.status.nodeInfo.kubeletVersion}")); got != pin {
			t.Errorf("the replacement runs k3s %s and the bundle pins %s", got, pin)
		}
		spareRunner := dialLabHost(t, env, spare)
		defer spareRunner.Close()
		if res, err := spareRunner.Run(context.Background(), "sudo -n vgs --noheadings -o vg_name kubenest-vg"); err != nil || res.ExitCode != 0 || !strings.Contains(res.Stdout, "kubenest-vg") {
			t.Errorf("the replacement has no kubenest-vg volume group (err %v, exit %d, out %q): a node without its volume group is not capacity for a local volume", err, res.ExitCode, res.Stdout)
		}

		// The machine that was replaced is out of the cluster and recorded
		// removed, with its identity kept — the bookkeeping, not fencing.
		if nodeExists(t, ctx, server, deadAddress) {
			t.Error("the Node object of the machine that was replaced still exists")
		}
		inventory, err = client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		var replaced *api.HostRecord
		for i := range inventory.Hosts {
			if inventory.Hosts[i].SSHAddress == deadAddress {
				replaced = &inventory.Hosts[i]
			}
		}
		if replaced == nil {
			t.Fatalf("the entry of the machine that was replaced was deleted: %+v", inventory.Hosts)
		}
		if replaced.LifecycleState != "removed" {
			t.Errorf("the replaced host is recorded as %q, want removed", replaced.LifecycleState)
		}
		if replaced.HostKeyFingerprint == "" {
			t.Error("the replaced host lost its host key, so the CLI would re-add the machine without a wipe")
		}

		// The workload on the agent that kept serving keeps the data written
		// AFTER the backup: nothing about this replace touched it, which is the
		// point of joining the replacement before removing the machine.
		if got := lab.readClaims(keeper); !strings.Contains(got, "keep-newer-a-"+tag) || !strings.Contains(got, "keep-newer-b-"+tag) {
			t.Errorf("the workload on the surviving agent reads %q, want the data written after the backup (keep-newer-a-%s, keep-newer-b-%s)", got, tag, tag)
		}

		// The command the replace printed is the one that brings the data back,
		// run now that the replacement is live, and activation lets the
		// namespace run again.
		restoreArgs := append(strings.Fields(strings.TrimSpace(strings.SplitN(commands[0], "   #", 2)[0]))[1:], lab.verbArgs()...)
		restoreArgs = append(restoreArgs, "--confirm")
		var restoreOut strings.Builder
		if err := lab.runCLI(&restoreOut, restoreArgs...); err != nil {
			t.Fatalf("the restore command the replace printed failed: %v\n%s", err, restoreOut.String())
		}
		operationID := s4OperationID(restoreOut.String())
		if operationID == "" {
			t.Fatalf("the printed restore command named no operation to activate:\n%s", restoreOut.String())
		}
		var activateOut strings.Builder
		if err := lab.runCLI(&activateOut, append([]string{"backup", "restore", "--activate", operationID}, lab.verbArgs()...)...); err != nil {
			t.Fatalf("activating the restored namespace failed: %v\n%s", err, activateOut.String())
		}
		lab.waitFor(10*time.Minute, "both stranded claims refilled and Bound on a live node", func() (bool, string) {
			out := lab.kubectl("-n " + s5Namespace + " get pvc -o jsonpath={range .items[*]}{.metadata.name}={.status.phase}{\" \"}{end}")
			for _, claim := range []string{workload + "-a", workload + "-b"} {
				if !strings.Contains(out, claim+"=Bound") {
					return false, out
				}
			}
			return true, out
		})
	})
}

// agents is the cluster's agent node names, sorted, so a gate can say which one
// dies and which one has to keep serving.
func (l *s5Lab) agents(t *testing.T) []string {
	t.Helper()
	out := l.kubectl("get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	names := strings.Fields(out)
	sort.Strings(names)
	return names
}

// restoreCommandLines is every restore command a node verb printed, taken from
// the output an operator reads rather than rebuilt from the flags: what the
// verb told them to run is what this gate runs.
func restoreCommandLines(out string) []string {
	var commands []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "kubenest backup restore") {
			commands = append(commands, line)
		}
	}
	return commands
}

// dialLabHost opens one SSH connection to a lab host the s5Lab does not hold
// (the machine that replaces a node, which is not one of S5's three).
func dialLabHost(t *testing.T, env gateEnv, address string) *sshx.Client {
	t.Helper()
	ep, err := sshx.Resolve(address, sshx.Options{User: env.sshUser, KeyPath: env.sshKey})
	if err != nil {
		t.Fatalf("resolving %s: %v", address, err)
	}
	runner, err := sshx.Dial(context.Background(), ep, sshx.Options{KeyPath: env.sshKey})
	if err != nil {
		t.Fatalf("connecting to %s: %v", address, err)
	}
	return runner
}
