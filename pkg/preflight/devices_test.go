package preflight_test

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
)

// The three-node lab shape the flag exists for: three hosts, each with its own
// data volume, so the stable /dev/disk/by-id/... path — which contains the
// volume's serial — is different on every one of them.
const (
	labNodeA = "10.0.3.10"
	labNodeB = "10.0.3.11"
	labNodeC = "10.0.3.12"

	labVolumeA = "/dev/disk/by-id/scsi-0HC_Volume_101"
	labVolumeB = "/dev/disk/by-id/scsi-0HC_Volume_102"
	labVolumeC = "/dev/disk/by-id/scsi-0HC_Volume_103"
)

// vgAbsent is what `vgs` prints on a host with no kubenest-vg.
var vgAbsent = sshx.Result{ExitCode: 5, Stderr: "Volume group \"kubenest-vg\" not found\n"}

// hostWithOneVolume is a healthy host whose ONLY block device is own: `test -b`
// on another host's by-id path fails, exactly as it did on the lab's agents.
func hostWithOneVolume(own string) func(string) (sshx.Result, error) {
	healthy := healthyHost(map[string]sshx.Result{"vgs": vgAbsent})
	return func(cmd string) (sshx.Result, error) {
		if device, ok := strings.CutPrefix(cmd, "test -b "); ok {
			if device == own {
				return sshx.Result{}, nil
			}
			return sshx.Result{ExitCode: 1, Stderr: "test: " + device + ": No such file or directory\n"}, nil
		}
		if _, ok := strings.CutPrefix(cmd, "sudo -n blkid -p -o export "); ok {
			// Verified on a real host (kn-bkwa): blank means empty output, exit 2.
			return sshx.Result{ExitCode: 2}, nil
		}
		if strings.Contains(cmd, "ss -ltnH") {
			// The node-to-node port probe stops its listeners before it
			// finishes and polls this to see them gone. A host that reports
			// "free" is a host whose listeners have exited, which is what the
			// probe must see; without it the probe waits out its own retry
			// loop and this test spends five seconds per node on a check it is
			// not about.
			return sshx.Result{Stdout: "free\n"}, nil
		}
		return healthy(cmd)
	}
}

// threeNodeLabOptions is the install the bead was found on: one server and two
// agents, each with a volume of its own. The roles matter here — the agents
// carry no control-plane port of their own, so the node-to-node port probe
// runs for the server alone and this test stays about storage.
func threeNodeLabOptions(t *testing.T) preflight.Options {
	t.Helper()
	opts := baseOptions(t, healthyHost(nil))
	opts.HATier = "single-server"
	opts.Nodes = nil
	for _, node := range []struct {
		address string
		role    string
		volume  string
	}{
		{labNodeA, "server", labVolumeA},
		{labNodeB, "agent", labVolumeB},
		{labNodeC, "agent", labVolumeC},
	} {
		opts.Nodes = append(opts.Nodes, preflight.Node{
			Address: node.address, Role: node.role,
			Runner: &componenttest.FakeRunner{Respond: hostWithOneVolume(node.volume)},
		})
	}
	return opts
}

func volumeGroupResult(rep preflight.Report, node string) (preflight.Result, bool) {
	for _, r := range rep.Results {
		if r.Check == preflight.CheckVolumeGroup && r.Node == node {
			return r, true
		}
	}
	return preflight.Result{}, false
}

// One device per node: every node is checked against its OWN device, and the
// report says so, because that is the only device it has.
func TestVolumeGroupCheckAsksEachNodeForItsOwnDevice(t *testing.T) {
	opts := threeNodeLabOptions(t)
	opts.StorageDevices = storage.Devices{PerHost: map[string]string{
		labNodeA: labVolumeA,
		labNodeB: labVolumeB,
		labNodeC: labVolumeC,
	}}

	rep, err := preflight.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("three hosts that each have the volume --storage-device names for them were refused:\n%v", err)
	}
	for node, device := range map[string]string{labNodeA: labVolumeA, labNodeB: labVolumeB, labNodeC: labVolumeC} {
		res, ok := volumeGroupResult(rep, node)
		if !ok {
			t.Fatalf("no volume-group result for %s in %v", node, rep.Results)
		}
		if res.Outcome != preflight.Pass {
			t.Errorf("%s: %s: %s", node, res.Outcome, res.Detail)
		}
		if !strings.Contains(res.Detail, device) {
			t.Errorf("%s passes but the report does not name %s, the device named for that node: %q", node, device, res.Detail)
		}
	}
}

// The bead's symptom, as a test: one device path applied to every node is
// refused on the nodes that do not have that volume — the refusal names the
// node and the device, which is what told the operator to name one device per
// host in the first place.
func TestTheSingleFormIsRefusedOnNodesThatDoNotHaveThatVolume(t *testing.T) {
	opts := threeNodeLabOptions(t)
	// Today's meaning of the flag: this one device, on every node.
	opts.StorageDevices = storage.Devices{All: labVolumeA}

	rep, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("one host's by-id path was accepted for all three hosts; on the lab that is the refusal that made a multi-node install impossible")
	}
	if res, ok := volumeGroupResult(rep, labNodeA); !ok || res.Outcome != preflight.Pass {
		t.Errorf("%s has %s and must pass, got %s: %s", labNodeA, labVolumeA, res.Outcome, res.Detail)
	}
	for _, node := range []string{labNodeB, labNodeC} {
		res, ok := volumeGroupResult(rep, node)
		if !ok {
			t.Fatalf("no volume-group result for %s in %v", node, rep.Results)
		}
		if res.Outcome != preflight.Fail {
			t.Errorf("%s does not have %s and must be refused, got %s", node, labVolumeA, res.Outcome)
		}
		if !strings.Contains(res.Detail, labVolumeA) {
			t.Errorf("%s: the refusal %q does not name the device it looked for", node, res.Detail)
		}
		if !strings.Contains(err.Error(), node) {
			t.Errorf("the aggregate refusal %q does not name %s", err, node)
		}
	}
}
