package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `kubenest backup now --control-plane` (kn-t47): the on-demand checkpoint.
//
// TWO THINGS ARE ASSERTED AT THE COMMAND SURFACE, and neither needs a host:
//
//   - the control-plane path has its OWN required set. It takes the server it
//     reaches and the bundle manifest it reads its deadline from, and it takes
//     no --cluster: the subject is the control plane, which runs in the
//     management cluster those two already address, and a cluster name would
//     only be used to select a workload cluster's backups.
//   - passing --cluster anyway is REFUSED rather than ignored. Silently
//     accepting it would leave an operator believing they had scoped the
//     command to a cluster when the run never looked at one.
//
// The workload path is unchanged and still demands --cluster: the two sets are
// asserted against each other so a change that made one of them the other's
// would fail here.

// bundleManifestFile writes a minimal manifest the command can load, so a run
// that reaches the manifest read fails on the transport rather than on the file.
func bundleManifestFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform-1.1.yaml")
	body := "bundle: \"1.1\"\nlimits:\n  timeouts:\n    component-ready: 1m\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runBackupNow(args ...string) error {
	root := NewRootCommand()
	root.SetArgs(append([]string{"backup", "now"}, args...))
	return root.Execute()
}

// The refusal, and it names both flags: an operator who typed neither by
// accident has to be told which one to drop.
func TestBackupNowControlPlaneRefusesACluster(t *testing.T) {
	err := runBackupNow("--control-plane", "--cluster", "prod-1",
		"--server", "10.0.1.10", "--bundle-manifest", bundleManifestFile(t))
	if err == nil {
		t.Fatal("--control-plane with --cluster must be refused: the control plane is not a workload cluster")
	}
	for _, want := range []string{"--cluster", "--control-plane"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, so the operator cannot tell what to drop: %v", want, err)
		}
	}
}

// The control-plane path's own required set: --server and --bundle-manifest,
// and never --cluster. The first case is the one that proves the branch: the
// workload path would refuse with "--cluster is required" long before it looked
// at a server.
func TestBackupNowControlPlaneNeedsAServerAndABundleNotACluster(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"server", []string{"--control-plane"}, "--server"},
		{"bundle", []string{"--control-plane", "--server", "10.0.1.10"}, "--bundle-manifest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := runBackupNow(c.args...)
			if err == nil {
				t.Fatalf("%v must refuse", c.args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%v: want a refusal naming %s, got: %v", c.args, c.want, err)
			}
			if strings.Contains(err.Error(), "--cluster is required") {
				t.Errorf("%v took the workload path: a control-plane run must not need a cluster\n%v", c.args, err)
			}
		})
	}
}

// The workload path still demands the cluster. Together with the refusals
// above, this is what makes "the flag means the other thing" a test failure
// rather than a change nobody noticed.
func TestBackupNowWithoutControlPlaneStillNeedsACluster(t *testing.T) {
	err := runBackupNow("--server", "10.0.1.10", "--bundle-manifest", bundleManifestFile(t))
	if err == nil || !strings.Contains(err.Error(), "--cluster is required") {
		t.Errorf("a workload backup without --cluster must be refused with --cluster named, got: %v", err)
	}
}

// Past validation the control-plane run reads the bundle and reaches the
// transport, which is where its remaining work is. The refusal below comes from
// the SSH key file, not from a flag, and that is the point: the two earlier
// subtests prove it did not stop on a flag, this one proves it did not stop for
// a stub either.
func TestBackupNowControlPlaneReachesTheTransport(t *testing.T) {
	missingKey := filepath.Join(t.TempDir(), "no-such-key")
	err := runBackupNow("--control-plane",
		"--server", "10.0.1.10",
		"--ssh-key", missingKey,
		"--bundle-manifest", bundleManifestFile(t))
	if err == nil {
		t.Fatal("a run whose SSH key does not exist cannot have connected to anything")
	}
	if !strings.Contains(err.Error(), missingKey) {
		t.Errorf("the run stopped somewhere other than the transport it needs: %v", err)
	}
}
