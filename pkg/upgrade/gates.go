package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"kubenest.io/cli/pkg/deprecation"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
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
func checkWindow(s *Session) GateResult {
	if s.Window == nil {
		return GateResult{
			Gate: GateWindow, Passed: true,
			Detail: "no maintenance window is configured for this cluster, so any time is inside it",
		}
	}
	now := s.now()
	if s.Window.Contains(now) {
		return GateResult{Gate: GateWindow, Passed: true, Detail: "inside " + s.Window.String()}
	}
	detail := fmt.Sprintf("now (%s) is outside %s", now.In(s.Window.Location).Format("Mon 15:04 MST"), s.Window)
	fix := "wait for the window, or change it with `kubenest cluster set-window`"
	if next, ok := s.Window.NextOpen(now); ok {
		fix = fmt.Sprintf("the window next opens %s; wait for it, or change it with `kubenest cluster set-window`",
			next.Format("Mon 2 Jan 15:04 MST"))
	}
	return GateResult{Gate: GateWindow, Passed: false, Detail: detail, Fix: fix}
}

// checkBundlePath refuses a transition the target bundle does not offer for
// this cluster's shape. Untested transitions are not offered.
func checkBundlePath(from, to *manifest.Manifest, profiles []string, haTier string) GateResult {
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
	return GateResult{
		Gate: GateBundlePath, Passed: true,
		Detail: fmt.Sprintf("%s → %s is offered for the %s tier and this cluster's profile set", from.Bundle, to.Bundle, haTier),
	}
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

// pdbList is what the disruption gate reads.
type pdbList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Status struct {
			DisruptionsAllowed int32 `json:"disruptionsAllowed"`
			CurrentHealthy     int32 `json:"currentHealthy"`
			DesiredHealthy     int32 `json:"desiredHealthy"`
			ExpectedPods       int32 `json:"expectedPods"`
		} `json:"status"`
	} `json:"items"`
}

// checkDisruptionBudgets asks whether a drain would FINISH, which is
// answerable in advance — not whether a PDB is reasonable, which is not.
//
// A budget that permits zero disruption stalls the upgrade forever, holding
// the cluster mid-transition, which is a strictly worse place than either
// finishing or not starting. The check is deliberately narrow: a PDB
// currently allowing no disruptions AND with no slack (every expected pod is
// needed) can never let a pod move.
func checkDisruptionBudgets(ctx context.Context, r k3s.Runner) GateResult {
	out, err := k3s.Kubectl(ctx, r, "get poddisruptionbudgets -A -o json")
	if err != nil {
		return GateResult{Gate: GateDisruption, Passed: false,
			Detail: "could not read pod disruption budgets: " + err.Error(),
			Fix:    "the cluster must be readable before it can be upgraded"}
	}
	var pdbs pdbList
	if err := json.Unmarshal([]byte(out), &pdbs); err != nil {
		return GateResult{Gate: GateDisruption, Passed: false, Detail: "unparsable pod disruption budgets",
			Fix: "the cluster must be readable before it can be upgraded"}
	}

	var blocking []string
	for _, p := range pdbs.Items {
		if p.Status.DisruptionsAllowed > 0 {
			continue
		}
		// Zero allowed right now is normal while something is rescheduling.
		// It is permanent only when the budget requires every pod it has.
		if p.Status.DesiredHealthy >= p.Status.ExpectedPods && p.Status.ExpectedPods > 0 {
			blocking = append(blocking, fmt.Sprintf("%s/%s requires %d of %d pods, so no pod may ever be evicted",
				p.Metadata.Namespace, p.Metadata.Name, p.Status.DesiredHealthy, p.Status.ExpectedPods))
		}
	}
	if len(blocking) > 0 {
		return GateResult{
			Gate: GateDisruption, Passed: false,
			Detail: strings.Join(blocking, "; "),
			Fix:    "relax the budget or add a replica. A drain that can never complete holds the cluster mid-upgrade — the upgrade never force-deletes a pod, because that is an operator's decision and not a tool's",
		}
	}
	return GateResult{Gate: GateDisruption, Passed: true,
		Detail: fmt.Sprintf("%d budget(s), none of which would block a drain indefinitely", len(pdbs.Items))}
}
