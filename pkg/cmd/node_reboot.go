package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/interlock"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/node"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/upgrade"
	"kubenest.io/cli/pkg/window"
)

// NodeRebootFlags is the flag surface of `kubenest node reboot` (PLAN 7.3).
//
// It is exported because the e2e gate drives the verb through the same entry
// point the command does: a gate that rebuilt the flag surface of its own would
// accept a command the CLI does not have.
type NodeRebootFlags struct {
	// Cluster is the cluster's NAME, as recorded at install.
	Cluster string
	// Node names the machine: a cluster node name, the SSH address the
	// inventory records, or a host ID from that inventory.
	Node string
	// Confirm is the confirmation. Without it the plan is printed and nothing
	// is touched.
	Confirm bool
	// Wait holds until the maintenance window opens, holding nothing while it
	// waits (7.2), then takes the operation record and re-checks every gate.
	Wait bool
	// Now is --now: it bypasses the maintenance window and NOTHING ELSE.
	Now bool
	// K3sOnly restarts the k3s service instead of rebooting the host.
	K3sOnly bool
	// SSHUser and SSHKey override how this laptop AUTHENTICATES to the address
	// the inventory records. They never decide WHICH machine is acted on.
	SSHUser string
	SSHKey  string
	// Resume continues an interrupted reboot by operation id.
	Resume string
}

// validate refuses the flag combinations before anything is read.
func (f NodeRebootFlags) validate() error {
	if f.Wait && f.Now {
		return fmt.Errorf("--wait and --now are mutually exclusive: --wait holds for the maintenance window, --now acts immediately and bypasses it")
	}
	if f.Wait && !f.Confirm {
		return fmt.Errorf("--wait without --confirm would hold for the maintenance window and then do nothing: pass --confirm with --wait")
	}
	return nil
}

// The three steps the CLI observes from OUTSIDE the node (PLAN 7.3). Each is
// named, because a timeout has to say WHICH step did not happen: "the reboot
// timed out" tells an operator nothing about whether the machine is on the
// network or whether k3s came back.
const (
	waitStepSSH     = "SSH reachable"
	waitStepService = "the k3s service active"
	waitStepAPI     = "the cluster API answering with the node Ready"
)

// The names of the checks that run whatever the window says (7.4 item 3).
const (
	gateQuorum        = "Quorum"
	gateStorage       = "Storage"
	gateRecoveryPoint = "Recovery point"
)

// nodeTransport is one open SSH connection to a host, with the host key the
// handshake negotiated (so the inventory's fingerprint can be re-checked) and
// the ability to be closed and redialled — a reboot kills the connection.
//
// *sshx.Client satisfies it.
type nodeTransport interface {
	k3s.Runner
	HostKeyFingerprint() string
	Close() error
}

// rebootGate is one pre-flight check. Check reports whether it passed, what it
// observed, and what to do when it did not; it must not change anything,
// because the operation record is created before the first side effect and a
// gate runs before the record.
type rebootGate struct {
	Name  string
	Check func(ctx context.Context) (passed bool, detail, fix string)
}

// gateResult is one gate's verdict in the run's own words.
type gateResult struct {
	Name   string
	Passed bool
	Detail string
	Fix    string
}

// nodeReboot is one `kubenest node reboot` run: the request, the seams a test
// drives, and the cluster facts the run resolves.
type nodeReboot struct {
	out    io.Writer
	f      NodeRebootFlags
	client *api.Client

	// now, sleep and poll are the run's clock. The outside wait is measured on
	// it, so a test measures a twenty-minute timeout instead of enduring it.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	poll  time.Duration

	// dial opens a fresh SSH connection to a host. Every observation after the
	// reboot goes through it again, because the connection that issued the
	// reboot is gone with the machine.
	dial func(ctx context.Context, host api.HostRecord) (nodeTransport, error)

	// gates are the checks that still run under --now. They are injectable for
	// the same reason the dialer is: what --now must NOT bypass is the whole
	// claim of the flag, and a test has to be able to make one refuse.
	gates []rebootGate

	// store builds the operation record's store. A test replaces it to keep the
	// record out of the fake runner, or to inspect it.
	store func(runner k3s.Runner) *operation.Store

	// resolved by run
	records   upgrade.ControlPlaneRecords
	bundle    *manifest.Manifest
	window    *window.Window
	windowErr error
	hosts     []api.HostRecord
	host      api.HostRecord
	server    api.HostRecord
	servers   int
	node      clusterNode
	nodeCount int
	ttl       time.Duration
	drainFor  time.Duration
	rebootFor time.Duration
	clusterID string

	serverConn nodeTransport
	target     nodeTransport

	store0 *operation.Store
	handle *operation.Handle
	skip   map[string]bool

	// weHeld records whether THIS run placed the reboot hold, so it lifts only
	// what it put there. Servers carry the hold permanently.
	weHeld bool

	// disruptID is the recorded identity of the reboot/k3s-restart step, so the
	// wait can finish it once the node is back. disruptDone is set when a
	// resume had already carried the step out.
	disruptID   string
	disruptDone bool
}

// runNodeReboot is the entry point the command and the e2e gate both call.
func runNodeReboot(ctx context.Context, out io.Writer, client *api.Client, f NodeRebootFlags) error {
	n := &nodeReboot{
		out:    out,
		f:      f,
		client: client,
		now:    func() time.Time { return time.Now().UTC() },
		sleep: func(ctx context.Context, d time.Duration) error {
			if d <= 0 {
				return ctx.Err()
			}
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
		poll: 5 * time.Second,
		dial: func(ctx context.Context, host api.HostRecord) (nodeTransport, error) {
			return dialNode(ctx, host, f.SSHUser, f.SSHKey)
		},
	}
	// The store is built lazily because the mirror is addressed by the
	// control plane's cluster ID, which is resolved on the way in. A mirror
	// without one would fail on every write and report it: the record in the
	// cluster is the lock, but a mirror that cannot be read is a record the
	// console cannot show.
	n.store = func(runner k3s.Runner) *operation.Store {
		return &operation.Store{
			Runner:          runner,
			Operator:        upgrade.OperatorName(),
			Mirror:          client,
			MirrorClusterID: n.clusterID,
		}
	}
	return n.run(ctx)
}

// dialNode opens one SSH connection to an inventory host.
//
// The address, port and user come from the RECORD (T5.0), with --ssh-user as
// the only override: a node verb that took its target from a local journal
// would act on whatever machine the laptop that ran the install happened to
// know about.
func dialNode(ctx context.Context, host api.HostRecord, user, key string) (nodeTransport, error) {
	if host.SSHAddress == "" {
		return nil, fmt.Errorf("the cluster's inventory records no SSH address for host %s", host.HostID)
	}
	if user == "" {
		user = host.SSHUser
	}
	opts := sshx.Options{User: user, KeyPath: key, Port: host.SSHPort}
	ep, err := sshx.Resolve(host.SSHAddress, opts)
	if err != nil {
		return nil, err
	}
	if host.SSHPort != 0 {
		ep.Port = host.SSHPort
	}
	client, err := sshx.Dial(ctx, ep, opts)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// run is the whole verb, in the order PLAN 7.3 and T5.1 fix:
//
//	the window   refused outside it (naming the next opening in local time and
//	             UTC), or held for with --wait, which holds NOTHING
//	the re-checks the host-key fingerprint and the Node UID, against the
//	             cluster as it is now
//	the gates    quorum, storage and recovery point — they run under --now too
//	the plan     printed, and an unconfirmed call stops here
//	the record   the record-as-lock, before the first side effect (T2.3)
//	the hold +   the node is held out of kured's pool, then kured's own lock is
//	the interlock taken (T5.1, T3.3) — the hold comes first because the hold
//	             helper releases a lock the held node owns, and that lock is
//	             about to be ours
//	the work     cordon, drain, reboot, wait from outside, uncordon
//	the release  the interlock, then the hold, then the terminal record
func (n *nodeReboot) run(ctx context.Context) error {
	if err := n.f.validate(); err != nil {
		return err
	}

	clusterID, err := resolveCluster(ctx, n.client, n.f.Cluster)
	if err != nil {
		return err
	}
	n.clusterID = clusterID
	n.records = upgrade.ControlPlaneRecords{Client: n.client, ClusterID: clusterID}
	// The inventory is read through pkg/upgrade's resolver so an absent one is
	// refused in the ONE wording that already exists for it: "assume" is never
	// one of the answers, because a node verb that guessed its machines takes
	// down the wrong one.
	inventory, err := upgrade.ResolveHosts(ctx, n.records)
	if err != nil {
		return err
	}
	n.hosts = inventory.Hosts
	recorded, err := n.records.Load(ctx)
	if err != nil {
		return err
	}
	n.bundle, err = fetchManifest(ctx, n.client, recorded.BundleVersion)
	if err != nil {
		return err
	}
	n.window, n.windowErr = n.records.Window(ctx)

	n.servers = 0
	for _, h := range n.hosts {
		if node.Role(h.Role) == node.RoleServer && node.LifecycleState(h.LifecycleState) != node.StateRemoved {
			n.servers++
		}
	}
	if n.ttl, err = interlock.LockTTLFor(n.bundle); err != nil {
		return err
	}
	if n.drainFor, err = n.bundle.Limits.Timeouts.For("node-drain"); err != nil {
		return err
	}
	if n.rebootFor, err = n.bundle.Limits.Timeouts.For("node-reboot"); err != nil {
		return err
	}
	if n.host, err = n.resolveHost(); err != nil {
		return err
	}
	if n.gates == nil {
		n.gates = []rebootGate{
			{Name: gateQuorum, Check: n.checkQuorum},
			{Name: gateStorage, Check: n.checkStorage},
			{Name: gateRecoveryPoint, Check: n.checkRecoveryPoint},
		}
	}

	if err := n.windowRule(ctx); err != nil {
		return err
	}
	defer n.closeAll()

	if err := n.connect(ctx); err != nil {
		return err
	}
	gates, gateErr := n.runGates(ctx)
	n.printPlan(gates)
	if gateErr != nil {
		return gateErr
	}
	if !n.f.Confirm {
		return fmt.Errorf("nothing has been changed: pass --confirm to %s node %s of cluster %s",
			n.disruptVerb(), n.node.Name, n.f.Cluster)
	}

	// The record exists before the first side effect: the hold label and the
	// kured lock below are both changes, and neither may happen with nothing
	// written down that a second laptop could resume from.
	if err := n.openRecord(ctx); err != nil {
		return err
	}
	runErr := n.act(ctx)
	n.finish(ctx, runErr)
	return runErr
}

// windowRule applies the maintenance window (7.4 item 3).
//
// --now bypasses THE WINDOW ONLY, and says so. --wait holds nothing until the
// window opens; the wait is pkg/upgrade's, so this verb cannot drift from the
// rule the upgrade and the control-plane upgrade follow.
func (n *nodeReboot) windowRule(ctx context.Context) error {
	if n.f.Now {
		fmt.Fprintf(n.out, "--now: the maintenance window is bypassed for this run. Every other check below still runs.\n")
		return nil
	}
	holder := &upgrade.Session{
		Opts:      upgrade.Options{Now: n.now, Sleep: n.sleep},
		Window:    n.window,
		WindowErr: n.windowErr,
		Out:       n.out,
	}
	if n.f.Wait {
		return holder.WaitForWindow(ctx, n.out)
	}
	switch {
	case n.windowErr != nil:
		return fmt.Errorf("the cluster's maintenance window could not be read, so whether now is inside it is unknown: %w", n.windowErr)
	case n.window == nil:
		return fmt.Errorf("%s: %s", window.NoWindow, window.NoWindowFix)
	}
	if err := n.window.Outside(n.now()); err != nil {
		return fmt.Errorf("%s\n%s", err.Error(), window.OutsideFix)
	}
	fmt.Fprintf(n.out, "Inside %s (now %s).\n", n.window, n.window.Moment(n.now()))
	return nil
}

// resolveHost picks the one inventory entry this run acts on.
//
// A node verb addresses a MACHINE, and the inventory is the only place this
// CLI can learn which machine a name refers to. The value may be the host ID
// the record minted, the SSH address it reaches the host at, the address the
// host joined through, or the Node UID the cluster's own object carries.
func (n *nodeReboot) resolveHost() (api.HostRecord, error) {
	var matches []api.HostRecord
	for _, h := range n.hosts {
		if node.LifecycleState(h.LifecycleState) == node.StateRemoved {
			continue
		}
		switch {
		case h.HostID == n.f.Node, h.SSHAddress == n.f.Node, h.JoinAddress == n.f.Node:
			matches = append(matches, h)
		case h.NodeUID != "" && h.NodeUID == n.f.Node:
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return api.HostRecord{}, fmt.Errorf("no host in cluster %q's inventory is %q, so there is no machine to act on. The inventory holds: %s",
			n.f.Cluster, n.f.Node, describeHosts(n.hosts))
	default:
		return api.HostRecord{}, fmt.Errorf("%q names %d hosts of cluster %q, which should be impossible: host IDs are unique. Name one by its host ID: %s",
			n.f.Node, len(matches), n.f.Cluster, describeHosts(matches))
	}
}

func describeHosts(hosts []api.HostRecord) string {
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		parts = append(parts, fmt.Sprintf("%s (%s, role %s, %s)", h.HostID, h.SSHAddress, h.Role, h.LifecycleState))
	}
	return strings.Join(parts, "; ")
}

// clusterNode is one Node object, as much of it as this verb reads.
type clusterNode struct {
	Name          string
	UID           string
	Addresses     []string
	Labels        map[string]string
	Annotations   map[string]string
	Ready         bool
	Unschedulable bool
}

// readClusterNodes reads every Node object once.
func readClusterNodes(ctx context.Context, r k3s.Runner) ([]clusterNode, error) {
	out, err := k3s.Kubectl(ctx, r, "get nodes -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's nodes: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				UID         string            `json:"uid"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Addresses []struct {
					Type    string `json:"type"`
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
		return nil, fmt.Errorf("parsing `kubectl get nodes -o json`: %w", err)
	}
	nodes := make([]clusterNode, 0, len(list.Items))
	for _, item := range list.Items {
		c := clusterNode{
			Name:          item.Metadata.Name,
			UID:           item.Metadata.UID,
			Labels:        item.Metadata.Labels,
			Annotations:   item.Metadata.Annotations,
			Unschedulable: item.Spec.Unschedulable,
		}
		for _, a := range item.Status.Addresses {
			if a.Address != "" {
				c.Addresses = append(c.Addresses, a.Address)
			}
		}
		for _, cond := range item.Status.Conditions {
			if cond.Type == "Ready" {
				c.Ready = cond.Status == "True"
			}
		}
		nodes = append(nodes, c)
	}
	return nodes, nil
}

// connect opens the connections the run needs and resolves the target's Node
// object, re-checking the Node UID and the host-key fingerprint BEFORE anything
// is changed.
func (n *nodeReboot) connect(ctx context.Context) error {
	n.server = n.pickServer()
	conn, err := n.dial(ctx, n.server)
	if err != nil {
		return fmt.Errorf("connecting to the cluster's server %s: %w", n.server.SSHAddress, err)
	}
	n.serverConn = conn
	if err := n.checkFingerprint(n.server, conn); err != nil {
		return err
	}
	nodes, err := readClusterNodes(ctx, conn)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("cluster %q reports no nodes, so there is nothing to reboot", n.f.Cluster)
	}
	n.nodeCount = len(nodes)
	if n.node, err = n.resolveNode(nodes); err != nil {
		return err
	}
	if n.sameHost(n.server, n.host) {
		// One machine: the server the CLI reads the cluster through IS the
		// machine being rebooted. One connection serves both, and the wait
		// re-dials it because the reboot takes it down.
		n.target = conn
		return nil
	}
	target, err := n.dial(ctx, n.host)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", n.host.SSHAddress, err)
	}
	n.target = target
	return n.checkFingerprint(n.host, target)
}

// pickServer chooses the server every cluster read goes through: the target
// itself when it is a server, otherwise the first active server in the
// inventory.
func (n *nodeReboot) pickServer() api.HostRecord {
	if node.Role(n.host.Role) == node.RoleServer {
		return n.host
	}
	for _, h := range n.hosts {
		if node.Role(h.Role) == node.RoleServer && node.LifecycleState(h.LifecycleState) == node.StateActive {
			return h
		}
	}
	return n.host
}

func (n *nodeReboot) sameHost(a, b api.HostRecord) bool {
	return a.HostID != "" && a.HostID == b.HostID
}

// resolveNode finds the target's Node object.
//
// By the name the operator gave, then by the Node UID the cluster's inventory
// recorded (kn-t50) — never by position or by count. A host that is in the
// inventory but is not a node of this cluster is refused with the addresses the
// cluster does know, because rebooting the wrong machine is not a recoverable
// mistake.
func (n *nodeReboot) resolveNode(nodes []clusterNode) (clusterNode, error) {
	for _, c := range nodes {
		if c.Name == n.f.Node {
			return n.afterUIDCheck(c)
		}
	}
	if n.host.NodeUID != "" {
		for _, c := range nodes {
			if c.UID == n.host.NodeUID {
				return n.afterUIDCheck(c)
			}
		}
	}
	for _, c := range nodes {
		for _, a := range c.Addresses {
			if a == n.host.SSHAddress || (n.host.JoinAddress != "" && a == n.host.JoinAddress) {
				return n.afterUIDCheck(c)
			}
		}
	}
	var known []string
	for _, c := range nodes {
		known = append(known, c.Name+" ("+strings.Join(c.Addresses, ", ")+")")
	}
	return clusterNode{}, fmt.Errorf("host %s (%s) is in cluster %q's inventory but is not one of its nodes, so there is no Node object to cordon and drain. The cluster's nodes are: %s",
		n.host.HostID, n.host.SSHAddress, n.f.Cluster, strings.Join(known, "; "))
}

// afterUIDCheck re-checks the Node UID the inventory recorded.
//
// A MISMATCH IS A REFUSAL, NOT A REPAIR. The UID is how a rebuilt, renamed or
// re-joined host is told apart from the machine the record describes: acting on
// the new Node object with the old record's assumptions is how a node verb
// destroys a machine nobody asked about. An EMPTY recorded UID is the honest
// "the installer could not read it" (install/plan.go records it that way), so
// the node is matched by address and the run says the UID was not verified
// rather than claiming it was.
func (n *nodeReboot) afterUIDCheck(c clusterNode) (clusterNode, error) {
	switch {
	case n.host.NodeUID == "":
		fmt.Fprintf(n.out, "  note:      the inventory records no Node UID for %s; it was matched by address and its UID %s is what this operation records\n",
			n.host.HostID, c.UID)
	case n.host.NodeUID != c.UID:
		return clusterNode{}, fmt.Errorf(
			"the cluster's inventory records Node UID %s for host %s (%s), and the cluster's node %s has UID %s: this is a DIFFERENT Node object (the host was rebuilt, re-joined or renamed), so this command refuses rather than taking down a machine the record does not describe. If the host really is this cluster's, re-run the install's record stage so the inventory describes it",
			n.host.NodeUID, n.host.HostID, n.host.SSHAddress, c.Name, c.UID)
	}
	return c, nil
}

// checkFingerprint re-checks the host key the live connection negotiated
// against the one the inventory recorded when the host joined.
//
// A machine that answers on a recorded address with a different host key is not
// the machine the record describes — a rebuilt host, a reused address, or
// something in the middle. An empty fingerprint on either side is "not
// recorded"/"not observed" and is not a match this command can claim.
func (n *nodeReboot) checkFingerprint(host api.HostRecord, conn nodeTransport) error {
	recorded := host.HostKeyFingerprint
	observed := conn.HostKeyFingerprint()
	if recorded == "" || observed == "" {
		return nil
	}
	if recorded != observed {
		return fmt.Errorf("the host at %s presented host key %s, and the cluster's inventory recorded %s for host %s when it joined: this is not the machine the record describes, so nothing was changed. If the host was rebuilt, re-run the install's record stage",
			host.SSHAddress, observed, recorded, host.HostID)
	}
	return nil
}

// runGates runs every check that must run whatever the window says.
func (n *nodeReboot) runGates(ctx context.Context) ([]gateResult, error) {
	results := make([]gateResult, 0, len(n.gates))
	var failed []gateResult
	for _, g := range n.gates {
		passed, detail, fix := g.Check(ctx)
		r := gateResult{Name: g.Name, Passed: passed, Detail: detail, Fix: fix}
		results = append(results, r)
		if !passed {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return results, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the reboot was refused (%d of %d checks failed). Nothing has been changed:", len(failed), len(results))
	for _, r := range failed {
		fmt.Fprintf(&b, "\n  [fail] %s: %s", r.Name, r.Detail)
		if r.Fix != "" {
			fmt.Fprintf(&b, "\n      fix: %s", r.Fix)
		}
	}
	return results, fmt.Errorf("%s", b.String())
}

// checkQuorum refuses to take a server down when the servers that remain cannot
// answer.
//
// It reads the cluster's own inventory rather than Ready Node counts, for the
// reason PLAN 7.3.1 gives for the etcd rule: what keeps the API answering is
// how many voting members are left, and a node that is Ready is not the same
// statement as a member that votes. A single-server cluster has no quorum to
// lose — that is exactly why its reboot is waited for from OUTSIDE — and an
// agent changes no vote at all.
func (n *nodeReboot) checkQuorum(_ context.Context) (bool, string, string) {
	if node.Role(n.host.Role) != node.RoleServer {
		return true, fmt.Sprintf("this is an agent, so rebooting it does not change how many servers vote (%d server(s) in the inventory)", n.servers), ""
	}
	if n.servers <= 1 {
		return true, "this cluster has one server and this node is it: there is no quorum to keep, which is why this reboot is waited for from outside", ""
	}
	remaining, needed := n.servers-1, n.servers/2+1
	if remaining < needed {
		return false,
			fmt.Sprintf("this cluster has %d servers; rebooting this one leaves %d voting member(s) where %d are needed for a quorum, so the API would stop answering", n.servers, remaining, needed),
			"bring the missing server back before rebooting another. Adding a server is the `ha` promotion's (bundle 1.3); until then a cluster with fewer servers than it was built with is one boot away from losing its API"
	}
	return true, fmt.Sprintf("%d of %d server(s) keep voting while this one is down, which is a quorum", remaining, n.servers), ""
}

// storageDevicePath is what a storage device recorded in the inventory may look
// like before it is put in a shell command. The inventory is written by this
// CLI, but it is read back from the control plane and reaches a shell on the
// host: a value carrying shell syntax would run as several commands, with sudo.
var storageDevicePath = regexp.MustCompile(`^/dev/[A-Za-z0-9._/+-]+$`)

// checkStorage refuses a node whose local storage is not in a state a reboot
// returns from.
//
// Two failures, both observed rather than assumed: the device the inventory
// records for this host is gone (its local volumes would come back empty), and
// the filesystem holding /var/lib is mounted read-only (a failing disk, a full
// device — a reboot of a node in that state is how a patch night becomes an
// unbootable node). Free space is REPORTED rather than thresholded: the bundle
// has no reboot-specific floor, and inventing one here would be a number
// nobody measured.
func (n *nodeReboot) checkStorage(ctx context.Context) (bool, string, string) {
	if dev := n.host.StorageDevice; dev != "" {
		if !storageDevicePath.MatchString(dev) {
			return false,
				fmt.Sprintf("the inventory records storage device %q for this host, which is not a plain /dev path, so this command will not put it in a shell command", dev),
				"re-run the install's record stage so the inventory holds the stable /dev/disk/by-id/... path, or clear it if this host has no KubeNest-managed volume group"
		}
		res, err := n.target.Run(ctx, "sudo -n test -b "+dev)
		if err != nil {
			return false, "the storage device the inventory records could not be checked: " + err.Error(),
				"the node must be reachable over SSH to be rebooted safely; check the connection and re-run"
		}
		if res.ExitCode != 0 {
			return false,
				fmt.Sprintf("%s is not a block device on this host: the local volumes that live on it would come back empty, and this reboot would turn a storage failure into data loss", dev),
				"restore the device (the host may have been rebuilt with different disks) before rebooting it"
		}
	}

	mounted, err := n.target.Run(ctx, "findmnt -no OPTIONS --target /var/lib")
	if err != nil || mounted.ExitCode != 0 {
		detail := "the filesystem holding /var/lib could not be inspected"
		if err != nil {
			detail += ": " + err.Error()
		} else {
			detail += fmt.Sprintf(" (findmnt exited %d)", mounted.ExitCode)
		}
		return false, detail, "a node whose datastore filesystem cannot be read is not a node to reboot: check the host, then re-run"
	}
	if readOnly(strings.TrimSpace(mounted.Stdout)) {
		return false,
			fmt.Sprintf("the filesystem holding /var/lib is mounted read-only (%s): the datastore cannot be written", strings.TrimSpace(mounted.Stdout)),
			"this is what a failed disk or a full device looks like. Repair the filesystem before rebooting: a reboot does not clear it, and the node may not come back"
	}

	res, err := n.target.Run(ctx, "df -B1 -P /var/lib | awk 'NR==2{print $4}'")
	if err != nil || res.ExitCode != 0 {
		return false, "the free space on /var/lib could not be measured",
			"a node whose storage cannot be measured is not a node to reboot: check the host, then re-run"
	}
	return true, fmt.Sprintf("the filesystem holding /var/lib is writable with %s free%s", humanBytes(strings.TrimSpace(res.Stdout)), recordedDeviceNote(n.host.StorageDevice)), ""
}

// readOnly reports whether a mount option list contains the read-only flag as
// its own option. A substring test would read "rw,relatime" as read-only on the
// strength of the "ro" in nothing at all — but it would read a hypothetical
// "rootcontext=..." option as read-only, which is why this is compared option
// by option.
func readOnly(options string) bool {
	for _, opt := range strings.Split(options, ",") {
		if strings.TrimSpace(opt) == "ro" {
			return true
		}
	}
	return false
}

func recordedDeviceNote(device string) string {
	if device == "" {
		return ""
	}
	return ", and the recorded device " + device + " is present"
}

func humanBytes(s string) string {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return s
	}
	return manifest.Quantity(n).String()
}

// checkRecoveryPoint refuses a reboot that would leave the cluster with no
// recovery point inside the bundle's allowance.
//
// WHAT IT PROTECTS is the recovery point the reboot could make stale: a reboot
// is the moment a node's local volumes are actually at risk, and "anything
// written since the last backup is gone" is the honest consequence PLAN 7.5
// states. So the newest COMPLETED workload backup must be younger than
// health.backup.max-backup-age.
//
// A CLUSTER WITH NO BACKUP TARGET IS NOT REFUSED, and that is not an absence
// read as a pass: such a cluster has no recovery point at all for a reboot to
// make stale (the installer says so in as many words, and every heartbeat
// reports `backup: unconfigured`). The fleet-health path is what reports that
// state; refusing every reboot of an unconfigured cluster would block the
// patching this verb exists for. A read that FAILS is refused, because an
// unread gate is not a passed gate.
func (n *nodeReboot) checkRecoveryPoint(ctx context.Context) (bool, string, string) {
	maxAge := n.bundle.Health.Backup.MaxBackupAge.Duration()
	if maxAge <= 0 {
		return false, "the bundle manifest carries no health.backup.max-backup-age, so the freshness a recovery point must have cannot be judged",
			"the bundle decides this threshold — a default in code would be a number nobody measured"
	}
	unconfigured, err := backup.Unconfigured(ctx, n.serverConn)
	switch {
	case err != nil:
		return false, "whether this cluster has a backup target could not be read, so whether a recovery point exists is unknown: " + err.Error(),
			"an unread check is not a passed check: fix the cluster read, then re-run"
	case unconfigured:
		return true, "this cluster has no backup target configured, so it has no recovery point a reboot could make stale (`kubenest backup set-target` is what gives it one)", ""
	}

	out, err := k3s.Kubectl(ctx, n.serverConn, "get backups.velero.io -n "+backup.Namespace+" -o json")
	if err != nil {
		return false, "the cluster's workload backups could not be read: " + err.Error(),
			"the recovery point this reboot could invalidate has to be readable before it is relied on: fix the read, then re-run"
	}
	var backups struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase               string `json:"phase"`
				CompletionTimestamp string `json:"completionTimestamp"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &backups); err != nil {
		return false, "the cluster's workload backups did not parse: " + err.Error(),
			"fix the read, then re-run"
	}
	newest := time.Time{}
	newestName := ""
	for _, b := range backups.Items {
		if b.Status.Phase != "Completed" {
			continue
		}
		stamp := b.Status.CompletionTimestamp
		if stamp == "" {
			stamp = b.Metadata.CreationTimestamp
		}
		at, err := time.Parse(time.RFC3339, stamp)
		if err != nil {
			continue
		}
		if at.After(newest) {
			newest, newestName = at, b.Metadata.Name
		}
	}
	if newest.IsZero() {
		return false,
			"this cluster has a backup target and NO completed workload backup, so a reboot now would risk data with nothing to restore from",
			"take one with `kubenest backup now` before rebooting"
	}
	age := n.now().Sub(newest)
	if age > maxAge {
		return false,
			fmt.Sprintf("the newest completed backup %s finished %s ago, older than the %s this bundle allows", newestName, age.Round(time.Minute), maxAge),
			fmt.Sprintf("take a fresh backup with `kubenest backup now`: a reboot is the moment this node's local volumes are at risk, and a recovery point older than %s is not the one the platform promised", maxAge)
	}
	return true, fmt.Sprintf("newest completed backup %s finished %s ago, inside the %s this bundle allows", newestName, age.Round(time.Minute), maxAge), ""
}

// printPlan writes what this run is about to do, with the gates' verdicts, and
// is the ONLY thing an unconfirmed call does.
func (n *nodeReboot) printPlan(gates []gateResult) {
	fmt.Fprintf(n.out, "%s node %s of cluster %s.\n", titleVerb(n.f.K3sOnly), n.node.Name, n.f.Cluster)
	fmt.Fprintf(n.out, "  host:      %s (%s, role %s)\n", n.host.HostID, n.host.SSHAddress, n.host.Role)
	fmt.Fprintf(n.out, "  node:      %s (uid %s), %s\n", n.node.Name, n.node.UID, readyWord(n.node.Ready))
	fmt.Fprintf(n.out, "  cluster:   %d node(s), %d server(s)\n", n.nodeCount, n.servers)
	if n.f.Now {
		fmt.Fprintf(n.out, "  window:    bypassed by --now\n")
	} else if n.window != nil {
		fmt.Fprintf(n.out, "  window:    %s; now %s\n", n.window, n.window.Moment(n.now()))
	}
	for _, g := range gates {
		mark := "pass"
		if !g.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(n.out, "  %-10s %s: %s\n", g.Name+":", mark, g.Detail)
	}
	if n.f.K3sOnly {
		fmt.Fprintf(n.out, "  plan:      restart %s on the host and wait for the API to answer. k3s rotates its leaf certificates on startup, so this is the supported way to renew them without rebooting the host; the host itself is NOT rebooted\n", n.serviceName())
	} else if n.nodeCount <= 1 {
		fmt.Fprintf(n.out, "  plan:      NOT draining: this cluster has one node and it is this one, so there is nowhere for the pods to go. The host is rebooted with `systemctl reboot` and waited for from outside over SSH\n")
	} else {
		fmt.Fprintf(n.out, "  plan:      cordon, then drain within %s (limits.timeouts.node-drain, never force-deleting a pod and never over a PodDisruptionBudget), then `systemctl reboot`, then wait from outside over SSH, then uncordon\n", n.drainFor)
	}
	fmt.Fprintf(n.out, "  waiting:   SSH reachable, then %s, then the cluster API answering with the node Ready — each within %s (limits.timeouts.node-reboot)\n", n.serviceName()+" active", n.rebootFor)
	fmt.Fprintf(n.out, "  interlock: kured's lock is taken for %s (node-drain + node-reboot), and held until the node is back and uncordoned\n", n.ttl)
}

func titleVerb(k3sOnly bool) string {
	if k3sOnly {
		return "Restarting k3s on"
	}
	return "Rebooting"
}

func (n *nodeReboot) disruptVerb() string {
	if n.f.K3sOnly {
		return "restart k3s on"
	}
	return "reboot"
}

func readyWord(ready bool) string {
	if ready {
		return "Ready"
	}
	return "NOT Ready"
}

// serviceName is the k3s unit on this host, which is a function of the host's
// role: a server runs k3s and an agent runs k3s-agent. `--k3s-only` restarts
// this unit and never `reboot`.
func (n *nodeReboot) serviceName() string {
	if node.Role(n.host.Role) == node.RoleServer {
		return "k3s"
	}
	return "k3s-agent"
}

// openRecord creates the operation record — the lock and the resume path — or
// takes over the one an interrupted run left behind.
func (n *nodeReboot) openRecord(ctx context.Context) error {
	n.store0 = n.store(n.serverConn)
	req := operation.Request{
		Kind:    operation.KindNodeReboot,
		Cluster: n.f.Cluster,
		Targets: []operation.Target{{HostID: n.host.HostID, NodeUID: n.node.UID}},
		Versions: map[string]string{
			"bundle": n.recordedBundleVersion(),
			"mode":   n.mode(),
		},
	}
	if n.f.Resume == "" {
		handle, err := n.store0.Acquire(ctx, req)
		if err != nil {
			return err
		}
		n.handle = handle
		fmt.Fprintf(n.out, "  record:    %s (resume with --resume %s if this is interrupted)\n", handle.OperationID(), handle.OperationID())
		return nil
	}
	// A resume reconciles BEFORE anything is repeated: the probes are
	// read-only, and what a resume cannot establish it stops on rather than
	// guessing (PLAN 7.2).
	plan, err := operation.Resume(ctx, n.store0, n.f.Resume)
	if err != nil {
		return err
	}
	if err := plan.Verify(req); err != nil {
		return err
	}
	fmt.Fprintf(n.out, "Resuming operation %s, stopped at %s: %d step(s) established, %d to repeat.\n",
		n.f.Resume, plan.Record.Stage, len(plan.Skip()), len(plan.Steps)-len(plan.Skip()))
	for _, step := range plan.Steps {
		fmt.Fprintf(n.out, "  %s %s (%s): %s\n", step.Decision, step.ActionID, step.Stage, step.Reason)
	}
	handle, err := operation.TakeOver(ctx, n.store0, n.f.Resume)
	if err != nil {
		return err
	}
	n.handle = handle
	n.skip = plan.Skip()
	return nil
}

func (n *nodeReboot) recordedBundleVersion() string {
	if n.bundle == nil {
		return ""
	}
	return n.bundle.Bundle
}

func (n *nodeReboot) mode() string {
	if n.f.K3sOnly {
		return "k3s-only"
	}
	return "host-reboot"
}

// act is everything that changes something, in the order T5.1 fixes: the record
// exists already, then the node is held and kured's lock is taken — both before
// the first disruptive step — and neither is released until the node is back.
func (n *nodeReboot) act(ctx context.Context) error {
	if err := n.hold(ctx); err != nil {
		return err
	}
	if err := n.takeInterlock(ctx); err != nil {
		return err
	}

	drained := false
	if !n.f.K3sOnly && n.nodeCount > 1 {
		if err := n.cordon(ctx); err != nil {
			return n.unwind(ctx, err)
		}
		if err := n.drain(ctx); err != nil {
			// The host never went down, so the node is put back into service
			// rather than left cordoned: a drain that could not finish is a
			// refusal, and a refusal leaves the cluster as it was.
			return n.unwind(ctx, err)
		}
		drained = true
	} else if !n.f.K3sOnly {
		fmt.Fprintf(n.out, "  not draining: this cluster has one node and it is this one, so there is nowhere for the pods to go. Waiting from outside, over SSH, once the host is down.\n")
	}

	if err := n.disrupt(ctx); err != nil {
		return err
	}
	if err := n.waitFromOutside(ctx); err != nil {
		// The record keeps the failed postcondition and the node stays
		// cordoned, and kured's lock stays held on purpose: with kured's
		// concurrency of 1 that halts the sequence instead of letting the next
		// node go down while somebody works out what happened (PLAN 7.4, "One
		// node at a time"). The TTL this command took bounds it.
		fmt.Fprintf(n.out, "\n%s is NOT back. It is left cordoned, and kured's lock is left held so no other node is rebooted until somebody has looked: the lock expires after %s from when it was taken, and\n  sudo -n k3s kubectl -n kube-system annotate daemonset kured %s-\nreleases it by hand (kured's own documented recovery).\n",
			n.node.Name, n.ttl, interlock.LockAnnotation)
		return err
	}

	if drained {
		if err := n.uncordon(ctx); err != nil {
			return err
		}
	}
	n.finishDisruptAction(ctx, true)
	n.release(ctx)
	return nil
}

// unwind puts back what a failure before the reboot changed: the node is
// uncordoned, the hold this run placed is lifted, and the lock is released,
// because nothing was taken down.
func (n *nodeReboot) unwind(ctx context.Context, cause error) error {
	if !n.f.K3sOnly && n.nodeCount > 1 {
		if _, err := k3s.Kubectl(ctx, n.serverConn, "uncordon "+n.node.Name); err != nil {
			fmt.Fprintf(n.out, "warning: %s could not be uncordoned: %v\n", n.node.Name, err)
		}
	}
	n.release(ctx)
	return cause
}

// hold takes the node out of kured's pool for the duration of this operation.
//
// It is the landed helper (T3.3) rather than a second implementation of the
// label, and it runs BEFORE kured's lock is taken for a reason that is easy to
// get wrong: the helper also releases a lock the held node still OWNS and
// uncordons it. Taken the other way round, it would release the lock this
// command had just taken.
func (n *nodeReboot) hold(ctx context.Context) error {
	if n.node.Labels[day2.NoAutoRebootLabel] == day2.NoAutoRebootValue {
		fmt.Fprintf(n.out, "  hold:      %s already carries %s=%s, so kured is not running there\n", n.node.Name, day2.NoAutoRebootLabel, day2.NoAutoRebootValue)
		return nil
	}
	guarded := &operation.Guarded{Inner: n.serverConn, Op: n.handle, Stage: "hold", Specs: rebootSpecs, Skip: n.skip}
	if err := day2.HoldAutomaticReboots(ctx, guarded, n.node.Name); err != nil {
		return fmt.Errorf("holding %s out of kured's pool: %w", n.node.Name, err)
	}
	n.weHeld = true
	fmt.Fprintf(n.out, "  hold:      %s labelled %s=%s, so kured cannot act on it while this operation runs\n", n.node.Name, day2.NoAutoRebootLabel, day2.NoAutoRebootValue)
	return nil
}

// takeInterlock takes kured's own lock, which is what stops kured and this
// command from both deciding to take a node down (T5.1, PLAN 7.2).
func (n *nodeReboot) takeInterlock(ctx context.Context) error {
	held, holder, err := interlock.Acquire(ctx, n.serverConn, n.node.Name, n.ttl)
	if err != nil {
		return fmt.Errorf("kured's lock could not be taken: %w", err)
	}
	if !held {
		return fmt.Errorf("kured's lock is held by %s, so a reboot of %s may already be under way: nothing was changed. Wait for that reboot to finish, then re-run",
			holder, holder)
	}
	fmt.Fprintf(n.out, "  interlock: kured's lock taken for %s (%s + %s), so kured cannot act on this cluster while this runs\n",
		n.ttl, n.drainFor, n.rebootFor)
	return nil
}

// release removes kured's lock and the hold this run placed. It reports a
// failure and never hides the run's own error: a node that is back is a
// success, and a lock left behind is bounded by its TTL.
func (n *nodeReboot) release(ctx context.Context) {
	if err := interlock.Release(ctx, n.serverConn, n.node.Name); err != nil {
		fmt.Fprintf(n.out, "warning: kured's lock could not be released: %v (it expires %s after it was taken)\n", err, n.ttl)
	} else {
		fmt.Fprintf(n.out, "  interlock: kured's lock released\n")
	}
	if !n.weHeld {
		return
	}
	// The node did not carry the hold before this run, so putting it back the
	// way it was found means lifting it. A node that DID carry it — every
	// server, permanently — is never touched here.
	if _, err := k3s.Kubectl(ctx, n.serverConn, "label node "+n.node.Name+" "+day2.NoAutoRebootLabel+"-"); err != nil {
		fmt.Fprintf(n.out, "warning: the reboot hold on %s could not be lifted: %v\n", n.node.Name, err)
	}
}

// cordon makes the node unschedulable. It is the first disruptive step, so it
// is recorded before it is submitted.
func (n *nodeReboot) cordon(ctx context.Context) error {
	guarded := &operation.Guarded{Inner: n.serverConn, Op: n.handle, Stage: "cordon", Specs: rebootSpecs, Skip: n.skip}
	if _, err := k3s.Kubectl(ctx, guarded, "cordon "+n.node.Name); err != nil {
		return fmt.Errorf("cordoning %s: %w", n.node.Name, err)
	}
	fmt.Fprintf(n.out, "  cordon:    %s is unschedulable\n", n.node.Name)
	return nil
}

// drain evicts the node's pods within the bundle's node-drain timeout, over the
// PodDisruptionBudgets. A pod is never force-deleted and a budget is never
// ignored: an operator's decision, not a tool's.
func (n *nodeReboot) drain(ctx context.Context) error {
	guarded := &operation.Guarded{Inner: n.serverConn, Op: n.handle, Stage: "drain", Specs: rebootSpecs, Skip: n.skip}
	args := "drain " + n.node.Name + " --ignore-daemonsets --delete-emptydir-data --timeout=" + n.drainFor.String()
	if _, err := k3s.Kubectl(ctx, guarded, args); err != nil {
		return fmt.Errorf("draining %s within %s (limits.timeouts.node-drain; the drain never force-deletes a pod, so a PodDisruptionBudget that permits no disruption stops it here): %w",
			n.node.Name, n.drainFor, err)
	}
	fmt.Fprintf(n.out, "  drain:     %s drained within %s, over the PodDisruptionBudgets\n", n.node.Name, n.drainFor)
	return nil
}

// uncordon puts the node back into service, and only once it is Ready.
func (n *nodeReboot) uncordon(ctx context.Context) error {
	guarded := &operation.Guarded{Inner: n.serverConn, Op: n.handle, Stage: "uncordon", Specs: rebootSpecs, Skip: n.skip}
	if _, err := k3s.Kubectl(ctx, guarded, "uncordon "+n.node.Name); err != nil {
		return fmt.Errorf("uncordoning %s: %w — the node is Ready but still unschedulable", n.node.Name, err)
	}
	fmt.Fprintf(n.out, "  uncordon:  %s is schedulable again\n", n.node.Name)
	return nil
}

// disrupt issues the reboot or the k3s restart, recording it before it is
// submitted.
//
// THE OUTCOME OF A REBOOT IS NOT THIS CALL'S TO DECIDE. `systemctl reboot`
// usually kills the SSH connection before it can answer, and that is the
// expected outcome rather than a failure; the node being Ready again is what
// establishes that it happened, so the action is left "submitted" — the state a
// successor reconciles by observation — and finished after the wait. A non-zero
// exit, by contrast, is the host refusing to do it, and that IS a failure.
func (n *nodeReboot) disrupt(ctx context.Context) error {
	command := "sudo -n systemctl reboot"
	if n.f.K3sOnly {
		// NEVER `reboot` in this mode. k3s rotates its leaf certificates when
		// the service starts, which is the whole point of the flag.
		command = "sudo -n systemctl restart " + n.serviceName()
	}
	spec, _ := rebootSpecs("disrupt", command)
	id := operation.ActionID("disrupt", command)
	if n.handle != nil {
		if n.skip[id] {
			fmt.Fprintf(n.out, "  resume:    this step was already carried out by the operation being resumed, so it is not repeated\n")
			n.disruptID, n.disruptDone = id, true
			return nil
		}
		if err := n.handle.RecordAction(ctx, id, "disrupt", spec); err != nil {
			return fmt.Errorf("this step was not submitted: it could not be recorded first: %w", err)
		}
		if err := n.handle.SubmitAction(ctx, id); err != nil {
			return fmt.Errorf("this step was not submitted: the record could not be told it was about to be: %w", err)
		}
	}
	n.disruptID = id

	res, err := n.target.Run(ctx, command)
	switch {
	case err != nil && !n.f.K3sOnly:
		// The connection died with the machine going down. Expected.
		fmt.Fprintf(n.out, "  disrupt:   `%s` issued; the connection went down with the host, which is what a reboot does\n", command)
	case err != nil:
		n.failDisruptAction(ctx, err)
		return fmt.Errorf("restarting %s on %s: %w", n.serviceName(), n.node.Name, err)
	case res.ExitCode != 0:
		n.failDisruptAction(ctx, fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr)))
		return fmt.Errorf("%s on %s exited %d: %s", command, n.node.Name, res.ExitCode, strings.TrimSpace(res.Stderr))
	default:
		fmt.Fprintf(n.out, "  disrupt:   `%s` returned %d\n", command, res.ExitCode)
	}
	return nil
}

// failDisruptAction marks a disrupted action that failed outright, so a resume
// repeats it instead of reconciling an outcome that never happened.
func (n *nodeReboot) failDisruptAction(ctx context.Context, cause error) {
	if n.handle == nil || n.disruptID == "" {
		return
	}
	if err := n.handle.FinishAction(ctx, n.disruptID, sshx.Result{}, cause); err != nil {
		fmt.Fprintf(n.out, "warning: the operation record could not be told the step failed: %v\n", err)
	}
}

// finishDisruptAction marks the disrupt action's outcome once the wait has
// established it.
func (n *nodeReboot) finishDisruptAction(ctx context.Context, ok bool) {
	if n.handle == nil || n.disruptID == "" || n.disruptDone {
		return
	}
	var cause error
	if !ok {
		cause = fmt.Errorf("the node did not come back")
	}
	if err := n.handle.FinishAction(ctx, n.disruptID, sshx.Result{}, cause); err != nil {
		fmt.Fprintf(n.out, "warning: the operation record could not be told the step's outcome: %v\n", err)
	}
}

// waitFromOutside observes the node coming back, over SSH, because nothing
// inside the cluster survives to observe a single-server reboot (PLAN 7.3).
//
// Each step is reported as it is observed, and each step has its own
// limits.timeouts.node-reboot deadline: a timeout says WHICH step did not
// happen, because "the node did not come back" is not actionable while "k3s
// never came up" is.
func (n *nodeReboot) waitFromOutside(ctx context.Context) error {
	fmt.Fprintf(n.out, "  waiting from outside, over SSH:\n")
	steps := []struct {
		name    string
		observe func(ctx context.Context) (bool, string, error)
	}{
		{waitStepSSH, n.observeSSH},
		{waitStepService, n.observeService},
		{waitStepAPI, n.observeAPI},
	}
	for _, step := range steps {
		deadline := n.now().Add(n.rebootFor)
		for {
			ok, detail, err := step.observe(ctx)
			if ok {
				fmt.Fprintf(n.out, "    - %s: %s\n", step.name, detail)
				break
			}
			if n.now().After(deadline) {
				why := detail
				if err != nil {
					why = err.Error()
				}
				return fmt.Errorf("%s did not happen within %s (limits.timeouts.node-reboot): %s", step.name, n.rebootFor, why)
			}
			if err := n.sleep(ctx, n.poll); err != nil {
				return err
			}
		}
	}
	return nil
}

// observeSSH is the first step: a fresh connection reaches the host. Every
// attempt dials again, because the previous connection died with the machine.
func (n *nodeReboot) observeSSH(ctx context.Context) (bool, string, error) {
	if n.target != nil && n.target != n.serverConn {
		n.target.Close()
	}
	n.target = nil
	conn, err := n.dial(ctx, n.host)
	if err != nil {
		return false, "not reachable yet", nil
	}
	n.target = conn
	return true, fmt.Sprintf("connected to %s", n.host.SSHAddress), nil
}

// observeService is the second step: k3s is active on the node itself.
func (n *nodeReboot) observeService(ctx context.Context) (bool, string, error) {
	if n.target == nil {
		return false, "no connection yet", nil
	}
	res, err := n.target.Run(ctx, "systemctl is-active "+n.serviceName())
	if err != nil {
		return false, "not observed", nil
	}
	state := strings.TrimSpace(res.Stdout)
	return res.ExitCode == 0 && state == "active", fmt.Sprintf("systemctl is-active %s = %s", n.serviceName(), state), nil
}

// observeAPI is the third step: the cluster's API answers and the node is Ready
// again.
//
// It is read from a SERVER, which for a single-server cluster is the machine
// that just rebooted: its own API answering is the control plane coming back.
// For an agent it is the cluster's other server answering, which is the only
// place a Node object can be read from.
func (n *nodeReboot) observeAPI(ctx context.Context) (bool, string, error) {
	runner, err := n.liveServer(ctx)
	if err != nil {
		return false, "no server connection yet", nil
	}
	ready, err := nodeReady(ctx, runner, n.node.Name)
	if err != nil {
		return false, "the API did not answer yet", nil
	}
	if !ready {
		return false, fmt.Sprintf("%s is up and k3s is active, but the node is not Ready yet", n.node.Name), nil
	}
	return true, fmt.Sprintf("the API answers and %s is Ready", n.node.Name), nil
}

// liveServer returns a live connection to a server, dialling again when the one
// it has is dead — which is what a single-server reboot leaves behind.
func (n *nodeReboot) liveServer(ctx context.Context) (nodeTransport, error) {
	if n.target != nil && n.sameHost(n.server, n.host) {
		return n.target, nil
	}
	if n.serverConn != nil {
		if _, err := n.serverConn.Run(ctx, "true"); err == nil {
			return n.serverConn, nil
		}
		n.serverConn.Close()
		n.serverConn = nil
	}
	conn, err := n.dial(ctx, n.server)
	if err != nil {
		return nil, err
	}
	n.serverConn = conn
	return conn, nil
}

// nodeReady asks the cluster API whether a node is Ready.
func nodeReady(ctx context.Context, r k3s.Runner, name string) (bool, error) {
	out, err := k3s.Kubectl(ctx, r, `get node `+name+` -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'`)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "True", nil
}

// finish closes the operation record the way the run ended.
func (n *nodeReboot) finish(ctx context.Context, runErr error) {
	if n.store0 == nil || n.handle == nil {
		return
	}
	result := operation.ResultSucceeded
	if runErr != nil {
		result = operation.ResultFailed
	}
	if err := n.store0.Complete(ctx, n.handle, result); err != nil {
		fmt.Fprintf(n.out, "warning: the operation record %s could not be closed: %v\n", n.handle.OperationID(), err)
		return
	}
	fmt.Fprintf(n.out, "  record:    %s marked %s\n", n.handle.OperationID(), result)
}

func (n *nodeReboot) closeAll() {
	if n.target != nil && n.target != n.serverConn {
		n.target.Close()
	}
	if n.serverConn != nil {
		n.serverConn.Close()
	}
}

// rebootSpecs decides which of the commands this verb submits are ACTIONS —
// written down before submission, with a postcondition a successor can check —
// and what a successor can establish about each.
//
// Reads are not actions: the postcondition of a read is the read. Recording
// every observation would fill the record with steps a resume could not safely
// skip, which is the opposite of what the record is for (PLAN 7.2).
func rebootSpecs(stage, command string) (operation.Spec, bool) {
	switch stage {
	case "cordon":
		if !strings.Contains(command, "kubectl cordon ") {
			return operation.Spec{}, false
		}
		name := lastWord(command)
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: name + " is unschedulable",
			Observe:       `test "$(sudo -n k3s kubectl get node ` + name + ` -o jsonpath='{.spec.unschedulable}')" = true`,
		}, true
	case "drain":
		if !strings.Contains(command, "kubectl drain ") {
			return operation.Spec{}, false
		}
		name := secondWord(command, "drain")
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "no pod that is not a DaemonSet remains on " + name,
			Observe: `test -z "$(sudo -n k3s kubectl get pods -A --field-selector spec.nodeName=` + name +
				` -o custom-columns=O:.metadata.ownerReferences[0].kind --no-headers 2>/dev/null | grep -v '^DaemonSet$')"`,
		}, true
	case "disrupt":
		name := "the node"
		switch {
		case strings.Contains(command, "systemctl reboot"):
			return operation.Spec{
				Kind:          operation.ActionSSH,
				Postcondition: "the host rebooted and " + name + " is Ready again",
				Observe:       `sudo -n k3s kubectl get node $(hostname) -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' | grep -q '^True$'`,
			}, true
		case strings.Contains(command, "systemctl restart"):
			return operation.Spec{
				Kind:          operation.ActionSSH,
				Postcondition: "k3s was restarted on " + name + " and it is Ready again",
				Observe:       `sudo -n k3s kubectl get node $(hostname) -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' | grep -q '^True$'`,
			}, true
		}
		return operation.Spec{}, false
	case "uncordon":
		if !strings.Contains(command, "kubectl uncordon ") {
			return operation.Spec{}, false
		}
		name := lastWord(command)
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: name + " is schedulable again",
			Observe:       `test "$(sudo -n k3s kubectl get node ` + name + ` -o jsonpath='{.spec.unschedulable}')" != true`,
		}, true
	case "hold":
		if !strings.Contains(command, "label node ") {
			return operation.Spec{}, false
		}
		return operation.Spec{
			Kind:          operation.ActionSSH,
			Postcondition: "the node is held out of kured's pool (" + day2.NoAutoRebootLabel + "=" + day2.NoAutoRebootValue + ")",
			Observe:       `sudo -n k3s kubectl get node $(hostname) -o jsonpath='{.metadata.labels.kubenest\.io/auto-reboot}' | grep -q '^false$'`,
		}, true
	}
	return operation.Spec{}, false
}

func lastWord(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// secondWord returns the argument that follows a flag word, e.g. the node name
// after `drain`.
func secondWord(command, verb string) string {
	fields := strings.Fields(command)
	for i, f := range fields {
		if f == verb && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
