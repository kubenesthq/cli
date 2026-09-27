package node

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"kubenest.io/cli/pkg/k3s"
)

// BoundLocalVolume is one PersistentVolume that lives on ONE node's disk and
// is bound: the volume, its claim, and the workload that holds the claim.
//
// A local volume has exactly one copy, on one machine, so removing that
// machine removes it. This type exists so `node remove` can say WHICH data is
// about to go and what to do about it, per workload, before anything is
// touched.
type BoundLocalVolume struct {
	// PV is the PersistentVolume's name.
	PV string
	// Namespace and PVC are the claim bound to it.
	Namespace string
	PVC       string
	// Workload is the controller holding the claim — "StatefulSet/pg",
	// "Deployment/web" — or empty for a claim no running pod mounts.
	Workload string
	// Driver is the CSI driver that provisions the volume, reported so the
	// operator can tell a KubeNest LVM volume from a hand-made local one.
	Driver string
}

// BoundLocalVolumes reads every BOUND PersistentVolume whose node affinity
// names node, with the claim and the workload that holds it.
//
// It reads the cluster's own objects rather than the storage class: a PV
// pinned to a node by affinity is stranded by removing that node whatever
// provisioned it, and a PV that is not bound holds nothing anybody would miss.
func BoundLocalVolumes(ctx context.Context, server k3s.Runner, node string) ([]BoundLocalVolume, error) {
	out, err := k3s.Kubectl(ctx, server, "get pv -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's persistent volumes: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				ClaimRef *claimRef `json:"claimRef"`
				CSI      *struct {
					Driver string `json:"driver"`
				} `json:"csi"`
				NodeAffinity *pvNodeAffinity `json:"nodeAffinity"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing `kubectl get pv -o json`: %w", err)
	}

	holders, err := claimHolders(ctx, server)
	if err != nil {
		return nil, err
	}

	var found []BoundLocalVolume
	for _, item := range list.Items {
		if item.Status.Phase != "Bound" || item.Spec.ClaimRef == nil {
			continue
		}
		if !pinnedTo(item.Spec.NodeAffinity, node) {
			continue
		}
		v := BoundLocalVolume{
			PV:        item.Metadata.Name,
			Namespace: item.Spec.ClaimRef.Namespace,
			PVC:       item.Spec.ClaimRef.Name,
			Workload:  holders[claimKey{item.Spec.ClaimRef.Namespace, item.Spec.ClaimRef.Name}],
		}
		if item.Spec.CSI != nil {
			v.Driver = item.Spec.CSI.Driver
		}
		found = append(found, v)
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Namespace != found[j].Namespace {
			return found[i].Namespace < found[j].Namespace
		}
		return found[i].PVC < found[j].PVC
	})
	return found, nil
}

// pvNodeAffinity is the little of a PersistentVolume's affinity this package
// reads: the selector that says which machines the volume may live on.
type pvNodeAffinity struct {
	Required *struct {
		NodeSelectorTerms []struct {
			MatchExpressions []struct {
				Key      string   `json:"key"`
				Operator string   `json:"operator"`
				Values   []string `json:"values"`
			} `json:"matchExpressions"`
		} `json:"nodeSelectorTerms"`
	} `json:"required"`
}

// claimRef is the claim a volume is bound to.
type claimRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// pinnedTo reports whether a volume's node affinity requires exactly this
// node. A term whose key is the hostname label and whose operator is In (or
// the single-value form of it) is the shape every CSI driver uses to say "this
// volume lives on that machine".
func pinnedTo(affinity *pvNodeAffinity, node string) bool {
	if affinity == nil || affinity.Required == nil {
		return false
	}
	for _, term := range affinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key != "kubernetes.io/hostname" {
				continue
			}
			switch expr.Operator {
			case "In":
				for _, v := range expr.Values {
					if v == node {
						return true
					}
				}
			case "Equals":
				if len(expr.Values) == 1 && expr.Values[0] == node {
					return true
				}
			}
		}
	}
	return false
}

// claimKey identifies a claim inside one namespace.
type claimKey struct {
	Namespace string
	Claim     string
}

// claimHolders maps each claim to the workload holding it, read from the pods
// that mount it.
//
// A pod's owner is the Deployment, StatefulSet or DaemonSet that created it,
// except that a Deployment creates a ReplicaSet which creates the pod; the
// ReplicaSet's generated name carries the Deployment's as a prefix, which is
// the only way to name the workload an operator recognises from the objects
// this reads. A claim no running pod mounts names no workload, and the caller
// groups those by namespace.
func claimHolders(ctx context.Context, server k3s.Runner) (map[claimKey]string, error) {
	out, err := k3s.Kubectl(ctx, server, "get pods --all-namespaces -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's pods, which is how each volume's workload is named: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace       string `json:"namespace"`
				OwnerReferences []struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				Volumes []struct {
					PersistentVolumeClaim *struct {
						ClaimName string `json:"claimName"`
					} `json:"persistentVolumeClaim"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing `kubectl get pods --all-namespaces -o json`: %w", err)
	}
	holders := map[claimKey]string{}
	for _, pod := range list.Items {
		workload := workloadName(pod.Metadata.OwnerReferences)
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName == "" {
				continue
			}
			key := claimKey{pod.Metadata.Namespace, v.PersistentVolumeClaim.ClaimName}
			if workload == "" {
				continue
			}
			// Two pods of the same workload agree; a claim mounted by two
			// different workloads is named by the first in this listing, and
			// the restore command covers every claim either way.
			if _, ok := holders[key]; !ok {
				holders[key] = workload
			}
		}
	}
	return holders, nil
}

// workloadName names the controller behind a pod from its owner references.
func workloadName(owners []struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}) string {
	if len(owners) == 0 {
		return ""
	}
	owner := owners[0]
	if owner.Kind == "ReplicaSet" {
		// "<deployment>-<podtemplatehash>": the hash is what makes the
		// ReplicaSet name unique per revision, and the Deployment is what the
		// operator knows the workload by.
		if i := strings.LastIndex(owner.Name, "-"); i > 0 {
			return "Deployment/" + owner.Name[:i]
		}
	}
	return owner.Kind + "/" + owner.Name
}

// RestoreCommands groups the volumes by the workload holding them and renders
// ONE restore command per group.
//
// Every claim of a group is in the SAME command on purpose: restoring one
// claim of a pod while another stays stranded leaves the pod unable to start,
// which is a restore that has to be run twice rather than a restore. Claims no
// running pod mounts are grouped by namespace, because that is the scope the
// restore command works in.
//
// The command is one an operator can RUN AS PRINTED, which is why it ends in
// --latest: a restore names the backup it restores, and after a node loss the
// only honest answer is the newest ELIGIBLE one — a newer backup that is not
// eligible is passed over with the reason, which is exactly what the operator
// has to see before accepting an older one. The environment flags a restore
// also needs (the server, the bundle manifest, the SSH key) are the operator's
// own, already in their shell.
func RestoreCommands(cluster string, volumes []BoundLocalVolume) []string {
	groups := map[string][]BoundLocalVolume{}
	var keys []string
	for _, v := range volumes {
		key := v.Namespace + "|" + v.Workload
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], v)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		claims := groups[key]
		var b strings.Builder
		b.WriteString("kubenest backup restore --cluster " + cluster + " --namespace " + claims[0].Namespace)
		for _, c := range claims {
			b.WriteString(" --pvc " + c.PVC)
		}
		b.WriteString(" --latest")
		what := claims[0].Workload
		if what == "" {
			what = "no running pod"
		}
		out = append(out, b.String()+"   # "+what)
	}
	return out
}
