// The compatibility window between bundles and the control plane (decision L).
//
// THE WINDOW IS EXPRESSED IN BUNDLE VERSIONS, AND IT IS DECLARED, NOT DERIVED.
// The control plane accepts agents from the current bundle and the one before
// it, and the bundle pins the agent (core.kubenest-agent). The control plane has
// no bundle-era identity of its own: a build stamp says which commit is running
// and does NOT say which window that build implements, and two build stamps
// cannot be ordered without the repository (kn-opj8). So "which window do I
// implement" travels as a release artifact — the `compatibility` block this file
// parses — deliberately separate from any build identity, because coupling them
// would tie control-plane releases to bundle releases, which decision L
// explicitly declined.
//
// TWO HALVES, AND THEY ARE NOT SYMMETRIC. One is the range of control-plane
// contract ERAS that support a bundle (the eras are the control plane's own
// monotonic counter, served by GET /api/v1/version, so they are orderable
// integers). The other is, for each era, the bundle versions whose agents that
// control plane accepts. The first half is read by the CLI's control-plane
// checks and by the backend; the second is what a hub needs to refuse an
// out-of-window agent at the door instead of failing in some subtler way later.
//
// SILENCE IS NOT A REFUSAL, and that rule is the same one the backend's reader
// carries: every manifest released before this block existed declares no window,
// and a reader that refused on an absent block would refuse the released
// catalog on a question it was never asked.
package manifest

import (
	"fmt"
	"sort"
	"strings"
)

// Compatibility is the window a bundle declares.
type Compatibility struct {
	// ControlPlane is the range of control-plane contract eras that support
	// this bundle. A zero value means the bundle states none.
	ControlPlane ControlPlaneWindow `yaml:"control-plane"`
	// Agents maps a control-plane contract era (as a decimal string, because
	// YAML keys are strings and the era is an integer) to the bundle versions
	// whose agents that control plane accepts.
	Agents map[string]AgentWindow `yaml:"agents"`
}

// ControlPlaneWindow is the range of control-plane contract eras a bundle works
// with. Both bounds are ERAS, never versions and never build stamps.
type ControlPlaneWindow struct {
	MinContract int `yaml:"min-contract"`
	// MaxContract is nil when the bundle declares no upper bound. An absent
	// maximum is "no bound", never zero — a zero would read as a window no
	// control plane can be inside.
	MaxContract *int `yaml:"max-contract"`
}

// AgentWindow is the set of bundle versions one control-plane era accepts.
type AgentWindow struct {
	Accepts []string `yaml:"accepts"`
}

// Declared reports whether the bundle states a control-plane window at all.
func (c Compatibility) Declared() bool {
	return c.ControlPlane.Declared() || len(c.Agents) > 0
}

// Declared reports whether the window carries a floor. A manifest with a
// `compatibility:` block but no `control-plane:` half states no control-plane
// window, which is not the same as one that refuses every control plane.
func (w ControlPlaneWindow) Declared() bool { return w.MinContract > 0 }

// Supports reports whether a control plane at this contract era is inside the
// declared range.
func (w ControlPlaneWindow) Supports(era int) bool {
	if !w.Declared() || era < w.MinContract {
		return false
	}
	return w.MaxContract == nil || era <= *w.MaxContract
}

// Accepts answers two questions at once, because they are different answers:
// whether the era's row exists at all, and whether the bundle is in it. An era
// with no row is UNDECLARED — the catalog does not say — and a caller must not
// read that as a refusal.
func (c Compatibility) Accepts(era int, agentBundle string) (accepted bool, declared bool) {
	window, ok := c.Agents[fmt.Sprintf("%d", era)]
	if !ok {
		return false, false
	}
	for _, version := range window.Accepts {
		if version == agentBundle {
			return true, true
		}
	}
	return false, true
}

// CheckAgent refuses an agent whose bundle the control plane's era does not
// accept, and names the bundle to step to. nil means accepted, OR undeclared —
// silence is not a refusal (see the package comment).
//
// The refusal is the shape the CLI's other gates use: what was observed, why it
// matters, and the fix. It is deliberately also a typed error, so a caller can
// act on the numbers rather than on the text.
func (c Compatibility) CheckAgent(era int, agentBundle string) error {
	accepted, declared := c.Accepts(era, agentBundle)
	if !declared || accepted {
		return nil
	}
	window := c.Agents[fmt.Sprintf("%d", era)]
	return &AgentWindowRefusal{
		Era:       era,
		Agent:     agentBundle,
		Accepts:   append([]string(nil), window.Accepts...),
		UpgradeTo: NextAcceptedBundle(window.Accepts, agentBundle),
	}
}

// AgentWindowRefusal is an agent outside the declared window.
type AgentWindowRefusal struct {
	// Era is the control plane's contract era.
	Era int
	// Agent is the bundle version the agent is running.
	Agent string
	// Accepts is the whole window, so the message can show it rather than
	// assert it.
	Accepts []string
	// UpgradeTo is the bundle to step to, or empty when no accepted bundle is
	// newer than the agent's: that case is a control plane older than the
	// cluster, and the fix is on the control-plane side.
	UpgradeTo string
}

func (r *AgentWindowRefusal) Error() string {
	window := "control plane era " + fmt.Sprintf("%d", r.Era) + " accepts bundle " + humanList(r.Accepts)
	if r.UpgradeTo != "" {
		return fmt.Sprintf(
			"this agent runs bundle %s, and the %s: it is outside the compatibility window, so the control plane "+
				"would work with it only by accident. Fix: upgrade the cluster to bundle %s — `kubenest platform "+
				"upgrade --to %s`. Two bundles are supported at a time, so a cluster that steps through stays in window",
			r.Agent, window, r.UpgradeTo, r.UpgradeTo)
	}
	return fmt.Sprintf(
		"this agent runs bundle %s, and the %s, so the cluster is NEWER than the control plane that has to serve it. "+
			"Fix: upgrade the control plane — this build does not accept an agent from a bundle the control plane "+
			"does not declare, and upgrading the agent further moves it further out of window",
		r.Agent, window)
}

// NextAcceptedBundle is the oldest accepted bundle NEWER than the agent's — the
// step to take, not the newest reachable: bundles are stepped through one at a
// time (decision K), so naming the jump would hand back a hop the path gate
// refuses. Empty when the agent is already at or beyond everything accepted,
// which is the control-plane-older-than-the-cluster case.
func NextAcceptedBundle(accepts []string, agentBundle string) string {
	ordered := append([]string(nil), accepts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		cmp, err := CompareBundleVersions(ordered[i], ordered[j])
		return err == nil && cmp < 0
	})
	for _, version := range ordered {
		if cmp, err := CompareBundleVersions(agentBundle, version); err == nil && cmp < 0 {
			return version
		}
	}
	return ""
}

// CompareBundleVersions orders two bundle versions: -1, 0 or 1.
//
// It is NOT CompareVersions: a bundle version has two parts ("1.10" is newer
// than "1.9", which a string comparison gets backwards) and a component pin has
// three, so the two comparators answer different questions.
func CompareBundleVersions(a, b string) (int, error) {
	pa, err := parseBundleVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseBundleVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, nil
		case pa[i] > pb[i]:
			return 1, nil
		}
	}
	return 0, nil
}

func parseBundleVersion(version string) ([2]int, error) {
	var out [2]int
	fields := strings.Split(strings.TrimSpace(version), ".")
	if len(fields) != 2 {
		return out, fmt.Errorf("%q is not a bundle version (want major.minor)", version)
	}
	for i, f := range fields {
		if _, err := fmt.Sscanf(f, "%d", &out[i]); err != nil {
			return out, fmt.Errorf("%q is not a bundle version (want major.minor)", version)
		}
	}
	return out, nil
}

// humanList renders a window as prose. Empty is "no bundle", which the caller
// must have ruled out before asking.
func humanList(values []string) string {
	switch len(values) {
	case 0:
		return "no bundle"
	case 1:
		return values[0]
	default:
		return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
	}
}
