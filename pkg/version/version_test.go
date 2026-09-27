package version

import (
	"strings"
	"testing"
)

// The refusal a command that needs a live control plane gives when the control
// plane is too old, and the two cases that are NOT refusals.
//
// WHY THIS IS ITS OWN TEST. The refusal is what turns "an unexplained 404 three
// steps into an install" into a sentence naming the fix, and the false-negative
// direction matters more than the false-positive one: a control plane that
// answers 404 cannot tell a CLI anything, so refusing it would condemn builds
// that already carry the behaviour the floor was minted for. Both directions
// are asserted here because a test that only checked the refusal would pass for
// an implementation that refused everything.
func TestControlPlaneVersionRefusalNamesTheUpgrade(t *testing.T) {
	required, ok := RequiredEra()
	if !ok {
		t.Fatal("this CLI records no floor at all, so the check is vacuous")
	}

	err := RequireControlPlane(Report{Era: required.Era - 1, Build: "deadbee"})
	if err == nil {
		t.Fatalf("a control plane one era below the floor (%d) was accepted", required.Era)
	}
	refusal, ok := err.(*Refusal)
	if !ok {
		t.Fatalf("the refusal is %T, so a caller cannot act on the numbers: %v", err, err)
	}
	if refusal.Required.Era != required.Era || refusal.Reported != required.Era-1 {
		t.Fatalf("the refusal carries the wrong numbers: required %d, reported %d", refusal.Required.Era, refusal.Reported)
	}

	// The message must name the fix. A refusal an operator cannot act on is a
	// dead end, and the fix here is an upgrade of the control plane.
	message := err.Error()
	for _, want := range []string{"kubenest platform upgrade", "--control-plane", "deadbee"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal does not name %q:\n%s", want, message)
		}
	}
	if !strings.Contains(message, required.Behaviour) {
		t.Fatalf("the refusal does not name the behaviour the floor was recorded against (%q):\n%s", required.Behaviour, message)
	}
	if !strings.Contains(message, "era") {
		t.Fatalf("the refusal does not say that a CONTRACT ERA is what is too low:\n%s", message)
	}

	// At or above the floor: nothing to refuse.
	if err := RequireControlPlane(Report{Era: required.Era}); err != nil {
		t.Fatalf("a control plane at the floor was refused: %v", err)
	}
	if err := RequireControlPlane(Report{Era: required.Era + 1}); err != nil {
		t.Fatalf("a newer control plane was refused: %v", err)
	}

	// AND THE CASE RECOVERY DEPENDS ON: a control plane that does not serve the
	// version route at all is "cannot tell", never a refusal. `kubenest
	// recovery-kit check`, recovery and resuming an operation must work against
	// a control plane whose version endpoint cannot be read — on the day
	// someone needs them, the control plane is the thing that is gone.
	if err := RequireControlPlane(Report{Absent: true}); err != nil {
		t.Fatalf("a control plane that does not serve /api/v1/version was refused, which would make recovery impossible against exactly the control plane it is for: %v", err)
	}
}

// The floors themselves have to be worth checking: an empty list would make
// every refusal above vacuous, and a floor minted against a bead rather than a
// behaviour cannot be evaluated against a running control plane.
func TestTheFloorsAreRecordedAgainstBehaviours(t *testing.T) {
	if err := ErrNoFloors(); err != nil {
		t.Fatal(err)
	}
	for _, floor := range Floors {
		if floor.Behaviour == "" {
			t.Fatalf("a floor names no behaviour, so nobody can check it against a control plane: %+v", floor)
		}
		if floor.Era < 1 {
			t.Fatalf("floor %s claims era %d: the counter begins at 1, so a floor below it is malformed rather than old", floor.Behaviour, floor.Era)
		}
		if !strings.Contains(floor.Note, " ") {
			t.Fatalf("floor %s has no note explaining why it matters, so an operator reading a refusal cannot judge it", floor.Behaviour)
		}
	}
	if required, ok := RequiredEra(); !ok || required.Behaviour == "" {
		t.Fatal("RequiredEra returned no behaviour, so a refusal could not name one")
	}
	if names := BehavioursNeedingEra(1); len(names) == 0 {
		t.Fatal("no behaviour is recorded at or above era 1, which is what a refusal would name")
	}
}
