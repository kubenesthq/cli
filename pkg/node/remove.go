package node

import (
	"context"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/stages"
)

// The stages of `node remove`, in the order they run.
//
//	resolve        which machine this is, and that it may be removed at all
//	volumes        the local volumes it holds, and the disposition decision
//	window         the maintenance window (PLAN 7.4 item 3)
//	lock           the operation record, the CLI-vs-CLI lock (T2.3)
//	hold           the node out of kured's pool, then kured's lock (T5.1)
//	record-removing the host is written down as removing, BEFORE the drain
//	cordon         the first disruptive step
//	drain          within the disruption budgets and node-drain
//	uninstall      k3s is removed from the host, if it can be reached
//	delete-node    the Node object goes
//	record         the host is marked removed and the recovery metadata is
//	               refreshed
const (
	StageRemoveResolve   = "resolve"
	StageRemoveVolumes   = "volumes"
	StageRemoveWindow    = "window"
	StageRemoveLock      = "lock"
	StageRemoveHold      = "hold"
	StageRemoveRemoving  = "record-removing"
	StageRemoveCordon    = "cordon"
	StageRemoveDrain     = "drain"
	StageRemoveUninstall = "uninstall"
	StageRemoveDelete    = "delete-node"
	StageRemoveRecord    = "record"
)

// RemoveOptions is one `kubenest node remove`.
type RemoveOptions struct {
	// Node names the machine: the inventory's host ID, its SSH address, or
	// the cluster's Node name. It is resolved THROUGH THE INVENTORY so a Node
	// name and a host ID cannot disagree about which machine this is.
	Node string
	// AbandonVolumes acknowledges the loss of every local volume on the node.
	// It certifies NOTHING about backups.
	AbandonVolumes bool
	// Resume continues an interrupted remove by operation id.
	Resume string
	// TakeOver takes over a removal whose record still says its executor is
	// running, on the operator's assertion that the previous executor and its
	// outstanding actions have stopped (PLAN 7.2, kn-yzuv). It requires
	// Confirm — the assertion is the operator's, and the CLI never infers it.
	TakeOver string
	// Confirm is that assertion, alongside TakeOver.
	Confirm bool
	// Wait holds until the maintenance window opens, holding nothing while it
	// waits.
	Wait bool
	// Now bypasses the maintenance window and NOTHING else.
	Now bool
}

// Recovery is the two recovery flags as pkg/operation's Recovery, where their
// rules live: mutually exclusive, and --take-over requires --confirm.
func (o RemoveOptions) Recovery() operation.Recovery {
	return operation.Recovery{Resume: o.Resume, TakeOver: o.TakeOver, Confirm: o.Confirm}
}

// Remove is `kubenest node remove`: the one node verb that can destroy data.
//
// OpenEBS LVM volumes live on ONE node's disk, so removing the node removes
// their only copy. The volume disposition is therefore the gate (PLAN 7.3.2):
// the CLI lists every claim bound to a local volume on the node and REFUSES
// while any remains, unless each has been restored elsewhere or the operator
// passes --abandon-volumes. k3s is never uninstalled before the disposition is
// settled.
type Remove struct {
	*Session
	Opts RemoveOptions

	// volumes is what the node's disk holds: read once, before anything is
	// changed, and reported in the refusal.
	volumes []BoundLocalVolume
}

// PlanRemove is the stage sequence the engine runs for one `node remove`.
func PlanRemove(r *Remove) []stages.Stage {
	return []stages.Stage{
		{Name: StageRemoveResolve, AlwaysRun: true, Run: r.stageResolve},
		{Name: StageRemoveVolumes, AlwaysRun: true, Run: r.stageVolumes},
		{Name: StageRemoveWindow, AlwaysRun: true, Run: r.stageWindow},
		{Name: StageRemoveLock, AlwaysRun: true, Run: r.stageLock},
		{Name: StageRemoveHold, AlwaysRun: true, Run: r.stageHold},
		{Name: StageRemoveRemoving, Run: r.stageRemoving},
		{Name: StageRemoveCordon, Run: r.stageCordon},
		{Name: StageRemoveDrain, Run: r.stageDrain},
		{Name: StageRemoveUninstall, Component: "k3s", Run: r.stageUninstall},
		{Name: StageRemoveDelete, Run: r.stageDelete},
		{Name: StageRemoveRecord, Run: r.stageRecord},
	}
}

// stageResolve finds the machine and checks that it may be removed at all, then
// opens the connection the removal's own steps need.
func (r *Remove) stageResolve(ctx context.Context) error {
	if err := r.ResolveOldHost(ctx); err != nil {
		return err
	}
	if err := r.CheckRemovable(); err != nil {
		return err
	}
	conn, err := r.Dial(ctx, r.Host)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", r.Host.SSHAddress, err)
	}
	return r.ResolveAccess(ctx, conn)
}

// ResolveOldHost finds the machine this removal is about: by the inventory's
// host ID, its SSH address or its Node UID, or — when the operator named a Node
// the cluster knows and the inventory does not — through the cluster's own Node
// object, so a Node name and a host ID cannot disagree about which machine this
// is. It changes nothing.
//
// It is separate from CheckRemovable and from the dial because a composed
// `node replace` resolves the machine a resume already knows by the host ID it
// recorded: the Node object may be gone (the replacement deleted it), and the
// gates belong to a request that has already been made.
func (r *Remove) ResolveOldHost(ctx context.Context) error {
	if err := r.resolveCluster(ctx); err != nil {
		return err
	}
	host, found := r.findHost(r.Opts.Node)
	if !found {
		// A NODE NAME is not an inventory field, so it is resolved through the
		// CLUSTER first and then mapped back to the machine that holds it:
		// resolving it any other way would let a Node name and a host ID
		// disagree about which machine this is.
		var err error
		if host, err = r.hostByNodeName(ctx); err != nil {
			return err
		}
	}
	r.Host = host
	r.Logf("Removing agent %s (%s) from cluster %s.", host.HostID, host.SSHAddress, r.Cluster)
	return nil
}

// CheckRemovable refuses a machine this verb may not remove: one the inventory
// already records as removed, and a SERVER, because a single-server cluster's
// only server is recovered by S6 (`kubenest platform restore` from the recovery
// kit and an off-host backup) and the ha tier's server operations arrive with
// its promotion.
//
// It is separate from ResolveOldHost because a replace resumes a removal THIS
// operation performed: there the host is marked removed because of its own
// work, and asking the fresh question again would refuse the operation it is
// resuming.
func (r *Remove) CheckRemovable() error {
	if state := LifecycleState(r.Host.LifecycleState); state == StateRemoved {
		return fmt.Errorf("host %s (%s) is already recorded as %q, so there is nothing to remove", r.Host.HostID, r.Host.SSHAddress, r.Host.LifecycleState)
	}
	if Role(r.Host.Role) == RoleServer {
		return fmt.Errorf("host %s (%s) is a SERVER of this cluster, and removing it is not this command. On a single-server cluster the only server is recovered by S6: `kubenest platform install --cluster <cluster-id> --restore-from latest --recovery-kit s3` onto a fresh host, from the recovery kit and the off-host backup; on the ha tier, adding and removing servers arrives with its promotion (T6.1, bundle 1.3). Nothing was changed",
			r.Host.HostID, r.Host.SSHAddress)
	}
	return nil
}

// ResolveAccess attaches the connection to the machine and reads the facts
// every later stage goes through: the host key the inventory recorded, the
// Ready server every cluster read and every kubectl write goes through, the
// cluster's nodes, and the Node object this machine is.
//
// The CONNECTION IS THE CALLER'S: `node remove` dials the machine itself, and a
// composed `node replace` passes the connection its own reachability step
// opened — or a runner that reports the machine unreachable, which is the
// premise of the order that removes first. An unreachable runner reports no
// host key, which is not a mismatch this function can claim to have checked:
// there was nothing to compare, and the reachability step has already said so.
func (r *Remove) ResolveAccess(ctx context.Context, conn Transport) error {
	r.Conn = conn
	if err := CheckFingerprint(r.Host, conn); err != nil {
		return err
	}
	server, serverConn, err := ReadyServer(ctx, r.Hosts, r.Dial)
	if err != nil {
		return err
	}
	r.Server, r.ServerConn = server, serverConn
	nodes, err := ReadClusterNodes(ctx, serverConn)
	if err != nil {
		return err
	}
	r.Nodes = nodes
	node, err := NodeFor(nodes, r.Host)
	if err != nil {
		return err
	}
	r.Node = node
	r.Logf("  server:    %s (%s) is Ready and answers the cluster's API", server.HostID, server.SSHAddress)
	r.Logf("  node:      %s (uid %s)", node.Name, node.UID)
	return nil
}

// hostByNodeName resolves a Node NAME to the inventory entry of the machine
// behind it.
//
// It exists because a Node name is not an inventory field: the record holds the
// host ID, the addresses and the Node UID. The cluster is asked which Node
// object carries the name, and the UID that answers is what the inventory is
// matched on — so the name the cluster uses and the machine the record
// describes cannot disagree.
//
// The match is hostForNode's, so an entry the cluster still holds is preferred
// when a Node object matches a removed record as well: a new machine at a
// removed host's address registers the same address, and the record of the
// machine that is gone must not answer for the machine that is there.
func (r *Remove) hostByNodeName(ctx context.Context) (api.HostRecord, error) {
	server, serverConn, err := ReadyServer(ctx, r.Hosts, r.Dial)
	if err != nil {
		return api.HostRecord{}, err
	}
	r.Server, r.ServerConn = server, serverConn
	nodes, err := ReadClusterNodes(ctx, serverConn)
	if err != nil {
		return api.HostRecord{}, err
	}
	r.Nodes = nodes
	for _, n := range nodes {
		if n.Name != r.Opts.Node {
			continue
		}
		if h, found := hostForNode(r.Hosts, n); found {
			return h, nil
		}
		return api.HostRecord{}, fmt.Errorf("the cluster's node %s (uid %s) is not in its inventory, so there is no machine this operation can record as removed. Re-run the install's record stage, or run `kubenest node add --agent <address>` to adopt the host explicitly",
			n.Name, n.UID)
	}
	return api.HostRecord{}, fmt.Errorf("no node of cluster %s is named %q. The inventory holds: %s",
		r.Cluster, r.Opts.Node, DescribeHosts(r.Hosts))
}

// stageVolumes decides what happens to the data on this node, and it is the
// ONLY thing that may authorise destroying it.
//
// It reads the volumes before anything is changed and refuses while a bound
// local volume remains, printing the command that moves each affected
// workload's data somewhere else. --abandon-volumes proceeds and says what
// that means in as many words: the data is gone, and nothing here certifies
// that a backup of it exists.
func (r *Remove) stageVolumes(ctx context.Context) error {
	volumes, err := BoundLocalVolumes(ctx, r.ServerConn, r.Node.Name)
	if err != nil {
		return err
	}
	r.volumes = volumes
	if len(volumes) == 0 {
		r.Logf("  volumes:   %s holds no bound local volume, so removing it destroys no data on this node's disks", r.Node.Name)
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s holds %d volume(s) that exist ONLY on this node's disk:\n", r.Node.Name, len(volumes))
	for _, v := range volumes {
		what := v.Workload
		if what == "" {
			what = "no running pod"
		}
		fmt.Fprintf(&b, "  %s/%s (volume %s, driver %s) — %s\n", v.Namespace, v.PVC, v.PV, orNone(v.Driver), what)
	}
	if !r.Opts.AbandonVolumes {
		fmt.Fprintf(&b, "\nRemoving the node destroys them, so this command refuses. Restore each affected workload's data somewhere else first:\n")
		for _, cmd := range RestoreCommands(r.Cluster, volumes) {
			fmt.Fprintf(&b, "  %s\n", cmd)
		}
		fmt.Fprintf(&b, "\nRestoring one claim of a pod while another stays stranded leaves the pod unable to start, which is why every claim of a workload is in ONE command above. If the data is meant to go, pass --abandon-volumes.")
		return fmt.Errorf("%s", b.String())
	}
	fmt.Fprintf(&b, "\n--abandon-volumes: the data on those %d claim(s) is GONE from this cluster when this node is removed. Nothing here certifies that any of it was backed up. If this workload matters, the lever is a more frequent Velero schedule for its namespace.", len(volumes))
	r.Logf("%s", b.String())
	for _, v := range volumes {
		r.Logf("  volume:    %s/%s on %s will be DESTROYED (%s)", v.Namespace, v.PVC, v.PV, orNone(v.Driver))
	}
	return nil
}

// stageWindow applies the cluster's maintenance window.
func (r *Remove) stageWindow(ctx context.Context) error {
	return r.windowRule(ctx, r.Opts.Now, r.Opts.Wait)
}

// stageLock creates the operation record: the CLI-vs-CLI lock.
func (r *Remove) stageLock(ctx context.Context) error {
	request := operation.Request{
		Kind:    operation.KindNodeRemove,
		Cluster: r.Cluster,
		Targets: []operation.Target{{HostID: r.Host.HostID, NodeUID: r.Node.UID}},
		Versions: map[string]string{
			"bundle":  r.Record.BundleVersion,
			"volumes": fmt.Sprintf("%d", len(r.volumes)),
		},
	}
	return r.openRecord(ctx, operation.KindNodeRemove, request, r.Opts.Recovery())
}

// stageHold takes the node out of kured's pool and then kured's own lock, in
// that order, before the first disruptive step.
//
// The order matters and is easy to get wrong: the hold helper ALSO releases a
// lock the held node owns (probe P1, finding 3), so taken the other way round
// it would release the lock this operation had just taken.
func (r *Remove) stageHold(ctx context.Context) error {
	if r.Node.Labels[day2.NoAutoRebootLabel] != day2.NoAutoRebootValue {
		guarded := r.guard(r.ServerConn, "hold", nodeSpecs)
		if err := day2.HoldAutomaticReboots(ctx, guarded, r.Node.Name); err != nil {
			return fmt.Errorf("holding %s out of kured's pool: %w", r.Node.Name, err)
		}
		if r.Node.Labels == nil {
			r.Node.Labels = map[string]string{}
		}
		r.Node.Labels[day2.NoAutoRebootLabel] = day2.NoAutoRebootValue
		r.Logf("  hold:      %s labelled %s=%s for the duration of the removal", r.Node.Name, day2.NoAutoRebootLabel, day2.NoAutoRebootValue)
	}
	ttl, err := r.lockTTL()
	if err != nil {
		return err
	}
	if err := r.takeInterlock(ctx, r.Node.Name, ttl); err != nil {
		return err
	}
	return nil
}

// stageRemoving writes the host down as removing, before the drain: an
// operation that dies mid-removal leaves a record that says so, and the
// inventory agrees with it.
func (r *Remove) stageRemoving(ctx context.Context) error {
	r.Host.LifecycleState = string(StateRemoving)
	if err := r.owe(ctx, "inventory", r.Host.HostID, "the host recorded as removing"); err != nil {
		return err
	}
	if err := r.writeInventory(ctx, r.replaceHost(r.Host)); err != nil {
		return err
	}
	if err := r.owed(ctx, "inventory", r.Host.HostID); err != nil {
		return err
	}
	r.Logf("  inventory: %s written as %s at revision %d", r.Host.HostID, StateRemoving, r.Revision-1)
	return nil
}

// stageCordon makes the node unschedulable. It is the first disruptive step,
// so the record and both locks exist before it.
func (r *Remove) stageCordon(ctx context.Context) error {
	guarded := r.guard(r.ServerConn, "cordon", nodeSpecs)
	if _, err := k3s.Kubectl(ctx, guarded, "cordon "+r.Node.Name); err != nil {
		return fmt.Errorf("cordoning %s: %w", r.Node.Name, err)
	}
	r.Logf("  cordon:    %s is unschedulable", r.Node.Name)
	return nil
}

// stageDrain evicts the node's pods within the bundle's node-drain timeout,
// over the PodDisruptionBudgets.
//
// The budget gate runs FIRST: a budget that permits no disruption stalls the
// drain until its timeout and then leaves the cluster mid-removal, which is a
// strictly worse place than not starting. A pod is NEVER force-deleted — that
// is an operator's decision, not a tool's.
func (r *Remove) stageDrain(ctx context.Context) error {
	if report := k3s.DrainWouldFinish(ctx, r.ServerConn); !report.Passed {
		return fmt.Errorf("the drain would not finish: %s\n      fix: %s", report.Detail, report.Fix)
	}
	drainFor, err := r.Bundle.Limits.Timeouts.For("node-drain")
	if err != nil {
		return err
	}
	guarded := r.guard(r.ServerConn, "drain", nodeSpecs)
	args := "drain " + r.Node.Name + " --ignore-daemonsets --delete-emptydir-data --timeout=" + drainFor.String()
	if _, err := k3s.Kubectl(ctx, guarded, args); err != nil {
		return fmt.Errorf("draining %s within %s (limits.timeouts.node-drain; the drain never force-deletes a pod, so a PodDisruptionBudget that permits no disruption stops it here): %w",
			r.Node.Name, drainFor, err)
	}
	r.Logf("  drain:     %s drained within %s, over the PodDisruptionBudgets", r.Node.Name, drainFor)
	return nil
}

// stageUninstall removes k3s from the host.
//
// A HOST THAT CANNOT BE REACHED DOES NOT FAIL THE REMOVAL. The Node object's
// deletion is what the cluster can assert; a machine that is gone is a machine
// an operator has to fence at the provider anyway (PLAN 7.3.2), and failing
// here would leave the record saying the node is still in the cluster when it
// is not. What cannot be reached is reported.
func (r *Remove) stageUninstall(ctx context.Context) error {
	// The path is pkg/uninstall's own constant (uninstall.go): a removal here
	// and a teardown there must name the same script, and the script is what
	// the k3s installer wrote.
	const agentUninstall = "/usr/local/bin/k3s-agent-uninstall.sh"
	command := fmt.Sprintf("if [ -x %s ]; then sudo -n %s; else echo 'no k3s installation'; fi", agentUninstall, agentUninstall)
	guarded := r.guard(r.Conn, "uninstall", nodeSpecs)
	res, err := guarded.Run(ctx, command)
	switch {
	case err != nil:
		r.Logf("  uninstall: %s could not be reached (%v), so k3s is still installed there. The cluster no longer has this node; fence the machine at the provider if it was lost", r.Host.SSHAddress, err)
		return nil
	case res.ExitCode != 0:
		r.Logf("  uninstall: the k3s uninstall script on %s exited %d: %s. The Node object is still deleted below, so the cluster is clean; the host may still hold k3s", r.Host.SSHAddress, res.ExitCode, firstLine(res.Stderr))
		return nil
	}
	r.Logf("  uninstall: k3s removed from %s", r.Host.SSHAddress)
	return nil
}

// stageDelete deletes the Node object.
func (r *Remove) stageDelete(ctx context.Context) error {
	guarded := r.guard(r.ServerConn, "delete-node", nodeSpecs)
	if _, err := k3s.Kubectl(ctx, guarded, "delete node "+r.Node.Name); err != nil {
		return fmt.Errorf("deleting the Node object %s: %w", r.Node.Name, err)
	}
	r.Logf("  node:      %s deleted from the cluster", r.Node.Name)
	return nil
}

// stageRecord marks the host removed and refreshes the recovery metadata.
//
// The entry STAYS, with its host ID and its host-key fingerprint: the record
// that this machine was here is what stops the CLI from re-adding it by
// accident without a wipe (PLAN 7.3.2).
func (r *Remove) stageRecord(ctx context.Context) error {
	// The Node object is gone (the delete stage completed, and its
	// postcondition is what a resume re-establishes), so the cluster has one
	// fewer node: the cluster-DNS layout is re-sized BEFORE this stage's own
	// record write, so a layout that cannot be put right leaves the write owed
	// and the resume clean. The stage re-runs whenever its own work did not
	// finish, so the step is re-applied rather than skipped (and it is
	// idempotent either way — Session.ensureCoreDNS).
	if err := r.ensureCoreDNS(ctx); err != nil {
		return err
	}
	r.Host.LifecycleState = string(StateRemoved)
	if err := r.owe(ctx, "inventory", r.Host.HostID, "the host marked removed"); err != nil {
		return err
	}
	if err := r.writeInventory(ctx, r.replaceHost(r.Host)); err != nil {
		return err
	}
	if err := r.owed(ctx, "inventory", r.Host.HostID); err != nil {
		return err
	}
	r.Logf("  inventory: %s is %s at revision %d; its host ID and host key are kept, so it is not re-added by accident",
		r.Host.HostID, StateRemoved, r.Revision-1)
	r.refreshMetadata(ctx)
	return nil
}

// Finish closes a `node remove` the way the run ended. It releases kured's
// lock — the cluster-wide interlock — and the CLI's own record.
func (r *Remove) Finish(ctx context.Context, runErr error, interrupted bool) {
	ttl, err := r.lockTTL()
	if err != nil {
		ttl = 0
	}
	r.releaseInterlock(ctx, r.interlockNode, ttl)
	r.finishRecord(ctx, runErr, interrupted)
	r.Close()
}

// replaceHost returns the inventory with this host's entry updated in place.
// Unlike an add, a remove acts on a host the inventory already has, so an
// entry that is not there is a programming error rather than a new machine.
func (r *Remove) replaceHost(host api.HostRecord) []api.HostRecord {
	out := make([]api.HostRecord, 0, len(r.Hosts))
	for _, h := range r.Hosts {
		if h.HostID == host.HostID {
			out = append(out, host)
			continue
		}
		out = append(out, h)
	}
	return out
}

// firstLine is the first line of remote output, for a message that must not
// paste a whole stderr into a terminal.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
