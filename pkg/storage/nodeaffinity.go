package storage

import "slices"

// A LOCAL VOLUME RECORDS THE ONE NODE ITS DATA LIVES ON IN ITS NODE AFFINITY,
// and the key it records it under is the DRIVER's, not Kubernetes': the
// hostname label for most provisioners, openebs.io/nodename for the LVM local
// driver this platform installs. A reader that knows only one of them reports
// "no node" for every volume of the other kind — and a volume with no node
// cannot be asked the one question that matters after a node loss, "is this
// data on a machine that is still there?".
//
// MEASURED ON lab w3 (2026-09-27, bundle 1.1, `kubenest-local` provisioner
// local.csi.openebs.io): a volume Velero had just provisioned fresh carried
//
//	spec.nodeAffinity.required.nodeSelectorTerms[].matchExpressions:
//	  [{key: openebs.io/nodename, operator: In, values: [kubenest-lab-w3-1]}]
//
// and the CSINode's topology keys were
// ["kubernetes.io/hostname", "openebs.io/nodename"]. Mode 2's restore then
// refused its own refilled claim — "claim dead-a is bound to pvc-b218dc9a-…,
// which records no node" — because the reader knew only the hostname label, so
// every LVM volume looked like a volume nobody could place.
//
// THE KEYS LIVE HERE, IN ONE PLACE, because two readers have to agree on them:
// `node remove`/`node replace`'s stranded-volume disposition (pkg/node) and the
// restore's claim-binding check (pkg/backup). They were written apart, and they
// disagreed on exactly this.
const (
	// HostnameAffinityKey is Kubernetes' own hostname label, the value of
	// which is the Node name. Most provisioners, k3s's local-path one
	// included, write it.
	HostnameAffinityKey = "kubernetes.io/hostname"
	// OpenEBSNodeAffinityKey is what OpenEBS's LVM local driver writes instead
	// of the hostname label — next to it in the CSINode's topology keys, and
	// alone in the volume's affinity. Its value is the Node name, the same
	// string the hostname label carries.
	OpenEBSNodeAffinityKey = "openebs.io/nodename"
)

// nodeAffinityKeys are the keys a local volume may name its node under.
var nodeAffinityKeys = []string{HostnameAffinityKey, OpenEBSNodeAffinityKey}

// NodeAffinity is the little of a PersistentVolume's node affinity a reader
// needs: the terms that say which machines the volume may live on. It is the
// same shape for every reader, so no two of them can read different fields of
// it.
type NodeAffinity struct {
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

// NodesOf returns every node name a volume's required node affinity POSITIVELY
// requires: the values of each In or Equals expression whose key is one of the
// keys above, in the order the terms name them.
//
// THE OPERATOR IS PART OF THE ANSWER. Values under NotIn name machines the
// volume must NOT be on, so they are not where its data is, and returning them
// would put a volume on a node that cannot hold it. A key this package does not
// know belongs to some other constraint (a zone, a pool) and is skipped for the
// same reason.
//
// Empty means the volume names no node: for a local volume that is an answer —
// "nobody can say where its data is" — and not a failure to read it.
func NodesOf(affinity *NodeAffinity) []string {
	if affinity == nil || affinity.Required == nil {
		return nil
	}
	var nodes []string
	for _, term := range affinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if !namesANode(expr.Key) {
				continue
			}
			if expr.Operator != "In" && expr.Operator != "Equals" {
				continue
			}
			nodes = append(nodes, expr.Values...)
		}
	}
	return nodes
}

// Requires reports whether a volume's node affinity requires exactly this node,
// which is what makes removing that node a data loss.
func Requires(affinity *NodeAffinity, node string) bool {
	return slices.Contains(NodesOf(affinity), node)
}

func namesANode(key string) bool {
	return slices.Contains(nodeAffinityKeys, key)
}
