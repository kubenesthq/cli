package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/deprecation"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/window"
)

// The pre-flight gates. Every one runs before anything is touched, and any
// failure stops the upgrade with nothing changed.
//
// These are not ceremony. Each exists because skipping it produces a
// specific, known bad outcome, and each names that outcome in its refusal —
// an operator who is told "gate failed" learns nothing, while one who is told
// "a PDB permits zero disruption, so the drain would never finish" can act.
//
// Every gate runs even after one has failed, for the same reason preflight
// does at install: an operator who fixes one condition, re-runs and hits the
// next has been failed by the tool, not by their cluster.

// Gate names, as upgrades.mdx's table names them.
const (
	GateDeprecatedAPIs = "Deprecated API scan"
	GateRestoreDrill   = "Restore drill"
	GateNodeReadiness  = "Node readiness"
	GateDiskHeadroom   = "Disk headroom"
	GateDisruption     = "Pod disruption budgets"
	GateWindow         = "Maintenance window"
	GateBundlePath     = "Bundle path"
)

// GateResult is one gate's verdict.
type GateResult struct {
	Gate   string
	Passed bool
	// Detail is what was observed.
	Detail string
	// Fix is what to do about it. A failed gate without one has told the
	// operator they have a problem and nothing more.
	Fix string
}

func (g GateResult) String() string {
	s := g.Gate + ": " + g.Detail
	if !g.Passed && g.Fix != "" {
		s += "\n      fix: " + g.Fix
	}
	return s
}

// GateReport is every gate that ran.
type GateReport struct {
	Results []GateResult
	// Deprecations is the scan's full report, kept so warnings can be
	// printed even when the gate passes.
	Deprecations deprecation.Report
}

func (r *GateReport) add(g GateResult) { r.Results = append(r.Results, g) }

// Failures returns the gates that refused the upgrade.
func (r GateReport) Failures() []GateResult {
	var out []GateResult
	for _, g := range r.Results {
		if !g.Passed {
			out = append(out, g)
		}
	}
	return out
}

// Err is the aggregate refusal: every failing gate, so one fix-and-re-run
// clears everything visible.
func (r GateReport) Err() error {
	failures := r.Failures()
	if len(failures) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the upgrade was refused (%d of %d gates failed). Nothing has been changed:\n", len(failures), len(r.Results))
	for _, g := range failures {
		fmt.Fprintf(&b, "  [fail] %s\n", g)
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

// DrillStatus is the restore-drill evidence, in the shape kn-f9lm publishes
// (contracts v1.21.0 restore_drill_result.json).
type DrillStatus struct {
	// Status is never_run, passed or failed.
	Status string `json:"status"`
	// CompletedAt is when the drill finished. Absent only for never_run.
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// Backup is the exact Velero backup the drill restored.
	Backup string `json:"backup,omitempty"`
	// Failure carries the reason when the drill failed.
	Failure *struct {
		Stage      string `json:"stage"`
		ReasonCode string `json:"reason_code"`
		Detail     string `json:"detail"`
	} `json:"failure,omitempty"`
}

// DrillSource reports the cluster's last verified restore drill.
//
// An interface because the drill is kn-f9lm's to produce and this is only its
// consumer: what matters here is the POLICY, which is the same whichever way
// the evidence is read. A source that returns an error fails the gate — no
// evidence is refused, never passed.
type DrillSource interface {
	LastRestoreDrill(ctx context.Context) (DrillStatus, error)
}

// checkDrill is the gate: only a FRESH PASS permits an upgrade.
//
// Rollback partly depends on restore, so an untested restore is not a
// rollback plan. The three refusals are never_run, failed, and a pass that is
// older than the manifest's health.backup.max-restore-drill-age — a drill
// from March that passed is not evidence about a cluster in August. The
// threshold comes from the bundle so the gate that refuses and the fleet
// alert that fires read the same number and cannot desync.
func checkDrill(drill DrillStatus, err error, maxAge time.Duration, now time.Time) GateResult {
	const gate = GateRestoreDrill
	if err != nil {
		return GateResult{
			Gate: gate, Passed: false,
			Detail: "the last restore drill could not be read: " + err.Error(),
			Fix:    "this gate fails closed — a gate that cannot see is not a gate that saw nothing. Fix the reporting path, or run a drill with `kubenest backup drill` and try again",
		}
	}
	switch drill.Status {
	case "passed":
		if drill.CompletedAt == nil {
			return GateResult{
				Gate: gate, Passed: false,
				Detail: "the last restore drill reports passed but carries no completion time, so its age cannot be judged",
				Fix:    "run a drill with `kubenest backup drill`",
			}
		}
		age := now.Sub(*drill.CompletedAt)
		if age > maxAge {
			return GateResult{
				Gate: gate, Passed: false,
				Detail: fmt.Sprintf("the last restore drill passed %s ago (%s), older than the %s this bundle allows",
					age.Round(time.Hour), drill.CompletedAt.Format(time.RFC3339), maxAge),
				Fix: "run a fresh drill with `kubenest backup drill` — a drill that passed months ago is not evidence about this cluster today",
			}
		}
		return GateResult{
			Gate: gate, Passed: true,
			Detail: fmt.Sprintf("restored %s successfully %s ago", drill.Backup, age.Round(time.Minute)),
		}
	case "failed":
		detail := "the last restore drill FAILED"
		if drill.Failure != nil {
			detail += fmt.Sprintf(" at %s (%s): %s", drill.Failure.Stage, drill.Failure.ReasonCode, drill.Failure.Detail)
		}
		return GateResult{
			Gate: gate, Passed: false, Detail: detail,
			Fix: "fix the restore path before upgrading. This gate failing is important information on its own: it means the state you would need to return to cannot be returned to",
		}
	case "never_run", "":
		return GateResult{
			Gate: gate, Passed: false,
			Detail: "no restore drill has ever completed on this cluster",
			Fix:    "run one with `kubenest backup drill`. An untested restore is not a rollback plan, and rollback is what makes an upgrade safe to attempt",
		}
	default:
		return GateResult{
			Gate: gate, Passed: false,
			Detail: fmt.Sprintf("the last restore drill reports an unrecognised status %q", drill.Status),
			Fix:    "this build understands never_run, passed and failed; an unknown status is refused rather than guessed",
		}
	}
}

// checkWindow refuses to START outside the cluster's maintenance window.
//
// Only the START is gated. When the window closes mid-upgrade, no new stage
// starts but the stage in progress finishes — abandoning a half-completed
// stage to respect a clock leaves the cluster worse than the overrun does.
//
// A MISSING WINDOW IS A REFUSAL, NOT A PASS. This gate used to return
// Passed: true for a nil window with the detail "no maintenance window is
// configured for this cluster, so any time is inside it" (kn-nqj), and that
// made one of seven documented gates approve every cluster, every time, while
// the operator who had set a window believed upgrades and reboots were
// confined to it. A window that cannot be READ is refused for the same reason:
// an unread gate is not a passed gate.
func checkWindow(s *Session) GateResult {
	if s.Opts.BypassWindow {
		return GateResult{Gate: GateWindow, Passed: true,
			Detail: "--now: the maintenance window is bypassed for this run; every other gate still runs"}
	}
	if s.WindowErr != nil {
		return GateResult{Gate: GateWindow, Passed: false,
			Detail: "the cluster's maintenance window could not be read, so whether now is inside it is unknown: " + s.WindowErr.Error(),
			Fix:    "an unread window is not a passed check; " + window.NoWindowFix}
	}
	if s.Window == nil {
		return GateResult{Gate: GateWindow, Passed: false,
			Detail: window.NoWindow,
			Fix:    window.NoWindowFix}
	}
	now := s.now()
	if err := s.Window.Outside(now); err != nil {
		return GateResult{Gate: GateWindow, Passed: false, Detail: err.Error(), Fix: window.OutsideFix}
	}
	return GateResult{Gate: GateWindow, Passed: true, Detail: "inside " + s.Window.String()}
}

// checkBundlePath refuses a transition the target bundle does not offer for
// this cluster's shape. Untested transitions are not offered.
//
// FIVE QUESTIONS, IN THIS ORDER, and the order is the message: is the target
// AHEAD of the running bundle at all (Kubernetes does not downgrade), is it a
// different bundle, does this cluster's shape still fit the target, does the
// catalog DECLARE the transition (plan 7.10) — and only then, is the target
// within one bundle of the running one in the catalog's published sequence
// (decision K, kn-mtpf). An operator who is told "you skipped a bundle" when the
// declared edges say nothing learns the wrong thing to go and fix.
//
// `sequence` is the catalog's published order. The gate does not compare version
// strings to measure distance: "1.10" sorts before "1.9" as text, and a
// security-only release must not consume a hop.
func checkBundlePath(from, to *manifest.Manifest, profiles []string, haTier string, sequence []bundles.Release) GateResult {
	// A BACKWARD transition is refused here, before anything is touched,
	// rather than discovered at the point of no return. Kubernetes does not
	// downgrade and neither does k3s: a bundle whose Kubernetes pin is older
	// than the running one cannot be reached by upgrading, only by restoring
	// a snapshot taken before the move. Without this gate the sequence
	// happily reaches the kubernetes stage and asks
	// system-upgrade-controller to do something it cannot do — observed on a
	// real cluster.
	if older, err := movesBackwards(from, to); err == nil && older {
		return GateResult{
			Gate: GateBundlePath, Passed: false,
			Detail: fmt.Sprintf("bundle %s pins Kubernetes %s, older than the %s this cluster runs",
				to.Bundle, to.Core["k3s"], from.Core["k3s"]),
			Fix: "Kubernetes does not support downgrading, so an older bundle cannot be reached by upgrading. To go back to a previous bundle, restore the datastore snapshot taken before the upgrade — `kubenest platform rollback`",
		}
	}
	if from.Bundle == to.Bundle {
		return GateResult{
			Gate: GateBundlePath, Passed: false,
			Detail: fmt.Sprintf("this cluster is already on bundle %s", to.Bundle),
			Fix:    "there is nothing to upgrade to; check `kubenest platform diff` for what a newer bundle would change",
		}
	}
	if err := to.OffersTier(haTier); err != nil {
		return GateResult{
			Gate: GateBundlePath, Passed: false,
			Detail: err.Error(),
			Fix:    "this cluster's HA tier is permanent, so a bundle that does not offer it cannot be a target",
		}
	}
	for _, p := range profiles {
		if _, err := to.Profiles.Get(p); err != nil {
			return GateResult{
				Gate: GateBundlePath, Passed: false,
				Detail: fmt.Sprintf("bundle %s does not offer profile %q, which this cluster has installed", to.Bundle, p),
				Fix:    "a profile set does not change during an upgrade, so a bundle that drops one cannot be a target for this cluster",
			}
		}
	}
	if refusal, ok := checkDeclaredUpgradeEdge(from, to); !ok {
		return refusal
	}
	if refusal, ok := checkOneBundleAhead(from, to, sequence); !ok {
		return refusal
	}
	return GateResult{
		Gate: GateBundlePath, Passed: true,
		Detail: fmt.Sprintf("%s → %s is offered for the %s tier and this cluster's profile set", from.Bundle, to.Bundle, haTier),
	}
}

// checkDeclaredUpgradeEdge refuses a transition the target's manifest does not
// declare (plan 7.10: "the catalog declares its upgrade edges ... undeclared
// transitions are refused").
//
// A TARGET THAT DECLARES NOTHING HAS NOT BEEN ASKED. 0.9, 1.0 and 1.1 are
// released documents with no `upgrade-from`, and refusing a transition into them
// because of that silence would refuse the upgrade the product demonstrates
// (1.0 -> 1.1, the one the compatibility tests run). What the field is FOR is the
// release that declares its sources and thereby says which ones it does NOT
// support: 1.2 declares 1.0 and 1.1, so a cluster on 0.9 is refused here — with
// the step to take, since "undeclared" is only actionable when the fix names a
// bundle.
func checkDeclaredUpgradeEdge(from, to *manifest.Manifest) (GateResult, bool) {
	if len(to.UpgradeFrom) == 0 {
		return GateResult{}, true
	}
	for _, version := range to.UpgradeFrom {
		if version == from.Bundle {
			return GateResult{}, true
		}
	}
	next := oldestNewerVersion(from.Bundle, to.UpgradeFrom)
	fix := "it may be upgraded from " + humanVersions(to.UpgradeFrom)
	if next != "" {
		fix = fmt.Sprintf("upgrade to %s first — `kubenest platform upgrade --to %s` — and then to %s; the declared sources for %s are %s",
			next, next, to.Bundle, to.Bundle, humanVersions(to.UpgradeFrom))
	}
	return GateResult{
		Gate: GateBundlePath, Passed: false,
		Detail: fmt.Sprintf("bundle %s declares no upgrade edge from %s", to.Bundle, from.Bundle),
		Fix:    fix,
	}, false
}

// checkOneBundleAhead refuses a target more than one bundle ahead in the
// catalog's PUBLISHED SEQUENCE, naming the intermediate bundle to step through
// (decision K, kn-mtpf).
//
// TWO BUNDLES ARE SUPPORTED AT A TIME, AND THE REASON IS THE SCAN. The
// deprecation scan runs against the Kubernetes version pinned by the named
// target and only that one, so an API removed at an intermediate bundle and
// irrelevant again later still breaks workloads there, and a hop over it never
// scans for it. Skipping a bundle is skipping the check that makes the upgrade
// safe.
//
// A SECURITY-ONLY RELEASE DOES NOT CONSUME THE HOP. When a bundle exists only
// because a component needed patching, it is skipped when the distance is
// measured — 1.0 -> 1.2 is one hop when 1.1 was security-only — because the
// operator did not choose to skip it, we chose to issue it.
//
// DISTANCE COMES FROM `sequence` AND NEVER FROM COMPARING VERSION STRINGS.
// "1.10" is newer than "1.9" and sorts first as text, so a string comparison
// would refuse a legal hop and permit an illegal one.
//
// AN UNMEASURABLE HOP IS REFUSED, not passed: if a bundle is missing from the
// sequence, this gate cannot say how far apart two bundles are, and "I could not
// tell" is not "they are adjacent". A BACKWARD move is not this check's
// business — the Kubernetes-pin check above owns it, and this gate falls through
// rather than inventing a second opinion on the same question.
func checkOneBundleAhead(from, to *manifest.Manifest, sequence []bundles.Release) (GateResult, bool) {
	index := make(map[string]int, len(sequence))
	for i, release := range sequence {
		index[release.Version] = i
	}
	fromIndex, knownFrom := index[from.Bundle]
	toIndex, knownTo := index[to.Bundle]
	if !knownFrom || !knownTo {
		missing := from.Bundle
		if knownFrom {
			missing = to.Bundle
		}
		return GateResult{
			Gate: GateBundlePath, Passed: false,
			Detail: fmt.Sprintf("bundle %s is not in the bundle catalog this CLI carries, so how far apart %s and %s are cannot be checked",
				missing, from.Bundle, to.Bundle),
			Fix: "upgrade the CLI to a release whose catalog carries both bundles — this gate will not pass a hop it cannot measure, because the check it stands for is the one that makes the upgrade safe",
		}, false
	}
	if toIndex <= fromIndex {
		// Backward or equal. Equal is refused earlier; a backward bundle move is
		// refused earlier too when it moves Kubernetes, and is otherwise the
		// product's existing behaviour rather than this rule's to change.
		return GateResult{}, true
	}
	var skipped []string
	for _, release := range sequence[fromIndex+1 : toIndex] {
		if release.SecurityOnly {
			continue
		}
		skipped = append(skipped, release.Version)
	}
	if len(skipped) == 0 {
		return GateResult{}, true
	}
	next := skipped[0]
	return GateResult{
		Gate: GateBundlePath, Passed: false,
		Detail: fmt.Sprintf("%s → %s skips %s: bundles are stepped through one at a time",
			from.Bundle, to.Bundle, humanVersions(skipped)),
		Fix: fmt.Sprintf("upgrade to %s first — `kubenest platform upgrade --to %s` — and then to %s. The deprecation scan runs against the Kubernetes version pinned by the target and only that one, so a bundle you jump over is never scanned for an API removal it carries",
			next, next, to.Bundle),
	}, false
}

// oldestNewerVersion is the oldest version in `versions` newer than `from`, or ""
// when there is none. It is the STEP to name in a refusal: bundles are stepped
// through one at a time, so naming the newest would hand back a hop the
// adjacency rule refuses.
func oldestNewerVersion(from string, versions []string) string {
	oldest := ""
	for _, version := range versions {
		cmp, err := manifest.CompareBundleVersions(from, version)
		if err != nil || cmp >= 0 {
			continue
		}
		if oldest == "" {
			oldest = version
			continue
		}
		if c, err := manifest.CompareBundleVersions(version, oldest); err == nil && c < 0 {
			oldest = version
		}
	}
	return oldest
}

// humanVersions renders bundle versions as prose.
func humanVersions(versions []string) string {
	switch len(versions) {
	case 0:
		return "none"
	case 1:
		return versions[0]
	default:
		return strings.Join(versions[:len(versions)-1], ", ") + " or " + versions[len(versions)-1]
	}
}

// movesBackwards reports whether the target's Kubernetes pin is older than
// the running one. Versions are compared numerically, not as strings:
// "v1.35.10" is newer than "v1.35.9" and a lexical comparison says otherwise.
func movesBackwards(from, to *manifest.Manifest) (bool, error) {
	current, err := semverParts(from.Core["k3s"])
	if err != nil {
		return false, err
	}
	target, err := semverParts(to.Core["k3s"])
	if err != nil {
		return false, err
	}
	for i := range current {
		if target[i] != current[i] {
			return target[i] < current[i], nil
		}
	}
	return false, nil
}

// semverParts turns "v1.35.7+k3s1" into {1, 35, 7}.
//
// Moved to pkg/manifest, which is where both callers can reach it:
// pkg/component/agent needs the same ordering to refuse rendering a credential
// into a chart too old to carry it. Kept as a wrapper so this package's gates
// read the same as before.
func semverParts(version string) ([3]int, error) {
	return manifest.SemverParts(version)
}

// nodeStatus is what the readiness and disruption gates read.
type nodeStatus struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type               string    `json:"type"`
				Status             string    `json:"status"`
				Reason             string    `json:"reason"`
				Message            string    `json:"message"`
				LastTransitionTime time.Time `json:"lastTransitionTime"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// checkNodesReady requires every node Ready, AND to have been Ready for the
// bundle's node-ready window.
//
// The dwell time is the point: a node flapping in and out of Ready passes a
// single sample and then fails mid-drain, turning one problem into two.
// Upgrading onto an already-degraded cluster is how a routine operation
// becomes an incident.
func checkNodesReady(ctx context.Context, r k3s.Runner, dwell time.Duration, now time.Time) GateResult {
	out, err := k3s.Kubectl(ctx, r, "get nodes -o json")
	if err != nil {
		return GateResult{Gate: GateNodeReadiness, Passed: false,
			Detail: "could not read node status: " + err.Error(),
			Fix:    "the cluster must be reachable before it can be upgraded"}
	}
	var nodes nodeStatus
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return GateResult{Gate: GateNodeReadiness, Passed: false, Detail: "unparsable node status",
			Fix: "the cluster must be readable before it can be upgraded"}
	}
	if len(nodes.Items) == 0 {
		return GateResult{Gate: GateNodeReadiness, Passed: false, Detail: "the cluster reports no nodes",
			Fix: "check the cluster is running"}
	}

	var problems []string
	for _, n := range nodes.Items {
		ready := false
		var since time.Time
		for _, c := range n.Status.Conditions {
			if c.Type != "Ready" {
				continue
			}
			ready = c.Status == "True"
			since = c.LastTransitionTime
			if !ready {
				problems = append(problems, fmt.Sprintf("%s is not Ready (%s: %s)", n.Metadata.Name, c.Reason, c.Message))
			}
		}
		if ready && !since.IsZero() && now.Sub(since) < dwell {
			problems = append(problems, fmt.Sprintf("%s became Ready only %s ago and may be flapping",
				n.Metadata.Name, now.Sub(since).Round(time.Second)))
		}
	}
	if len(problems) > 0 {
		return GateResult{
			Gate: GateNodeReadiness, Passed: false,
			Detail: strings.Join(problems, "; "),
			Fix:    fmt.Sprintf("every node must be Ready and have been for %s before an upgrade starts — upgrading onto a degraded cluster turns one problem into two", dwell),
		}
	}
	return GateResult{Gate: GateNodeReadiness, Passed: true,
		Detail: fmt.Sprintf("%d node(s) Ready, and steady for at least %s", len(nodes.Items), dwell)}
}

// checkDiskHeadroom requires free space on the filesystem holding
// /var/lib/rancher, on EVERY node.
//
// The new images land beside the old ones, so the requirement is free space
// rather than total size. Running out of disk mid-upgrade is a hard failure at
// the worst possible moment — after the point of no return, on a node that is
// already drained.
func checkDiskHeadroom(ctx context.Context, nodes []Node, need manifest.Quantity) GateResult {
	var short []string
	for _, node := range nodes {
		res, err := node.Runner.Run(ctx, "df -B1 -P /var/lib | awk 'NR==2{print $4}'")
		if err != nil || res.ExitCode != 0 {
			short = append(short, fmt.Sprintf("%s: could not measure free space", node.Address))
			continue
		}
		free, convErr := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
		if convErr != nil {
			short = append(short, fmt.Sprintf("%s: could not measure free space", node.Address))
			continue
		}
		if free < need.Bytes() {
			short = append(short, fmt.Sprintf("%s has %s free, needs %s",
				node.Address, manifest.Quantity(free), need))
		}
	}
	if len(short) > 0 {
		return GateResult{
			Gate: GateDiskHeadroom, Passed: false,
			Detail: strings.Join(short, "; "),
			Fix:    "free space on the filesystem holding /var/lib/rancher. The new images land beside the old ones, so this is about free space rather than total size",
		}
	}
	return GateResult{Gate: GateDiskHeadroom, Passed: true,
		Detail: fmt.Sprintf("every node has at least %s free on /var/lib", need)}
}

// checkDisruptionBudgets asks whether a drain would FINISH, which is
// answerable in advance — not whether a PDB is reasonable, which is not.
//
// The question itself lives in pkg/k3s because the node verbs ask it too
// (`kubenest node remove`, PLAN 7.3): there is one answer to "would this drain
// finish", and the upgrade's gate is this package's rendering of it.
func checkDisruptionBudgets(ctx context.Context, r k3s.Runner) GateResult {
	report := k3s.DrainWouldFinish(ctx, r)
	return GateResult{Gate: GateDisruption, Passed: report.Passed, Detail: report.Detail, Fix: report.Fix}
}
