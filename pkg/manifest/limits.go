package manifest

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Quantity is a byte count written in the manifest's binary form (Mi/Gi/Ti).
//
// Binary is not a stylistic choice. `MemTotal` in /proc/meminfo is KiB and
// statfs reports blocks, so a floor written as "4 GB" and compared against
// what the kernel reports refuses a host that meets the specification — a
// 4 GB machine delivers 3.7 GiB, and the floor FAILS rather than warns
// (observed on a real host, kn-bkwa). The manifest therefore carries the
// numbers already in the units the check reads.
type Quantity int64

// binary unit suffixes, largest first so parsing matches the longest.
var quantityUnits = []struct {
	suffix string
	scale  int64
}{
	{"Ti", 1 << 40},
	{"Gi", 1 << 30},
	{"Mi", 1 << 20},
}

// ParseQuantity parses "3.7Gi" into bytes. Decimal suffixes are refused
// rather than converted: accepting "4GB" here would quietly reintroduce the
// unit confusion this type exists to remove.
func ParseQuantity(s string) (Quantity, error) {
	raw := strings.TrimSpace(s)
	for _, u := range quantityUnits {
		mantissa, ok := strings.CutSuffix(raw, u.suffix)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(mantissa, 64)
		if err != nil || f <= 0 {
			return 0, fmt.Errorf("%q is not a quantity (want a positive number then Mi, Gi or Ti)", s)
		}
		return Quantity(f * float64(u.scale)), nil
	}
	return 0, fmt.Errorf("%q has no binary unit: quantities are Mi, Gi or Ti, in the units the kernel reports (a floor in GB compared against MemTotal refuses correctly-sized hosts)", s)
}

// UnmarshalYAML parses the binaryQuantity form from bundle-manifest.schema.json.
func (q *Quantity) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := ParseQuantity(raw)
	if err != nil {
		return err
	}
	*q = parsed
	return nil
}

// Bytes returns the quantity as a byte count.
func (q Quantity) Bytes() int64 { return int64(q) }

// String renders the quantity in the largest binary unit that keeps it >= 1,
// which is how preflight reports both the threshold and what it measured —
// the two must be legible against each other.
func (q Quantity) String() string {
	for _, u := range quantityUnits {
		if int64(q) >= u.scale {
			return strconv.FormatFloat(float64(q)/float64(u.scale), 'f', 1, 64) + u.suffix
		}
	}
	return strconv.FormatInt(int64(q), 10) + "B"
}

// MachineResources is one node's thresholds: the floor that fails and the
// recommendation that warns.
type MachineResources struct {
	CPU    int      `yaml:"cpu"`
	Memory Quantity `yaml:"memory"`
	Disk   Quantity `yaml:"disk"`
}

func (m MachineResources) complete() bool {
	return m.CPU > 0 && m.Memory > 0 && m.Disk > 0
}

// Resources is limits.resources: what preflight compares every node against.
type Resources struct {
	Floor           MachineResources `yaml:"floor"`
	Recommended     MachineResources `yaml:"recommended"`
	UpgradeHeadroom UpgradeHeadroom  `yaml:"upgrade-headroom"`
}

// UpgradeHeadroom is the free disk an upgrade needs before it starts.
type UpgradeHeadroom struct {
	Disk Quantity `yaml:"disk"`
}

// Floor returns the hard floor, below which preflight FAILS. Missing is an
// error, never a built-in default: a hardcoded floor is exactly how a
// provisional number outlives the measurement that was meant to replace it
// (kn-ze1 replaces these by editing the manifest, not the installer).
func (l Limits) Floor() (MachineResources, error) {
	if !l.Resources.Floor.complete() {
		return MachineResources{}, fmt.Errorf("bundle manifest has no limits.resources.floor: the bundle decides the sizing thresholds, add them to the manifest rather than defaulting in code")
	}
	return l.Resources.Floor, nil
}

// Recommended returns the recommendation, below which preflight WARNS. The
// warn/fail split is why a provisional number cannot refuse a correctly-sized
// customer host.
func (l Limits) Recommended() (MachineResources, error) {
	if !l.Resources.Recommended.complete() {
		return MachineResources{}, fmt.Errorf("bundle manifest has no limits.resources.recommended: the bundle decides the sizing thresholds, add them to the manifest rather than defaulting in code")
	}
	return l.Resources.Recommended, nil
}

// OS is the tested OS matrix. It moves with the bundle, not with a docs edit.
type OS struct {
	Supported []string `yaml:"supported"`
}

// Supports reports whether an os-release ID and VERSION_ID pair is in the
// tested matrix. The manifest's form is "ubuntu-24.04"; /etc/os-release gives
// ID=ubuntu and VERSION_ID="24.04".
func (o OS) Supports(id, versionID string) bool {
	want := strings.ToLower(id) + "-" + versionID
	for _, s := range o.Supported {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// Profile is one optional component set installed on top of core.
type Profile struct {
	// Provisional marks a profile not yet tested against core as a unit.
	Provisional bool `yaml:"provisional"`
	// Components maps component name to pinned version. The `ha` profile is
	// a topology change and carries none.
	Components Components `yaml:"components"`
}

// Profiles maps profile name to its definition.
type Profiles map[string]Profile

// Get returns a profile by name. An unknown profile is REJECTED, not ignored:
// silently installing core when someone asked for `--profile observability`
// produces a cluster that does not match its own record, and the record is
// what every day-2 operation trusts.
func (p Profiles) Get(name string) (Profile, error) {
	prof, ok := p[name]
	if !ok {
		return Profile{}, fmt.Errorf("bundle does not offer a profile named %q: this bundle offers %s", name, orNone(p.Names()))
	}
	return prof, nil
}

// Names returns the offered profile names, sorted.
func (p Profiles) Names() []string {
	names := make([]string, 0, len(p))
	for name := range p {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// OffersTier reports whether this bundle offers an HA tier, so an install
// asking for one it does not offer fails clearly instead of silently
// installing single-server.
func (m *Manifest) OffersTier(tier string) error {
	for _, t := range m.HATiers {
		if t == tier {
			return nil
		}
	}
	return fmt.Errorf("bundle %s does not offer the %q tier: it offers %s", m.Bundle, tier, orNone(m.HATiers))
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// sortStrings is sort.Strings without pulling sort into this file's imports
// for one call. Profile lists are single digits long.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Upgrade is the tooling pinned with the release for moving between bundles.
type Upgrade struct {
	DeprecationScanner DeprecationScanner `yaml:"deprecation-scanner"`
}

// DeprecationScanner pins the pre-flight API deprecation scan (decision D,
// 2026-08-20): pluto, adopted not rebuilt.
//
// The DATASET pin is the load-bearing one. A scanner whose deprecation data
// drifts reports confidence it has not earned — it will tell a customer their
// workloads are clean against a Kubernetes version whose removals it has
// never heard of, which is worse than not scanning at all, because they will
// believe it.
type DeprecationScanner struct {
	Tool    string `yaml:"tool"`
	Version string `yaml:"version"`
	Dataset string `yaml:"dataset"`
}

// Scanner returns the pinned deprecation scanner. A manifest without one
// cannot gate an upgrade, and the missing pin is an error rather than a
// silent skip: skipping the scan is the one outcome that must never happen
// quietly.
func (u Upgrade) Scanner() (DeprecationScanner, error) {
	s := u.DeprecationScanner
	if s.Tool == "" || s.Version == "" || s.Dataset == "" {
		return DeprecationScanner{}, fmt.Errorf("bundle manifest does not pin upgrade.deprecation-scanner (tool, version and dataset): the scan is the gate that stops an upgrade taking a customer's product down, and it may not be skipped because a pin is missing")
	}
	return s, nil
}
