package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/hostpolicy"
	"kubenest.io/cli/pkg/interlock"
	"kubenest.io/cli/pkg/k3s"
)

// Stage 6: the host policy. THE TWO HOST CHANGES AN EXISTING CLUSTER NEEDS.
//
// Section 7.10 is exact about what an existing 1.1 workload cluster gets at the
// host level when it is upgraded rather than reinstalled: the APT drop-in and
// the reboot labels, and nothing else. Secrets encryption, kits and the
// inventory are for clusters INSTALLED at 1.2 (decision F19), so this stage
// deliberately backfills neither.
//
// The drop-in is pkg/hostpolicy's — the same bytes the installer writes, from
// the same package — and this stage's only job is to converge it and report
// what it saw. Without it a 1.1 cluster keeps Ubuntu's own policy: security
// updates installed "at a random time each day", and a reboot decision nobody
// chose. The reboot policy 1.2 sells would then be a property of the IMAGE
// rather than of the product, and `kubenest node reboot` against automatic
// reboot would be decided by luck (plan 6.3's "APT policy on hosts").
//
// The hold is the node label kubenest.io/auto-reboot=false, kured's own
// interlock (T3.3). Three rules, and the third is the one a naive
// implementation gets wrong:
//
//	servers keep it. In 1.2 a server never reboots by itself — its reboots go
//	through `kubenest node reboot` — so a server that predates the label gets
//	it and keeps it.
//
//	a node that already carries it keeps it: a node `node add` has not
//	recorded yet, or one held because a new window is still applying, must not
//	be released by an upgrade that happened to pass by.
//
//	every other node is held for the DURATION OF THIS OPERATION and then put
//	back exactly as it was. The upgrade is disruptive, so the nodes are held
//	while it runs; but the label is restored to its PRE-OPERATION value — the
//	recorded one, not "removed" — when this stage completes, because an agent
//	whose label is stripped loses the automatic reboots it is entitled to, and
//	one that is left held silently never reboots again.
//
// IDEMPOTENT AND RESUMABLE, and those are requirements rather than niceties.
// Re-running on an already-policy'd cluster converges with no write at all, and
// a resume after a killed laptop re-reads rather than re-does: the
// pre-operation values are written to the journal's state BEFORE the first
// label is touched, so a second process restores what was there before the
// operation instead of mistaking the operation's own hold for the original
// value. Journal.Completed is what makes a fully completed stage skip; this
// stage's own reads are what make an interrupted one converge.
//
// ROLLBACK DOES NOT UNDO IT, deliberately (see rollback.go). Reverting the
// drop-in would restore Ubuntu's random install time, which is the behaviour
// this stage exists to replace.

// hostPolicyNode is one node's entry in the journal's state for this stage: the
// reboot-hold value that was there BEFORE the operation, and what the stage
// wrote. The restore at the end of the stage reads HoldBefore/HoldValue, never
// the node's current label — after the hold has been applied the current value
// IS the operation's, and restoring that would leave every agent held.
type hostPolicyNode struct {
	// Node is the Kubernetes node name the label lives on.
	Node string `json:"node"`
	// Address is the host this CLI reaches it at. It is the key a resume
	// matches on: the node list is part of the upgrade's identity, so the
	// address is stable across a resume while the Kubernetes name is read
	// fresh.
	Address string `json:"address"`
	// Server marks a node whose hold is permanent in 1.2.
	Server bool `json:"server"`
	// HoldBefore is whether the node carried the hold label before this
	// operation and HoldValue what it carried then. They are the restore
	// target.
	HoldBefore bool   `json:"hold_before"`
	HoldValue  string `json:"hold_value,omitempty"`
	// HoldApplied records that THIS operation wrote the hold. A node that
	// already carried it is not "applied" and is not restored to anything
	// else — its value is already its pre-operation value.
	HoldApplied bool `json:"hold_applied"`
	// PolicyFiles names the drop-in files this stage WROTE on the host, from
	// the writer's own report. Empty means the host already held exactly what
	// the platform writes, which is the observable postcondition of a re-run.
	PolicyFiles []string `json:"policy_files,omitempty"`
	// PolicyBefore is what the host's EFFECTIVE APT configuration was before
	// this stage, in the writer's own words, when it was not the platform's
	// policy. It is recorded rather than only printed: an existing policy that
	// contradicted this one is being replaced, and the record must be able to
	// say so afterwards.
	PolicyBefore string `json:"policy_before,omitempty"`
}

// stageHostPolicy converges the host policy of every node this upgrade acts on.
func stageHostPolicy(ctx context.Context, s *Session) error {
	server, err := s.Server()
	if err != nil {
		return err
	}

	// ONE read of the cluster's nodes: it answers three questions the stage
	// needs together — which Node object each host is, what the hold label
	// currently says, and which node a resume's recorded names refer to.
	nodes, err := readHostNodes(ctx, server)
	if err != nil {
		return fmt.Errorf("reading this cluster's nodes: %w", err)
	}
	names, err := nodes.matchNodes(s)
	if err != nil {
		return err
	}

	// THE PRE-OPERATION VALUES, FIRST AND PERSISTED. A record left by an
	// earlier, interrupted run wins over what the cluster says now: by the
	// time a resume runs, the current label may be the hold that run applied,
	// and reading it as "the value before the operation" is exactly how every
	// agent ends up held for ever.
	recorded := map[string]hostPolicyNode{}
	for _, r := range s.Record.HostPolicy {
		recorded[r.Address] = r
	}
	states := make([]hostPolicyNode, 0, len(s.Nodes))
	index := map[string]int{}
	for _, node := range s.Nodes {
		name := names[node.Address]
		state, ok := recorded[node.Address]
		if !ok {
			state = hostPolicyNode{Address: node.Address}
			if value, held := nodes.labels(name)[day2.NoAutoRebootLabel]; held {
				state.HoldBefore, state.HoldValue = true, value
			}
		}
		state.Node = name
		state.Server = node.Server
		index[node.Address] = len(states)
		states = append(states, state)
	}
	s.Record.HostPolicy = states
	if err := s.saveRecord(); err != nil {
		return fmt.Errorf("recording the reboot value each node was held with: %w", err)
	}

	// THE APT DROP-IN, on every node over that node's own connection, exactly
	// as the installer writes it: security origin only, Automatic-Reboot
	// "false", and k3s out of needrestart's reach. The postcondition is read
	// back from the host's EFFECTIVE configuration — the file we wrote is not
	// evidence that the policy survives, because a file that sorts after ours
	// has the last word.
	for i := range s.Nodes {
		node := s.Nodes[i]
		state := &states[index[node.Address]]

		before, err := hostpolicy.ReadEffective(ctx, node.Runner)
		if err != nil {
			return fmt.Errorf("reading the effective APT policy on %s: %w", node.Address, err)
		}
		// Recorded once, on the first run that sees it: this is the policy the
		// platform REPLACED, and a resume that reads the host after the write
		// cannot know what was there before it.
		if problem := before.NonCompliance(); problem != "" && state.PolicyBefore == "" {
			// Reported, never silent: this host is running a policy nobody
			// here chose, and the platform's drop-in is about to replace it.
			// It is not a refusal — refusing would leave the cluster on
			// Ubuntu's own policy, which is the whole thing this stage is
			// here to remove — but the operator is told what was there.
			state.PolicyBefore = problem
			s.Logf("  %s: the platform's APT policy replaces a host policy that %s", node.Address, problem)
		}

		report, err := hostpolicy.Converge(ctx, node.Runner)
		if err != nil {
			return fmt.Errorf("host policy on %s: %w", node.Address, err)
		}
		state.PolicyFiles = report.Written

		after, err := hostpolicy.ReadEffective(ctx, node.Runner)
		if err != nil {
			return fmt.Errorf("reading the effective APT policy on %s: %w", node.Address, err)
		}
		if problem := after.NonCompliance(); problem != "" {
			return fmt.Errorf("the APT policy on %s is not the one this platform writes: %s", node.Address, problem)
		}
		s.Logf("  %s: %s", node.Address, describeAPT(after, report.Written))
	}

	// THE HOLD, one node at a time, through T3.3's own helper: it labels the
	// node and releases a kured lock the label has just orphaned (P1's
	// finding — a held node that keeps kured's lock stops every other node's
	// reboot for ever). A node that already carries the hold is left strictly
	// alone: writing the label again would be a write, and a re-run that
	// changes nothing is the property this stage is accepted on.
	for i := range states {
		state := &states[i]
		if nodes.labels(state.Node)[day2.NoAutoRebootLabel] == day2.NoAutoRebootValue {
			continue
		}
		if err := day2.HoldAutomaticReboots(ctx, server, state.Node); err != nil {
			return fmt.Errorf("holding automatic reboots on %s: %w", state.Node, err)
		}
		state.HoldApplied = true
	}

	// AND PUT THE NODES BACK. Servers keep the hold: in 1.2 a server never
	// reboots by itself. Every other node goes back to what it was: the
	// recorded value where there was one, and no label at all where there was
	// not — never "removed" as a blanket rule, because a node that carried the
	// hold before this operation is entitled to keep it.
	//
	// The labels are read back first, and that read is the difference between a
	// re-check and a re-do: a node already at its pre-operation value — which is
	// every node on a second run, and every node whose hold a killed run had
	// already lifted — is left strictly alone.
	after, err := readHostNodes(ctx, server)
	if err != nil {
		return fmt.Errorf("re-reading the reboot hold on this cluster's nodes: %w", err)
	}
	for i := range states {
		state := &states[i]
		if state.Server {
			s.Logf("  %s: server — keeps %s=%s; its reboots go through `kubenest node reboot` in the window",
				state.Node, day2.NoAutoRebootLabel, day2.NoAutoRebootValue)
			continue
		}
		current, held := after.labels(state.Node)[day2.NoAutoRebootLabel]
		switch {
		case state.HoldBefore && (!held || current != state.HoldValue):
			if err := restoreHold(ctx, server, state.Node, state.HoldValue); err != nil {
				return err
			}
			s.Logf("  %s: reboot hold restored to %s=%s", state.Node, day2.NoAutoRebootLabel, state.HoldValue)
		case !state.HoldBefore && held:
			if err := releaseHold(ctx, server, state.Node); err != nil {
				return err
			}
			s.Logf("  %s: reboot hold lifted — the node reboots automatically in its window again", state.Node)
		default:
			s.Logf("  %s: reboot hold unchanged (%s)", state.Node, describeHold(state))
		}
	}

	s.Record.HostPolicy = states
	if err := s.saveRecord(); err != nil {
		return fmt.Errorf("recording the host step's outcome: %w", err)
	}
	// The statement the operator needs at the moment the stage completes, and
	// the one rollback repeats: what is left on the hosts is deliberate.
	s.Logf("  The APT policy and the servers' reboot hold are left in place: a rollback does not undo them, because reverting the drop-in would restore Ubuntu's own random install time, which is the behaviour being replaced.")
	return nil
}

// describeAPT renders one host's postcondition: the effective policy, and what
// this run had to write to reach it.
func describeAPT(effective hostpolicy.Effective, written []string) string {
	policy := fmt.Sprintf("APT policy security-only (%s), automatic reboot %v", strings.Join(effective.AllowedOrigins, ", "), effective.AutomaticReboot)
	if len(written) == 0 {
		return policy + " — already in force, nothing written"
	}
	return policy + " — wrote " + strings.Join(written, ", ")
}

// describeHold renders a hold that needed no change.
func describeHold(state *hostPolicyNode) string {
	if !state.HoldBefore {
		return "it was not held before this operation and is not held now"
	}
	return "it was already " + state.HoldValue + " before this operation"
}

// restoreHold puts a node's reboot hold back to the value it carried before
// this operation.
func restoreHold(ctx context.Context, r k3s.Runner, node, value string) error {
	if !interlock.ValidNodeName(node) {
		return fmt.Errorf("restore the reboot hold on %q: not a node name", node)
	}
	if _, err := k3s.Kubectl(ctx, r, "label node "+node+" "+day2.NoAutoRebootLabel+"="+value+" --overwrite"); err != nil {
		return fmt.Errorf("restoring the reboot hold on %s: %w", node, err)
	}
	return nil
}

// releaseHold takes a reboot hold OFF a node that did not carry one before this
// operation.
//
// The command is `kubectl label node <name> <key>-`, which is how a label is
// removed. There is no helper in day2 for this direction: applying the hold is
// the platform's dynamic case and this stage is the only thing that ever lifts
// one.
func releaseHold(ctx context.Context, r k3s.Runner, node string) error {
	if !interlock.ValidNodeName(node) {
		return fmt.Errorf("lift the reboot hold on %q: not a node name", node)
	}
	if _, err := k3s.Kubectl(ctx, r, "label node "+node+" "+day2.NoAutoRebootLabel+"-"); err != nil {
		return fmt.Errorf("lifting the reboot hold on %s: %w", node, err)
	}
	return nil
}

// hostNodes is the slice of `kubectl get nodes -o json` this stage reads: the
// node's name, the UID the cluster's host inventory records (kn-t50), every
// address the cluster knows it by, and its labels.
type hostNodes struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			UID    string            `json:"uid"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Addresses []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"status"`
	} `json:"items"`
}

// readHostNodes reads the cluster's nodes once, for the whole stage.
func readHostNodes(ctx context.Context, server k3s.Runner) (hostNodes, error) {
	out, err := k3s.Kubectl(ctx, server, "get nodes -o json")
	if err != nil {
		return hostNodes{}, err
	}
	var nodes hostNodes
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return hostNodes{}, fmt.Errorf("parsing `kubectl get nodes -o json`: %w", err)
	}
	return nodes, nil
}

// labels returns a node's labels by node name, never nil.
func (n hostNodes) labels(name string) map[string]string {
	for _, item := range n.Items {
		if item.Metadata.Name == name {
			if item.Metadata.Labels == nil {
				return map[string]string{}
			}
			return item.Metadata.Labels
		}
	}
	return map[string]string{}
}

// byAddress maps every address the cluster knows a node by to that node's name:
// its hostname plus whatever InternalIP/ExternalIP the kubelet registered. The
// two sides name a machine differently — this CLI has the address the operator
// typed or the install journal recorded — which is the same reason pkg/k3s's
// NodeUIDsByAddress matches on all of them (kn-t50).
func (n hostNodes) byAddress() map[string]string {
	out := make(map[string]string, len(n.Items))
	for _, item := range n.Items {
		if item.Metadata.Name == "" {
			continue
		}
		out[item.Metadata.Name] = item.Metadata.Name
		for _, a := range item.Status.Addresses {
			if a.Address != "" {
				out[a.Address] = item.Metadata.Name
			}
		}
	}
	return out
}

// byUID maps the Kubernetes Node UID to the node's name.
func (n hostNodes) byUID() map[string]string {
	out := make(map[string]string, len(n.Items))
	for _, item := range n.Items {
		if item.Metadata.Name != "" && item.Metadata.UID != "" {
			out[item.Metadata.UID] = item.Metadata.Name
		}
	}
	return out
}

// known lists the nodes and the addresses they answer to, for a refusal that
// tells the operator what the cluster actually has.
func (n hostNodes) known() string {
	var out []string
	for _, item := range n.Items {
		var addresses []string
		for _, a := range item.Status.Addresses {
			if a.Address != "" {
				addresses = append(addresses, a.Address)
			}
		}
		out = append(out, item.Metadata.Name+" ("+strings.Join(addresses, ", ")+")")
	}
	return strings.Join(out, "; ")
}

// matchNodes maps each host this run acts on to the cluster's Node object.
//
// By every address the cluster knows, then by the node UID the cluster's own
// record carries for the host (kn-t50) — NEVER by position or by count.
// Labelling the wrong node holds a machine the operator did not ask about and
// leaves theirs free to reboot outside its window, which is worse than
// refusing: a hold that cannot be placed on a known node is reported with the
// addresses the cluster does know.
func (n hostNodes) matchNodes(s *Session) (map[string]string, error) {
	byAddress, byUID := n.byAddress(), n.byUID()
	out := make(map[string]string, len(s.Nodes))
	for _, node := range s.Nodes {
		if name := byAddress[node.Address]; name != "" {
			out[node.Address] = name
			continue
		}
		// The record's inventory is the second chance: a host whose SSH
		// address differs from every address its kubelet registered is still
		// named by the Node UID the install recorded for it.
		if uid := inventoryNodeUID(s, node.Address); uid != "" {
			if name := byUID[uid]; name != "" {
				out[node.Address] = name
				continue
			}
		}
		return nil, fmt.Errorf(
			"host %s is not one of this cluster's nodes, so its reboot hold cannot be placed. "+
				"The cluster's nodes are: %s. Re-run with the addresses the cluster knows them by",
			node.Address, n.known())
	}
	return out, nil
}

// inventoryNodeUID is the Node UID the cluster's recorded inventory holds for a
// host, matched on either address the record carries for it.
func inventoryNodeUID(s *Session, address string) string {
	for _, host := range s.Cluster.Hosts {
		if host.NodeUID == "" {
			continue
		}
		if host.SSHAddress == address || host.JoinAddress == address {
			return host.NodeUID
		}
	}
	return ""
}
