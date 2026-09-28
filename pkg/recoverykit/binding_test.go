package recoverykit

import (
	"strings"
	"testing"
)

// A control-plane kit is an INSTANCE resource, and its binding must not be able
// to name an organisation.
//
// kn-t48-all-host-recovery-s11-prao.2: passing `--organisation` with
// `--kind control-plane` was refused with "the artifact belongs to another
// instance, organisation or cluster", which names an empty organisation and
// says nothing about the kind. The rule belongs here, where the wire type is
// defined, so a binding that cannot mean anything is refused at construction
// rather than compared later.
func TestAControlPlaneBindingCarriesNoOrganisation(t *testing.T) {
	if err := (Binding{Kind: KindControlPlane, InstanceID: "inst-1"}).validate(); err != nil {
		t.Fatalf("a control-plane binding with an instance and no organisation was refused: %v", err)
	}
	if err := (Binding{Kind: KindControlPlane, InstanceID: "inst-1", ClusterID: "mgmt-1"}).validate(); err != nil {
		t.Fatalf("a control-plane binding naming the MANAGEMENT cluster was refused: %v", err)
	}

	err := (Binding{Kind: KindControlPlane, InstanceID: "inst-1", OrganisationID: "org-1"}).validate()
	if err == nil {
		t.Fatal("a control-plane binding was allowed to name an organisation: a control-plane kit is not a cluster resource and carries none")
	}
	for _, want := range []string{"INSTANCE", "organisation"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not say %q, so a caller cannot tell whether the kind or the value is wrong: %v", want, err)
		}
	}

	// And a CLUSTER binding still requires all three: the rule was tightened
	// for one kind, not loosened for the other.
	if err := (Binding{Kind: KindCluster, InstanceID: "inst-1", ClusterID: "c-1"}).validate(); err == nil {
		t.Fatal("a cluster binding with no organisation was accepted")
	}
	if err := (Binding{Kind: KindCluster, InstanceID: "inst-1", OrganisationID: "org-1", ClusterID: "c-1"}).validate(); err != nil {
		t.Fatalf("a complete cluster binding was refused: %v", err)
	}
}
