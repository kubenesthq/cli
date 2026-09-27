package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/day2"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
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
