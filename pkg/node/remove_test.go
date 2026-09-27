package node

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

// removeFixture is the world `node remove` runs in: the cluster's two nodes
// and a connection to the agent that is being removed.
func removeFixture(t *testing.T, hosts ...api.HostRecord) *nodeFixture {
	t.Helper()
	if len(hosts) == 0 {
		hosts = []api.HostRecord{serverHost(), agentHost()}
	}
	f := newFixture(t, hosts...)
	// The machine being removed answers on its own address with the host key
	// the inventory recorded for it.
	f.addHost("10.0.3.8", "SHA256:agent", joinedNodes(t, testAgentNode, true))
	return f
}

// runRemove drives the real staging engine over the real stage sequence, and
// closes the operation the way the command does.
func (f *nodeFixture) runRemove(r *Remove) (stages.Result, error) {
	f.t.Helper()
	result, err := stages.Execute(context.Background(), r, PlanRemove(r))
	r.Finish(context.Background(), err, false)
	return result, err
}

func (f *nodeFixture) newRemove(opts RemoveOptions) *Remove {
	if opts.Node == "" {
		// The inventory's own host ID, which is what an operator types when
		// the machine has several names.
		opts.Node = "h-agt"
	}
	return &Remove{Session: f.session, Opts: opts}
}

// boundLocalVolume makes the cluster report one bound local volume on the node
// being removed, held by the named workload.
func (f *nodeFixture) boundLocalVolume(t *testing.T, namespace, pvc, workload string) {
	t.Helper()
	node := testAgentNode
	f.server.on("get pv -o json", ok(`{"items":[{"metadata":{"name":"pv-`+pvc+`"},"spec":{`+
		`"claimRef":{"namespace":"`+namespace+`","name":"`+pvc+`"},`+
		`"csi":{"driver":"local.csi.openebs.io"},`+
		`"nodeAffinity":{"required":{"nodeSelectorTerms":[{"matchExpressions":[`+
		`{"key":"kubernetes.io/hostname","operator":"In","values":["`+node+`"]}]}]}}},`+
		`"status":{"phase":"Bound"}}]}`))
	f.server.on("get pods --all-namespaces -o json", ok(`{"items":[{"metadata":{"namespace":"`+namespace+`",`+
		`"ownerReferences":[{"kind":"StatefulSet","name":"`+workload+`"}]},`+
		`"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"`+pvc+`"}}]}}]}`))
}

// A server is not removable by this command: the single-server tier is
// recovered by S6, and the ha tier's node operations arrive with its
// promotion. Both are named, because the operator has to know which one they
// are looking at.
func TestRemoveRefusesAServerNode(t *testing.T) {
	f := removeFixture(t)

	_, err := f.runRemove(f.newRemove(RemoveOptions{Node: testServerAddr}))
	if err == nil {
		t.Fatal("a server was accepted for removal")
	}
	for _, want := range []string{"S6", "promotion"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if len(f.log.commands(testServerAddr)) != 0 {
		t.Error("the server was touched before it was refused")
	}
	if f.log.hasCommand("cordon") || f.log.hasCommand("drain") || f.log.hasCommand("uninstall") {
		t.Error("something was changed on a node that may not be removed")
	}
}

// The volume disposition is the gate: a node that still holds a bound local
// volume is refused, and the refusal carries the command that moves each
// affected workload's data somewhere else — with every stranded claim of one
// workload in ONE command, because restoring one claim of a pod while another
// stays stranded leaves the pod unable to start.
func TestRemoveRefusesWhileABoundLocalVolumeRemains(t *testing.T) {
	f := removeFixture(t)
	// One workload with two claims, both on the node: one restore command.
	f.server.on("get pv -o json", ok(`{"items":[`+
		`{"metadata":{"name":"pv-a"},"spec":{"claimRef":{"namespace":"db","name":"data-pg-0"},`+
		`"csi":{"driver":"local.csi.openebs.io"},"nodeAffinity":{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["`+testAgentNode+`"]}]}]}}},`+
		`"status":{"phase":"Bound"}},`+
		`{"metadata":{"name":"pv-b"},"spec":{"claimRef":{"namespace":"db","name":"data-pg-1"},`+
		`"csi":{"driver":"local.csi.openebs.io"},"nodeAffinity":{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["`+testAgentNode+`"]}]}]}}},`+
		`"status":{"phase":"Bound"}}]}`))
	f.server.on("get pods --all-namespaces -o json", ok(`{"items":[{"metadata":{"namespace":"db",`+
		`"ownerReferences":[{"kind":"StatefulSet","name":"pg"}]},`+
		`"spec":{"volumes":[{"persistentVolumeClaim":{"claimName":"data-pg-0"}},`+
		`{"persistentVolumeClaim":{"claimName":"data-pg-1"}}]}}]}`))

	_, err := f.runRemove(f.newRemove(RemoveOptions{}))
	if err == nil {
		t.Fatal("a node that holds bound local volumes was removed without --abandon-volumes")
	}
	command := "kubenest backup restore --cluster prod-1 --namespace db --pvc data-pg-0 --pvc data-pg-1"
	if !strings.Contains(err.Error(), command) {
		t.Errorf("the refusal does not carry the grouped restore command %q:\n%v", command, err)
	}
	if !strings.Contains(err.Error(), "--abandon-volumes") {
		t.Errorf("the refusal does not say how to proceed deliberately: %v", err)
	}
	if n := strings.Count(err.Error(), "kubenest backup restore"); n != 1 {
		t.Errorf("%d restore commands for one workload's two claims, want 1: the pod cannot start unless both are restored together", n)
	}
	if f.log.hasCommand("cordon") || f.log.hasCommand("drain") || f.log.hasCommand("uninstall") {
		t.Error("k3s was touched before the volume disposition was settled")
	}
	if len(f.records.savedRecords()) != 0 {
		t.Error("the inventory was written before the volume disposition was settled")
	}
}

// --abandon-volumes proceeds, and says in as many words what that means: the
// data is gone, and nothing here certifies that a backup of it exists.
func TestRemoveProceedsWithAbandonVolumesAndStatesTheDataLoss(t *testing.T) {
	f := removeFixture(t)
	f.boundLocalVolume(t, "db", "data-pg-0", "pg")

	if _, err := f.runRemove(f.newRemove(RemoveOptions{AbandonVolumes: true})); err != nil {
		t.Fatalf("--abandon-volumes did not proceed: %v\n%s", err, f.out.String())
	}
	out := f.out.String()
	for _, want := range []string{"GONE", "nothing"} {
		if !strings.Contains(strings.ToUpper(out), strings.ToUpper(want)) {
			t.Errorf("the run does not state the consequence (%q):\n%s", want, out)
		}
	}
	if !f.log.hasCommand("k3s-agent-uninstall.sh") {
		t.Error("k3s was not uninstalled from the host")
	}
	if !f.log.hasCommand("delete node " + testAgentNode) {
		t.Error("the Node object was not deleted")
	}
	if state := f.lastState("h-agt"); state != string(StateRemoved) {
		t.Errorf("the inventory says the host is %q, want %q", state, StateRemoved)
	}
	if _, ok := f.hostIn("h-agt"); !ok {
		t.Error("the host's entry was deleted rather than kept as removed")
	}
}

// WHATEVER THE VOLUME DISPOSITION SAYS, k3s is uninstalled only after it has
// been settled. Reverting the order is how a node loses the only copy of a
// workload's data.
func TestRemoveNeverUninstallsK3sBeforeTheVolumeDisposition(t *testing.T) {
	// The refusal path: no uninstall at all.
	refused := removeFixture(t)
	refused.boundLocalVolume(t, "db", "data-pg-0", "pg")
	if _, err := refused.runRemove(refused.newRemove(RemoveOptions{})); err == nil {
		t.Fatal("the node was removed while it held a bound local volume")
	}
	if refused.log.hasCommand("k3s-agent-uninstall.sh") {
		t.Error("k3s was uninstalled on the refusal path")
	}

	// The proceed path: the volumes are read BEFORE anything is uninstalled.
	proceeded := removeFixture(t)
	proceeded.boundLocalVolume(t, "db", "data-pg-0", "pg")
	if _, err := proceeded.runRemove(proceeded.newRemove(RemoveOptions{AbandonVolumes: true})); err != nil {
		t.Fatalf("--abandon-volumes did not proceed: %v\n%s", err, proceeded.out.String())
	}
	read, uninstall := proceeded.log.indexOf("get pv -o json"), proceeded.log.indexOf("k3s-agent-uninstall.sh")
	if read < 0 || uninstall < 0 {
		t.Fatalf("the fixture did not exercise both steps (read %d, uninstall %d)", read, uninstall)
	}
	if uninstall < read {
		t.Errorf("k3s was uninstalled at %d, before the volumes were read at %d", uninstall, read)
	}
}

// The drain is bounded by the bundle's own timeout, never by a constant here.
func TestRemoveDrainsWithinTheManifestTimeout(t *testing.T) {
	f := removeFixture(t)

	if _, err := f.runRemove(f.newRemove(RemoveOptions{})); err != nil {
		t.Fatalf("the removal failed: %v\n%s", err, f.out.String())
	}
	drain := ""
	for _, cmd := range f.log.commands(testServerAddr) {
		if strings.Contains(cmd, "kubectl drain ") {
			drain = cmd
		}
	}
	if drain == "" {
		t.Fatal("the node was never drained")
	}
	if !strings.Contains(drain, "--timeout=15m0s") {
		t.Errorf("the drain does not carry limits.timeouts.node-drain (15m): %q", drain)
	}
	if strings.Contains(drain, "--force") {
		t.Errorf("the drain force-deletes pods, which is an operator's decision and not a tool's: %q", drain)
	}
	if !strings.Contains(drain, "--ignore-daemonsets") {
		t.Errorf("the drain would refuse over its own DaemonSet pods: %q", drain)
	}
}

// A budget that permits no disruption stalls the drain until its timeout and
// leaves the cluster mid-removal, so it is refused BEFORE the drain starts.
func TestRemoveRefusesWhenADisruptionBudgetWouldNeverLetTheDrainFinish(t *testing.T) {
	f := removeFixture(t)
	f.server.on("get poddisruptionbudgets -A -o json", ok(`{"items":[{"metadata":{"name":"pg","namespace":"db"},`+
		`"status":{"disruptionsAllowed":0,"currentHealthy":1,"desiredHealthy":1,"expectedPods":1}}]}`))

	_, err := f.runRemove(f.newRemove(RemoveOptions{}))
	if err == nil {
		t.Fatal("the drain started over a budget that can never let it finish")
	}
	if !strings.Contains(err.Error(), "db/pg") {
		t.Errorf("the refusal does not name the budget: %v", err)
	}
	if f.log.hasCommand("kubectl drain") {
		t.Error("the drain ran although the budget gate refused it")
	}
}

// Sanity: the fixture's agent entry is reachable, or every removal test would
// pass for the wrong reason.
func TestRemoveFixtureIsReachable(t *testing.T) {
	f := removeFixture(t)
	host, ok := f.hostIn("h-agt")
	if !ok {
		t.Fatal("the agent is not in the fixture's inventory")
	}
	conn, err := f.dialer.dial(context.Background(), host)
	if err != nil {
		t.Fatalf("the fixture cannot reach the machine it removes: %v", err)
	}
	if _, err := conn.Run(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Run(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	var _ sshx.Result
}

// An address a removed host still holds can be given to a new machine, and the
// inventory then holds two entries at that address. Both ways of naming that
// machine mean THE MACHINE THAT IS THERE: its address, and the name of its Node
// object. The removed record answers only when it is what the operator actually
// named (by its host ID, which nothing else can match).
//
// Hardware, 2026-09-27 (lab w3): after the add arm gave w5 w4's address,
// `node remove --cluster lab-w3 --node 167.233.20.250` resolved to w4's REMOVED
// entry and refused — "already recorded as \"removed\", so there is nothing to
// remove" — with the machine that is actually there never looked at.
func TestRemovePrefersTheMachineAtAnAddressOverTheRemovedRecordItSharesItWith(t *testing.T) {
	cases := []struct {
		name string
		node string
	}{
		{"named by the address it shares with the removed record", testAgentAddr},
		{"named by its Node object's name", testReusedNode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, serverHost(), removedAgent(), reusedAddressHost())
			f.server.setNodes(nodesJSON(t,
				testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
				testNode{Name: testReusedNode, UID: testReusedNodeUID, Addresses: []string{testAgentAddr}, Ready: true},
			))

			if _, err := f.runRemove(f.newRemove(RemoveOptions{Node: tc.node})); err != nil {
				t.Fatalf("the machine at a removed host's address could not be removed: %v\n%s", err, f.out.String())
			}
			if state := f.lastState(testReusedHostID); state != string(StateRemoved) {
				t.Errorf("the machine at the address is recorded as %q, want %q", state, StateRemoved)
			}
			if !f.log.hasCommand("delete node " + testReusedNode) {
				t.Error("the Node object of the machine at the address was not deleted")
			}
			gone, found := f.hostIn("h-gone")
			if !found {
				t.Fatal("the record of the machine that was already gone was deleted")
			}
			if gone.LifecycleState != string(StateRemoved) || gone.HostKeyFingerprint != "SHA256:gone" || gone.NodeUID != "uid-gone" {
				t.Errorf("the removal changed the record of the machine that was already gone: %+v", gone)
			}
		})
	}
}

// The record of a machine that is gone is still what is acted on when it is what
// the operator named: by its own host ID, or at an address nothing else holds.
// Both are refused exactly as before — removing the same machine twice is not a
// repair, and its disks may still hold another cluster's data.
func TestRemoveStillRefusesARemovedHostNamedByItsHostID(t *testing.T) {
	cases := []struct {
		name string
		node string
	}{
		{"named by its host ID", "h-gone"},
		{"named by the address only it carries", testAgentAddr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, serverHost(), removedAgent())

			_, err := f.runRemove(f.newRemove(RemoveOptions{Node: tc.node}))
			if err == nil {
				t.Fatalf("a host that is already removed was removed again:\n%s", f.out.String())
			}
			// The refusal is about the record the operator named, and it changed
			// nothing: no connection, no inventory write, no Node object deleted.
			if !strings.Contains(err.Error(), "h-gone") {
				t.Errorf("the refusal does not name the removed record it refused: %v", err)
			}
			if f.dialer.dialed(testAgentAddr) {
				t.Error("a machine was dialled for a host that is refused")
			}
			if len(f.records.savedRecords()) != 0 {
				t.Errorf("%d inventory write(s) happened for a refused host", len(f.records.savedRecords()))
			}
			if f.log.hasCommand("delete node") || f.log.hasCommand("k3s-agent-uninstall.sh") {
				t.Error("a refused host was acted on")
			}
			if state := f.lastState("h-gone"); state != string(StateRemoved) {
				t.Errorf("the removed entry is now %q, want %q", state, StateRemoved)
			}
		})
	}
}
