package node

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

// The commands that carry the cluster-DNS layout. The replica count is in the
// PATCH BODY, not in the command, which is why every assertion below reads the
// streamed payload as well as the command.
const (
	corednsPatchCmd = "patch deployment coredns"
	corednsPodCmd   = "get pods -n kube-system -l k8s-app=kube-dns -o json"
)

// oneNodeJSON is the cluster's node list before a machine joins: the server
// alone. It is built here rather than taken from joinedNodes so the count the
// layout reads is the test's own.
func oneNodeJSON(t *testing.T) string {
	t.Helper()
	return nodesJSON(t,
		testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
	)
}

// clusterOf is the cluster's node list for the machines the fixture knows:
// the server and the agent keep the UIDs the inventory records for them (a
// node whose UID the record does not know is refused as a rebuilt host), and
// any other name is a machine that has just joined.
func clusterOf(t *testing.T, names ...string) string {
	t.Helper()
	uids := map[string]string{testServerNode: "uid-srv", testAgentNode: "uid-agt"}
	nodes := make([]testNode, 0, len(names))
	for _, name := range names {
		uid, ok := uids[name]
		if !ok {
			uid = "uid-" + name
		}
		nodes = append(nodes, testNode{Name: name, UID: uid, Addresses: []string{testServerAddr}, Ready: true})
	}
	return nodesJSON(t, nodes...)
}

// assertCoreDNSPatched is the layout assertion: the verb patched the CoreDNS
// Deployment TO `replicas`, through a SERVER connection, and waited for the
// pods rather than only writing the field.
func assertCoreDNSPatched(t *testing.T, f *nodeFixture, replicas int) {
	t.Helper()
	if !f.log.hasCommand(corednsPatchCmd) {
		t.Fatalf("no CoreDNS replica patch was issued, so this cluster keeps the replica count k3s shipped and its DNS layout follows nothing; what ran on the server was:\n%s",
			strings.Join(f.log.commands(testServerAddr), "\n"))
	}
	onServer := false
	for _, cmd := range f.log.commands(testServerAddr) {
		if strings.Contains(cmd, corednsPatchCmd) {
			onServer = true
		}
	}
	if !onServer {
		t.Errorf("the CoreDNS patch did not go through a server node: %v", f.log.commands(testServerAddr))
	}
	want := fmt.Sprintf(`"replicas":%d`, replicas)
	if got := f.log.inputOf(corednsPatchCmd); !strings.Contains(got, want) {
		t.Errorf("the patch body does not carry %s:\n%s", want, got)
	}
	if !f.log.hasCommand(corednsPodCmd) {
		t.Errorf("the replica count was written and never checked: the field the patch set does not say whether a second CoreDNS pod ever became Ready")
	}
}

// A cluster that GROWS from one node to two must leave with CoreDNS on both.
//
// This is the half of kn-t43 the installer cannot cover: it ran when there was
// one node, and it does not run again. Without this, the cluster that just
// gained a machine still has k3s's single CoreDNS replica, and losing the node
// that holds it stops every in-cluster name lookup — which is what made a
// Velero restore on lab w3 fail on its own backup target.
func TestAddResizesClusterDNSWhenTheClusterGrows(t *testing.T) {
	f := newFixture(t, serverHost())
	f.server.setNodes(oneNodeJSON(t))
	const newNode = "prod-1-agt-2"
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(clusterOf(t, testServerNode, newNode))
		return sshx.Result{}, nil
	})

	if _, err := f.runAdd(f.newAdd(AddOptions{})); err != nil {
		t.Fatalf("the add failed: %v\n%s", err, f.out.String())
	}
	assertCoreDNSPatched(t, f, 2)
}

// The mirror of it: a cluster that SHRINKS from two nodes to one must come back
// to a single replica.
//
// Two replicas under the required one-pod-per-node anti-affinity on a one-node
// cluster leaves one Pending for the life of the cluster, and nothing else
// would ever put it right.
func TestRemoveResizesClusterDNSWhenTheClusterShrinks(t *testing.T) {
	f := removeFixture(t)
	// The delete lands, and the cluster's node list shrinks with it: the layout
	// is read after the Node object is gone, which is what makes the count
	// one.
	f.server.on("delete node "+testAgentNode, func(h *fakeHost, _ string) (sshx.Result, error) {
		h.setNodes(oneNodeJSON(t))
		return sshx.Result{}, nil
	})

	if _, err := f.runRemove(f.newRemove(RemoveOptions{})); err != nil {
		t.Fatalf("the removal failed: %v\n%s", err, f.out.String())
	}
	assertCoreDNSPatched(t, f, 1)
}

// Growth beyond two nodes re-asserts the layout and changes nothing: the rule
// is two replicas spread one per node, not one replica per node.
func TestAddReAssertingTheLayoutOnALargerClusterIsHarmless(t *testing.T) {
	f := newFixture(t, serverHost(), agentHost())
	const newNode = "prod-1-agt-2"
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(clusterOf(t, testServerNode, testAgentNode, newNode))
		return sshx.Result{}, nil
	})

	if _, err := f.runAdd(f.newAdd(AddOptions{})); err != nil {
		t.Fatalf("the add failed: %v\n%s", err, f.out.String())
	}
	assertCoreDNSPatched(t, f, 2)
	if got := f.log.count(corednsPatchCmd); got != 1 {
		t.Errorf("%d CoreDNS patch(es), want exactly one for this run", got)
	}
}

// A resumed add whose record stage had not run still ends with the right
// layout: the step sits in a stage a resume re-runs, and it is idempotent, so
// "the previous process did not get to it" is not a state the cluster can be
// left in.
func TestAResumedAddStillResizesClusterDNS(t *testing.T) {
	f := newFixture(t, serverHost())
	f.server.setNodes(oneNodeJSON(t))
	const newNode = "prod-1-agt-2"
	// The FIRST run's join fails, which is the interruption this test is about:
	// nothing after the join stage ran, so the layout was never touched.
	tries := 0
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		tries++
		if tries == 1 {
			return sshx.Result{ExitCode: 1, Stderr: "curl: (7) Failed to connect to get.k3s.io"}, nil
		}
		f.server.setNodes(clusterOf(t, testServerNode, newNode))
		return sshx.Result{}, nil
	})

	if _, err := f.runAdd(f.newAdd(AddOptions{})); err == nil {
		t.Fatal("the planted join failure did not fail the run")
	}
	if got := f.log.count(corednsPatchCmd); got != 0 {
		t.Fatalf("the interrupted run issued %d CoreDNS patch(es) although it never reached the record stage", got)
	}

	// A second process: the same world, the same journal, a new run id.
	next := &Add{Session: f.sessionFor(t, "run-2"), Opts: AddOptions{Agent: testAgentAddr}}
	_, resumeErr := stages.Execute(context.Background(), next, PlanAdd(next))
	next.Finish(context.Background(), resumeErr, false)
	if resumeErr != nil {
		t.Fatalf("the resume failed: %v\n%s", resumeErr, f.out.String())
	}
	assertCoreDNSPatched(t, f, 2)
	if got := f.log.count(corednsPatchCmd); got != 1 {
		t.Errorf("the resumed run left %d CoreDNS patch(es) in the log, want exactly the one it applied", got)
	}
}

// podsOnEveryNode is the CoreDNS pod list a cluster with this node list has:
// one Ready pod per node. It is the honest model of a cluster whose DNS is
// healthy, and it carries the property the swap depends on — the pod that sat
// on a node disappears with that node, and the replacement's pod appears on the
// node that joined.
func podsOnEveryNode(t *testing.T, nodes string) string {
	t.Helper()
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(nodes), &list); err != nil {
		t.Fatalf("the fixture's node list is not JSON: %v", err)
	}
	items := make([]map[string]any, 0, len(list.Items))
	for i, n := range list.Items {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": fmt.Sprintf("coredns-%d", i)},
			"spec":     map[string]any{"nodeName": n.Metadata.Name},
			"status": map[string]any{
				"phase":      "Running",
				"conditions": []map[string]any{{"type": "Ready", "status": "True"}},
			},
		})
	}
	raw, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A replace composes the add and remove halves (PlanReplaceBranch -> addHalf,
// removeHalf), and the halves keep the verbs' OWN stage functions, so each
// half's record stage runs the layout step and the cluster ends with the
// replica count that its — unchanged — node count calls for.
//
// THE DEAD-NODE CASE IS WHY THIS MATTERS. The replica that sat on the machine
// being replaced is gone with its Node object, and the layout step's wait
// requires the replacement's pod to be Ready on a node DIFFERENT from the
// survivor: the pod list here is built from the node list, so the run only
// succeeds once the replacement has actually taken the replica.
func TestReplaceKeepsTheClusterDNSLayoutAcrossTheSwap(t *testing.T) {
	f := replaceFixture(t, true, true)
	const newNode = "prod-1-agt-9"
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		f.server.setNodes(clusterOf(t, testServerNode, testAgentNode, newNode))
		return sshx.Result{}, nil
	})
	f.server.on("delete node "+testAgentNode, func(h *fakeHost, _ string) (sshx.Result, error) {
		// The count is KEPT: one machine leaves and one joins.
		h.setNodes(clusterOf(t, testServerNode, newNode))
		return sshx.Result{}, nil
	})
	f.server.on(corednsPodCmd, func(h *fakeHost, _ string) (sshx.Result, error) {
		h.mu.Lock()
		nodes := h.nodes
		h.mu.Unlock()
		return sshx.Result{Stdout: podsOnEveryNode(t, nodes)}, nil
	})

	if _, err := f.runReplace(f.newReplace(ReplaceOptions{})); err != nil {
		t.Fatalf("the replace failed: %v\n%s", err, f.out.String())
	}
	assertCoreDNSPatched(t, f, 2)
	patches := f.log.inputsOf(corednsPatchCmd)
	if len(patches) != 2 {
		t.Fatalf("%d layout patch(es) across the two halves, want one each: %v", len(patches), patches)
	}
	for i, patch := range patches {
		if !strings.Contains(patch, `"replicas":2`) {
			t.Errorf("half %d patched the layout to %s, want two replicas: a three-node cluster that becomes two needs two, and never more", i+1, patch)
		}
	}
}

// The DEAD-NODE order, which is the case kn-t43 is about: the machine being
// replaced is gone, so the replica it was holding is gone with it. The removal
// half sees a one-node cluster and sizes DNS down to one; the join half then
// sees two nodes again and sizes it back up — and its wait requires the
// replacement's pod to be Ready on a node DIFFERENT from the survivor, so the
// replica really does come back rather than being assumed to.
func TestReplaceAfterAnUnreachableMachineReschedulesTheReplica(t *testing.T) {
	f := replaceFixture(t, false, false)
	const newNode = "prod-1-agt-9"
	f.server.on("delete node "+testAgentNode, func(h *fakeHost, _ string) (sshx.Result, error) {
		h.setNodes(clusterOf(t, testServerNode))
		return sshx.Result{}, nil
	})
	f.agent.on("get.k3s.io", func(h *fakeHost, _ string) (sshx.Result, error) {
		// The count is KEPT: the replacement takes the machine it replaced.
		f.server.setNodes(clusterOf(t, testServerNode, newNode))
		return sshx.Result{}, nil
	})
	f.server.on(corednsPodCmd, func(h *fakeHost, _ string) (sshx.Result, error) {
		h.mu.Lock()
		nodes := h.nodes
		h.mu.Unlock()
		return sshx.Result{Stdout: podsOnEveryNode(t, nodes)}, nil
	})

	if _, err := f.runReplace(f.newReplace(ReplaceOptions{ConfirmIsolated: true})); err != nil {
		t.Fatalf("the replace failed: %v\n%s", err, f.out.String())
	}
	patches := f.log.inputsOf(corednsPatchCmd)
	if len(patches) != 2 {
		t.Fatalf("%d layout patch(es), want one from each half: %v", len(patches), patches)
	}
	if !strings.Contains(patches[0], `"replicas":1`) {
		t.Errorf("the removal half patched the layout to %s: with the machine gone the cluster has ONE node, and two replicas under the required spread would leave one Pending for good", patches[0])
	}
	if !strings.Contains(patches[1], `"replicas":2`) {
		t.Errorf("the join half patched the layout to %s, so the replica the dead machine was holding never came back", patches[1])
	}
}
