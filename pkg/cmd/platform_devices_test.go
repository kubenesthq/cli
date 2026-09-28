package cmd

import (
	"strings"
	"testing"
)

// The --storage-device shapes that cannot describe one install are refused at
// flag validation, which runs before anything is read from this machine's
// config and before a host is dialled. Each refusal has to name the node it is
// about and offer the way on.
func TestInstallRefusesStorageDeviceShapesBeforeTouchingAHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	threeServers := []string{
		"platform", "install", "--bundle", "1.0", "--name", "prod-ha", "--ha", "ha",
		"--server", "10.0.3.10", "--server", "10.0.3.11", "--server", "10.0.3.12",
	}
	with := func(values ...string) []string {
		args := append([]string(nil), threeServers...)
		for _, value := range values {
			args = append(args, "--storage-device", value)
		}
		return args
	}

	for _, c := range []struct {
		name string
		args []string
		// want is the node the refusal has to name.
		want string
	}{
		{
			name: "mixing one device with per-host values",
			args: with("10.0.3.10=/dev/nvme1n1", "10.0.3.11=/dev/nvme1n1", "10.0.3.12=/dev/nvme1n1", "/dev/nvme1n1"),
			want: "10.0.3.10",
		},
		{
			name: "a host that is not a node of this install",
			args: with("10.0.3.10=/dev/nvme1n1", "10.0.3.11=/dev/nvme1n1", "10.0.3.19=/dev/nvme1n1"),
			want: "10.0.3.19",
		},
		{
			name: "one host named twice",
			args: with("10.0.3.10=/dev/nvme1n1", "10.0.3.10=/dev/nvme2n1", "10.0.3.11=/dev/nvme1n1", "10.0.3.12=/dev/nvme1n1"),
			want: "10.0.3.10",
		},
		{
			name: "a per-host set that leaves a node out",
			args: with("10.0.3.10=/dev/nvme1n1", "10.0.3.11=/dev/nvme1n1"),
			want: "10.0.3.12",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := NewRootCommand()
			root.SetArgs(c.args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("%v was accepted; this install would either fail later or build on the wrong device", c.args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal %q does not name %s", err, c.want)
			}
			if !strings.Contains(err.Error(), "--storage-device") {
				t.Errorf("refusal %q does not name the flag to change", err)
			}
		})
	}
}

// The positive control for the refusals above: per-host values that name every
// node pass flag validation, so the refusals come from the shapes and not from
// the flag being refused outright.
func TestInstallAcceptsOneDevicePerNode(t *testing.T) {
	f := InstallFlags{
		Bundle:  "1.0",
		Name:    "prod-ha",
		HATier:  "ha",
		Servers: []string{"10.0.3.10", "10.0.3.11", "10.0.3.12"},
		StorageDevices: []string{
			"10.0.3.10=/dev/disk/by-id/scsi-0HC_Volume_101",
			"10.0.3.11=/dev/disk/by-id/scsi-0HC_Volume_102",
			"10.0.3.12=/dev/disk/by-id/scsi-0HC_Volume_103",
		},
	}
	if err := (&f).Validate(); err != nil {
		t.Errorf("one device per node was refused: %v", err)
	}
	devices, err := f.storageDevices()
	if err != nil {
		t.Fatal(err)
	}
	if got := devices.For("10.0.3.11"); got != "/dev/disk/by-id/scsi-0HC_Volume_102" {
		t.Errorf("the resolved mapping gives 10.0.3.11 %q, want its own volume", got)
	}

	// And the single form still validates, for a cluster whose nodes share a
	// device path.
	f.StorageDevices = []string{"/dev/nvme1n1"}
	if err := (&f).Validate(); err != nil {
		t.Errorf("the single device was refused: %v", err)
	}
}
