package storage

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/sshx"
)

// fakeRunner scripts command → result and records every command run, so a
// test can assert both what happened and what deliberately did not.
type fakeRunner struct {
	t       *testing.T
	replies map[string]sshx.Result
	ran     []string
}

func (f *fakeRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	f.ran = append(f.ran, command)
	res, ok := f.replies[command]
	if !ok {
		f.t.Fatalf("unscripted command: %q", command)
	}
	return res, nil
}

const (
	vgsCmd   = "sudo -n vgs kubenest-vg --noheadings -o vg_free --units b --nosuffix"
	dev      = "/dev/disk/by-id/scsi-0HC_Volume_103279599"
	testBCmd = "test -b " + dev
	blkidCmd = "sudo -n blkid -p -o export " + dev
)

// vgMissing is what vgs returns for an absent volume group.
var vgMissing = sshx.Result{ExitCode: 5, Stderr: "Volume group \"kubenest-vg\" not found\n"}

func TestOption1PassesOnExistingVGWithFreeSpace(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		vgsCmd: {Stdout: "  53682896896\n"},
	}}
	own, err := PreflightVolumeGroup(context.Background(), r, "")
	if err != nil {
		t.Fatal(err)
	}
	if own != CustomerCreated {
		t.Errorf("ownership = %q, want customer-created (the backend enum value)", own)
	}
}

func TestOption1FailureNamesTheFix(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{vgsCmd: vgMissing}}
	_, err := PreflightVolumeGroup(context.Background(), r, "")
	if err == nil {
		t.Fatal("missing volume group passed preflight")
	}
	for _, want := range []string{"kubenest-vg", "vgcreate", "--storage-device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q — failures must name the fix", err, want)
		}
	}
}

func TestOption1RefusesAFullVolumeGroup(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{vgsCmd: {Stdout: "  0\n"}}}
	_, err := PreflightVolumeGroup(context.Background(), r, "")
	if err == nil || !strings.Contains(err.Error(), "free extents") {
		t.Fatalf("err = %v, want a no-free-extents refusal", err)
	}
}

func TestOption2PassesOnABlankDevice(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		vgsCmd:   vgMissing,
		testBCmd: {},
		// Verified on a real host (kn-bkwa): blank means empty output, exit 2.
		blkidCmd: {ExitCode: 2},
	}}
	own, err := PreflightVolumeGroup(context.Background(), r, dev)
	if err != nil {
		t.Fatal(err)
	}
	if own != InstallerCreated {
		t.Errorf("ownership = %q, want installer-created", own)
	}
}

func TestOption2RefusesANonBlankDeviceQuotingWhatItFound(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		vgsCmd:   vgMissing,
		testBCmd: {},
		blkidCmd: {Stdout: "TYPE=LVM2_member\n"},
	}}
	_, err := PreflightVolumeGroup(context.Background(), r, dev)
	if err == nil {
		t.Fatal("non-blank device passed preflight — this is the overwrite guard")
	}
	if !strings.Contains(err.Error(), "LVM2_member") {
		t.Errorf("refusal %q does not quote what blkid found", err)
	}
}

func TestOption2RefusesWhenVGAlreadyExists(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		vgsCmd: {Stdout: "  53682896896\n"},
	}}
	_, err := PreflightVolumeGroup(context.Background(), r, dev)
	if err == nil || !strings.Contains(err.Error(), "omit --storage-device") {
		t.Fatalf("err = %v, want the exists-plus-flag contradiction named", err)
	}
}

func TestOption2RefusesAMissingDevice(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		vgsCmd:   vgMissing,
		testBCmd: {ExitCode: 1},
	}}
	_, err := PreflightVolumeGroup(context.Background(), r, dev)
	if err == nil || !strings.Contains(err.Error(), "not a block device") {
		t.Fatalf("err = %v, want a device-not-present refusal", err)
	}
}

func TestOption2RefusesNonDevPaths(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{}}
	if _, err := PreflightVolumeGroup(context.Background(), r, "sda; rm -rf /"); err == nil {
		t.Fatal("a shell-suspect device path was accepted")
	}
	if len(r.ran) != 0 {
		t.Errorf("commands ran for an invalid device path: %v", r.ran)
	}
}

func TestEnsureIsANoOpWhenTheVGExists(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		vgsCmd: {Stdout: "  53682896896\n"},
	}}
	if err := EnsureVolumeGroup(context.Background(), r, dev); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range r.ran {
		if strings.Contains(cmd, "pvcreate") || strings.Contains(cmd, "vgcreate") {
			t.Errorf("EnsureVolumeGroup wrote to devices although the VG exists: %q", cmd)
		}
	}
}

func TestEnsureCreatesPVThenVGOnABlankDevice(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{
		vgsCmd:                                vgMissing,
		testBCmd:                              {},
		blkidCmd:                              {ExitCode: 2},
		"sudo -n pvcreate " + dev:             {},
		"sudo -n vgcreate kubenest-vg " + dev: {},
	}}
	if err := EnsureVolumeGroup(context.Background(), r, dev); err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, cmd := range r.ran {
		if strings.Contains(cmd, "pvcreate") || strings.Contains(cmd, "vgcreate") {
			writes = append(writes, cmd)
		}
	}
	if len(writes) != 2 || !strings.Contains(writes[0], "pvcreate") || !strings.Contains(writes[1], "vgcreate") {
		t.Errorf("device writes = %v, want pvcreate then vgcreate", writes)
	}
}

// Option 1 never writes: a missing VG at ensure time means the node changed
// after preflight, and the answer is re-running preflight, not improvising.
func TestEnsureWithoutDeviceNeverCreates(t *testing.T) {
	r := &fakeRunner{t: t, replies: map[string]sshx.Result{vgsCmd: vgMissing}}
	err := EnsureVolumeGroup(context.Background(), r, "")
	if err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("err = %v, want a re-run-preflight error", err)
	}
	for _, cmd := range r.ran {
		if strings.Contains(cmd, "create") {
			t.Errorf("Option 1 wrote to a device: %q", cmd)
		}
	}
}
