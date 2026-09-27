package k3s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"kubenest.io/cli/pkg/converge"
)

// k3s's cluster DNS is the coredns Deployment in kube-system, deployed from
// k3s's OWN packaged manifest — manifests/coredns.yaml in the k3s tree, which
// k3s's stager writes to ManifestDir on every server start with its %{...}%
// template variables substituted (pkg/deploy/stage.go Stage, k3s
// v1.35.7+k3s1, the version bundle 1.1 and 1.2 pin).
//
// KubeNest does not own that manifest. This file owns the one property of it
// that a cluster of more than one node needs and k3s does not set: how many
// replicas it runs, and the guarantee that no two of them are on one node.
//
// The packaged manifest declares no spec.replicas at all, so a fresh cluster
// gets Kubernetes' default of one. One replica is enough until its node stops:
// from that moment every in-cluster name lookup fails until the node is marked
// NotReady, the pod is evicted past its toleration and rescheduled somewhere
// else — which is how a Velero restore on lab w3 died at
// `lookup 23.88.125.22.sslip.io on 10.43.0.10:53: read udp ...: i/o timeout`
// while it was the node hosting the only CoreDNS pod that had stopped
// (kn-t43-restore-workload-s-stranded-11o7.2).
const (
	CoreDNSNamespace  = "kube-system"
	CoreDNSDeployment = "coredns"
	// CoreDNSLabelKey and CoreDNSLabelValue are the label k3s puts on the
	// CoreDNS pods and selects them by, on the kube-dns Service and in its own
	// hostname topologySpreadConstraint.
	CoreDNSLabelKey   = "k8s-app"
	CoreDNSLabelValue = "kube-dns"
)

// CoreDNSSelector is the label selector those pods answer to.
const CoreDNSSelector = CoreDNSLabelKey + "=" + CoreDNSLabelValue

// coreDNSTopologyKey is the failure domain the replicas are spread across:
// one pod per NODE, because a node is what is lost.
const coreDNSTopologyKey = "kubernetes.io/hostname"

// CoreDNSReplicas is how many CoreDNS replicas a cluster of `nodes` nodes
// runs: two on any cluster with at least two nodes, one on a single-node one.
//
// ONE NODE KEEPS ONE, and not because nothing is lost with one replica —
// nothing is gained either: k3s's packaged manifest spreads these pods with a
// hostname topologySpreadConstraint of maxSkew 1 and
// whenUnsatisfiable: DoNotSchedule (manifests/coredns.yaml, v1.35.7+k3s1), so
// a second replica on a one-node cluster can never be scheduled and would sit
// Pending for the life of the cluster while the Deployment reported itself
// under-replicated.
//
// TWO NODES GET TWO. The second replica is not redundancy in the abstract: it
// is what makes a lookup succeed while the node holding the other replica is
// gone, and the spread below is what keeps both of them off that node.
func CoreDNSReplicas(nodes int) int {
	if nodes >= 2 {
		return 2
	}
	return 1
}

// coreDNSPatchCommand streams the patch in on stdin, never in the command
// string: the same rule WriteManifest follows, so nothing this CLI writes to a
// host is ever readable in that host's process list.
const coreDNSPatchCommand = "sudo -n k3s kubectl -n " + CoreDNSNamespace +
	" patch deployment " + CoreDNSDeployment + " --type=strategic --patch-file /dev/stdin"

// EnsureCoreDNSReplicas makes a cluster of `nodes` nodes answer a name lookup
// after any ONE of them stops: it patches k3s's CoreDNS Deployment to
// CoreDNSReplicas(nodes) replicas with a REQUIRED one-pod-per-node
// anti-affinity, then waits until that many pods are Ready on that many
// distinct nodes.
//
// WHY A PATCH ON K3S'S OWN OBJECT, AND NOT A MANIFEST WE OWN. k3s's deploy
// controller applies the manifest with the same machinery as `kubectl apply`:
// wrangler's THREE-WAY strategic merge patch, whose `original` is the object
// k3s itself last applied, kept on the target in the
// `objectset.rio.cattle.io/applied` annotation
// (rancher/wrangler/v3 pkg/apply/desiredset_compare.go doPatch, "adapted from
// kubectl apply"; k3s v1.35.7+k3s1 pins wrangler v3.6.0 in its go.mod). k3s's
// packaged manifest declares neither spec.replicas nor
// spec.template.spec.affinity, so both are fields k3s has never applied and
// the patch it computes never mentions them — it preserves them.
//
// THAT HOLDS ACROSS A RESTART AND ACROSS A k3s UPGRADE, which is the part
// worth being precise about, because the obvious reading of the deploy
// controller says otherwise. It skips a manifest whose SHA-256 equals the
// checksum of the last successful apply (pkg/deploy/controller.go deploy,
// `compareChecksum && checksum == addon.Spec.Checksum`), which would suggest a
// scale survives only until the manifest's bytes change. But the watcher's
// FIRST pass is forced — `force := true`, then `w.deploy(path, !force)`
// (listFiles/deploy, same file) — so on every server start the manifest is
// re-applied with that comparison off, and the checksum is not what protects
// the replica count. What protects it is that the patch has nothing to say
// about replicas.
//
// The alternatives are worse and are named so nobody re-derives them as
// cheaper. Writing `<ManifestDir>/coredns.yaml.skip` plus our own copy of the
// manifest makes us the owner of a k3s artefact that changes with every k3s
// release — image tag, Corefile, RBAC, and the %{CLUSTER_DOMAIN}% /
// %{CLUSTER_DNS}% substitutions k3s's stager performs, which we would have to
// reproduce — so a k3s upgrade would silently diverge from us. `--disable=coredns`
// is worse still: it DELETES the manifest from the host, needs a k3s restart
// to take effect, and still leaves us owning that copy.
//
// THE RESIDUAL RISK, and where it is answered. A future k3s that declared
// spec.replicas in its packaged manifest would make the field one the manifest
// manages, and the three-way patch would then reset it. So this is re-asserted
// by `kubenest platform upgrade` after the Kubernetes stage
// (pkg/upgrade/kubernetes.go): whatever an upgrade reverts, that run ends by
// putting back.
func EnsureCoreDNSReplicas(ctx context.Context, r Runner, nodes int, deadline time.Duration, rep converge.Reporter) error {
	want := CoreDNSReplicas(nodes)
	patch, err := CoreDNSPatch(want)
	if err != nil {
		return err
	}
	res, err := r.RunInput(ctx, coreDNSPatchCommand, bytes.NewReader(patch))
	if err != nil {
		return fmt.Errorf("patching the CoreDNS deployment to %d replicas: %w", want, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("patching the CoreDNS deployment to %d replicas: exit %d: %s",
			want, res.ExitCode, firstLine(res.Stderr))
	}
	result, err := converge.Wait(ctx, coreDNSSpreadProbe(r, want), converge.Options{
		Name:     "coredns-replicas-spread",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return result.Err()
}

// CoreDNSPatch is the strategic merge patch EnsureCoreDNSReplicas applies.
//
// It carries TWO things and nothing else, so a k3s upgrade's own fields — the
// image, the Corefile mount, the tolerations, the resource requests — are
// never described here and can never be dragged back by this CLI.
//
//   - spec.replicas, the count.
//
//   - a REQUIRED podAntiAffinity on kubernetes.io/hostname, matching these
//     pods only. REQUIRED, NOT PREFERRED: a preferred rule lets the scheduler
//     put both replicas on one node when it is convenient, and those are
//     exactly the two that go down together when that node stops. k3s's own
//     manifest already asks for the same spread with DoNotSchedule, so this
//     does not tighten a working cluster — it makes the guarantee KubeNest's
//     rather than a property of a k3s file we do not own, which is what lets
//     the count and the spread be re-asserted together after an upgrade.
func CoreDNSPatch(replicas int) ([]byte, error) {
	if replicas < 1 {
		return nil, fmt.Errorf("CoreDNS replicas: %d is not a replica count", replicas)
	}
	return json.Marshal(map[string]any{
		"spec": map[string]any{
			"replicas": replicas,
			"template": map[string]any{
				"spec": map[string]any{
					"affinity": map[string]any{
						"podAntiAffinity": map[string]any{
							"requiredDuringSchedulingIgnoredDuringExecution": []any{
								map[string]any{
									"labelSelector": map[string]any{
										"matchLabels": map[string]any{CoreDNSLabelKey: CoreDNSLabelValue},
									},
									"topologyKey": coreDNSTopologyKey,
								},
							},
						},
					},
				},
			},
		},
	})
}

// coreDNSPods is the slice of `kubectl get pods -o json` the spread check
// reads: which node each pod landed on, and whether it is Ready.
type coreDNSPods struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// coreDNSSpreadProbe observes the guarantee itself rather than the field that
// asks for it: `want` pods Ready, on `want` DISTINCT nodes.
//
// Reading spec.replicas back would prove only that the patch landed. A second
// replica that is Pending — no node left that the required anti-affinity
// allows, an image that will not pull — leaves the cluster exactly as
// unprotected as one replica does, and the failure is invisible until the day
// the node stops.
func coreDNSSpreadProbe(r Runner, want int) converge.Probe {
	object := fmt.Sprintf("%d CoreDNS pods, at most one per node", want)
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := Kubectl(ctx, r, "get pods -n "+CoreDNSNamespace+" -l "+CoreDNSSelector+" -o json")
		if err != nil {
			return false, converge.State{Object: object, Status: "unobservable"}, err
		}
		var pods coreDNSPods
		if err := json.Unmarshal([]byte(out), &pods); err != nil {
			return false, converge.State{Object: object, Status: "unparsable"}, err
		}
		perNode := map[string]int{}
		detail := ""
		for _, pod := range pods.Items {
			ready := false
			for _, c := range pod.Status.Conditions {
				switch {
				case c.Type == "Ready":
					ready = c.Status == "True"
				case c.Type == "PodScheduled" && c.Status != "True" && c.Message != "" && detail == "":
					// "0/2 nodes are available: 1 node(s) didn't match pod
					// anti-affinity rules" is a fix; "not ready" is not.
					detail = c.Message
				}
			}
			if ready && pod.Spec.NodeName != "" {
				perNode[pod.Spec.NodeName]++
			}
		}
		ready, nodes := 0, len(perNode)
		for _, n := range perNode {
			ready += n
		}
		state := converge.State{
			Object: object,
			Status: fmt.Sprintf("%d Ready on %d node(s), want %d on %d", ready, nodes, want, want),
			Detail: detail,
		}
		return ready >= want && nodes >= want, state, nil
	}
}
