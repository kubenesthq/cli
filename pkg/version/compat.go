// Compatibility with the control plane's CONTRACT ERA (T7.0), alongside the
// build stamp this package already carries.
//
// THE CLI IS THE ARTIFACT THAT MOVES FIRST. A new CLI reaches a control plane
// that is still the previous one, and the operations it runs — install,
// upgrade, backup — need behaviours that control plane may not implement. The
// control plane declares what it implements as a monotonic integer
// (CONTROL_PLANE_CONTRACT, served by GET /api/v1/version), so the CLI records
// the era its operations need and refuses anything below it, naming the
// control-plane upgrade as the fix.
//
// A FLOOR IS RECORDED AGAINST A BEHAVIOUR, NEVER AGAINST A BEAD
// (kn-recorded-floors-assert-open-beads-ale2). That rule comes from the
// control plane's own contract module and applies here for the same reason:
// a bead can carry several defects, so a floor keyed by bead silently promotes
// "one of its defects is fixed" into "the bead is fixed". Each Floor below
// names an era behaviour, says which era implements it, and carries the bead
// that produced it only as provenance.
//
// FLOORS HERE MIRROR, THEY DO NOT DECIDE. The behaviour identifiers are the
// control plane's (app/core/contract.py, ERA_BEHAVIOURS) and this file must
// not invent a behaviour the control plane does not declare, because the
// comparison would then be against a claim nobody can check from the running
// control plane. What this file adds is the CLI's side of the question: which
// of those behaviours the commands in pkg/cmd need.
package version

import (
	"fmt"
	"sort"
)

// Behaviour identifiers the CLI depends on. They are the control plane's own
// ids, not paraphrases: a reader can evaluate them against a running control
// plane's documented eras.
const (
	// BehaviourVersionEndpointAuthenticated is era 1's "GET /api/v1/version
	// answers a contract era and a build stamp to an authenticated reader".
	BehaviourVersionEndpointAuthenticated = "version-endpoint-authenticated"

	// BehaviourArgocdRegistrationNeverUnrestricted is era 2's "a cluster is
	// never registered with ArgoCD unrestricted".
	BehaviourArgocdRegistrationNeverUnrestricted = "argocd-registration-never-unrestricted"
)

// Floor is "this CLI requires control plane contract >= Era", recorded against
// a behaviour.
//
// Provenance names where the requirement came from, and is deliberately NOT a
// lookup key — see the package comment.
type Floor struct {
	Behaviour  string
	Era        int
	Provenance string
	Note       string
}

// Floors are what the CLI's control-plane-facing commands need.
//
// Both are ALREADY IMPLEMENTED by every control plane this CLI can meet, and
// that is the point: the floor is not a wish list, it is the minimum below
// which the CLI knows its own operations are wrong. The value of recording it
// is the future — the 1.3 CLI records era 4 or 5 here, and a 1.2 control plane
// is then refused by name rather than by an unexplained 404 three steps in.
var Floors = []Floor{
	{
		Behaviour:  BehaviourArgocdRegistrationNeverUnrestricted,
		Era:        2,
		Provenance: "kn-k9ko",
		Note: "The install and upgrade stages register a cluster and then expect it to " +
			"become manageable. Below era 2 the control plane registers it unrestricted, " +
			"the narrowed bearer is refused on the first type ArgoCD's cache reaches and " +
			"NOTHING syncs — so the cluster the CLI just installed silently does not work. " +
			"Command-level fixes cannot compensate for that: the CLI does not choose how " +
			"the control plane registers it.",
	},
	{
		Behaviour:  BehaviourVersionEndpointAuthenticated,
		Era:        1,
		Provenance: "kn-control-plane-version-identity-frev",
		Note: "The check itself. A control plane that REPORTS an era below 1 is one " +
			"whose answer cannot be trusted at all — the counter begins at 1 — and the " +
			"CLI refuses rather than proceeding on a number it knows is malformed. A " +
			"control plane that answers 404 is a different case and is NOT refused: see " +
			"RequireControlPlane.",
	},
}

// RequiredEra is the highest era any floor asks for, and the floor that asked
// for it, so a refusal can name the specific requirement rather than a number.
func RequiredEra() (Floor, bool) {
	highest := Floor{}
	found := false
	for _, floor := range Floors {
		if !found || floor.Era > highest.Era {
			highest, found = floor, true
		}
	}
	return highest, found
}

// Report is what a control plane answered about itself.
//
// Absent IS A THIRD STATE, and it is the reason this type exists rather than a
// bare int. A control plane older than the counter answers 404: that is "cannot
// tell", and the CLI must not turn it into "does not have the behaviour" — the
// population it would condemn includes builds that already have the fix the
// era was minted for.
type Report struct {
	// Absent is true when the control plane does not serve the version route.
	Absent bool
	// Era is the contract era it reported. Meaningless when Absent.
	Era int
	// Build is its build stamp, or "" when the image was not stamped.
	Build string
}

// Refusal is the error RequireControlPlane returns.
//
// It carries the pieces so a caller can act on the numbers rather than on the
// text, and its message follows the CLI's existing fail-closed style
// (pkg/upgrade/gates.go): what was observed, why it matters, and the fix.
type Refusal struct {
	Required Floor
	Reported int
	Build    string
}

func (r *Refusal) Error() string {
	build := r.Build
	if build == "" {
		build = "not stamped"
	}
	return fmt.Sprintf(
		"the control plane is at contract era %d (build %s), but this CLI needs era %d for %s.\n"+
			"      Why: %s\n"+
			"      Fix: upgrade the control plane — `kubenest platform upgrade --control-plane --to <bundle>` — then run this command again. A CLI that carried on would fail later, on a route the older control plane does not serve, with nothing naming the cause",
		r.Reported, build, r.Required.Era, r.Required.Behaviour, r.Required.Note)
}

// RequireControlPlane turns a control plane's answer into a refusal, or nil.
//
// THE ABSENT CASE COMES FIRST AND IS NEVER A REFUSAL. A 404 at
// /api/v1/version is not a determination — see Report — and a caller that
// treated it as one would refuse a command it has no evidence to refuse.
func RequireControlPlane(report Report) error {
	if report.Absent {
		return nil
	}
	required, ok := RequiredEra()
	if !ok || report.Era >= required.Era {
		return nil
	}
	return &Refusal{Required: required, Reported: report.Era, Build: report.Build}
}

// ErrNoFloors reports a Floors list with nothing in it, which would make the
// check vacuous.
func ErrNoFloors() error {
	if len(Floors) == 0 {
		return fmt.Errorf("pkg/version records no floor, so the control-plane check cannot refuse anything")
	}
	return nil
}

// BehavioursNeedingEra returns the behaviours whose floor is at or above era,
// sorted, for a caller reporting what a control plane is missing.
func BehavioursNeedingEra(era int) []string {
	var out []string
	for _, floor := range Floors {
		if floor.Era >= era {
			out = append(out, floor.Behaviour)
		}
	}
	sort.Strings(out)
	return out
}
