package storage

import (
	"strings"
	"testing"
)

// The three-node case the flag exists for: each host has its own data volume,
// and the stable by-id path names that volume's serial, so no two hosts share
// a device path.
const (
	hostA = "10.0.3.10"
	hostB = "10.0.3.11"
	hostC = "10.0.3.12"

	volA = "/dev/disk/by-id/scsi-0HC_Volume_101"
	volB = "/dev/disk/by-id/scsi-0HC_Volume_102"
	volC = "/dev/disk/by-id/scsi-0HC_Volume_103"
)

func threeNodes() []string { return []string{hostA, hostB, hostC} }

// A per-host mapping answers with each node's own device — the whole point:
// one value applied to every node cannot describe three hosts whose by-id
// paths differ.
func TestPerHostDevicesAnswerWithEachNodesOwnDevice(t *testing.T) {
	d, err := ParseDevices([]string{hostA + "=" + volA, hostB + "=" + volB, hostC + "=" + volC}, threeNodes())
	if err != nil {
		t.Fatal(err)
	}
	if d.All != "" {
		t.Errorf("a per-host mapping also carries the single device %q: only one form may be in play", d.All)
	}
	for host, want := range map[string]string{hostA: volA, hostB: volB, hostC: volC} {
		if got := d.For(host); got != want {
			t.Errorf("For(%s) = %q, want %q", host, got, want)
		}
	}
	if d.Empty() {
		t.Error("a mapping that names three devices reports itself empty, so the install would treat every volume group as the operator's")
	}
}

// The single form keeps today's meaning: one device, every node.
func TestSingleDeviceAnswersForEveryNode(t *testing.T) {
	d, err := ParseDevices([]string{"/dev/nvme1n1"}, threeNodes())
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range threeNodes() {
		if got := d.For(host); got != "/dev/nvme1n1" {
			t.Errorf("For(%s) = %q, want the single device on every node", host, got)
		}
	}
}

// No flag at all is Option 1: the operator created kubenest-vg themselves and
// the installer touches no block device.
func TestNoValuesMeansTheOperatorOwnsTheVolumeGroup(t *testing.T) {
	d, err := ParseDevices(nil, threeNodes())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Empty() {
		t.Errorf("no --storage-device produced %+v, want the empty mapping", d)
	}
	for _, host := range threeNodes() {
		if got := d.For(host); got != "" {
			t.Errorf("For(%s) = %q, want no device", host, got)
		}
	}
}

// The order the flags were written in is not part of the request: a resume
// that reorders them is the same install, and must not be refused as a changed
// mapping.
func TestTheMappingIsTheSameWhateverOrderItWasWrittenIn(t *testing.T) {
	forward, err := ParseDevices([]string{hostA + "=" + volA, hostB + "=" + volB, hostC + "=" + volC}, threeNodes())
	if err != nil {
		t.Fatal(err)
	}
	backward, err := ParseDevices([]string{hostC + "=" + volC, hostB + "=" + volB, hostA + "=" + volA}, threeNodes())
	if err != nil {
		t.Fatal(err)
	}
	if forward.Identity() != backward.Identity() {
		t.Errorf("the same mapping written in two orders identifies as\n  %q\n  %q\na resume would refuse the reordered command", forward.Identity(), backward.Identity())
	}
	// And a mapping that really differs must identify differently, or a
	// resume would silently keep using the first run's device.
	changed, err := ParseDevices([]string{hostA + "=" + volA, hostB + "=" + volA, hostC + "=" + volC}, threeNodes())
	if err != nil {
		t.Fatal(err)
	}
	if changed.Identity() == forward.Identity() {
		t.Error("two different mappings share one identity, so a resume cannot tell them apart")
	}
}

func refused(t *testing.T, values []string, hosts []string, mustName ...string) string {
	t.Helper()
	d, err := ParseDevices(values, hosts)
	if err == nil {
		t.Fatalf("--storage-device %v was accepted as %+v; it cannot describe one install", values, d)
	}
	for _, want := range mustName {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q — a refusal has to name the node and the fix", err, want)
		}
	}
	return err.Error()
}

// Mixing the two forms has no meaning: one value claims every node and the
// other claims a node inside that set.
func TestMixingTheTwoFormsIsRefused(t *testing.T) {
	refused(t,
		[]string{hostA + "=" + volA, hostB + "=" + volB, hostC + "=" + volC, "/dev/nvme1n1"},
		threeNodes(), "--storage-device", hostA, hostB, hostC)
}

// A host that is not a node of this install would silently leave a real node
// unnamed — the install would then create the volume group on some nodes only.
func TestAHostThatIsNotANodeIsRefused(t *testing.T) {
	refused(t,
		[]string{hostA + "=" + volA, hostB + "=" + volB, "10.0.3.19=" + volC},
		threeNodes(), "--storage-device", "10.0.3.19", hostA, hostC)
}

// Two values for one node would make which device wins a matter of flag order.
func TestAHostNamedTwiceIsRefused(t *testing.T) {
	refused(t,
		[]string{hostA + "=" + volA, hostA + "=" + volB, hostC + "=" + volC},
		threeNodes(), "--storage-device", hostA)
}

// A per-host set has to name every node. The node it left out is the node the
// refusal must name, because that is the one the operator has to add.
func TestAPerHostSetThatSkipsANodeIsRefusedNamingIt(t *testing.T) {
	refused(t,
		[]string{hostA + "=" + volA, hostB + "=" + volB},
		threeNodes(), "--storage-device", hostC)
}

// A per-host value whose device is blank says nothing about where the volume
// group comes from, so it is refused rather than read as "no device here".
func TestAHostWithNoDeviceIsRefused(t *testing.T) {
	refused(t,
		[]string{hostA + "=", hostB + "=" + volB, hostC + "=" + volC},
		threeNodes(), "--storage-device", hostA)
}

// A value with no host before the "=" names nothing at all.
func TestAValueWithNoHostIsRefused(t *testing.T) {
	refused(t, []string{"=" + volA}, threeNodes(), "--storage-device")
}

// The single form given twice contradicts itself: two devices cannot both be
// the one used on every node.
func TestTheSingleFormGivenTwiceIsRefused(t *testing.T) {
	refused(t, []string{"/dev/nvme1n1", "/dev/nvme2n1"}, threeNodes(), "--storage-device")
}

// The order the nodes were given is the order a refusal lists them in, and the
// missing-node refusal is the one an operator hits by adding a --agent and
// forgetting its device.
func TestTheMissingNodeRefusalNamesTheNodeAndBothWaysOn(t *testing.T) {
	message := refused(t,
		[]string{hostA + "=" + volA, hostC + "=" + volC},
		threeNodes(), "--storage-device", hostB)
	if !strings.Contains(message, "one device on every node") {
		t.Errorf("refusal %q does not offer the single form as a way on, which is the other way an operator could describe this install", message)
	}
}
