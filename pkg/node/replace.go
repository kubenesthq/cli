package node

import (
	"context"
	"fmt"
	"io"
	"strings"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

// The stages of `node replace`, in the order they run. They are the journal's
// vocabulary — a resume reads them by name — and a resume RE-ENTERS the order
// the operation started in.
//
//	resolve       the two machines, and that both may be what the operator says
//	window        the maintenance window (PLAN 7.4 item 3), BEFORE the locks and
//	              before the facts the order depends on
//	reachability  is the machine being replaced there? — the order follows from it
//	volumes       what that machine's disks hold, and whether that blocks the order
//	lock          the operation record (T2.3): ONE record, so a replace cannot
//	              race an add, and the order is part of its immutable request
//
// and then ONE of two orders, composed from T5.2's add stages and T5.3's remove
// stages — neither is reimplemented here:
//
//	add-first     add:* then remove:* — the replacement joins before the machine
//	              it replaces is touched, so capacity is never reduced to zero
//	remove-first  remove:* then add:*, then restore-commands — etcd's own order,
//	              and the only one that works when the old machine may come back
//
// THE NAMES AN ACTION CARRIES IN THE OPERATION RECORD ARE THE HALVES' OWN
// (`join`, `drain`, `delete-node`). An action's identity is its stage and its
// command together (operation.ActionID), so relabelling them for this journal
// would make a resume fail to recognise the actions it must not submit twice.
const (
	StageReplaceResolve      = "resolve"
	StageReplaceReachability = "reachability"
	StageReplaceVolumes      = "volumes"
	StageReplaceWindow       = "window"
	StageReplaceLock         = "lock"

	StageReplaceAddResolve   = "add:resolve"
	StageReplaceAddPreflight = "add:preflight"
	StageReplaceAddJoining   = "add:record-joining"
	StageReplaceAddHold      = "add:hold"
	StageReplaceAddJoin      = "add:join"
	StageReplaceAddStorage   = "add:storage"
	StageReplaceAddRecord    = "add:record"

	StageReplaceRemoveResolve   = "remove:resolve"
	StageReplaceRemoveHold      = "remove:hold"
	StageReplaceRemoveRemoving  = "remove:record-removing"
	StageReplaceRemoveCordon    = "remove:cordon"
	StageReplaceRemoveDrain     = "remove:drain"
	StageReplaceRemoveUninstall = "remove:uninstall"
	StageReplaceRemoveDelete    = "remove:delete-node"
	StageReplaceRemoveRecord    = "remove:record"

	StageReplaceRestoreCommands = "restore-commands"
)

// Branch is the order a replace runs in. It follows from the machine — is it
// there? — and not from a flag: an operator asking for an order is asking the
// command to decide for them, which is exactly what PLAN 7.3 refuses. It is
// part of the operation's immutable request, so a resume re-enters the order the
// operation started in even when the machine has answered since.
type Branch string

const (
	// BranchAddFirst adds the replacement before the machine being replaced is
	// touched, which is what keeps capacity for the workload set.
	BranchAddFirst Branch = "add-first"
	// BranchRemoveFirst takes the machine being replaced out first, which is
	// etcd's order for a member that is gone and the only order that works when
	// the old machine may come back.
	BranchRemoveFirst Branch = "remove-first"
)

// ReplaceOptions is one `kubenest node replace`.
type ReplaceOptions struct {
	// Node names the machine being replaced: the inventory's host ID, its SSH
	// address, or the cluster's Node name.
	Node string
	// With names the machine that replaces it: required. Its address, or a host
	// ID the inventory already knows (an interrupted join leaves one).
	With string
	// ConfirmIsolated is the operator's affirmative answer that a machine that
	// does not answer is POWERED OFF or ISOLATED at the provider, and that it
	// stays that way until it is wiped. It is a confirmation, not a flag read
	// silently: the order that removes first refuses without it.
	ConfirmIsolated bool
	// StorageDevice is the stable path of a blank device to create kubenest-vg
	// on on the machine that joins, exactly as `node add` takes it. Empty means
	// the volume group already exists there.
	StorageDevice string
	// Resume continues an interrupted replace by operation id.
	Resume string
	// TakeOver takes over a replace whose record still says its executor is
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
// rules live: mutually exclusive, and --take-over requires --confirm. The
// composed halves both continue the ONE operation this replace holds.
func (o ReplaceOptions) Recovery() operation.Recovery {
	return operation.Recovery{Resume: o.Resume, TakeOver: o.TakeOver, Confirm: o.Confirm}
}

// replaceState is what a resumed replace carries across processes: the order
// this operation committed to and the machine it is replacing.
//
// It is journalled because the machine's Node object may be GONE by the time a
// resume runs — deleting it is this operation's own work — and because the
// order is a fact about the machine at the moment the operation started, which
// a resume must not re-decide (a machine that answers SSH again is exactly the
// case that must not flip a remove-first operation into an add-first one).
//
// It holds no credential, and it is written MERGED into the journal's state
// document, which the add half it composes also writes (session.mergeState).
type replaceState struct {
	Branch    Branch `json:"replace_branch,omitempty"`
	OldHostID string `json:"replace_old_host_id,omitempty"`
	OldNode   string `json:"replace_old_node,omitempty"`
}

// Replace is `kubenest node replace`: the verb an operator reaches for when a
// machine is gone, and the one where the ORDER carries the safety property.
//
// It composes T5.2's add and T5.3's remove over ONE Session — one journal, one
// operation record, one interlock — and reimplements neither: the machine that
// joins goes through the add's own stages, and the machine that leaves goes
// through the remove's own stages, in the order the branch decides.
type Replace struct {
	*Session
	Opts ReplaceOptions

	// add and remove are those two verbs over this operation's Session. Their
	// stage functions are what this verb runs; only the ordering, the branch and
	// the volume disposition are replace-specific.
	add    *Add
	remove *Remove

	// branch is the order this run committed to, decided once and carried in
	// both the journal and the operation record.
	branch Branch
	// old is the machine being replaced, kept here because the composed halves
	// share one Session: the add half's own resolution points the Session's Host
	// at the machine that JOINS, and every stage of this verb that is about the
	// machine that LEAVES has to say so for itself.
	old api.HostRecord
	// node is that machine's Node object, found once by the reachability stage
	// and kept here for the same reason.
	node ClusterNode
	// volumes is what the machine being replaced holds: read before anything is
	// changed, and the set the restore command covers.
	volumes []BoundLocalVolume
	// contact is the connection to the machine being replaced. For a machine
	// that does not answer it is a runner that says so, which is what makes the
	// removal's uninstall step report it instead of inventing a connection.
	contact Transport
}

// NewReplace wires a replace over the session the command layer built: the two
// composed verbs share it.
func NewReplace(s *Session, opts ReplaceOptions) *Replace {
	r := &Replace{Session: s, Opts: opts}
	r.add = &Add{Session: s, Opts: AddOptions{
		Agent:         opts.With,
		StorageDevice: opts.StorageDevice,
		Resume:        opts.Resume,
		TakeOver:      opts.TakeOver,
		Confirm:       opts.Confirm,
		Wait:          opts.Wait,
		Now:           opts.Now,
	}}
	r.remove = &Remove{Session: s, Opts: RemoveOptions{
		Node:     opts.Node,
		Resume:   opts.Resume,
		TakeOver: opts.TakeOver,
		Confirm:  opts.Confirm,
		Wait:     opts.Wait,
		Now:      opts.Now,
	}}
	return r
}

// unreachable is the machine being replaced when it does not answer SSH, which
// IS the premise of the order that removes first.
//
// It reports no host key rather than a made-up one: CheckFingerprint treats an
// unobserved key as "cannot claim to have verified", which is the truth about a
// machine nobody can reach. Every command asked of it fails, so the removal's
// uninstall step reports the machine as gone the way it does for a host that is
// really gone.
type unreachable struct{ address string }

func (u unreachable) Run(context.Context, string) (sshx.Result, error) {
	return sshx.Result{}, fmt.Errorf("%s did not answer SSH when this replace started, so nothing can be run on it", u.address)
}

func (u unreachable) RunInput(context.Context, string, io.Reader) (sshx.Result, error) {
	return sshx.Result{}, fmt.Errorf("%s did not answer SSH when this replace started, so nothing can be run on it", u.address)
}

func (u unreachable) HostKeyFingerprint() string { return "" }
func (u unreachable) Close() error               { return nil }

// PlanReplace is the stages that decide the order and take the locks. The
// order's own stages are PlanReplaceBranch, because the engine runs one fixed
// sequence and this verb's order is a fact about the machine that is only known
// once the machine has been read.
//
// The OPERATION RECORD (T2.3, the CLI-vs-CLI lock) is taken here, and KURED'S OWN
// LOCK (T5.1) is taken by the half that runs first, in the place `node add` and
// `node remove` already take it: the join's hold the moment its node appears, and
// the removal's hold before it cordons. It cannot be taken here instead — kured's
// lock names a node, and which machine this operation is taking down is exactly
// what the order decides — and one operation holds it once
// (Session.takeInterlock), so the second half does not take it again under the
// other machine's name.
func PlanReplace(r *Replace) []stages.Stage {
	return []stages.Stage{
		{Name: StageReplaceResolve, AlwaysRun: true, Run: r.stageResolve},
		// The window comes before the facts the ORDER depends on, and before the
		// connection to the machine being replaced: a --wait run must wait while
		// holding nothing and having dialled nobody, and it must read whether the
		// machine is there, and what its disks hold, at the moment the window
		// actually opens rather than hours earlier. The machine-identity gates
		// still run first, so an operator outside the window who aimed at a server
		// hears about the server.
		{Name: StageReplaceWindow, AlwaysRun: true, Run: r.stageWindow},
		{Name: StageReplaceReachability, AlwaysRun: true, Run: r.stageReachability},
		{Name: StageReplaceVolumes, AlwaysRun: true, Run: r.stageVolumes},
		{Name: StageReplaceLock, AlwaysRun: true, Run: r.stageLock},
	}
}

// PlanReplaceBranch is the stages of the order this replace committed to: the
// machine that joins first, or the machine that leaves first.
func PlanReplaceBranch(r *Replace) ([]stages.Stage, error) {
	switch r.branch {
	case BranchAddFirst:
		return append(addHalf(r), removeHalf(r)...), nil
	case BranchRemoveFirst:
		// The restore commands come LAST, after the replacement has joined:
		// the fresh volumes must land on a node that is live.
		return append(append(removeHalf(r), addHalf(r)...), restoreCommandsStage(r)), nil
	}
	return nil, fmt.Errorf("this replace has no order yet: the reachability stage decides it, and it has not run. Nothing was changed")
}

// RunReplace drives one `node replace` to the end.
//
// It runs the operation in TWO passes over ONE operation — the same Session,
// the same journal, the same operation record, the same interlock — because the
// order is a fact about the machine and the engine runs a fixed sequence. The
// first pass decides the order and takes the locks; the second is that order,
// under the two verbs' own stage names, so the journal a resume reads names
// what actually ran rather than a slot named for its position.
func RunReplace(ctx context.Context, r *Replace) (stages.Result, error) {
	result, err := stages.Execute(ctx, r, PlanReplace(r))
	if err != nil {
		return result, err
	}
	sequence, err := PlanReplaceBranch(r)
	if err != nil {
		return result, err
	}
	rest, err := stages.Execute(ctx, r, sequence)
	result.Elapsed += rest.Elapsed
	result.Ran = append(result.Ran, rest.Ran...)
	result.Skipped = append(result.Skipped, rest.Skipped...)
	if rest.Paused != "" {
		result.Paused = rest.Paused
	}
	return result, err
}

// state reads what a previous process wrote down about this replace.
func (r *Replace) state() replaceState {
	var st replaceState
	if r.Jnl == nil {
		return st
	}
	// A state document that cannot be decoded leaves the zero state: it is the
	// record of a first run, and refusing here would be refusing work that has
	// not happened.
	_ = r.Jnl.DecodeState(&st)
	return st
}

func (r *Replace) saveState(st replaceState) error { return r.mergeState(st) }

// removalDone reports whether this operation has already deleted the machine's
// Node object. It is the JOURNAL's word and not the cluster's: a Node object
// that is missing because some other removal deleted it is not this operation's
// work, and only the operation's own record may excuse its absence.
func (r *Replace) removalDone() bool {
	if r.Jnl == nil {
		return false
	}
	_, done := r.Jnl.Completed(StageReplaceRemoveDelete)
	return done
}

// stageResolve answers which two machines this replace is about, and that both
// may be what the operator says they are.
//
// BOTH PRE-CONDITIONS RUN HERE, BEFORE ANY SIDE EFFECT: the machine being
// replaced must be an agent (a server is S6's or the ha promotion's, never this
// verb's), the machine that replaces it must be a machine this cluster does not
// already hold. A machine refused here has cost nothing.
func (r *Replace) stageResolve(ctx context.Context) error {
	st := r.state()
	if st.OldHostID != "" {
		// A RESUME: the machine the first run found, by the host ID it wrote
		// down. Its gates are not re-asked — the request they protect is pinned
		// in the operation record, and the host is marked removed exactly
		// because THIS operation removed it — and `--node` may name a Node
		// object the replacement has since deleted.
		if err := r.resolveRecordedHost(ctx, st.OldHostID); err != nil {
			return err
		}
	} else {
		if err := r.remove.ResolveOldHost(ctx); err != nil {
			return err
		}
		if r.Opts.Recovery().ID() == "" {
			if err := r.remove.CheckRemovable(); err != nil {
				return err
			}
		}
	}
	r.old = r.Host
	// The machine that replaces it: `node add`'s own resolution, which reuses
	// or mints its inventory entry, dials it, and picks the Ready server every
	// cluster read then goes through. It runs here, before anything is changed,
	// because a machine the inventory already holds must be refused BEFORE the
	// machine being replaced is touched.
	return r.resolveSpare(ctx)
}

// resolveRecordedHost is the machine a resume already knows, found by the host
// ID this operation recorded rather than by what the operator typed: `--node`
// may name a Node object that no longer exists, and the inventory is the only
// place the machine outlives its Node.
func (r *Replace) resolveRecordedHost(ctx context.Context, hostID string) error {
	if err := r.resolveCluster(ctx); err != nil {
		return err
	}
	host, found := FindHost(r.Hosts, hostID)
	if !found {
		return fmt.Errorf("host %s, which this operation is replacing, is no longer in the cluster's inventory, so a resume cannot tell which machine it is about. Nothing was changed", hostID)
	}
	r.Host = host
	r.Logf("Replacing agent %s (%s) of cluster %s, the machine this operation recorded.", host.HostID, host.SSHAddress, r.Cluster)
	return nil
}

// stageReachability answers whether the machine being replaced is THERE, and
// the answer IS the order (PLAN 7.3).
//
// Reachable means BOTH halves: the machine answers SSH with the host key the
// inventory recorded, and the cluster reports its Node object Ready. SSH alone
// would call a machine reachable that the cluster has already ejected, and
// Ready alone reads a condition that lags the machine's death — the case this
// verb exists for — so neither half is enough on its own.
//
// A machine that does not answer is removed first, and that order is where this
// command stops making the operator's decision for them: it refuses until the
// operator has confirmed, at the provider, that the machine is powered off or
// isolated, naming it by host ID and address. Nothing here fences anything, and
// the refusal says so.
func (r *Replace) stageReachability(ctx context.Context) error {
	host := r.old
	conn, dialErr := r.Dial(ctx, host)
	if dialErr != nil {
		r.Logf("  reachability: %s (%s) did not answer SSH: %v", host.HostID, host.SSHAddress, dialErr)
		r.contact = unreachable{address: host.SSHAddress}
	} else {
		if err := CheckFingerprint(host, conn); err != nil {
			return err
		}
		r.contact = conn
		r.Logf("  reachability: %s (%s) answered SSH with the host key the inventory recorded", host.HostID, host.SSHAddress)
	}

	node, err := r.oldNode(ctx)
	if err != nil {
		return err
	}
	r.node, r.Node = node, node
	if node.UID != "" {
		r.old.NodeUID = node.UID
	}
	if node.Name != "" && !node.Ready {
		r.Logf("  reachability: the cluster reports %s NotReady, so it is not capacity this replace may add behind", node.Name)
	}
	reachable := dialErr == nil && node.Ready

	st := r.state()
	switch {
	case st.Branch != "":
		// A RESUME RE-ENTERS THE ORDER IT STARTED IN, whatever the machine says
		// now: the decision was made when the operation took its locks, and a
		// machine that has answered since does not un-make it.
		r.branch = st.Branch
		r.Logf("  branch:    the %s order this operation committed to, from this operation's own record", r.branch)
	case reachable:
		r.branch = BranchAddFirst
		r.Logf("  branch:    %s is there, so the replacement is added first and the cluster never loses capacity for the workload set", host.SSHAddress)
	default:
		r.branch = BranchRemoveFirst
		r.Logf("  branch:    %s is not there, so it is removed first — etcd's own order, and the only order that works when the old machine may come back", host.SSHAddress)
	}

	if r.branch == BranchRemoveFirst && !r.Opts.ConfirmIsolated {
		return fmt.Errorf(`%s (%s) did not answer, so this replace takes it out of the cluster BEFORE the replacement joins. That order hands the operator a decision the command must not make for them, so it refuses until you have confirmed it: at the provider, check that %s is POWERED OFF or ISOLATED, and confirm that it stays that way until it is wiped.

--confirm-isolated records that answer. Nothing here can tell a powered-off machine from a switched-off one, and nothing here fences anything: deleting the Node object removes it from the cluster, it does not stop the machine coming back, and it does not revoke the credentials already on that host.

Nothing has been changed.`, host.HostID, host.SSHAddress, host.SSHAddress)
	}
	if r.branch == BranchAddFirst && r.Opts.ConfirmIsolated {
		r.Logf("--confirm-isolated: %s answers and is Ready, so this run is add-first and there was nothing to confirm.", host.SSHAddress)
	}

	// The decision is written down only once it is allowed to stand: a refusal
	// above must not commit the operation to an order the operator has not
	// confirmed.
	return r.saveState(replaceState{Branch: r.branch, OldHostID: host.HostID, OldNode: node.Name})
}

// oldNode is the cluster's Node object for the machine being replaced.
//
// A machine whose Node object is GONE is tolerated on a resume whose removal
// already deleted it — that deletion is this operation's own work — and the
// name the removal used comes from the journal, because the object it named no
// longer exists.
func (r *Replace) oldNode(ctx context.Context) (ClusterNode, error) {
	nodes, err := ReadClusterNodes(ctx, r.ServerConn)
	if err != nil {
		return ClusterNode{}, err
	}
	node, err := NodeFor(nodes, r.old)
	if err == nil {
		return node, nil
	}
	if st := r.state(); st.OldNode != "" && r.removalDone() {
		return ClusterNode{Name: st.OldNode}, nil
	}
	return ClusterNode{}, err
}

// stageVolumes reads what the machine being replaced holds, and decides what
// that means for the order.
//
// A REACHABLE machine that holds bound local volumes is refused HERE, BEFORE
// the replacement is added: local volumes live on one machine's disk, so adding
// the replacement first would strand a SECOND set of volumes while the first is
// still unresolved, and the work that moves the data would have to be done
// twice, on two nodes. The refusal prints the planned migration.
//
// An UNREACHABLE machine's volumes block nothing: the data is already beyond
// this cluster's reach, and what is left to do is restore it. It is read here so
// that the restore command can be printed after the replacement joins.
func (r *Replace) stageVolumes(ctx context.Context) error {
	volumes, err := BoundLocalVolumes(ctx, r.ServerConn, r.node.Name)
	if err != nil {
		return err
	}
	r.volumes = volumes
	if len(volumes) == 0 {
		r.Logf("  volumes:   %s holds no bound local volume, so no workload's data is stranded by this replace", r.node.Name)
	} else {
		r.Logf("  volumes:   %s holds %d claim(s) that exist ONLY on that machine's disk", r.node.Name, len(volumes))
		for _, v := range volumes {
			r.Logf("  volume:    %s/%s (volume %s, driver %s) — %s", v.Namespace, v.PVC, v.PV, orNone(v.Driver), orNone(v.Workload))
		}
	}
	if r.branch != BranchAddFirst || len(volumes) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) answers, and its disks hold %d claim(s) that exist ONLY on that machine:\n", r.old.HostID, r.old.SSHAddress, len(volumes))
	for _, v := range volumes {
		fmt.Fprintf(&b, "  %s/%s (volume %s, driver %s) — %s\n", v.Namespace, v.PVC, v.PV, orNone(v.Driver), orNone(v.Workload))
	}
	fmt.Fprintf(&b, "\nThis command refuses BEFORE the replacement is added. Adding it first would strand a SECOND set of volumes while this one is unresolved: a local volume lives on one machine's disk, and moving this data is work that would have to be done twice, on two nodes.\n\nPlanned migration, in this order:\n  1. back the data up:      kubenest backup now --cluster %s\n  2. restore each workload's claims elsewhere, once the workload runs somewhere with room for them:\n", r.Cluster)
	for _, cmd := range RestoreCommands(r.Cluster, volumes) {
		fmt.Fprintf(&b, "       %s\n", cmd)
	}
	fmt.Fprintf(&b, "  3. replace the machine:   kubenest node replace --cluster %s --node %s --with %s [--storage-device DEV]\n\nEvery claim of a workload is in ONE restore command: restoring one claim of a pod while another stays stranded leaves the pod unable to start. Nothing has been added, drained or changed.", r.Cluster, r.Opts.Node, r.Opts.With)
	return fmt.Errorf("%s", b.String())
}

// stageWindow applies the cluster's maintenance window. A --wait run waits here,
// holding nothing and having dialled nobody, so the order and the volume
// disposition are read after it from the cluster as it is then.
func (r *Replace) stageWindow(ctx context.Context) error {
	return r.windowRule(ctx, r.Opts.Now, r.Opts.Wait)
}

// stageLock creates the operation record: the CLI-vs-CLI lock, and the reason a
// replace cannot race an add — both write the same record.
//
// THE REQUEST HOLDS BOTH MACHINES AND BOTH HALVES' VERSIONS. The machine being
// replaced is a target by its inventory identity; the machine that replaces it
// travels as the request's own `with`, because its host ID is minted by this
// operation and does not exist until the join it performs — a resume that minted
// a different ID for the same machine must not be refused over that. The order
// is in the request on purpose (PLAN 7.3): a resume continues the order it
// started in.
func (r *Replace) stageLock(ctx context.Context) error {
	request := operation.Request{
		Kind:    operation.KindNodeReplace,
		Cluster: r.Cluster,
		Targets: []operation.Target{{HostID: r.old.HostID, NodeUID: r.old.NodeUID}},
		Versions: map[string]string{
			"bundle":         r.Record.BundleVersion,
			"k3s":            r.add.k3sVersion(),
			"branch":         string(r.branch),
			"with":           r.Opts.With,
			"storage-device": r.Opts.StorageDevice,
		},
	}
	return r.openRecord(ctx, operation.KindNodeReplace, request, r.Opts.Recovery())
}

// resolveSpare is the resolution of the machine that JOINS, run from a session
// that is not still pointing at the machine that leaves.
//
// THE SESSION'S MACHINE IS CLEARED FIRST, and that is not cosmetic. The two
// halves share one Session: the removal's own resolution left the machine it is
// taking out there, `node add`'s resolution dials whatever the session holds for
// a machine its inventory lookup does not find, and its hold stage acts on
// whatever node the session holds. So a replace would dial the machine it is
// replacing and hold a node that is gone. Clearing both restores the add's own
// meaning — the machine the operator named with --with is dialled, and there is
// no node to hold yet — and the add's resolution fills them in again from its
// own journal when this operation has already joined it.
func (r *Replace) resolveSpare(ctx context.Context) error {
	r.Host = api.HostRecord{}
	r.Node = ClusterNode{}
	return r.add.ResolveNewHost(ctx, r.Opts.Recovery().ID() != "")
}

// stageRemoveResolve is the removal's own resolution, pointed at the machine
// being replaced and given the connection this operation holds: `node remove`
// dials the machine in its resolve, and this verb has already decided what there
// is to reach — which, for the order that removes first, is nothing at all.
func (r *Replace) stageRemoveResolve(ctx context.Context) error {
	// The re-pointing is not optional, and it runs on every attempt: the stages
	// of this half read the machine they act on from the shared session, and the
	// half that ran before this one — the add's, whose resolution points the
	// session at the machine that JOINS — has left its own machine there.
	r.Host, r.Node = r.old, r.node
	if r.removalDone() {
		// The Node object is already deleted — this operation deleted it — so
		// there is nothing to attach to: what remains of the removal is its
		// record write, which needs the inventory entry and no connection.
		return nil
	}
	return r.remove.ResolveAccess(ctx, r.contact)
}

// stageRemoveHold is the removal's hold, skipped when this operation has already
// deleted the machine's Node object: there is nothing left to hold, and
// labelling a Node that no longer exists is a failure about a machine that is
// already out of the cluster.
func (r *Replace) stageRemoveHold(ctx context.Context) error {
	if r.removalDone() {
		return nil
	}
	return r.remove.stageHold(ctx)
}

// stageRestoreCommands prints ONE restore command per workload whose claims were
// stranded on the machine that is gone.
//
// It runs AFTER the replacement has joined, because the restored volumes must
// land on a node that is live. It claims nothing about what happened to the
// data: everything written on that machine since its last backup's capture
// began is gone, and the printer says so rather than leaving the operator to
// infer it.
func (r *Replace) stageRestoreCommands(ctx context.Context) error {
	if len(r.volumes) == 0 {
		r.Logf("Nothing to restore: %s held no bound local volume when this replace started.", r.Opts.Node)
		return nil
	}
	commands := RestoreCommands(r.Cluster, r.volumes)
	r.Logf("The machine that is gone held %d claim(s) that exist ONLY on its disk, so each of these workloads' data comes back from its last eligible backup, onto live nodes:", len(r.volumes))
	for _, cmd := range commands {
		r.Logf("  %s", cmd)
	}
	r.Logf("Run each command now that the replacement has joined. Anything written on %s after its last backup's capture began is gone; a data loss window shorter than that needs a more frequent Velero schedule for the namespace.", r.old.SSHAddress)
	return nil
}

// restoreCommandsStage is the restore printer as a stage of the order that
// removes first. It always runs: a resume must print the commands in front of
// the operator rather than assume they still have them, and printing changes
// nothing.
func restoreCommandsStage(r *Replace) stages.Stage {
	return stages.Stage{Name: StageReplaceRestoreCommands, AlwaysRun: true, Run: r.stageRestoreCommands}
}

// addHalf is T5.2's stages for the machine that joins, relabelled for this
// journal: ONE journal cannot hold two stages called `resolve` or `record`, and
// the label is what a resume reads.
//
// The window and the add's own operation record are left out: this verb applies
// the window once for both halves, and holds ONE record — a second one would be
// the race this verb exists to prevent.
func addHalf(r *Replace) []stages.Stage {
	plan := PlanAdd(r.add)
	out := make([]stages.Stage, 0, len(plan))
	for _, s := range plan {
		switch s.Name {
		case StageAddResolve:
			// The add's own resolution, run through this verb's clearing
			// wrapper: resolveSpare says why the session is cleared first.
			s.Name, s.Run = StageReplaceAddResolve, r.resolveSpare
		case StageAddPreflight:
			s.Name = StageReplaceAddPreflight
		case StageAddJoining:
			s.Name = StageReplaceAddJoining
		case StageAddHold:
			s.Name = StageReplaceAddHold
		case StageAddJoin:
			s.Name = StageReplaceAddJoin
		case StageAddStorage:
			s.Name = StageReplaceAddStorage
		case StageAddRecord:
			s.Name = StageReplaceAddRecord
		default:
			continue
		}
		out = append(out, s)
	}
	return out
}

// removeHalf is T5.3's stages for the machine that leaves, relabelled the same
// way and for the same reason.
//
// The volume disposition is left out — that decision is this verb's own
// precondition, because a reachable machine that holds local volumes is refused
// before anything is added rather than after — and the removal's own window and
// record, for the reasons above.
func removeHalf(r *Replace) []stages.Stage {
	plan := PlanRemove(r.remove)
	out := make([]stages.Stage, 0, len(plan))
	for _, s := range plan {
		switch s.Name {
		case StageRemoveResolve:
			s.Name, s.Run = StageReplaceRemoveResolve, r.stageRemoveResolve
		case StageRemoveHold:
			s.Name, s.Run = StageReplaceRemoveHold, r.stageRemoveHold
		case StageRemoveRemoving:
			s.Name = StageReplaceRemoveRemoving
		case StageRemoveCordon:
			s.Name = StageReplaceRemoveCordon
		case StageRemoveDrain:
			s.Name = StageReplaceRemoveDrain
		case StageRemoveUninstall:
			s.Name = StageReplaceRemoveUninstall
		case StageRemoveDelete:
			s.Name = StageReplaceRemoveDelete
		case StageRemoveRecord:
			s.Name = StageReplaceRemoveRecord
		default:
			continue
		}
		out = append(out, s)
	}
	return out
}

// Finish closes a `node replace` the way the run ended, and states the
// bookkeeping — and only the bookkeeping — that the removal leaves behind.
//
// A MACHINE THAT JOINED BUT IS NOT RECORDED AS ACTIVE KEEPS THE HOLD, exactly as
// `node add` keeps it: kured must not reboot a node whose inventory entry does
// not say it is active, and the hold is lifted only by a run that finishes the
// record. Saying so is the difference between a failed replace and a mystery
// about a node nobody may reboot.
func (r *Replace) Finish(ctx context.Context, runErr error, interrupted bool) {
	if runErr != nil {
		r.finishOutstandingHold(ctx)
	}
	if runErr == nil {
		r.Logf("  bookkeeping: the machine that was replaced is recorded removed in the cluster's inventory, with its host ID and host key, so it is not re-added by accident. That is bookkeeping, not fencing: deleting its Node object did not stop the machine coming back, and it did not revoke the credentials already on that host. A returning, unwiped machine is NOT automatically refused.")
	}
	ttl, err := r.lockTTL()
	if err != nil {
		ttl = 0
	}
	r.releaseInterlock(ctx, r.interlockNode, ttl)
	r.finishRecord(ctx, runErr, interrupted)
	r.Close()
}

// finishOutstandingHold reports a hold that a failed run leaves on the machine
// that joined, so the next operator knows why a node is not being rebooted.
func (r *Replace) finishOutstandingHold(ctx context.Context) {
	st := r.add.stageState()
	if st.NodeName == "" {
		return
	}
	record, err := r.Records.Load(ctx)
	if err != nil {
		return
	}
	if host, found := heldHost(record.Hosts, r.Opts.With); found && LifecycleState(host.LifecycleState) == StateActive {
		return
	}
	r.Logf("  hold:      %s keeps %s=%s: it is not recorded as active yet, so kured must not reboot it. Re-run this command to finish the record",
		st.NodeName, day2.NoAutoRebootLabel, day2.NoAutoRebootValue)
}

// unreachable stands in for the connection this verb does not have, so it is
// held to the same interface as one.
var _ Transport = unreachable{}
