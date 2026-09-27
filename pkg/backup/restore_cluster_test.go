package backup

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/sshx"
)

// THE READER UNDER TEST IS THE ONE THAT FAILED ON HARDWARE, so this test drives
// the real k3sCluster over the JSON a real cluster answers with. The fake
// Cluster the rest of these tests use hands over an already-parsed
// ClaimBinding, which is exactly the step that broke: on lab w3 (2026-09-27,
// bundle 1.1) mode 2 refused its own refilled claim with "claim dead-a is bound
// to pvc-b218dc9a-…, which records no node", because the LVM driver records a
// volume's node as openebs.io/nodename and the reader knew only
// kubernetes.io/hostname. The claim and the volume below are the measured ones.
func TestClaimBindingReadsTheNodeFromTheKeyTheDriverWrote(t *testing.T) {
	const (
		namespace = "e2e-restore-volumes"
		claim     = "dead-a"
		volume    = "pvc-b218dc9a-0f5e-4c4a-9a8e-2f9b1e6f3d11"
		node      = "kubenest-lab-w3-1"
	)
	claimJSON := `{"spec":{"volumeName":"` + volume + `","storageClassName":"kubenest-local"},"status":{"phase":"Bound"}}`

	cases := []struct {
		name string
		pv   string
		want string
	}{
		{
			name: "the hostname label",
			pv: `{"spec":{"nodeAffinity":{"required":{"nodeSelectorTerms":[` +
				`{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["` + node + `"]}]}]}}}}`,
			want: node,
		},
		{
			name: "openebs.io/nodename, the shape an LVM volume carries",
			pv: `{"spec":{"nodeAffinity":{"required":{"nodeSelectorTerms":[` +
				`{"matchExpressions":[{"key":"openebs.io/nodename","operator":"In","values":["` + node + `"]}]}]}}}}`,
			want: node,
		},
		{
			name: "a volume that records no node is read as one that records none",
			pv:   `{"spec":{}}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{Respond: func(command string) (sshx.Result, error) {
				switch {
				case strings.Contains(command, "get persistentvolumeclaim "+claim+" -n "+namespace):
					return sshx.Result{Stdout: claimJSON}, nil
				case strings.Contains(command, "get persistentvolume "+volume):
					return sshx.Result{Stdout: tc.pv}, nil
				}
				t.Errorf("unscripted read: %s", command)
				return sshx.Result{ExitCode: 1, Stderr: "unscripted"}, nil
			}}
			binding, err := NewK3sCluster(r).ClaimBinding(context.Background(), namespace, claim)
			if err != nil {
				t.Fatalf("ClaimBinding: %v", err)
			}
			if binding.Node != tc.want {
				t.Errorf("ClaimBinding.Node = %q, want %q: a volume has to be placed from whichever key its driver recorded the node under", binding.Node, tc.want)
			}
			if !binding.Bound || binding.Volume != volume {
				t.Errorf("ClaimBinding = %+v, want the Bound claim on %s", binding, volume)
			}
		})
	}
}

// A volume that was provisioned for the node that died must still name that
// node, because that name is what makes the restore refuse to call the data
// live — the reader is not allowed to "help" by dropping an inconvenient node.
func TestClaimBindingReportsTheNodeEvenWhenItIsTheDeadOne(t *testing.T) {
	const volume = "pvc-b218dc9a-0f5e-4c4a-9a8e-2f9b1e6f3d11"
	r := &fakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "get persistentvolumeclaim"):
			return sshx.Result{Stdout: `{"spec":{"volumeName":"` + volume + `"},"status":{"phase":"Bound"}}`}, nil
		case strings.Contains(command, "get persistentvolume "):
			return sshx.Result{Stdout: `{"spec":{"nodeAffinity":{"required":{"nodeSelectorTerms":[` +
				`{"matchExpressions":[{"key":"openebs.io/nodename","operator":"In","values":["kubenest-lab-w3-2"]}]}]}}}}`}, nil
		}
		t.Errorf("unscripted read: %s", command)
		return sshx.Result{ExitCode: 1}, nil
	}}
	binding, err := NewK3sCluster(r).ClaimBinding(context.Background(), "e2e-restore-volumes", "dead-a")
	if err != nil {
		t.Fatalf("ClaimBinding: %v", err)
	}
	if binding.Node != "kubenest-lab-w3-2" {
		t.Errorf("ClaimBinding.Node = %q, want the dead node the volume names (kubenest-lab-w3-2)", binding.Node)
	}
}
