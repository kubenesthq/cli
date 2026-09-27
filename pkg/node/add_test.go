package node

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/window"
)

// runAdd drives the real staging engine over the real stage sequence, and
// closes the operation the way the command does — so what these tests exercise
// is the verb and not a rehearsal of it.
func (f *nodeFixture) runAdd(a *Add) (stages.Result, error) {
	f.t.Helper()
	result, err := stages.Execute(context.Background(), a, PlanAdd(a))
	a.Finish(context.Background(), err, false)
	return result, err
}

// newAdd is one `node add` against the fixture's world.
func (f *nodeFixture) newAdd(opts AddOptions) *Add {
	if opts.Agent == "" {
		opts.Agent = testAgentAddr
	}
	return &Add{Session: f.session, Opts: opts}
}

// The pre-flight checks decide whether the machine may join at all, and they
// run before anything is written — to the host, to the cluster or to the
// record. A release the bundle was never tested on is refused with nothing
// written, which is what makes abandoning the run free.
func TestAddDoesNotStartOnAnUnsupportedUbuntu(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	f.agent.on("/etc/os-release", ok("ID=ubuntu\nVERSION_ID=\"22.04\"\nPRETTY_NAME=\"Ubuntu 22.04.3 LTS\"\n"))

	_, err := f.runAdd(f.newAdd(AddOptions{}))
	if err == nil {
		t.Fatal("an unsupported Ubuntu release was accepted")
	}
	if !strings.Contains(err.Error(), "ubuntu-24.04") {
		t.Errorf("the refusal does not name the releases the bundle is tested on: %v", err)
	}
	if f.log.hasCommand("get.k3s.io") {
		t.Error("the join was attempted on a release the bundle does not support")
	}
	if len(f.records.savedRecords()) != 0 {
		t.Errorf("%d inventory write(s) were issued before the pre-flight checks passed: a host must not be recorded as joining before it is allowed to join", len(f.records.savedRecords()))
	}
	if f.log.hasCommand("create -f -") || f.log.hasCommand("inventory-write") {
		t.Error("the operation record or the inventory was written before pre-flight passed")
	}
}

// A join goes through a server that ANSWERS and is Ready, not through the
// first server the inventory happens to list: a NotReady server fails in a way
// that looks like a fault in the machine being added.
func TestAddJoinsThroughAReadyServerFromTheInventory(t *testing.T) {
	// The inventory lists the NotReady server FIRST, which is what makes the
	// difference observable.
	notReady := serverHost()
	notReady.HostID, notReady.SSHAddress = "h-srv-a", "10.0.3.6"
	notReady.NodeUID, notReady.HostKeyFingerprint = "uid-srv-a", "SHA256:server-a"
	f := newFixture(t, notReady, serverHost(), agentHost())
	f.addHost("10.0.3.6", "SHA256:server-a", nodesJSON(t,
		testNode{Name: "prod-1-srv-a", UID: "uid-srv-a", Addresses: []string{"10.0.3.6"}, Ready: false},
	))
	const newNode = "prod-1-agt-2"
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(joinedNodes(t, newNode, true))
		return sshx.Result{}, nil
	})

	if _, err := f.runAdd(f.newAdd(AddOptions{})); err != nil {
		t.Fatalf("the add was refused although a Ready server was in the inventory: %v\n%s", err, f.out.String())
	}
	if !f.log.hasCommand("label node " + newNode) {
		t.Errorf("the hold was not applied through the Ready server; what ran on 10.0.3.6 was:\n%s",
			strings.Join(f.log.commands("10.0.3.6"), "\n"))
	}
	// Every write went through the server that answered: the NotReady one was
	// talked to (to find out it was NotReady) and nothing else.
	for _, cmd := range f.log.commands("10.0.3.6") {
		if strings.Contains(cmd, "kubectl") && !strings.Contains(cmd, "get ") {
			t.Errorf("a write went through the NotReady server: %q", cmd)
		}
	}
	if f.records.inventory()[len(f.records.inventory())-1].LifecycleState != string(StateActive) {
		t.Error("the host was not recorded as active")
	}
}

// joinedNodes is the cluster while the new node is joining: the two nodes the
// fixture starts with, plus the new one.
func joinedNodes(t *testing.T, name string, ready bool) string {
	t.Helper()
	return nodesJSON(t,
		testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{"10.0.3.8"}, Ready: true},
		testNode{Name: name, UID: "uid-new", Addresses: []string{testAgentAddr, "10.0.4.9"}, Ready: ready},
	)
}

// The order the writes happen in IS the safety property: the host is written
// down as joining before it is touched, it is held as soon as its Node object
// exists and before anything waits for it to be Ready, it is marked active
// only once it IS Ready, and the hold is lifted last.
func TestAddWritesJoiningBeforeTheJoinAndActiveOnlyAfterReady(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	const newNode = "prod-1-agt-2"
	joined := false
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		joined = true
		f.server.setNodes(joinedNodes(t, newNode, false))
		return sshx.Result{}, nil
	})
	// The node appears without the hold and NOT Ready; the read that discovers
	// it is not the readiness observation, and the probe that observes it
	// Ready is the second read after the join. So the marker below is a real
	// observation of the node becoming Ready, and the hold has to be in place
	// before it.
	postJoin := 0
	f.server.on("get nodes -o json", func(h *fakeHost, _ string) (sshx.Result, error) {
		if !joined {
			return sshx.Result{Stdout: nodesJSON(t,
				testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
				testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{"10.0.3.8"}, Ready: true},
			)}, nil
		}
		postJoin++
		if postJoin == 1 {
			return sshx.Result{Stdout: joinedNodes(t, newNode, false)}, nil
		}
		if postJoin == 2 {
			f.log.record("marker", "observed the new node Ready", nil)
		}
		return sshx.Result{Stdout: joinedNodes(t, newNode, true)}, nil
	})

	if _, err := f.runAdd(f.newAdd(AddOptions{})); err != nil {
		t.Fatalf("the add failed: %v\n%s", err, f.out.String())
	}

	joiner := hostWithAddress(f.records.inventory(), testAgentAddr)
	if joiner.HostID == "" {
		t.Fatal("the new machine was never written into the inventory")
	}
	joining := f.log.indexOf("inventory-write " + joiner.HostID + " joining")
	active := f.log.indexOf("inventory-write " + joiner.HostID + " active")
	install := f.log.indexOf("get.k3s.io")
	label := f.log.indexOf("label node " + newNode + " " + day2.NoAutoRebootLabel + "=" + day2.NoAutoRebootValue)
	observedReady := f.log.indexOf("observed the new node Ready")
	lift := f.log.indexOf("label node " + newNode + " " + day2.NoAutoRebootLabel + "-")

	switch {
	case joining < 0:
		t.Fatal("the host was never recorded as joining")
	case install < 0:
		t.Fatal("the agent was never installed")
	case joining > install:
		t.Errorf("the host was written down as joining AFTER the join was attempted (record %d, join %d): an interrupted join must leave a host that is identifiable", joining, install)
	case label < 0:
		t.Fatal("the node never carried the reboot hold")
	case label < install:
		t.Errorf("the hold was applied before the machine was joined (hold %d, join %d)", label, install)
	case observedReady >= 0 && label > observedReady:
		t.Errorf("the hold was applied only after the node was observed Ready (hold %d, ready %d): it must be held while it is still coming up", label, observedReady)
	case active < 0:
		t.Fatal("the host was never marked active")
	case observedReady >= 0 && active < observedReady:
		t.Errorf("the host was marked active before the node was observed Ready (active %d, ready %d)", active, observedReady)
	case lift < 0:
		t.Fatal("the hold was never lifted")
	case lift < active:
		t.Errorf("the hold was lifted before the host was recorded as active (lift %d, active %d): an unrecorded node must stay out of kured's pool", lift, active)
	}

	entry, _ := f.hostIn(joiner.HostID)
	if entry.LifecycleState != string(StateActive) || entry.NodeUID != "uid-new" {
		t.Errorf("the final inventory entry is %+v, want an active host with the node's UID", entry)
	}
	if f.server.kuredLock() != "" {
		t.Errorf("kured's lock is still held after the operation finished: %s", f.server.kuredLock())
	}
	if f.meta.calls != 1 {
		t.Errorf("%d recovery-metadata refreshes, want exactly 1", f.meta.calls)
	}
}

// A resume finishes the work an interrupted run left: the stages the journal
// says completed are not run again, so a join that already happened is not
// repeated, and the inventory write it was waiting on is performed.
func TestAddResumeFinishesTheInventoryWriteWithoutJoiningTwice(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	const newNode = "prod-1-agt-2"
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(joinedNodes(t, newNode, true))
		return sshx.Result{}, nil
	})
	// The FIRST run's active write does not land, which is the interruption
	// this test is about: the host joined, and the record does not say so yet.
	f.records.failOn = 2

	first := f.newAdd(AddOptions{})
	if _, err := f.runAdd(first); err == nil {
		t.Fatal("the planted inventory-write failure did not fail the run")
	}
	joiner := hostWithAddress(f.records.inventory(), testAgentAddr)
	if joiner.LifecycleState != string(StateJoining) {
		t.Fatalf("the interrupted run left the host as %q, want %q", joiner.LifecycleState, StateJoining)
	}

	installsBefore := f.log.count("get.k3s.io")
	// A second process: the same world, the same journal, a new run id.
	session := f.sessionFor(t, "run-2")
	next := &Add{Session: session, Opts: AddOptions{Agent: testAgentAddr}}
	_, resumeErr := stages.Execute(context.Background(), next, PlanAdd(next))
	next.Finish(context.Background(), resumeErr, false)
	if resumeErr != nil {
		t.Fatalf("the resume failed: %v\n%s", resumeErr, f.out.String())
	}
	if got := f.log.count("get.k3s.io"); got != installsBefore {
		t.Errorf("the resume installed the agent again (%d installs, was %d): the journal says the join completed, so it must not be repeated", got, installsBefore)
	}
	if state := f.lastState(joiner.HostID); state != string(StateActive) {
		t.Errorf("the resume left the host as %q, want %q", state, StateActive)
	}
	if f.meta.calls != 1 {
		t.Errorf("%d recovery-metadata refreshes after the resume, want exactly 1", f.meta.calls)
	}
}

// addedHostAt is the entry an add wrote for a machine at an address a removed
// entry already holds: everything at that address except the removed record.
// Exactly one is expected, and the count is the assertion that a resume did not
// mint the machine a second identity.
func addedHostAt(t *testing.T, f *nodeFixture, address, removedHostID string) api.HostRecord {
	t.Helper()
	var added []api.HostRecord
	for _, h := range f.records.inventory() {
		if h.SSHAddress == address && h.HostID != removedHostID {
			added = append(added, h)
		}
	}
	if len(added) != 1 {
		t.Fatalf("%d entries exist for %s besides the removed %s, want exactly 1: %+v", len(added), address, removedHostID, added)
	}
	return added[0]
}

// A removed entry keeps its address, and an address is handed to the next
// machine that asks for it: a static or elastic address moved to a replacement
// VM, or a cloud giving a freed address to the next machine. The address alone
// cannot tell the machine that was removed from a new one wearing its address,
// and the host key that answers when it is dialled can.
//
// Hardware, 2026-09-27 (lab w3): w4 (167.233.20.250) was removed, destroyed, and
// the next machine (w5) was given the same address; `node add --agent
// 167.233.20.250` was refused before it connected. The advice to wipe the
// machine cannot help, because the match is on the address — after any
// `node remove`, no new machine at that address could be added.
func TestAddGivesANewMachineTheAddressOfARemovedHost(t *testing.T) {
	f := newFixture(t, serverHost(), removedAgent())
	const newNode = "prod-1-agt-2"
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(joinedNodes(t, newNode, true))
		return sshx.Result{}, nil
	})

	if _, err := f.runAdd(f.newAdd(AddOptions{})); err != nil {
		t.Fatalf("a new machine at a removed host's address was refused: %v\n%s", err, f.out.String())
	}

	// The removed record is left exactly as it was: the same host ID, the same
	// state, the same host key and the same node it became.
	gone, found := f.hostIn("h-gone")
	if !found {
		t.Fatal("the removed host's entry is gone from the inventory")
	}
	if gone.LifecycleState != string(StateRemoved) || gone.HostKeyFingerprint != "SHA256:gone" || gone.NodeUID != "uid-gone" {
		t.Errorf("the address reuse changed the removed entry: %+v", gone)
	}

	// The machine that answered is a NEW host: a host ID of its own, active,
	// carrying the host key it presented.
	added := addedHostAt(t, f, testAgentAddr, "h-gone")
	if added.HostID == "h-gone" {
		t.Error("the new machine was given the removed machine's host ID")
	}
	if added.LifecycleState != string(StateActive) {
		t.Errorf("the new machine is recorded as %q, want %q: %+v", added.LifecycleState, StateActive, added)
	}
	if added.HostKeyFingerprint != "SHA256:newagent" {
		t.Errorf("the new host records host key %q, want the key the machine answered with", added.HostKeyFingerprint)
	}
	if added.NodeUID != "uid-new" {
		t.Errorf("the new host records node uid %q, want the node that joined", added.NodeUID)
	}

	// The operator is told why this was allowed: the run names the removed
	// record whose address the machine was given, rather than leaving them to
	// guess which record it touched.
	if !strings.Contains(f.out.String(), "h-gone") {
		t.Errorf("the run does not name the removed host whose address was reused:\n%s", f.out.String())
	}
}

// The same address where the machine is the one the removed entry describes is
// still refused, and so is an entry that records no host key at all: the machine
// that was removed may be the one answering, and its disks may still hold
// another cluster's data. Neither case is proof of a DIFFERENT machine, and only
// a different machine may take a removed host's address. Nothing is written and
// nothing is joined.
func TestAddStillRefusesTheMachineRecordedAsRemovedAtItsAddress(t *testing.T) {
	cases := []struct {
		name     string
		recorded string
	}{
		{"the machine answers with the recorded host key", "SHA256:newagent"},
		{"the entry records no host key", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gone := removedAgent()
			gone.HostKeyFingerprint = tc.recorded
			f := newFixture(t, serverHost(), gone)

			_, err := f.runAdd(f.newAdd(AddOptions{}))
			if err == nil {
				t.Fatalf("the machine recorded as removed was re-added at its own address:\n%s", f.out.String())
			}
			if !f.dialer.dialed(testAgentAddr) {
				t.Error("the machine at the removed host's address was never dialled, so the two host keys were never compared")
			}
			if !strings.Contains(err.Error(), "h-gone") {
				t.Errorf("the refusal does not name the record it refused: %v", err)
			}
			if len(f.records.savedRecords()) != 0 {
				t.Errorf("%d inventory write(s) happened, want none: a refused machine is not written anywhere", len(f.records.savedRecords()))
			}
			if state := f.lastState("h-gone"); state != string(StateRemoved) {
				t.Errorf("the removed entry is now %q, want %q", state, StateRemoved)
			}
			if f.log.hasCommand("get.k3s.io") {
				t.Error("the join was attempted on the machine that was removed")
			}
		})
	}
}

// `--agent` naming a removed host by its HOST ID names the record and not a
// machine, so it is refused as it always was — and nothing is dialled, because
// there is nothing about a machine to compare.
func TestAddRefusesARemovedHostNamedByItsHostID(t *testing.T) {
	f := newFixture(t, serverHost(), removedAgent())

	_, err := f.runAdd(f.newAdd(AddOptions{Agent: "h-gone"}))
	if err == nil {
		t.Fatalf("a removed host was re-added by its host ID:\n%s", f.out.String())
	}
	if !strings.Contains(err.Error(), "h-gone") {
		t.Errorf("the refusal does not name the record it refused: %v", err)
	}
	if f.dialer.dialed(testAgentAddr) {
		t.Error("the machine was dialled although the removed host was named by its host ID")
	}
	if len(f.records.savedRecords()) != 0 {
		t.Errorf("%d inventory write(s) happened for a refused machine", len(f.records.savedRecords()))
	}
}

// A resumed add that gave a new machine a removed host's address continues on
// the entry IT wrote, not on the removed record that shares the address and
// comes earlier in the inventory: taking the removed one would dial a machine
// the journal already knows, mint it a second host ID, and leave a stale
// joining entry at the address.
func TestAddResumeContinuesTheNewHostGivenARemovedAddress(t *testing.T) {
	f := newFixture(t, serverHost(), removedAgent())
	const newNode = "prod-1-agt-2"
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(joinedNodes(t, newNode, true))
		return sshx.Result{}, nil
	})
	// The first run's ACTIVE write does not land, which is the interruption
	// this test is about: the host joined, and the record does not say so yet.
	f.records.failOn = 2

	first := f.newAdd(AddOptions{})
	if _, err := f.runAdd(first); err == nil {
		t.Fatal("the planted inventory-write failure did not fail the run")
	}
	joiner := addedHostAt(t, f, testAgentAddr, "h-gone")
	if joiner.LifecycleState != string(StateJoining) {
		t.Fatalf("the interrupted run left the new host as %q, want %q", joiner.LifecycleState, StateJoining)
	}

	// A second process: the same world, the same journal, a new run id.
	session := f.sessionFor(t, "run-2")
	next := &Add{Session: session, Opts: AddOptions{Agent: testAgentAddr}}
	_, resumeErr := stages.Execute(context.Background(), next, PlanAdd(next))
	next.Finish(context.Background(), resumeErr, false)
	if resumeErr != nil {
		t.Fatalf("the resume failed: %v\n%s", resumeErr, f.out.String())
	}

	after := addedHostAt(t, f, testAgentAddr, "h-gone")
	if after.HostID != joiner.HostID {
		t.Errorf("the resume put the machine under host ID %s, want the %s the interrupted run minted: the removed entry at this address was mistaken for the machine", after.HostID, joiner.HostID)
	}
	if after.LifecycleState != string(StateActive) {
		t.Errorf("the resume left the new host as %q, want %q", after.LifecycleState, StateActive)
	}
	if state := f.lastState("h-gone"); state != string(StateRemoved) {
		t.Errorf("the resume changed the removed entry to %q, want %q", state, StateRemoved)
	}
}

// A second operation against the same cluster is refused by NAME: the record
// is the lock, and "another operation is running" without saying who is a
// refusal an operator cannot act on.
func TestAddRefusesWhileAnotherOperationRuns(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	// Another laptop's live record, taken straight from the store the verb
	// uses, so this is the real lock and not a stub.
	store := &operation.Store{Runner: f.server, Operator: "ana@laptop"}
	if _, err := store.Acquire(context.Background(), operation.Request{
		Kind: operation.KindNodeRemove, Cluster: "prod-1",
		Targets: []operation.Target{{HostID: "h-agt"}},
	}); err != nil {
		t.Fatal(err)
	}

	_, err := f.runAdd(f.newAdd(AddOptions{Wait: false}))
	if err == nil {
		t.Fatal("a second operation was accepted while another one held the record")
	}
	if !errors.Is(err, operation.ErrLocked) {
		t.Fatalf("the refusal is not the record's own refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "ana@laptop") {
		t.Errorf("the refusal does not name the running operation's executor: %v", err)
	}
	if f.log.hasCommand("get.k3s.io") {
		t.Error("the join was attempted despite the refusal")
	}
}

// Outside the maintenance window a disruptive verb refuses and names the next
// opening in LOCAL TIME AND UTC, because an operator reading it has to be able
// to decide whether to wait.
func TestAddOutsideTheWindowRefusesAndNamesTheOpening(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	// 09:00 UTC on a Sunday; the window is 02:00-06:00, so it closed three
	// hours ago and opens tomorrow.
	closed := &window.Window{Days: []time.Weekday{time.Sunday}, Start: 2 * 60, End: 6 * 60, Location: time.UTC}
	f.session.Window = closed

	_, err := f.runAdd(f.newAdd(AddOptions{}))
	if err == nil {
		t.Fatal("a disruptive operation was accepted outside the maintenance window")
	}
	for _, want := range []string{"next opens", "UTC"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q: %v", want, err)
		}
	}
	if f.log.hasCommand("get.k3s.io") || f.log.hasCommand("create -f -") {
		t.Error("something was changed outside the maintenance window")
	}
	if len(f.records.savedRecords()) != 0 {
		t.Error("the inventory was written outside the maintenance window")
	}
}

// --wait holds until the window opens, HOLDING NOTHING while it waits: the
// operation record and kured's lock are taken only once the window is open.
func TestAddWaitTakesTheLocksOnlyWhenTheWindowOpens(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	// The window opens at 10:00; the run starts at 09:00.
	opening := &window.Window{Days: []time.Weekday{time.Sunday}, Start: 10 * 60, End: 23 * 60, Location: time.UTC}
	f.session.Window = opening
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(joinedNodes(t, "prod-1-agt-2", true))
		return sshx.Result{}, nil
	})

	if _, err := f.runAdd(f.newAdd(AddOptions{Wait: true})); err != nil {
		t.Fatalf("a --wait run that the window opened for failed: %v\n%s", err, f.out.String())
	}
	if len(*f.slept) == 0 {
		t.Fatal("the run did not wait at all, so this test proves nothing")
	}
	recorded, ok := f.log.at("create -f - -o json")
	if !ok {
		t.Fatal("the operation record was never created")
	}
	if !opening.Contains(recorded) {
		t.Errorf("the operation record was created at %s, which is outside the window %s", recorded, opening)
	}
	if f.now.Before(recorded) {
		t.Errorf("the fake clock reads %s, before the record was created at %s", f.now, recorded)
	}
}

// hostWithAddress finds an inventory entry by address.
func hostWithAddress(hosts []api.HostRecord, address string) api.HostRecord {
	for _, h := range hosts {
		if h.SSHAddress == address {
			return h
		}
	}
	return api.HostRecord{}
}

// hasCommandOn reports whether any command run on one host contains substr.
func hasCommandOn(commands []string, substr string) bool {
	for _, cmd := range commands {
		if strings.Contains(cmd, substr) {
			return true
		}
	}
	return false
}

// joiningEntryFor finds the entry written for one host while it was JOINING —
// the write that happens before the volume group exists on it.
func joiningEntryFor(t *testing.T, records []api.BundleRecord, address string) api.HostRecord {
	t.Helper()
	for _, rec := range records {
		for _, h := range rec.Hosts {
			if h.SSHAddress == address && h.LifecycleState == string(StateJoining) {
				return h
			}
		}
	}
	t.Fatalf("no joining entry for %s was ever written: %+v", address, records)
	return api.HostRecord{}
}

// The joining entry is written BEFORE the volume group exists on the new host,
// and the control plane requires one of the two ownership values on every host
// entry, including a joining one (app/schemas/cluster.py:235). `--storage-device`
// means the installer will create kubenest-vg; no `--storage-device` means the
// operator created it, and preflight has just proven it exists.
//
// Hardware, 2026-09-27: without this the joining write carried
// volume_group_ownership "" and the control plane answered 422, so every add
// stopped at record-joining.
func TestAddRecordsTheVolumeGroupOwnershipInTheJoiningEntry(t *testing.T) {
	cases := []struct {
		name   string
		device string
		want   string
	}{
		{"--storage-device: the installer will create it", testDevice, string(storage.InstallerCreated)},
		{"no --storage-device: the operator created it", "", string(storage.CustomerCreated)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, serverHost(), agentHost())
			if tc.device != "" {
				// A fresh host: no volume group yet, so the blank device is the
				// path being asked for.
				f.agent.on("vgs", fail(1, "Volume group \"kubenest-vg\" not found"))
			}
			const newNode = "prod-1-agt-2"
			f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
				f.server.setNodes(joinedNodes(t, newNode, true))
				return sshx.Result{}, nil
			})

			if _, err := f.runAdd(f.newAdd(AddOptions{StorageDevice: tc.device})); err != nil {
				t.Fatalf("the add failed: %v\n%s", err, f.out.String())
			}
			entry := joiningEntryFor(t, f.records.savedRecords(), testAgentAddr)
			if entry.VolumeGroupOwnership != tc.want {
				t.Errorf("the joining entry carries volume_group_ownership %q, want %q", entry.VolumeGroupOwnership, tc.want)
			}
		})
	}
}

// An inventory write the control plane refuses for its BODY is a 422, not a
// revision conflict: "the control plane refuses a write based on a revision
// another operator has moved on from" sends the operator looking for a
// conflict that is not there. Only the 409 compare-and-swap gets that advice.
func TestAnInventoryWriteRefusedForItsBodyDoesNotClaimARevisionConflict(t *testing.T) {
	refusal := func(status int) error {
		return apiRefusal{
			detail: fmt.Sprintf("PUT /api/v1/clusters/prod-1/bundle: [%d] refused", status),
			err:    &api.Error{Status: status, Detail: "refused"},
		}
	}
	cases := []struct {
		name       string
		status     int
		wantAdvice bool
	}{
		{"422: the body was refused", http.StatusUnprocessableEntity, false},
		{"409: the revision moved on", http.StatusConflict, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, serverHost(), agentHost())
			f.records.failSave = refusal(tc.status)

			_, err := f.runAdd(f.newAdd(AddOptions{}))
			if err == nil {
				t.Fatal("a refused inventory write must fail the stage")
			}
			got := err.Error()
			if strings.Contains(got, "moved on from") != tc.wantAdvice {
				if tc.wantAdvice {
					t.Errorf("a %d is the revision compare-and-swap and must say so:\n%v", tc.status, err)
				} else {
					t.Errorf("a %d must not be described as a revision conflict:\n%v", tc.status, err)
				}
			}
			if !strings.Contains(got, "writing the cluster's host inventory at revision") {
				t.Errorf("the refusal must still name the write it was answering:\n%v", err)
			}
		})
	}
}

// The cluster's existing server is a member of the cluster: it runs k3s and it
// already has kubenest-vg. `node add` uses it as the other end of the
// node-to-node port checks and as the join path, and must NOT run the
// install's HOST checks against it — those refuse any machine that is already
// running the cluster, so running them on the server makes the verb impossible
// against a live cluster.
//
// Hardware, 2026-09-27: a lab-w3 node add was refused with "Existing Kubernetes
// on 5.75.252.113: already present: k3s" and "Volume group on 5.75.252.113:
// kubenest-vg already exists on this node: omit --storage-device".
func TestAddRunsTheHostChecksOnlyOnTheNewHost(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	// The server is a real cluster member...
	f.server.on("command -v", ok("k3s\ncontainerd\n"))
	// ...and the new host is fresh: no kubenest-vg, so --storage-device is the
	// path being asked for.
	f.agent.on("vgs", fail(1, "Volume group \"kubenest-vg\" not found"))
	const newNode = "prod-1-agt-2"
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(joinedNodes(t, newNode, true))
		return sshx.Result{}, nil
	})
	const device = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive2"

	if _, err := f.runAdd(f.newAdd(AddOptions{StorageDevice: device})); err != nil {
		t.Fatalf("an add against a running cluster must pass preflight and join: %v\n%s", err, f.out.String())
	}

	// The server is still the other end of the port checks.
	if !hasCommandOn(f.log.commands(testServerAddr), "kubenest-preflight-port-probe") {
		t.Errorf("the existing server must still be checked as the other end of the node-to-node ports; what ran on it:\n%s",
			strings.Join(f.log.commands(testServerAddr), "\n"))
	}
	// But no host check ran on it. These are the commands the per-host checks
	// issue (OS, privilege, existing Kubernetes, sizing, volume group, egress).
	serverCommands := strings.Join(f.log.commands(testServerAddr), "\n")
	for _, hostCheck := range []string{
		"/etc/os-release", "apt-config dump", "sudo -n true",
		"k3s rke2 kubelet containerd", "MemTotal", "vgs kubenest-vg",
		"blkid -p", "test -b ", "curl -s",
	} {
		if strings.Contains(serverCommands, hostCheck) {
			t.Errorf("the install's host checks must not run on the existing server: %q did", hostCheck)
		}
	}
	// The new host got them, including the existing-Kubernetes check and the
	// blank-device check the flag asks for.
	agentCommands := strings.Join(f.log.commands(testAgentAddr), "\n")
	for _, hostCheck := range []string{"k3s rke2 kubelet containerd", "blkid -p"} {
		if !strings.Contains(agentCommands, hostCheck) {
			t.Errorf("the new host must still get %q; what ran on it:\n%s", hostCheck, agentCommands)
		}
	}
}

// The server's exception is not a general weakening: the machine being added
// still gets every host check, so one that already runs Kubernetes is refused.
func TestAddStillRefusesANewHostThatRunsKubernetes(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	f.agent.on("command -v", ok("k3s\ncontainerd\n"))

	_, err := f.runAdd(f.newAdd(AddOptions{}))
	if err == nil {
		t.Fatal("a machine that already runs Kubernetes must not join")
	}
	if !strings.Contains(err.Error(), "Existing Kubernetes") {
		t.Errorf("the refusal must be the Existing Kubernetes check: %v", err)
	}
	if !strings.Contains(err.Error(), testAgentAddr) {
		t.Errorf("the refusal must be about the new host, not the server: %v", err)
	}
	if f.log.hasCommand("get.k3s.io") {
		t.Error("the join was attempted on a machine the pre-flight refused")
	}
}

// --storage-device is a check on the NEW host: a device that is not blank is
// still refused there, and the refusal names the new host.
func TestAddStillRefusesANewHostWhoseDeviceIsNotBlank(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	f.agent.on("vgs", fail(1, "Volume group \"kubenest-vg\" not found"))
	f.agent.on("blkid -p", ok("TYPE=\"ext4\"\n"))
	const device = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive2"

	_, err := f.runAdd(f.newAdd(AddOptions{StorageDevice: device}))
	if err == nil {
		t.Fatal("a --storage-device that is not blank must be refused")
	}
	if !strings.Contains(err.Error(), device) {
		t.Errorf("the refusal must name the device: %v", err)
	}
	if !strings.Contains(err.Error(), testAgentAddr) {
		t.Errorf("the refusal must be about the new host, not the server: %v", err)
	}
	if strings.Contains(err.Error(), testServerAddr) {
		t.Errorf("the existing server must not be refused: %v", err)
	}
	if f.log.hasCommand("get.k3s.io") {
		t.Error("the join was attempted on a machine the pre-flight refused")
	}
}
