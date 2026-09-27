package node

import (
	"context"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/storage"
)

// The stages of `node add`, in the order they run. They are the journal's
// vocabulary: a resume reads them by name, so renaming one is a coordinated
// change.
//
//	resolve      what the cluster is, and which machine joins it
//	window       the maintenance window (PLAN 7.4 item 3), BEFORE the locks
//	preflight    the install's own host checks against the new machine
//	lock         the operation record, the CLI-vs-CLI lock (T2.3)
//	joining      the host is written down as joining, BEFORE it is touched
//	hold         the node out of kured's pool, then kured's lock (T5.1) — a
//	             no-op before the node exists, which is why the join repeats it
//	join         the join itself, and the hold the moment the node appears
//	storage      the volume group kubenest-vg is created or verified
//	record       the host is marked active, the recovery metadata is refreshed,
//	             and the hold is lifted LAST
const (
	StageAddResolve   = "resolve"
	StageAddWindow    = "window"
	StageAddPreflight = "preflight"
	StageAddLock      = "lock"
	StageAddJoining   = "record-joining"
	StageAddHold      = "hold"
	StageAddJoin      = "join"
	StageAddStorage   = "storage"
	StageAddRecord    = "record"
)

// AddOptions is one `kubenest node add`.
type AddOptions struct {
	// Agent is the machine to add: its SSH address, or a host ID the
	// inventory already knows (an interrupted join leaves one).
	Agent string
	// StorageDevice is the stable path of a blank device to create
	// kubenest-vg on. Empty means Option 1 — the volume group already exists,
	// exactly as `platform install` treats it.
	StorageDevice string
	// Resume continues an interrupted add by operation id.
	Resume string
	// Wait holds until the maintenance window opens, holding nothing while it
	// waits.
	Wait bool
	// Now bypasses the maintenance window and NOTHING else.
	Now bool
}

// addState is what a resumed add carries across processes: the host it is
// about, the node that host became, and what the storage pre-flight decided.
//
// It is journalled beside the stage transitions because the stages that
// resolve it are SKIPPED on a resume — a skipped stage does not run, so
// anything a later stage needs from it has to be written down. It holds no
// credential.
type addState struct {
	HostID    string `json:"host_id,omitempty"`
	NodeName  string `json:"node_name,omitempty"`
	NodeUID   string `json:"node_uid,omitempty"`
	Ownership string `json:"ownership,omitempty"`
}

// Add is `kubenest node add`: the capacity verb (PLAN 7.3).
//
// It reuses the install engine's stages for ONE host — the same pre-flight
// checks, the same k3s installer at the bundle's pin, the same volume-group
// work — and its own order: the host is written down before it is touched, the
// hold goes on before the node can be rebooted, and the inventory is complete
// only once the node is Ready.
type Add struct {
	*Session
	Opts AddOptions

	// ownership is what the storage pre-flight decided about kubenest-vg on
	// the new host, recorded in its inventory entry.
	ownership storage.Ownership
}

// PlanAdd is the stage sequence the engine runs for one `node add`.
func PlanAdd(a *Add) []stages.Stage {
	return []stages.Stage{
		{Name: StageAddResolve, AlwaysRun: true, Run: a.stageResolve},
		{Name: StageAddWindow, AlwaysRun: true, Run: a.stageWindow},
		{Name: StageAddPreflight, AlwaysRun: true, Run: a.stagePreflight},
		{Name: StageAddLock, AlwaysRun: true, Run: a.stageLock},
		{Name: StageAddJoining, Run: a.stageJoining},
		{Name: StageAddHold, AlwaysRun: true, Run: a.stageHold},
		{Name: StageAddJoin, Component: "k3s", Run: a.stageJoin},
		{Name: StageAddStorage, Component: storage.ComponentKey, Run: a.stageStorage},
		{Name: StageAddRecord, AlwaysRun: true, Run: a.stageRecord},
	}
}

// stageState reads what a previous process wrote down, if anything.
func (a *Add) stageState() addState {
	var st addState
	if a.Jnl == nil {
		return st
	}
	// A journal that cannot be decoded leaves the zero state: it is the record
	// of a first run, and refusing here would be refusing work that has not
	// happened.
	_ = a.Jnl.DecodeState(&st)
	return st
}

// saveState writes the state a resume needs.
func (a *Add) saveState(st addState) error {
	if a.Jnl == nil {
		return nil
	}
	return a.Jnl.SetState(st)
}

// stageResolve answers what this cluster IS and which machine joins it.
//
// The server is a READY one chosen from the inventory, never the first one
// installed: a join through a server that is down or NotReady fails in a way
// that looks like a fault in the NEW host. The new machine is dialled, and its
// host key is what the inventory will record for it.
func (a *Add) stageResolve(ctx context.Context) error {
	if err := a.resolveCluster(ctx); err != nil {
		return err
	}
	a.Logf("Adding an agent to cluster %s (bundle %s, %d host(s) in the inventory).", a.Cluster, a.Record.BundleVersion, len(a.Hosts))

	// The machine being added: an entry the inventory already has — an
	// interrupted join left one, and its host ID is that host's identity for
	// the rest of its life in this cluster, so it is REUSED rather than minted
	// again — or a brand-new host, recorded when it joins.
	if existing, found := a.findHost(a.Opts.Agent); found {
		switch LifecycleState(existing.LifecycleState) {
		case StateJoining:
			a.Host = existing
			a.Logf("  host:      %s is already recorded as joining (%s); continuing that entry", existing.HostID, existing.SSHAddress)
		case StateActive:
			return fmt.Errorf("host %s (%s) is already an ACTIVE host of this cluster, so there is nothing to add. A machine that is in the cluster is not how capacity grows: run `kubenest node remove` if this host's entry is wrong",
				existing.HostID, existing.SSHAddress)
		default:
			return fmt.Errorf("host %s (%s) is recorded as %q: a host that was removed is not re-added by this command (its host ID is kept, and its disks may still hold another cluster's data). Wipe the machine and add it as a new host, or restore its record deliberately",
				existing.HostID, existing.SSHAddress, existing.LifecycleState)
		}
	}

	conn, err := a.Dial(ctx, a.hostToDial())
	if err != nil {
		return fmt.Errorf("connecting to the machine to add (%s): %w", a.Opts.Agent, err)
	}
	a.Conn = conn
	if a.Host.HostKeyFingerprint != "" {
		if err := CheckFingerprint(a.Host, conn); err != nil {
			return err
		}
	} else {
		a.Logf("  host:      %s answers on %s; the host key %s is what this operation records for it",
			a.Opts.Agent, a.Opts.Agent, conn.HostKeyFingerprint())
	}
	// A brand-new machine enters the inventory with a HOST ID and nothing
	// else: the ID is minted here, before the operation record names this host
	// as its target, and the entry itself is only written once the locks are
	// held and the window allows it.
	if a.Host.HostID == "" {
		entry, err := Entry{
			Role:               RoleAgent,
			SSHAddress:         a.Opts.Agent,
			SSHPort:            HostPort(conn),
			SSHUser:            HostUser(conn),
			HostKeyFingerprint: conn.HostKeyFingerprint(),
			JoinAddress:        "",
			LifecycleState:     StateJoining,
			StorageDevice:      a.Opts.StorageDevice,
		}.Record()
		if err != nil {
			return err
		}
		a.Host = entry
	}

	server, serverConn, err := ReadyServer(ctx, a.Hosts, a.Dial)
	if err != nil {
		return err
	}
	a.Server, a.ServerConn = server, serverConn
	nodes, err := ReadClusterNodes(ctx, serverConn)
	if err != nil {
		return err
	}
	a.Nodes = nodes
	a.Logf("  server:    %s (%s) is Ready and answers the cluster's API (%d node(s) in the cluster)",
		server.HostID, server.SSHAddress, len(nodes))

	// What a previous process wrote down: the node this host became (a resume
	// re-runs the stages after a completed join without running the join
	// again) and what the storage pre-flight decided.
	st := a.stageState()
	if st.NodeName != "" {
		for _, n := range nodes {
			if n.Name == st.NodeName && (st.NodeUID == "" || n.UID == st.NodeUID) {
				a.Node = n
				a.Host.NodeUID = n.UID
				a.Logf("  node:      %s (uid %s) is the node this operation joined, from the journal", n.Name, n.UID)
			}
		}
	}
	a.ownership = storage.Ownership(st.Ownership)
	return nil
}

// hostToDial is the address to reach the machine being added at: the
// inventory's address for a host it already knows, or what the operator typed.
func (a *Add) hostToDial() api.HostRecord {
	if a.Host.SSHAddress != "" {
		return a.Host
	}
	return api.HostRecord{Role: string(RoleAgent), SSHAddress: a.Opts.Agent}
}

// stageWindow applies the cluster's maintenance window. It runs BEFORE the
// pre-flight checks and the locks, so that a --wait run checks the new host
// and takes the locks at the moment the window actually opens rather than
// hours earlier.
func (a *Add) stageWindow(ctx context.Context) error {
	return a.windowRule(ctx, a.Opts.Now, a.Opts.Wait)
}

// stagePreflight runs the install's own checks against the new host.
//
// It is the SAME preflight the installer runs — the same code, the same
// checks, the same bundle-decided Ubuntu matrix — because a machine that may
// not join a cluster at install may not join one later. Nothing has been
// written anywhere at this point, which is what makes abandoning an add here
// free.
func (a *Add) stagePreflight(ctx context.Context) error {
	report, err := preflight.Run(ctx, preflight.Options{
		Bundle:        a.Bundle,
		BundleVersion: a.Record.BundleVersion,
		HATier:        a.Record.HATier,
		Profiles:      a.Record.Profiles,
		StorageDevice: a.Opts.StorageDevice,
		Nodes: []preflight.Node{
			{Address: a.Server.SSHAddress, Role: a.Server.Role, Runner: a.ServerConn},
			{Address: a.Opts.Agent, Role: string(RoleAgent), Runner: a.Conn},
		},
		Egress:  a.Egress,
		Catalog: a.Catalog,
	})
	for _, warning := range report.Warnings() {
		a.Logf("  warning: %s", warning)
	}
	for _, r := range report.Results {
		if r.Outcome == preflight.Pass {
			a.Logf("  ok   %s on %s: %s", r.Check, r.Node, r.Detail)
		}
	}
	return err
}

// stageLock creates the operation record, which is the CLI-vs-CLI lock
// (PLAN 7.2): a second laptop starting an add against this cluster is refused
// and told who is running one.
func (a *Add) stageLock(ctx context.Context) error {
	request := operation.Request{
		Kind:    operation.KindNodeAdd,
		Cluster: a.Cluster,
		Targets: []operation.Target{{HostID: a.Host.HostID, NodeUID: a.Host.NodeUID}},
		Versions: map[string]string{
			"bundle": a.Record.BundleVersion,
			"k3s":    a.k3sVersion(),
		},
	}
	return a.openRecord(ctx, operation.KindNodeAdd, request, a.Opts.Resume)
}

// stageJoining writes the host into the inventory BEFORE anything is done to
// it.
//
// The write is what makes an interrupted join identifiable instead of
// invisible (PLAN 7.2): a host that half-joined exists in the record, so the
// next operator can see it and decide. The operation's own record keeps the
// write owed until it lands.
func (a *Add) stageJoining(ctx context.Context) error {
	st := a.stageState()
	// The host ID was minted in the resolve stage, so the operation record
	// could name its target. What is written HERE is the inventory entry: the
	// host is in the record before anything is done to it.
	a.Host.LifecycleState = string(StateJoining)
	a.Host.JoinAddress = JoinURL(a.Server)
	st.HostID = a.Host.HostID
	if err := a.saveState(st); err != nil {
		return err
	}

	if err := a.owe(ctx, "inventory", a.Host.HostID, "the host recorded as joining"); err != nil {
		return err
	}
	if err := a.writeInventory(ctx, a.upsertHost(a.Host)); err != nil {
		return err
	}
	if err := a.owed(ctx, "inventory", a.Host.HostID); err != nil {
		return err
	}
	a.Logf("  inventory: %s written as %s at revision %d, so an interrupted join is identifiable", a.Host.HostID, StateJoining, a.Revision-1)
	return nil
}

// stageJoin joins the machine: the token is read on the server, k3s is
// installed at the bundle's pin, the node is HELD as soon as its object
// exists, and only then waited for.
//
// THE HOLD COMES AS EARLY AS IT CAN. A Node object must exist before it can
// carry a label, and kured's affinity is `NotIn [false]` — an ABSENT label
// satisfies that, so an unheld node is one kured may reboot. The earliest a
// node can be held is the moment its object appears, which this stage waits
// for and labels BEFORE any readiness wait.
//
// Every step here is idempotent, which is why the stage runs again on a
// resume: the installer checks the installed version and does nothing if it
// already matches, the hold is a label, and the lock is a compare-and-swap
// that a second take by the same node satisfies.
func (a *Add) stageJoin(ctx context.Context) error {
	token, err := k3s.NodeToken(ctx, a.ServerConn)
	if err != nil {
		return fmt.Errorf("reading the cluster's join token on %s: %w", a.Server.SSHAddress, err)
	}
	guarded := a.guard(a.Conn, "join", nodeSpecs)
	if err := k3s.InstallAgent(ctx, guarded, a.Bundle, JoinURL(a.Server), token, a.waitReporter()); err != nil {
		return fmt.Errorf("joining %s to the cluster running k3s %s: %w", a.Opts.Agent, a.k3sVersion(), err)
	}
	node, err := a.waitForJoinedNode(ctx)
	if err != nil {
		return err
	}
	a.Node = node
	a.Host.NodeUID = node.UID
	a.Logf("  join:      %s is node %s (uid %s)", a.Opts.Agent, node.Name, node.UID)

	// The hold and kured's lock go on HERE as well as in the hold stage,
	// because this is the first instant they CAN: the lock document names a
	// node, and until the machine registers there is no node to name.
	if err := a.holdAndLock(ctx); err != nil {
		return err
	}

	want := len(a.Nodes) + 1
	a.Logf("  join:      waiting for %d node(s) to be Ready (limits.timeouts.node-ready)", want)
	return k3s.WaitNodesReady(ctx, a.ServerConn, a.Bundle, want, a.waitReporter())
}

// stageHold puts the node out of kured's pool and takes kured's own lock
// (T5.1), which is what stops an automatic reboot and this operation from both
// taking a node down.
//
// BEFORE THE NODE EXISTS THERE IS NOTHING TO HOLD, and this stage says so
// rather than inventing something: kured's lock names a node and its
// schedulability is read back through kubectl, so a machine that is not yet a
// node cannot hold one. The join stage calls the same function the moment the
// node appears, and this stage is what re-establishes both on a resume whose
// join already happened — the journal skips the join, so without this the hold
// and the lock would be missing for the rest of a resumed operation.
func (a *Add) stageHold(ctx context.Context) error {
	if a.Node.Name == "" {
		a.Logf("  hold:      the node does not exist yet; the hold and kured's lock go on the moment it registers")
		return nil
	}
	return a.holdAndLock(ctx)
}

// holdAndLock is the hold plus kured's lock, in that order and idempotent.
//
// The ORDER is the point and is easy to get wrong: the landed hold helper
// (T3.3) also releases a lock the held node OWNS (probe P1, finding 3), so
// taken the other way round it would release the lock this operation had just
// taken.
func (a *Add) holdAndLock(ctx context.Context) error {
	if err := a.hold(ctx); err != nil {
		return err
	}
	ttl, err := a.lockTTL()
	if err != nil {
		return err
	}
	return a.takeInterlock(ctx, a.Node.Name, ttl)
}

// stageStorage makes kubenest-vg exist on the new node, exactly as the
// install's storage stage does for a host it installed.
//
// It does not run again on a resume: with --storage-device given, "the volume
// group already exists" is this stage's own work, and a second run would
// refuse it as a contradiction rather than recognising it. What the stage
// decided is carried in the journal for the record stage.
func (a *Add) stageStorage(ctx context.Context) error {
	ownership, err := storage.PreflightVolumeGroup(ctx, a.Conn, a.Opts.StorageDevice)
	if err != nil {
		return err
	}
	a.ownership = ownership
	if err := storage.EnsureVolumeGroup(ctx, a.guard(a.Conn, "storage", nodeSpecs), a.Opts.StorageDevice); err != nil {
		return err
	}
	st := a.stageState()
	st.Ownership = string(ownership)
	if err := a.saveState(st); err != nil {
		return err
	}
	if a.Opts.StorageDevice == "" {
		a.Logf("  storage:   the existing volume group %s on %s has free extents (%s)", storage.VolumeGroup, a.Opts.Agent, ownership)
		return nil
	}
	a.Logf("  storage:   volume group %s created on %s on %s (%s)", storage.VolumeGroup, a.Opts.StorageDevice, a.Opts.Agent, ownership)
	return nil
}

// stageRecord finishes the record: the host is marked ACTIVE only now, once
// the node is Ready and the volume group exists; then the recovery metadata is
// refreshed; and the hold is lifted LAST.
//
// It runs again on a resume, which is what makes a run that died between the
// join and the inventory write recoverable: the write is a compare-and-swap on
// the revision, so re-applying it either lands or is refused by name.
func (a *Add) stageRecord(ctx context.Context) error {
	if a.Host.HostID == "" {
		return fmt.Errorf("no host entry was recorded for %s, so there is nothing to complete: the record-joining stage did not run, which means the journal and this command disagree about where the operation is",
			a.Opts.Agent)
	}
	a.Host.LifecycleState = string(StateActive)
	a.Host.NodeUID = a.Node.UID
	a.Host.StorageDevice = a.Opts.StorageDevice
	if a.ownership != "" {
		a.Host.VolumeGroupOwnership = string(a.ownership)
	}
	if err := a.owe(ctx, "inventory", a.Host.HostID, "the host marked active"); err != nil {
		return err
	}
	if err := a.writeInventory(ctx, a.upsertHost(a.Host)); err != nil {
		return err
	}
	if err := a.owed(ctx, "inventory", a.Host.HostID); err != nil {
		return err
	}
	a.Logf("  inventory: %s is %s (node %s, storage %s) at revision %d",
		a.Host.HostID, StateActive, a.Node.Name, orNone(a.Host.StorageDevice), a.Revision-1)

	a.refreshMetadata(ctx)
	return a.liftHold(ctx, a.Node, RoleAgent)
}

// Finish closes a `node add` the way the run ended.
//
// THE HOLD IS KEPT WHEN THE RUN FAILED. A node that joined but whose inventory
// write did not land is not recorded, and an unrecorded node is exactly what
// kured must not reboot (PLAN 7.4) — so the hold stays until a re-run finishes
// the record, and this says so. kured's lock is released either way: it is the
// cluster-wide interlock, and a failed add has not taken any node down.
func (a *Add) Finish(ctx context.Context, runErr error, interrupted bool) {
	if runErr != nil && a.Node.Name != "" && a.Host.LifecycleState != string(StateActive) {
		a.Logf("  hold:      %s keeps %s=%s: it is not recorded as active yet, so kured must not reboot it. Re-run this command to finish the record",
			a.Node.Name, day2.NoAutoRebootLabel, day2.NoAutoRebootValue)
	}
	ttl, err := a.lockTTL()
	if err != nil {
		ttl = 0
	}
	a.releaseInterlock(ctx, a.interlockNode, ttl)
	a.finishRecord(ctx, runErr, interrupted)
	a.Close()
}

// hold takes the node out of kured's pool until this operation records it.
//
// It is the landed helper (T3.3) rather than a second implementation of the
// label, and that helper also releases a lock the held node still OWNS — a
// node labelled while kured held its lock would otherwise keep the lock AND
// the cordon (probe P1, finding 3).
func (a *Add) hold(ctx context.Context) error {
	if a.Node.Labels[day2.NoAutoRebootLabel] == day2.NoAutoRebootValue {
		a.Logf("  hold:      %s already carries %s=%s", a.Node.Name, day2.NoAutoRebootLabel, day2.NoAutoRebootValue)
		return nil
	}
	guarded := a.guard(a.ServerConn, "hold", nodeSpecs)
	if err := day2.HoldAutomaticReboots(ctx, guarded, a.Node.Name); err != nil {
		return fmt.Errorf("holding %s out of kured's pool: %w", a.Node.Name, err)
	}
	if a.Node.Labels == nil {
		a.Node.Labels = map[string]string{}
	}
	a.Node.Labels[day2.NoAutoRebootLabel] = day2.NoAutoRebootValue
	a.Logf("  hold:      %s labelled %s=%s, so kured cannot reboot it until it is recorded", a.Node.Name, day2.NoAutoRebootLabel, day2.NoAutoRebootValue)
	return nil
}

// waitForJoinedNode finds the Node object the new machine registered.
//
// A node a previous process already joined is found by the name in the journal
// (a resume must not wait for a node that is already there). A brand-new node
// has no UID and no recorded address yet: what identifies it is that it was
// NOT in the cluster when this run started, so the difference between the two
// readings is the machine that just joined. TWO new nodes at once is a refusal
// — the CLI cannot tell which one this operation is about, and guessing is how
// the wrong machine is recorded.
func (a *Add) waitForJoinedNode(ctx context.Context) (ClusterNode, error) {
	if st := a.stageState(); st.NodeName != "" {
		nodes, err := ReadClusterNodes(ctx, a.ServerConn)
		if err != nil {
			return ClusterNode{}, err
		}
		for _, n := range nodes {
			if n.Name == st.NodeName && (st.NodeUID == "" || n.UID == st.NodeUID) {
				return n, nil
			}
		}
	}
	deadline, err := a.Bundle.Limits.Timeouts.For("node-ready")
	if err != nil {
		return ClusterNode{}, err
	}
	before := map[string]bool{}
	for _, n := range a.Nodes {
		before[n.Name] = true
	}
	until := a.now().Add(deadline)
	for {
		nodes, err := ReadClusterNodes(ctx, a.ServerConn)
		if err == nil {
			if a.Host.NodeUID != "" {
				if node, err := NodeFor(nodes, a.Host); err == nil {
					return a.remembered(node)
				}
			}
			var fresh []ClusterNode
			for _, n := range nodes {
				if !before[n.Name] {
					fresh = append(fresh, n)
				}
			}
			switch len(fresh) {
			case 1:
				return a.remembered(fresh[0])
			case 0:
			default:
				var names []string
				for _, n := range fresh {
					names = append(names, n.Name+" ("+strings.Join(n.Addresses, ", ")+")")
				}
				return ClusterNode{}, fmt.Errorf("two nodes joined this cluster while this operation ran, and nothing here can tell which one is %s: %s. Nothing was recorded for either; run `kubenest node remove` for the one that does not belong, or re-run this command naming the host's Node UID",
					a.Opts.Agent, strings.Join(names, "; "))
			}
		}
		if a.now().After(until) {
			return ClusterNode{}, fmt.Errorf("%s did not register as a node within %s (limits.timeouts.node-ready): the agent installer returned, but no new Node object appeared in the cluster. Check the host's k3s-agent service and its network path to %s",
				a.Host.SSHAddress, deadline, a.Server.SSHAddress)
		}
		if err := a.sleep(ctx, a.poll()); err != nil {
			return ClusterNode{}, err
		}
	}
}

// remembered writes the node down in the journal, so a resume finds it by name
// instead of waiting for a node that is already there.
func (a *Add) remembered(node ClusterNode) (ClusterNode, error) {
	st := a.stageState()
	st.NodeName, st.NodeUID = node.Name, node.UID
	if err := a.saveState(st); err != nil {
		return ClusterNode{}, err
	}
	return node, nil
}

// upsertHost returns the inventory with this host's entry replaced or added,
// preserving the order it already had.
func (a *Add) upsertHost(host api.HostRecord) []api.HostRecord {
	out := make([]api.HostRecord, 0, len(a.Hosts)+1)
	replaced := false
	for _, h := range a.Hosts {
		if h.HostID == host.HostID {
			out = append(out, host)
			replaced = true
			continue
		}
		out = append(out, h)
	}
	if !replaced {
		out = append(out, host)
	}
	return out
}

// writeInventory writes the cluster's host inventory back, carrying the
// revision this run read — the compare-and-swap that stops two operators from
// silently overwriting each other's inventory.
//
// Every field the control plane requires on the write is round-tripped from
// the record that was read, so a node operation cannot erase the bundle
// version, the profiles or the volume-group ownership it did not intend to
// touch.
func (a *Add) writeInventory(ctx context.Context, hosts []api.HostRecord) error {
	record := api.BundleRecord{
		BundleVersion:        a.Record.BundleVersion,
		Profiles:             a.Record.Profiles,
		HATier:               a.Record.HATier,
		VolumeGroupOwnership: a.Record.VolumeGroupOwnership,
		Hosts:                hosts,
		Revision:             a.Revision,
	}
	if err := a.Records.Save(ctx, record); err != nil {
		return fmt.Errorf("writing the cluster's host inventory at revision %d: %w. The control plane refuses a write based on a revision another operator has moved on from, so re-read the record and re-apply", a.Revision, err)
	}
	a.Hosts = hosts
	a.Revision++
	return nil
}

// k3sVersion is the version this cluster runs, from the bundle the cluster
// records: a node joins at the cluster's version, never at one of its own.
func (a *Add) k3sVersion() string {
	if a.Bundle == nil {
		return ""
	}
	v, err := a.Bundle.Core.Version("k3s")
	if err != nil {
		return ""
	}
	return v
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
