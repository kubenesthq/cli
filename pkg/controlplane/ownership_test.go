package controlplane

import (
	"errors"
	"strings"
	"testing"
)

// THE CASE HARDWARE FOUND (2026-09-25). helm-controller applies the chart
// server-side with --force-conflicts=false; the chart rendered
// control-plane-checkpoint-status with a placeholder `data.status`, the
// checkpoint runner merge-patched that field, and the NEXT upgrade failed with
// `conflict with "kubectl-patch" using v1: .data.status`. The gate must report
// exactly that field before the apply, or the apply is what reports it — as a
// failed release, under failurePolicy: abort, waiting for an operator.
func TestAFieldTwoManagersOwnIsRefusedByName(t *testing.T) {
	objects := `{
	  "items": [{
	    "kind": "ConfigMap",
	    "metadata": {
	      "name": "control-plane-checkpoint-status",
	      "managedFields": [
	        {"manager": "helm", "operation": "Apply", "fieldsV1": {"f:data": {"f:status": {}}}},
	        {"manager": "kubectl-patch", "operation": "Update", "fieldsV1": {"f:data": {"f:status": {}}}},
	        {"manager": "helm", "operation": "Apply", "fieldsV1": {"f:metadata": {"f:labels": {}}}}
	      ]
	    }
	  }]
	}`
	report, err := ownershipConflicts([]byte(objects))
	if err != nil {
		t.Fatal(err)
	}
	if report.Objects != 1 {
		t.Fatalf("read %d object(s), want 1", report.Objects)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("found %d conflict(s), want exactly the one field both managers own: %+v", len(report.Conflicts), report.Conflicts)
	}
	conflict := report.Conflicts[0]
	if conflict.Object != "configmap/control-plane-checkpoint-status" {
		t.Errorf("the conflict names %q", conflict.Object)
	}
	if conflict.Field != ".data.status" {
		t.Errorf("the conflict names field %q, want .data.status — the field the operator has to read in helm's error", conflict.Field)
	}
	if strings.Join(conflict.Managers, ",") != "helm,kubectl-patch" {
		t.Errorf("the conflict names managers %v, want both of them", conflict.Managers)
	}

	refusal := (&OwnershipRefusal{Report: report}).Error()
	for _, want := range []string{"control-plane-checkpoint-status", ".data.status", "kubectl-patch", "helm"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, refusal)
		}
	}
	if !strings.Contains(refusal, "Fix:") {
		t.Errorf("the refusal does not say what to do:\n%s", refusal)
	}
}

// A field ONE manager owns is not a conflict, whoever that manager is: the
// gate's condition is Server-Side Apply's own, and a gate that refused an
// object merely because some tool once applied it would refuse everything.
func TestAFieldOneManagerOwnsIsNotAConflict(t *testing.T) {
	objects := `{
	  "items": [{
	    "kind": "Deployment",
	    "metadata": {
	      "name": "kubenest-cp-backend",
	      "managedFields": [
	        {"manager": "helm", "fieldsV1": {"f:spec": {"f:replicas": {}}}},
	        {"manager": "kubectl-client-side-apply", "fieldsV1": {"f:metadata": {"f:annotations": {}}}}
	      ]
	    }
	  }]
	}`
	report, err := ownershipConflicts([]byte(objects))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conflicts) != 0 {
		t.Fatalf("a field with a single owner was reported as a conflict: %+v", report.Conflicts)
	}
	if len(report.Managers) != 2 {
		t.Errorf("managers = %v, want both of them reported so an operator has the whole picture", report.Managers)
	}
}

// Siblings under a shared parent are not the same field, and a gate that said
// they were would refuse every object any tool has ever labelled.
func TestDifferentFieldsUnderTheSameParentAreNotAConflict(t *testing.T) {
	objects := `{
	  "items": [{
	    "kind": "ConfigMap",
	    "metadata": {
	      "name": "x",
	      "managedFields": [
	        {"manager": "helm", "fieldsV1": {"f:data": {"f:status": {}}}},
	        {"manager": "kubectl-patch", "fieldsV1": {"f:data": {"f:other": {}}}}
	      ]
	    }
	  }]
	}`
	report, err := ownershipConflicts([]byte(objects))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conflicts) != 0 {
		t.Fatalf("two different fields under data were reported as one conflict: %+v", report.Conflicts)
	}
}

// Nothing read is not a pass: a gate that inspected no object has established
// nothing, and reporting "no conflicts" for it would be the exact failure this
// gate exists to prevent.
func TestAnEmptyReadIsAnErrorNotAPass(t *testing.T) {
	if _, err := ownershipConflicts([]byte("")); err == nil {
		t.Fatal("an empty read passed the field-ownership check")
	}
	if _, err := ownershipConflicts([]byte(`{"items": []}`)); err != nil {
		t.Fatalf("a read that found no objects answered %v; it should report zero objects and no conflicts", err)
	}
}

// The refusal carries the shape a caller matches on, not only its text.
func TestTheOwnershipRefusalIsTyped(t *testing.T) {
	report := OwnershipReport{Objects: 1, Conflicts: []OwnershipConflict{{
		Object: "configmap/x", Field: ".data.status", Managers: []string{"helm", "kubectl-patch"},
	}}}
	var err error = &OwnershipRefusal{Report: report}
	var refusal *OwnershipRefusal
	if !errors.As(err, &refusal) {
		t.Fatal("the refusal is not matchable with errors.As")
	}
}
