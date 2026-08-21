package install

import (
	"errors"
	"fmt"
)

// ComponentError names WHICH component of a stage failed.
//
// Two of the thirteen stages install more than one component —
// platform-networking places the Gateway API CRDs and Traefik,
// platform-day2 places system-upgrade-controller and kured — and one places a
// component plus the platform's Gateway defaults. A stage-level constant
// therefore cannot answer "which component broke": it reports the stage's
// first component whatever actually failed, so a kured failure arrives in the
// record as system-upgrade-controller.
//
// That is the same defect class as the acceptance check that hunted a
// HelmChart resource name it had guessed (f2143f3): a record that names the
// wrong thing teaches the operator to distrust the record, and the record is
// what every day-2 operation reads. The name has to come from the call that
// failed.
//
// Component is always the EXACT key from the bundle manifest's core section,
// because that is what the contract's component field means and what a
// failure-injection run matches against.
type ComponentError struct {
	// Component is the manifest core key, e.g. "gateway-api", "kured".
	Component string
	Err       error
}

func (e *ComponentError) Error() string {
	return fmt.Sprintf("%s: %v", e.Component, e.Err)
}

func (e *ComponentError) Unwrap() error { return e.Err }

// failing tags an installer's error with the component it was installing.
// Returns nil unchanged, so call sites read as a plain sequence:
//
//	if err := failing("gateway-api", gatewayapi.Install(...)); err != nil {
//	    return err
//	}
func failing(component string, err error) error {
	if err == nil {
		return nil
	}
	// Do not re-wrap: the innermost tag is the one that names the actual
	// component, and a stage that calls another stage's helper must not
	// relabel it.
	if ComponentOf(err) != "" {
		return err
	}
	return &ComponentError{Component: component, Err: err}
}

// ComponentOf returns the component an error was tagged with, or "" if it
// carries none. The engine falls back to the stage's declared component when
// this is empty, so an untagged failure still names something true.
func ComponentOf(err error) string {
	var ce *ComponentError
	if errors.As(err, &ce) {
		return ce.Component
	}
	return ""
}

// NewComponentError tags an error with the component that produced it.
// Exported for callers outside this package (and for tests) to build the same
// shape the stage implementations use.
func NewComponentError(component string, err error) error {
	return failing(component, err)
}

// taggedComponents is every manifest key the plan can attach to a failure.
// Kept beside the call sites it mirrors so a test can assert each one is a
// key the shipped bundle actually pins — a typo here would be a wrong record
// discovered on a customer's cluster rather than a failing test.
var taggedComponents = []string{
	"k3s",
	"gateway-api",
	"traefik",
	"cert-manager",
	"openebs-lvm-localpv",
	"velero",
	"system-upgrade-controller",
	"kured",
	"kubenest-agent",
}

// TaggedComponents returns the component keys the plan can report.
func TaggedComponents() []string {
	return append([]string(nil), taggedComponents...)
}
