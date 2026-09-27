package storage

import (
	"encoding/json"
	"reflect"
	"testing"
)

// affinityOf builds a PersistentVolume's node affinity from the JSON the
// cluster holds, so every case below states the real nested shape — required,
// nodeSelectorTerms, matchExpressions — rather than a struct literal that could
// drift from it.
func affinityOf(t *testing.T, raw string) *NodeAffinity {
	t.Helper()
	var pv struct {
		Spec struct {
			NodeAffinity *NodeAffinity `json:"nodeAffinity"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(`{"spec":{"nodeAffinity":`+raw+`}}`), &pv); err != nil {
		t.Fatalf("parsing %s: %v", raw, err)
	}
	return pv.Spec.NodeAffinity
}

// THE KEY IS THE DRIVER'S, and this test exists because two readers agreed on
// one key while the platform's own storage class writes another (lab w3,
// 2026-09-27): the hostname label for most provisioners, openebs.io/nodename
// for the LVM volumes `kubenest-local` provisions. A volume whose node cannot
// be read looks like a volume nobody can place, and both things that ask —
// "is this data on a live node?" (the restore) and "which data goes with this
// machine?" (node remove/replace) — then answer wrongly.
func TestNodesOfReadsTheLocalVolumeKeyTheDriverUses(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "the hostname label, which most provisioners write",
			raw:  `{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["lab-w3-1"]}]}]}}`,
			want: []string{"lab-w3-1"},
		},
		{
			// The measured shape: this is ALL a fresh LVM PV carried.
			name: "openebs.io/nodename, which the platform's LVM driver writes",
			raw:  `{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"openebs.io/nodename","operator":"In","values":["lab-w3-1"]}]}]}}`,
			want: []string{"lab-w3-1"},
		},
		{
			name: "a term per key, as a driver whose CSINode lists both topology keys writes",
			raw: `{"required":{"nodeSelectorTerms":[` +
				`{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["lab-w3-1"]}]},` +
				`{"matchExpressions":[{"key":"openebs.io/nodename","operator":"In","values":["lab-w3-1"]}]}]}}`,
			want: []string{"lab-w3-1", "lab-w3-1"},
		},
		{
			name: "the single-value form some drivers render as Equals",
			raw:  `{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"openebs.io/nodename","operator":"Equals","values":["lab-w3-1"]}]}]}}`,
			want: []string{"lab-w3-1"},
		},
		{
			// A NotIn expression names machines the volume may NOT be on:
			// returning one would place the data on a node that cannot hold it.
			name: "NotIn is not where the volume is",
			raw:  `{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"openebs.io/nodename","operator":"NotIn","values":["lab-w3-1"]}]}]}}`,
			want: nil,
		},
		{
			// A foreign key is another constraint (a zone, a pool), not this
			// volume's node.
			name: "a key no local volume records its node under is ignored",
			raw:  `{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"topology.example.io/rack","operator":"In","values":["r1"]}]}]}}`,
			want: nil,
		},
		{
			name: "a recognised key next to an unrecognised one is still read",
			raw: `{"required":{"nodeSelectorTerms":[{"matchExpressions":[` +
				`{"key":"topology.example.io/rack","operator":"In","values":["r1"]},` +
				`{"key":"openebs.io/nodename","operator":"In","values":["lab-w3-1"]}]}]}}`,
			want: []string{"lab-w3-1"},
		},
		{
			name: "no node affinity at all names no node, which is an answer",
			raw:  `{}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NodesOf(affinityOf(t, tc.raw)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NodesOf = %v, want %v", got, tc.want)
			}
		})
	}
}

// Requires is the question `node remove` asks before it takes a machine away:
// does this volume's data live on it? BOTH keys have to answer yes, or half the
// platform's volumes go away unreported.
func TestRequiresAnswersForBothKeys(t *testing.T) {
	for _, key := range []string{HostnameAffinityKey, OpenEBSNodeAffinityKey} {
		affinity := affinityOf(t, `{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"`+key+`","operator":"In","values":["lab-w3-1"]}]}]}}`)
		if !Requires(affinity, "lab-w3-1") {
			t.Errorf("a volume pinned to lab-w3-1 under %s must require it", key)
		}
		if Requires(affinity, "lab-w3-2") {
			t.Errorf("a volume pinned to lab-w3-1 under %s must not require lab-w3-2", key)
		}
	}
	if Requires(nil, "lab-w3-1") {
		t.Error("a volume with no affinity must require no node")
	}
}
