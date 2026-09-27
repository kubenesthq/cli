package k3s

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/sshx"
)

// dnsRunner scripts the two calls EnsureCoreDNSReplicas makes — the patch
// (streamed) and the pod list — and records both, so a test can assert the
// command, the payload and their order. An unscripted command fails the test:
// every remote call this code makes has to be visible in a test, because the
// whole argument for the mechanism is that it touches nothing else.
type dnsRunner struct {
	t    *testing.T
	pods string
	cmds []string
	body []string
}

func (d *dnsRunner) Run(_ context.Context, cmd string) (sshx.Result, error) {
	d.cmds = append(d.cmds, cmd)
	switch {
	case strings.Contains(cmd, "get pods -n kube-system -l "+CoreDNSSelector+" -o json"):
		return sshx.Result{Stdout: d.pods}, nil
	}
	d.t.Fatalf("unscripted command: %q", cmd)
	return sshx.Result{}, nil
}

func (d *dnsRunner) RunInput(_ context.Context, cmd string, stdin io.Reader) (sshx.Result, error) {
	d.cmds = append(d.cmds, cmd)
	b, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	d.body = append(d.body, string(b))
	switch {
	case strings.Contains(cmd, "patch deployment "+CoreDNSDeployment):
		return sshx.Result{}, nil
	}
	d.t.Fatalf("unscripted streamed command: %q", cmd)
	return sshx.Result{}, nil
}

// A cluster of one node keeps k3s's single replica; every larger cluster needs
// two, because one replica is what a node loss takes away.
func TestCoreDNSReplicasFollowsTheNodeCount(t *testing.T) {
	cases := []struct {
		nodes int
		want  int
	}{
		{1, 1},
		{2, 2},
		{3, 2},
		{9, 2},
	}
	for _, c := range cases {
		if got := CoreDNSReplicas(c.nodes); got != c.want {
			t.Errorf("CoreDNSReplicas(%d) = %d, want %d", c.nodes, got, c.want)
		}
	}
}

// The patch carries the replica count and a REQUIRED one-pod-per-node
// anti-affinity, and it carries NOTHING ELSE.
//
// The second half is the load-bearing assertion, not a tidiness check: k3s
// applies this Deployment three-way from its own packaged manifest, so every
// field this patch mentions becomes a field KubeNest manages — and a field the
// patch mentioned that k3s also owns (the image, the tolerations, the resource
// requests, the node selector) would be frozen at the value in this CLI's
// source long after k3s had moved on.
func TestCoreDNSPatchSetsTheCountAndRequiresOnePodPerNode(t *testing.T) {
	raw, err := CoreDNSPatch(2)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the patch is not JSON: %v\n%s", err, raw)
	}
	spec := wantMap(t, doc, "spec")
	if len(spec) != 2 {
		t.Fatalf("the patch describes %v; it may set spec.replicas and spec.template.spec.affinity and nothing else", spec)
	}
	if got := spec["replicas"]; got != float64(2) {
		t.Errorf("spec.replicas = %v, want 2", got)
	}

	templateSpec := wantMap(t, wantMap(t, wantMap(t, spec, "template"), "spec"), "affinity")
	anti := wantMap(t, templateSpec, "podAntiAffinity")
	if len(anti) != 1 {
		t.Fatalf("the patch sets %v in podAntiAffinity, want only the required rule", anti)
	}
	if _, preferred := anti["preferredDuringSchedulingIgnoredDuringExecution"]; preferred {
		t.Error("the anti-affinity is PREFERRED: two replicas may then be placed on one node, which are exactly the two that go down together when it stops")
	}
	terms, ok := anti["requiredDuringSchedulingIgnoredDuringExecution"].([]any)
	if !ok || len(terms) != 1 {
		t.Fatalf("requiredDuringSchedulingIgnoredDuringExecution = %v, want one term", anti["requiredDuringSchedulingIgnoredDuringExecution"])
	}
	term := terms[0].(map[string]any)
	if term["topologyKey"] != "kubernetes.io/hostname" {
		t.Errorf("the spread is keyed on %v: the failure domain that has to be survived is a NODE", term["topologyKey"])
	}
	selector := wantMap(t, wantMap(t, term, "labelSelector"), "matchLabels")
	if selector[CoreDNSLabelKey] != CoreDNSLabelValue {
		t.Errorf("the anti-affinity selects %v, want only CoreDNS's own pods (%s=%s)", selector, CoreDNSLabelKey, CoreDNSLabelValue)
	}
}

func wantMap(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := doc[key]
	if !ok {
		t.Fatalf("the patch has no %q: %v", key, doc)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want a map", key, v)
	}
	return m
}

// The mechanism end to end: patch k3s's Deployment, then wait for the pods
// rather than for the field the patch set.
func TestEnsureCoreDNSReplicasPatchesThenWaitsForTheSpread(t *testing.T) {
	r := &dnsRunner{t: t, pods: corednsPods(
		[]string{"kubenest-lab-w3-1", "kubenest-lab-w3-2"}, nil)}

	if err := EnsureCoreDNSReplicas(context.Background(), r, 3, time.Minute, nil); err != nil {
		t.Fatalf("three nodes: %v", err)
	}
	if len(r.cmds) != 2 {
		t.Fatalf("made %d call(s): %v", len(r.cmds), r.cmds)
	}
	const patchCmd = "sudo -n k3s kubectl -n kube-system patch deployment coredns" +
		" --type=strategic --patch-file /dev/stdin"
	if r.cmds[0] != patchCmd {
		t.Errorf("first call = %q, want the patch %q", r.cmds[0], patchCmd)
	}
	if !strings.Contains(r.body[0], `"replicas":2`) {
		t.Errorf("the patch body does not ask for two replicas: %s", r.body[0])
	}
	if !strings.Contains(r.cmds[1], "get pods -n kube-system -l "+CoreDNSSelector+" -o json") {
		t.Errorf("second call = %q, want the pod list: the field the patch set does not say whether a second CoreDNS pod ever became Ready", r.cmds[1])
	}
}

// The spread is what makes the second replica worth having, so a second Ready
// pod on the node that already has one must NOT satisfy the check: those are
// the two replicas a single node loss takes away together.
func TestCoreDNSSpreadNeedsDistinctNodesNotJustTwoPods(t *testing.T) {
	cases := []struct {
		name string
		pods string
		want bool
	}{
		{
			"two Ready pods on two nodes",
			corednsPods([]string{"node-a", "node-b"}, nil),
			true,
		},
		{
			"two Ready pods on the SAME node",
			corednsPods([]string{"node-a", "node-a"}, nil),
			false,
		},
		{
			"the second pod is still Pending",
			corednsPods([]string{"node-a"}, []string{"node-a"}),
			false,
		},
		{
			"neither replica is up yet",
			corednsPods(nil, nil),
			false,
		},
	}
	// The third case is the one that matters: one Ready pod plus a Pending
	// second must not pass, or a second replica that can never be scheduled
	// would be reported as a cluster that survives a node loss.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &dnsRunner{t: t, pods: c.pods}
			done, state, err := coreDNSSpreadProbe(r, 2)(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if done != c.want {
				t.Errorf("done = %v (%s), want %v", done, state, c.want)
			}
		})
	}
}

// The Pending pod's schedule message is carried into the observation: "0/2
// nodes are available: 1 node(s) didn't match pod anti-affinity rules" is a
// fix, "not ready" is not (pkg/converge).
func TestCoreDNSSpreadReportsWhyTheSecondPodIsNotScheduled(t *testing.T) {
	r := &dnsRunner{t: t, pods: `{"items":[
	  {"metadata":{"name":"coredns-a"},"spec":{"nodeName":"node-a"},
	   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
	  {"metadata":{"name":"coredns-b"},"spec":{"nodeName":""},
	   "status":{"phase":"Pending","conditions":[
	     {"type":"PodScheduled","status":"False",
	      "message":"0/1 nodes are available: 1 node(s) didn't match pod anti-affinity rules."}]}}
	]}`}
	done, state, err := coreDNSSpreadProbe(r, 2)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("one Ready pod and one Pending pod is not a cluster that survives a node loss")
	}
	if !strings.Contains(state.Detail, "didn't match pod anti-affinity rules") {
		t.Errorf("the observation carries no fix-shaped detail: %+v", state)
	}
}

// corednsPods builds the pod list kubectl would return: one Running and Ready
// pod per node in ready, one Pending pod per node in pending.
func corednsPods(ready, pending []string) string {
	items := make([]string, 0, len(ready)+len(pending))
	for i, node := range ready {
		items = append(items, `{"metadata":{"name":"coredns-ready-`+string(rune('a'+i))+`"},`+
			`"spec":{"nodeName":"`+node+`"},`+
			`"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`)
	}
	for i, node := range pending {
		items = append(items, `{"metadata":{"name":"coredns-pending-`+string(rune('a'+i))+`"},`+
			`"spec":{"nodeName":"`+node+`"},`+
			`"status":{"phase":"Pending","conditions":[{"type":"Ready","status":"False"}]}}`)
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}
