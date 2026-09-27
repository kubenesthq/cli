package recovery

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/storage"
)

// StorageRequirementKey is the recovery set's declared requirement: the number
// of BYTES the volume group must hold for the restored volumes to fit.
//
// It is written into the set's Versions map by the install that writes the kit,
// because that is the last moment anybody measured the host the workloads
// actually ran on: a replacement host that cannot hold what the dead host held
// cannot complete the restore, and finding that out halfway through a restore
// costs the operator the whole operation. Nothing writes it yet for sets
// updated by `kubenest backup now` alone (that path preserves the key it finds),
// and an absent value is reported as "cannot tell", never as a pass.
const StorageRequirementKey = "restore.volume-group-bytes"

// Capacity is what the replacement host can hold, against what the recovery
// set says the restored volumes need.
type Capacity struct {
	// Required is the declared requirement, in bytes. Meaningless when
	// Declared is false.
	Required int64
	// Declared is whether the set states a requirement at all.
	Declared bool
	// Available is the replacement host's kubenest-vg size, or its blank
	// --storage-device size when no volume group exists yet, in bytes.
	Available int64
	// Known is whether the host's capacity could be read.
	Known bool
}

// Sufficient reports whether the host is provably large enough. It is false
// when either number is unknown — "cannot tell" is never a pass.
func (c Capacity) Sufficient() bool {
	return c.Declared && c.Known && c.Available >= c.Required
}

// Report is the one-line answer, for the operation record and the terminal.
func (c Capacity) Report() string {
	switch {
	case !c.Declared:
		return fmt.Sprintf("this recovery set declares no %s requirement (sets written by `kubenest backup now` alone do not), so the replacement host's %s capacity was not judged; the restore will fail if it does not fit", StorageRequirementKey, storage.VolumeGroup)
	case !c.Known:
		return fmt.Sprintf("the recovery set requires %d bytes in %s and this host's capacity could not be read (no %s and no --storage-device)", c.Required, storage.VolumeGroup, storage.VolumeGroup)
	case c.Available >= c.Required:
		return fmt.Sprintf("%s on this host holds %d bytes and the recovery set requires %d", storage.VolumeGroup, c.Available, c.Required)
	default:
		return fmt.Sprintf("%s on this host holds %d bytes and the recovery set requires %d: it is %d bytes short", storage.VolumeGroup, c.Available, c.Required, c.Required-c.Available)
	}
}

// RequiredVolumeGroupBytes reads the recovery set's declared requirement. The
// second return value is false when the set declares none, which is a
// different fact from a requirement of zero.
func RequiredVolumeGroupBytes(set *recoverykit.Set) (int64, bool, error) {
	if set == nil {
		return 0, false, errors.New("no recovery set: the requirement comes from the set that binds the kit to the backup")
	}
	raw, ok := set.Versions[StorageRequirementKey]
	if !ok || strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("the recovery set's %s is %q, which is not a byte count: the check it drives cannot be skipped silently, and a number nobody can read is worse than no number", StorageRequirementKey, raw)
	}
	if n < 0 {
		return 0, false, fmt.Errorf("the recovery set's %s is negative (%d)", StorageRequirementKey, n)
	}
	return n, true, nil
}

// VolumeGroupBytes reads the size of the host's kubenest-vg. Known is false
// when the host has no such volume group yet — a fresh host the installer is
// about to create one on.
func VolumeGroupBytes(ctx context.Context, r k3s.Runner) (int64, bool, error) {
	res, err := r.Run(ctx, "sudo -n vgs --noheadings --units b --nosuffix -o vg_size "+storage.VolumeGroup)
	if err != nil {
		return 0, false, fmt.Errorf("reading %s on the replacement host: %w", storage.VolumeGroup, err)
	}
	if res.ExitCode != 0 {
		// No volume group yet is the ordinary state of a fresh host, not a
		// failure: the installer creates one from --storage-device.
		return 0, false, nil
	}
	n, err := parseFirstInt(res.Stdout)
	if err != nil {
		return 0, false, fmt.Errorf("reading the size of %s from `vgs`: %w", storage.VolumeGroup, err)
	}
	return n, true, nil
}

// DeviceBytes reads a blank block device's size, for the install that will
// create the volume group on it.
func DeviceBytes(ctx context.Context, r k3s.Runner, device string) (int64, bool, error) {
	if strings.TrimSpace(device) == "" {
		return 0, false, nil
	}
	res, err := r.Run(ctx, "sudo -n blockdev --getsize64 "+device)
	if err != nil {
		return 0, false, fmt.Errorf("reading the size of %s: %w", device, err)
	}
	if res.ExitCode != 0 {
		return 0, false, fmt.Errorf("reading the size of %s: exit %d: %s", device, res.ExitCode, firstLine(res.Stderr))
	}
	n, err := parseFirstInt(res.Stdout)
	if err != nil {
		return 0, false, fmt.Errorf("reading the size of %s: %w", device, err)
	}
	return n, true, nil
}

// CheckCapacity refuses a replacement host too small for the volumes the
// recovery is about to restore, naming the volume group and the shortfall.
//
// IT IS A PRE-FLIGHT CHECK, BEFORE THE FIRST WRITE ANYWHERE. A recovery that
// discovers halfway through that the disks cannot hold the data has already
// destroyed the new machine's usefulness and has a half-restored cluster in
// front of the operator; the same fact costs nothing two minutes earlier.
func CheckCapacity(ctx context.Context, r k3s.Runner, set *recoverykit.Set, storageDevice string) (Capacity, error) {
	required, declared, err := RequiredVolumeGroupBytes(set)
	if err != nil {
		return Capacity{}, err
	}
	out := Capacity{Required: required, Declared: declared}
	available, known, err := VolumeGroupBytes(ctx, r)
	if err != nil {
		return out, err
	}
	if !known {
		available, known, err = DeviceBytes(ctx, r, storageDevice)
		if err != nil {
			return out, err
		}
	}
	out.Available, out.Known = available, known
	if declared && known && available < required {
		return out, fmt.Errorf("the replacement host is too small for this recovery: %s would hold %d bytes and the restored volumes need %d — a shortfall of %d bytes. The recovery set's %s records what the dead host's volume group held, and a restore into less than that fails partway through with a cluster that is neither the old one nor a working new one. Give this host a %s of at least %d bytes (a larger --storage-device, or one you created yourself) and run the same command again",
			storage.VolumeGroup, available, required, required-available, StorageRequirementKey, storage.VolumeGroup, required)
	}
	return out, nil
}

func parseFirstInt(out string) (int64, error) {
	for _, field := range strings.Fields(out) {
		if n, err := strconv.ParseInt(field, 10, 64); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("%q contains no number", strings.TrimSpace(out))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return strings.TrimSpace(s)
}
