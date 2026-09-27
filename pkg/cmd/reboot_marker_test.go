package cmd

// The planned-reboot marker (T2.7, PLAN 7.4).
//
// A reboot this verb orders and a node that died look identical to the backend
// unless the node says which it is. `kubenest node reboot` therefore annotates
// the node BEFORE the first disruptive step and clears both annotations once
// the node is back, Ready and uncordoned — the operator reads them off the Node
// object and reports `planned_reboot`, and the backend grants such a node the
// planned-reboot grace instead of the short not-ready one. Without the marker a
// slow server reboot trips NODE_NOT_READY for a reboot the platform ordered
// itself.
//
// Everything here is asserted on the command log and the operation record of
// the fake cluster, which is where the ordering, the values and the recorded
// action are observable: a test that only checked the output text would pass
// while the annotate call went missing.
//
// THE ORDERING CLAIMS ARE MADE WITHIN ONE COMMAND LOG. The marker is written
// and cleared through a SERVER connection, and so are the cordon, the drain,
// the readiness probe and the uncordon; only the reboot itself runs on the
// target. So a multi-node case asserts the marker against the cordon and the
// drain, and the reboot cases (single-server, and --k3s-only, where the target
// IS the log's host) assert it against the reboot and the restart. Comparing an
// index from one fake host's log with an index from another's would be a
// comparison of unrelated numbers, which is a green test that proves nothing.

import (
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/operation"
)

// markerCommand is the annotate call that WRITES the marker for a node.
func markerCommand(node string) string {
	return "annotate node " + node + " " + PlannedRebootStartedAtAnnotation + "="
}

// markerClearCommand is the annotate call that CLEARS it: kubectl deletes an
// annotation whose key is given with a trailing dash.
func markerClearCommand(node string) string {
	return "annotate node " + node + " " + PlannedRebootStartedAtAnnotation + "-"
}

// readyProbeCommand is the readiness observation the wait makes, which is how
// "the node is back" is located in the log.
const readyProbeCommand = `-o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'`

// markerValues reads the two annotation values out of the command the verb
// issued, so the test asserts what was actually sent rather than what the code
// intended to send. It scans the whole command log: `mutations` deliberately
// lists only the commands that change the cluster, and reading through it would
// make this test pass while the annotate call was missing from it.
func markerValues(t *testing.T, h *fakeHost, node string) (startedAt, operationID, command string) {
	t.Helper()
	h.mu.Lock()
	commands := append([]string(nil), h.commands...)
	h.mu.Unlock()
	for _, c := range commands {
		if !strings.Contains(c, markerCommand(node)) {
			continue
		}
		command = c
		for _, field := range strings.Fields(c) {
			switch {
			case strings.HasPrefix(field, PlannedRebootStartedAtAnnotation+"="):
				startedAt = strings.TrimPrefix(field, PlannedRebootStartedAtAnnotation+"=")
			case strings.HasPrefix(field, PlannedRebootOperationAnnotation+"="):
				operationID = strings.TrimPrefix(field, PlannedRebootOperationAnnotation+"=")
			}
		}
	}
	return startedAt, operationID, command
}

// lastIndexOf is the LAST command containing substr, which the readiness claim
// needs: the wait observes the same readiness on every poll.
func lastIndexOf(h *fakeHost, substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	at := -1
	for i, c := range h.commands {
		if strings.Contains(c, substr) {
			at = i
		}
	}
	return at
}

func hasActionWithStage(actions []operation.Action, stage string) (operation.Action, bool) {
	for _, a := range actions {
		if a.Stage == stage {
			return a, true
		}
	}
	return operation.Action{}, false
}

// assertMarkerValues checks the two values the operator and the backend read:
// an RFC3339 UTC instant equal to the run's own clock, and the operation
// record's id.
func assertMarkerValues(t *testing.T, f *rebootFixture, h *fakeHost, node string) {
	t.Helper()
	startedAt, operationID, command := markerValues(t, h, node)
	if startedAt == "" || operationID == "" {
		t.Fatalf("the annotate call carries %s=%q and %s=%q; both are required: %s",
			PlannedRebootStartedAtAnnotation, startedAt, PlannedRebootOperationAnnotation, operationID, command)
	}
	parsed, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		t.Errorf("the marker's %s value %q is not RFC3339, which is the layout the operator parses: %v",
			PlannedRebootStartedAtAnnotation, startedAt, err)
	} else {
		if parsed.Location() != time.UTC {
			t.Errorf("the marker's instant is %q, want a UTC one: a node that reports its own offset makes two readers of one reboot disagree", startedAt)
		}
		if want := f.clock.now().UTC(); !parsed.Equal(want) {
			t.Errorf("the marker says %s, want the instant the reboot was ordered (%s)", parsed.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
	if got, want := operationID, h.liveRecord(t).OperationID; got != want {
		t.Errorf("the marker names operation %q, want the record's own id %q: the two are how a reader joins a node to the operation rebooting it", got, want)
	}
}

// A MARKER IS WRITTEN BEFORE ANYTHING IS DISRUPTED, AND IT SAYS WHICH OPERATION
// IS REBOOTING THE NODE.
func TestNodeRebootMarksThePlannedRebootBeforeDisrupting(t *testing.T) {
	t.Run("on a multi-node cluster the marker precedes the cordon and the drain", func(t *testing.T) {
		server := scriptedServer(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		agent := scriptedAgent(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost(), agentHost()},
			server: server,
			target: agent,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err != nil {
			t.Fatalf("reboot failed: %v\n%s", err, out)
		}
		mark := server.indexOf(markerCommand(testAgentNode))
		if mark < 0 {
			t.Fatalf("the node was never marked as being in a planned reboot:\n%s", commandLog(server))
		}
		for name, at := range map[string]int{
			"cordon": server.indexOf("kubectl cordon " + testAgentNode),
			"drain":  server.indexOf("kubectl drain " + testAgentNode),
		} {
			if at < 0 {
				t.Fatalf("the %s never happened, so the ordering claim is about nothing:\n%s", name, commandLog(server))
			}
			if mark > at {
				t.Errorf("the marker was written at %d, AFTER the %s at %d: the node must be marked before anything is disrupted, or the reboot the platform ordered reads as a node that died",
					mark, name, at)
			}
		}
		// The host reboot runs on the target, so it is asserted in the
		// single-server case below; here it is enough that it happened.
		if !agent.hasCommand("systemctl reboot") {
			t.Fatalf("the host was never rebooted:\n%s", commandLog(agent))
		}
		assertMarkerValues(t, f, server, testAgentNode)
		if got := server.recordRequest(t).Cluster; got != testCluster {
			t.Errorf("the record is about cluster %q", got)
		}
		// A recorded action, like the cordon and the drain: a resume can see it
		// on the node and skip it rather than resetting a pending reboot's age.
		actions := recordedActions(t, server)
		markAction, ok := hasActionWithStage(actions, plannedRebootStage)
		if !ok {
			t.Fatalf("the marker write is not in the operation record, so a resume would rewrite it and reset a pending reboot's age: %+v", actions)
		}
		if markAction.Postcondition == "" {
			t.Error("the marker action has no postcondition, so a successor could not tell whether it happened")
		}
		if markAction.Status != operation.ActionSucceeded {
			t.Errorf("the marker action's recorded status is %q, want succeeded", markAction.Status)
		}
	})

	t.Run("on a single-server host the marker precedes the reboot", func(t *testing.T) {
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err != nil {
			t.Fatalf("single-server reboot failed: %v\n%s", err, out)
		}
		mark := server.indexOf(markerCommand(testNodeName))
		reboot := server.indexOf("systemctl reboot")
		if mark < 0 {
			t.Fatalf("the server was never marked:\n%s", commandLog(server))
		}
		if reboot < 0 {
			t.Fatalf("the host was not rebooted:\n%s", commandLog(server))
		}
		if mark > reboot {
			t.Errorf("the marker (%d) came after the reboot (%d): the marker's whole value is that the node is already marked when it stops answering", mark, reboot)
		}
		assertMarkerValues(t, f, server, testNodeName)
	})

	t.Run("--k3s-only marks the node before the service is restarted", func(t *testing.T) {
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		server.on("sudo -n systemctl restart k3s", ok(""))
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true, K3sOnly: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err != nil {
			t.Fatalf("--k3s-only failed: %v\n%s", err, out)
		}
		mark := server.indexOf(markerCommand(testNodeName))
		restart := server.indexOf("sudo -n systemctl restart k3s")
		if mark < 0 {
			t.Fatalf("--k3s-only did not mark the node:\n%s", commandLog(server))
		}
		if restart < 0 {
			t.Fatalf("--k3s-only did not restart k3s:\n%s", commandLog(server))
		}
		if mark > restart {
			t.Errorf("--k3s-only marked the node (%d) after restarting k3s (%d); a restart that outlives the short grace would be alerted on as a dead node", mark, restart)
		}
		assertMarkerValues(t, f, server, testNodeName)
	})

	t.Run("a refusal writes nothing", func(t *testing.T) {
		// The window gate refuses before anything is changed, and a marker is a
		// claim that a reboot is under way: a refused run must not leave one.
		// The fake clock is Wednesday noon UTC, so a Monday window is shut.
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			window: windowRecord([]string{"mon"}, "02:00", "03:00", "UTC"),
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err == nil {
			t.Fatalf("a reboot outside the window proceeded:\n%s", out)
		}
		if server.hasCommand("annotate node") {
			t.Errorf("a refused run annotated a node:\n%s", commandLog(server))
		}
		if server.hasCommand("systemctl reboot") {
			t.Error("a refused run rebooted the host")
		}
	})

	t.Run("an unconfirmed run marks nothing", func(t *testing.T) {
		// The plan is printed and the run stops: a dry run must not leave a
		// marker behind, because a marker is a claim that a reboot is under
		// way.
		server := scriptedServer(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		agent := scriptedAgent(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testAgentAddr},
			hosts:  []api.HostRecord{serverHost(), agentHost()},
			server: server,
			target: agent,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err == nil {
			t.Fatalf("an unconfirmed run proceeded:\n%s", out)
		}
		if server.hasCommand("annotate node") || agent.hasCommand("annotate node") {
			t.Errorf("an unconfirmed run annotated a node:\n%s\n%s", commandLog(server), commandLog(agent))
		}
		if agent.hasCommand("systemctl reboot") {
			t.Error("an unconfirmed run rebooted the host")
		}
	})
}

// THE MARKER COMES OFF ONLY ONCE THE NODE IS BACK — NEVER WHILE IT IS STILL
// DOWN, AND NEVER WHEN IT NEVER CAME BACK.
func TestNodeRebootRemovesTheMarkerOnlyAfterTheNodeIsBack(t *testing.T) {
	t.Run("a reboot that finishes clears the marker after the node is back and uncordoned", func(t *testing.T) {
		server := scriptedServer(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		agent := scriptedAgent(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost(), agentHost()},
			server: server,
			target: agent,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err != nil {
			t.Fatalf("reboot failed: %v\n%s", err, out)
		}
		mark := server.indexOf(markerCommand(testAgentNode))
		clear := server.indexOf(markerClearCommand(testAgentNode))
		uncordon := server.indexOf("kubectl uncordon " + testAgentNode)
		readyProbe := lastIndexOf(server, readyProbeCommand)
		if mark < 0 || clear < 0 {
			t.Fatalf("the marker was written and not cleared:\n%s", commandLog(server))
		}
		for name, at := range map[string]int{"the uncordon": uncordon, "the Ready observation": readyProbe} {
			if at < 0 {
				t.Fatalf("%s never happened, so \"cleared only after the node is back\" is not being tested:\n%s", name, commandLog(server))
			}
		}
		if !(mark < readyProbe && readyProbe <= uncordon && uncordon < clear) {
			t.Errorf("the order is wrong: marker=%d ready=%d uncordon=%d clear=%d; the marker must be written before the reboot and cleared only once the node is back, Ready and uncordoned",
				mark, readyProbe, uncordon, clear)
		}
		// Both annotations go in ONE command: a node left carrying only one of
		// them is a node whose second write failed.
		clearCmd := server.commands[clear]
		for _, want := range []string{PlannedRebootStartedAtAnnotation + "-", PlannedRebootOperationAnnotation + "-"} {
			if !strings.Contains(clearCmd, want) {
				t.Errorf("the clearing command does not remove %s, so the marker stays half-set: %s", want, clearCmd)
			}
		}
		actions := recordedActions(t, server)
		clearAction, ok := hasActionWithStage(actions, plannedRebootClearStage)
		if !ok {
			t.Fatalf("the clearing write is not in the operation record: %+v", actions)
		}
		if clearAction.Status != operation.ActionSucceeded {
			t.Errorf("the clearing action's recorded status is %q, want succeeded", clearAction.Status)
		}
	})

	t.Run("on a single-server host the marker is cleared after the reboot", func(t *testing.T) {
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err != nil {
			t.Fatalf("single-server reboot failed: %v\n%s", err, out)
		}
		mark := server.indexOf(markerCommand(testNodeName))
		reboot := server.indexOf("systemctl reboot")
		clear := server.indexOf(markerClearCommand(testNodeName))
		if mark < 0 || reboot < 0 || clear < 0 {
			t.Fatalf("the single-server run must mark, reboot and clear:\n%s", commandLog(server))
		}
		if !(mark < reboot && reboot < clear) {
			t.Errorf("the order is wrong: marker=%d reboot=%d clear=%d; clearing before the reboot would tell the backend a reboot had finished while it had not started", mark, reboot, clear)
		}
	})

	t.Run("a --k3s-only restart clears it too", func(t *testing.T) {
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		server.on("sudo -n systemctl restart k3s", ok(""))
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true, K3sOnly: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err != nil {
			t.Fatalf("--k3s-only failed: %v\n%s", err, out)
		}
		mark := server.indexOf(markerCommand(testNodeName))
		restart := server.indexOf("sudo -n systemctl restart k3s")
		clear := server.indexOf(markerClearCommand(testNodeName))
		if mark < 0 || restart < 0 || clear < 0 {
			t.Fatalf("--k3s-only must mark and clear as well as restart:\n%s", commandLog(server))
		}
		if !(mark < restart && restart < clear) {
			t.Errorf("the order is wrong: marker=%d restart=%d clear=%d", mark, restart, clear)
		}
	})

	t.Run("a node that never comes back keeps the marker", func(t *testing.T) {
		server := scriptedServer(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		agent := scriptedAgent(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		agent.prepend("systemctl is-active k3s-agent", fail(3, "inactive\n"))
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost(), agentHost()},
			server: server,
			target: agent,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err == nil {
			t.Fatalf("a node that never came back was reported as rebooted:\n%s", out)
		}
		if !server.hasCommand(markerCommand(testAgentNode)) {
			t.Fatalf("the node was never marked:\n%s", commandLog(server))
		}
		if server.hasCommand(markerClearCommand(testAgentNode)) {
			t.Errorf("the marker was cleared although the node never came back: the backend would stop reporting an overrun for a host that is still down")
		}
	})

	t.Run("a drain that cannot finish clears the marker it wrote", func(t *testing.T) {
		// Nothing was taken down, so nothing may be left claiming a reboot is
		// under way: the refusal puts the cluster back the way it found it.
		server := scriptedServer(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		server.prepend("kubectl drain", fail(1, "error when evicting pods: Cannot evict pod as it would violate the pod's disruption budget"))
		agent := scriptedAgent(t,
			testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
			testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
		)
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost(), agentHost()},
			server: server,
			target: agent,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err == nil {
			t.Fatalf("a drain that could not finish was accepted:\n%s", out)
		}
		if !server.hasCommand(markerCommand(testAgentNode)) {
			t.Fatalf("the node was never marked:\n%s", commandLog(server))
		}
		if !server.hasCommand(markerClearCommand(testAgentNode)) {
			t.Errorf("the marker outlived a refusal that took nothing down, so the node reads as rebooting forever:\n%s", commandLog(server))
		}
		if agent.hasCommand("systemctl reboot") {
			t.Error("the host was rebooted after its drain failed")
		}
	})
}
