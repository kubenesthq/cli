// Package storage installs the platform's persistent storage: OpenEBS Local
// PV LVM out of the kubenest-vg volume group, with a default StorageClass
// (kn-1nn). Replicated storage is a profile, never core — durability in the
// default configuration comes from verified backups plus application-level
// replication, because replicated block storage is itself a source of the
// operational dread this product sells against.
//
// The volume-group rules follow install.mdx "Disks and storage" exactly:
//
//   - Option 1 (default): the customer created kubenest-vg on each
//     data-bearing node; preflight verifies it exists and has free extents,
//     and the installer never touches block devices.
//   - Option 2 (--storage-device): the installer creates kubenest-vg, but
//     ONLY on a device blkid reports as blank. A partition table, a
//     filesystem or an existing LVM PV is a refusal, never an overwrite.
//     There is no device auto-detection, and carving free space from the
//     root disk is not supported.
//
// Which option ran is RECORDED as the volume-group ownership, because it
// gates what uninstall may destroy: a customer-created volume group is never
// removed, on any path. The ownership strings are the backend's
// VolumeGroupOwnership enum values (kn-boj) verbatim.
//
// All findings from the kn-bkwa run on a real Ubuntu 24.04 host are baked
// in: lvm2 is pre-installed on the stock cloud image (Option 1 asks nothing
// extra of the customer), device paths should be the stable
// /dev/disk/by-id/... form because /dev/sdX moves between boots, and blkid
// is the blank-vs-not oracle (empty output on a blank device).
package storage

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"kubenest.io/cli/pkg/k3s"
)

// VolumeGroup is the volume group name the platform requires on every
// data-bearing node. install.mdx names it literally; it is not configurable.
const VolumeGroup = "kubenest-vg"

// Ownership records who created kubenest-vg. The values are the backend's
// VolumeGroupOwnership enum (app/models/cluster.py, kn-boj) and are what the
// installer's record stage writes to PUT /clusters/{id}/bundle.
type Ownership string

const (
	// CustomerCreated: Option 1 — the volume group predates the installer.
	// Uninstall must never remove it.
	CustomerCreated Ownership = "customer-created"
	// InstallerCreated: Option 2 — the installer created it on a blank
	// device named by --storage-device. Uninstall --destroy-data may remove
	// it.
	InstallerCreated Ownership = "installer-created"
)

// devicePath keeps device arguments shell-safe. Stable by-id paths
// (/dev/disk/by-id/scsi-0HC_Volume_123) fit; anything stranger is refused
// rather than quoted.
var devicePath = regexp.MustCompile(`^/dev/[A-Za-z0-9/_.:+-]+$`)

// PreflightVolumeGroup runs the volume-group preflight check on ONE node
// (the installer runs it on every data-bearing node). It writes nothing
// anywhere — preflight is the whole reason a failed install is cheap.
//
// device == "" is Option 1: kubenest-vg must exist and have free extents.
// device != "" is Option 2: the device must exist and be blank per blkid.
//
// The returned Ownership is what the record stage will write if the install
// proceeds.
func PreflightVolumeGroup(ctx context.Context, r k3s.Runner, device string) (Ownership, error) {
	if device == "" {
		free, err := vgFreeBytes(ctx, r)
		if err != nil {
			return "", err
		}
		if free <= 0 {
			return "", fmt.Errorf(
				"volume group %s exists but has no free extents: extend it (vgextend %s <device>) or free space in it — Local PV LVM carves volumes from its free space",
				VolumeGroup, VolumeGroup)
		}
		return CustomerCreated, nil
	}

	if !devicePath.MatchString(device) {
		return "", fmt.Errorf("--storage-device %q is not a /dev path; use the stable /dev/disk/by-id/... form (unstable /dev/sdX names move between boots)", device)
	}

	// A present kubenest-vg plus --storage-device is a contradiction, not a
	// merge: the flag exists only to create the volume group.
	if _, err := vgFreeBytes(ctx, r); err == nil {
		return "", fmt.Errorf(
			"%s already exists on this node: omit --storage-device to use the existing volume group (the installer never adds devices to a volume group it did not create)",
			VolumeGroup)
	}

	if err := deviceIsBlank(ctx, r, device); err != nil {
		return "", err
	}
	return InstallerCreated, nil
}

// EnsureVolumeGroup makes kubenest-vg exist, per the option preflight chose.
// This is the write half that stage 7 runs AFTER preflight passed:
//
//   - Option 1 (device == ""): verify only; the installer never touches
//     block devices the customer manages.
//   - Option 2: pvcreate + vgcreate on the (still blank) device. If
//     kubenest-vg already exists the call is a no-op, which is what makes a
//     resumed install idempotent — the journal, not this function, knows
//     whether an earlier attempt created it.
func EnsureVolumeGroup(ctx context.Context, r k3s.Runner, device string) error {
	if _, err := vgFreeBytes(ctx, r); err == nil {
		return nil // exists — Option 1, or a resumed Option 2
	}
	if device == "" {
		return fmt.Errorf("volume group %s not found: preflight verifies it before install, so a missing volume group here means the node changed under the installer — re-run preflight", VolumeGroup)
	}
	if !devicePath.MatchString(device) {
		return fmt.Errorf("--storage-device %q is not a /dev path; use the stable /dev/disk/by-id/... form", device)
	}
	if err := deviceIsBlank(ctx, r, device); err != nil {
		return err
	}
	for _, cmd := range []string{
		"sudo -n pvcreate " + device,
		"sudo -n vgcreate " + VolumeGroup + " " + device,
	} {
		res, err := r.Run(ctx, cmd)
		if err != nil {
			return fmt.Errorf("%s: %w", cmd, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("%s: exit %d: %s", cmd, res.ExitCode, firstLine(res.Stderr))
		}
	}
	return nil
}

// vgFreeBytes reports kubenest-vg's free bytes, or an error naming the fix
// when the volume group does not exist (or lvm is unqueryable).
func vgFreeBytes(ctx context.Context, r k3s.Runner) (int64, error) {
	res, err := r.Run(ctx, fmt.Sprintf("sudo -n vgs %s --noheadings -o vg_free --units b --nosuffix", VolumeGroup))
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, fmt.Errorf(
			"volume group %s not found on this node: create it before installing (pvcreate <device> && vgcreate %s <device> — lvm2 is preinstalled on Ubuntu 24.04), or pass --storage-device <blank-device> to let the installer create it",
			VolumeGroup, VolumeGroup)
	}
	var free int64
	if _, err := fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &free); err != nil {
		return 0, fmt.Errorf("vgs %s returned unparsable free-space %q", VolumeGroup, strings.TrimSpace(res.Stdout))
	}
	return free, nil
}

// deviceIsBlank refuses any device blkid can identify. Empty blkid output is
// the definition of blank (install.mdx preflight table; verified on a real
// host in kn-bkwa): a partition table reports PTTYPE, a filesystem or LVM PV
// reports TYPE, and either is data this installer must not overwrite.
func deviceIsBlank(ctx context.Context, r k3s.Runner, device string) error {
	res, err := r.Run(ctx, "test -b "+device)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("--storage-device %s is not a block device on this node: attach the volume, or check the by-id path (ls /dev/disk/by-id/)", device)
	}
	// -p probes the device directly instead of trusting the blkid cache — a
	// device wiped or written since the cache was built must not be misread.
	res, err = r.Run(ctx, "sudo -n blkid -p -o export "+device)
	if err != nil {
		return err
	}
	if out := strings.TrimSpace(res.Stdout); out != "" {
		return fmt.Errorf(
			"--storage-device %s is NOT blank and the installer will not overwrite it (%s): wipe it deliberately yourself if it is disposable, or name a different device",
			device, firstLine(out))
	}
	// blkid exits 2 with empty output on a truly blank device; exit 0 with
	// empty output does not occur, but empty output is the contract either
	// way. A missing blkid binary (127) would be a broken base image.
	if res.ExitCode != 0 && res.ExitCode != 2 {
		return fmt.Errorf("blkid -p %s: exit %d: %s", device, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
