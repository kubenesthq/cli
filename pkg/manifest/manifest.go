// Package manifest reads the bundle manifest — the versioned record of what a
// platform release contains (see docs.kubenest.io/platform/bundle).
//
// This package parses only what the CLI consumes: the core pins, the tested
// OS matrix, the HA tiers, the profile set, and limits — the deadlines for
// every convergence check and the sizing thresholds preflight enforces.
// Limits are part of the bundle, not constants in the code: a missing timeout
// or a missing floor is an error here, never a hardcoded default. The shipped
// manifest carries the defaults; the code carries none.
//
// The manifest schema itself is owned by kubenest-contracts (kn-boj); the
// field names here follow bundle.mdx and must track that schema.
package manifest

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is the subset of the bundle manifest the CLI reads.
type Manifest struct {
	// Bundle is the platform version this manifest pins, e.g. "1.0".
	Bundle string `yaml:"bundle"`
	// Core maps each core component to its pinned version — Helm chart
	// version for chart-installed components, upstream release tag otherwise
	// (the pin rule in bundle-manifest.schema.json).
	Core Components `yaml:"core"`
	// OS is the tested OS matrix; preflight refuses a node outside it.
	OS OS `yaml:"os"`
	// HATiers are the tiers this bundle offers. Asking for one it does not
	// offer is a refusal, not a silent downgrade to single-server.
	HATiers  []string `yaml:"ha-tiers"`
	Limits   Limits   `yaml:"limits"`
	Backup   Backup   `yaml:"backup"`
	Profiles Profiles `yaml:"profiles"`
}

// Components maps component name to pinned version.
type Components map[string]string

// Version returns the pin for a named component. A missing component is an
// error for the same reason a missing timeout is: the manifest is the record
// of what the platform installs, and a version constant in code would
// silently unrecord it.
func (c Components) Version(name string) (string, error) {
	v, ok := c[name]
	if !ok {
		return "", fmt.Errorf("bundle manifest does not pin core.%s: the bundle decides every version, add the pin to the manifest rather than defaulting in code", name)
	}
	return v, nil
}

type Limits struct {
	// Resources are the sizing thresholds preflight compares every node
	// against, in binary units (see limits.go).
	Resources Resources `yaml:"resources"`
	Timeouts  Timeouts  `yaml:"timeouts"`
}

// Timeouts maps a wait's name (node-ready, component-ready, install-total, …)
// to its deadline. Nothing in the platform waits forever, and nothing decides
// how long to wait outside the manifest.
type Timeouts map[string]time.Duration

// UnmarshalYAML parses each value as a Go duration string ("5m", "2h").
func (t *Timeouts) UnmarshalYAML(node *yaml.Node) error {
	raw := map[string]string{}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	out := make(Timeouts, len(raw))
	for key, val := range raw {
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("limits.timeouts.%s: %q is not a duration (want e.g. \"5m\", \"2h\")", key, val)
		}
		if d <= 0 {
			return fmt.Errorf("limits.timeouts.%s: %q must be positive", key, val)
		}
		out[key] = d
	}
	*t = out
	return nil
}

// For returns the deadline for a named wait. A missing key is an error — the
// caller must not fall back to a constant, because then the manifest would no
// longer be the record of what the platform does.
func (t Timeouts) For(name string) (time.Duration, error) {
	d, ok := t[name]
	if !ok {
		return 0, fmt.Errorf("bundle manifest has no limits.timeouts.%s: the bundle decides every deadline, add it to the manifest rather than defaulting in code", name)
	}
	return d, nil
}

// Backup is the manifest's backup section: the pinned object-store plugin
// and the default schedules (decision E, 2026-08-20).
type Backup struct {
	ObjectStorePlugin ObjectStorePlugin `yaml:"object-store-plugin"`
	Defaults          BackupDefaults    `yaml:"defaults"`
}

// ObjectStorePlugin pins the Velero object-store plugin the installer ships
// as an init container. One plugin serves every S3-compatible target; its
// major.minor pairs with the Velero app version (upstream compatibility
// matrix), which is why it is pinned beside the schedules and not hardcoded.
type ObjectStorePlugin struct {
	Provider string `yaml:"provider"`
	Version  string `yaml:"version"`
}

// Plugin returns the pinned object-store plugin. A manifest without one
// cannot configure any backup target, and the version must not fall back to
// a constant for the same reason as every other pin.
func (b Backup) Plugin() (ObjectStorePlugin, error) {
	p := b.ObjectStorePlugin
	if p.Provider == "" || p.Version == "" {
		return ObjectStorePlugin{}, fmt.Errorf("bundle manifest does not pin backup.object-store-plugin: the bundle decides the plugin and its version, add the pin to the manifest rather than defaulting in code")
	}
	return p, nil
}

// BackupDefaults carries the default cadences and retention (decision E:
// datastore snapshot hourly / 24 kept, workload backup daily / 14 kept,
// drill weekly).
type BackupDefaults struct {
	DatastoreSnapshot BackupSchedule `yaml:"datastore-snapshot"`
	WorkloadBackup    BackupSchedule `yaml:"workload-backup"`
	RestoreDrill      DrillSchedule  `yaml:"restore-drill"`
}

// Workload returns the workload-backup schedule. Missing means the manifest
// is not carrying its defaults — an error, not a fallback.
func (d BackupDefaults) Workload() (BackupSchedule, error) {
	s := d.WorkloadBackup
	if s.Interval <= 0 || s.Keep <= 0 {
		return BackupSchedule{}, fmt.Errorf("bundle manifest has no backup.defaults.workload-backup: the bundle decides the schedule and retention, add them to the manifest rather than defaulting in code")
	}
	return s, nil
}

// BackupSchedule is one cadence-plus-retention pair. Keep counts backups:
// retention is Keep × Interval.
type BackupSchedule struct {
	Interval Interval `yaml:"interval"`
	Keep     int      `yaml:"keep"`
}

// DrillSchedule is the restore-drill cadence (wave 3 reads it; parsed now so
// the section round-trips whole).
type DrillSchedule struct {
	Interval Interval `yaml:"interval"`
}

// Interval is a backup cadence. The manifest schema allows Nm, Nh and Nd —
// days exist here (restore-drill: 7d) and time.ParseDuration does not speak
// them, which is why this is not Timeouts' parser.
type Interval time.Duration

// UnmarshalYAML parses an interval per the schema pattern ^[0-9]+(m|h|d)$.
func (i *Interval) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	if len(raw) < 2 {
		return fmt.Errorf("%q is not an interval (want e.g. \"1h\", \"24h\", \"7d\")", raw)
	}
	unit := time.Duration(0)
	switch raw[len(raw)-1] {
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return fmt.Errorf("%q is not an interval (want e.g. \"1h\", \"24h\", \"7d\")", raw)
	}
	n, err := strconv.Atoi(raw[:len(raw)-1])
	if err != nil || n <= 0 {
		return fmt.Errorf("%q is not an interval (want a positive count then m, h or d)", raw)
	}
	*i = Interval(time.Duration(n) * unit)
	return nil
}

// Duration returns the interval as a time.Duration.
func (i Interval) Duration() time.Duration { return time.Duration(i) }

// Load reads and parses a bundle manifest file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses bundle manifest bytes.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse bundle manifest: %w", err)
	}
	if m.Bundle == "" {
		return nil, fmt.Errorf("bundle manifest has no `bundle:` version")
	}
	if len(m.Limits.Timeouts) == 0 {
		return nil, fmt.Errorf("bundle manifest %s has no limits.timeouts: every wait needs a deadline from the bundle", m.Bundle)
	}
	return &m, nil
}
