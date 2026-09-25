package version

import (
	"errors"
	"strings"
	"testing"
)

// The refusal is the whole point of the check: naming the control-plane
// upgrade is what turns "something 404s later" into an action.
func TestAControlPlaneBelowTheFloorIsRefusedWithTheUpgradeNamed(t *testing.T) {
	required, ok := RequiredEra()
	if !ok {
		t.Fatal("no floor is recorded, so the check refuses nothing")
	}
	if required.Era < 2 {
		t.Fatalf("the highest floor is era %d; the test needs a control plane below it to exist", required.Era)
	}

	err := RequireControlPlane(Report{Era: required.Era - 1, Build: "9e9698d"})
	if err == nil {
		t.Fatalf("a control plane at era %d was accepted against a floor of %d", required.Era-1, required.Era)
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v (%T), want a *Refusal so a caller can act on the numbers", err, err)
	}
	if refusal.Reported != required.Era-1 || refusal.Required.Era != required.Era {
		t.Errorf("refusal reports %+v, want reported %d required %d", refusal, required.Era-1, required.Era)
	}
	message := err.Error()
	if !strings.Contains(message, "platform upgrade --control-plane") {
		t.Errorf("the refusal does not name the control-plane upgrade as the fix:\n%s", message)
	}
	if !strings.Contains(message, required.Behaviour) {
		t.Errorf("the refusal does not name the behaviour it needs:\n%s", message)
	}
	if !strings.Contains(message, "9e9698d") {
		t.Errorf("the refusal does not carry the build it observed:\n%s", message)
	}

	// AT THE FLOOR IS FINE, and so is above it. A floor is a minimum, not an
	// equality: the CLI is not asking the control plane to be any exact build.
	if err := RequireControlPlane(Report{Era: required.Era, Build: "c121ed8"}); err != nil {
		t.Errorf("a control plane at exactly the floor was refused: %v", err)
	}
	if err := RequireControlPlane(Report{Era: required.Era + 7}); err != nil {
		t.Errorf("a newer control plane was refused: %v", err)
	}
}

// A 404 is "cannot tell". Reading it as "does not have the behaviour" would
// refuse every control plane built before the counter — a population that
// includes builds that already implement what the era was minted for.
func TestAnOlderControlPlaneAnswering404IsNotReportedAsHavingTheBehaviour(t *testing.T) {
	if err := RequireControlPlane(Report{Absent: true}); err != nil {
		t.Fatalf("a control plane that does not serve the version route was refused: %v", err)
	}

	// The absent branch must win over the era comparison, not fall through it:
	// an absent report carries era 0, which is below every floor.
	required, _ := RequiredEra()
	absent := Report{Era: 0}
	if absent.Era >= required.Era {
		t.Fatalf("the test does not exercise the case it claims: era 0 is at the floor %d", required.Era)
	}
	if err := RequireControlPlane(Report{Absent: true, Era: absent.Era}); err != nil {
		t.Fatalf("an absent answer with era 0 was refused: %v", err)
	}

	// And it must not be reported as a capability the control plane has: the
	// only honest statement about a 404 is that nothing was determined.
	if got := BehavioursNeedingEra(1); len(got) == 0 {
		t.Error("no behaviour is recorded at era 1 or above, so the floor list is empty")
	}
}

func TestEveryFloorNamesABehaviourRatherThanABead(t *testing.T) {
	if err := ErrNoFloors(); err != nil {
		t.Fatal(err)
	}
	for _, floor := range Floors {
		if strings.HasPrefix(floor.Behaviour, "kn-") {
			t.Errorf("floor %+v names a bead id as its behaviour; a bead is provenance, not a requirement", floor)
		}
		if floor.Era < 1 {
			t.Errorf("floor %+v records era %d; the control plane's counter begins at 1, so a floor below it is unexpressible", floor, floor.Era)
		}
		if floor.Provenance == "" {
			t.Errorf("floor %+v records no provenance", floor)
		}
	}
}
