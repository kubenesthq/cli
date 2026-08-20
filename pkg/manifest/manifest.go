// Package manifest reads the bundle manifest — the versioned record of what a
// platform release contains (see docs.kubenest.io/platform/bundle).
//
// This package deliberately parses only what the CLI consumes today:
// limits.timeouts, the deadlines for every convergence check. Limits are part
// of the bundle, not constants in the code: a missing timeout key is an
// error here, never a hardcoded default. The shipped manifest carries the
// defaults; the code carries none.
//
// The manifest schema itself is owned by kubenest-contracts (kn-boj); the
// field names here follow bundle.mdx and must track that schema.
package manifest

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is the subset of the bundle manifest the CLI reads.
type Manifest struct {
	// Bundle is the platform version this manifest pins, e.g. "1.0".
	Bundle string `yaml:"bundle"`
	Limits Limits `yaml:"limits"`
}

type Limits struct {
	Timeouts Timeouts `yaml:"timeouts"`
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
