package backup

import (
	"context"
	"os/exec"
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

// THE REMOTE SHELL PARSES THE COMMAND STRING BEFORE kubectl EVER SEES IT. Every
// call this package makes is sent as ONE string (k3s.Kubectl -> runner.Run ->
// `ssh host <string>`, which the login shell runs as `bash -c <string>`), so a
// character the shell reads as syntax is not a path expression at all — the
// command never runs. Hardware (lab w3, 2026-09-27) died there: NodeReady's
// unquoted `{.status.conditions[?(@.type=="Ready")].status}` exited 2 with
// "bash: -c: line 1: syntax error near unexpected token `('", and the whole
// restore stopped on a read of a node that was up.
//
// SO EVERY COMMAND GOES THROUGH THE SHELL'S OWN PARSER, with no cluster
// involved: `bash -n` parses and runs nothing. THE LIST IS NOT CURATED — the
// walk below touches every method this package has that builds a command
// (reads and changes), and every string that came out is checked, because the
// defect is a property of the strings and not of a hand-picked few.
func TestEveryClusterCommandParsesUnderTheRemoteShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not on this machine and it is the shell that parses these commands: %v", err)
	}
	ctx := context.Background()
	r := &fakeRunner{
		Respond: func(command string) (sshx.Result, error) {
			if strings.Contains(command, "get persistentvolumeclaim ") {
				// A bound claim, so ClaimBinding goes on to read its volume.
				return sshx.Result{Stdout: `{"spec":{"volumeName":"pvc-1"},"status":{"phase":"Bound"}}`}, nil
			}
			return sshx.Result{Stdout: "{}"}, nil
		},
		RespondInput: func(string, []byte) (sshx.Result, error) {
			return sshx.Result{Stdout: "{}"}, nil
		},
	}
	c := NewK3sCluster(r)

	// Every read.
	c.Namespace(ctx, "payments")
	c.Claims(ctx, "payments")
	c.Workloads(ctx, "payments")
	c.Pods(ctx, "payments")
	c.CronJobs(ctx, "payments")
	c.Jobs(ctx, "payments")
	c.Applications(ctx, "payments")
	c.ProjectHold(ctx, "payments")
	c.DrillRestore(ctx)
	c.VolumeRestores(ctx, "kubenest-restore-volumes-0f3a57c")
	c.PodVolumeBackups(ctx, "daily-1")
	c.RestoreOutcome(ctx, "kubenest-restore-volumes-0f3a57c")
	c.ClaimBinding(ctx, "payments", "data-0")
	c.NodeReady(ctx, "lab-w3-1")
	c.ControllerOwner(ctx, "payments", "replicaset", "web-6d9f")
	// Every change.
	c.AnnotateProject(ctx, "payments", PauseAnnotationKey, "0f3a57c")
	c.ClearProjectAnnotation(ctx, "payments", PauseAnnotationKey)
	c.ScaleWorkload(ctx, "deployment", "web", "payments", 0)
	c.DeleteNamespace(ctx, "payments")
	c.DeleteClaim(ctx, "payments", "data-0")
	c.SuspendCronJob(ctx, "payments", "nightly", true)
	c.SuspendJob(ctx, "payments", "nightly-1", true)
	c.CreateBackup(ctx, "daily-1", []byte("apiVersion: velero.io/v1\nkind: Backup\n"))
	c.CreateRestore(ctx, "r", []byte("apiVersion: velero.io/v1\nkind: Restore\n"))
	c.Apply(ctx, "a manifest", []byte("apiVersion: v1\nkind: ConfigMap\n"))
	c.Delete(ctx, "restore r -n velero")

	commands := r.Commands()
	var reads, changes int
	for _, command := range commands {
		if strings.Contains(command, "kubectl get ") {
			reads++
		} else {
			changes++
		}
	}
	if reads == 0 || changes == 0 {
		t.Fatalf("the walk sent %d read(s) and %d change(s): the parse check below would then be about half the commands", reads, changes)
	}
	for _, command := range commands {
		if out, err := exec.Command(bash, "-n", "-c", command).CombinedOutput(); err != nil {
			t.Errorf("the remote shell cannot parse this command, so kubectl would never run it:\n  %s\n  %v: %s", command, err, strings.TrimSpace(string(out)))
		}
	}
}
