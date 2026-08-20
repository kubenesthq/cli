// Package component holds the platform component installers (S3): each
// component is one small package with one shape — render from the bundle
// manifest → apply via pkg/k3s → wait through pkg/converge → verify.
//
// Every installer has the same signature so the 13-stage installer (kn-7k8)
// can sequence them uniformly:
//
//	Install(ctx, runner, bundle, reporter) error
//
// Versions come from the bundle manifest's core pins and deadlines from its
// limits.timeouts — nothing here declares either. This package itself carries
// only the probes shared between components.
package component

import (
	"context"
	"encoding/json"
	"fmt"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
)

// conditionedObject is the part of any Kubernetes object a condition probe
// reads: status.conditions.
type conditionedObject struct {
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// CheckCondition observes one object's status condition. It is one
// observation for converge.Wait: not-found and not-yet-True are converging
// states, never verdicts. `resource` is kubectl's <kind>/<name> form;
// namespace may be empty for cluster-scoped kinds.
func CheckCondition(ctx context.Context, r k3s.Runner, resource, namespace, condType string) (bool, converge.State, error) {
	args := "get " + resource + " -o json"
	object := resource
	if namespace != "" {
		args += " -n " + namespace
		object += " in " + namespace
	}
	out, err := k3s.Kubectl(ctx, r, args)
	if err != nil {
		// Not created yet (or the API is briefly away) — an observation.
		return false, converge.State{Object: object, Status: "not found yet"}, err
	}

	var obj conditionedObject
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		return false, converge.State{Object: object, Status: "unparsable"}, err
	}
	for _, c := range obj.Status.Conditions {
		if c.Type != condType {
			continue
		}
		if c.Status == "True" {
			return true, converge.State{Object: object, Status: condType}, nil
		}
		status := condType + "=" + c.Status
		if c.Reason != "" {
			status += " (" + c.Reason + ")"
		}
		return false, converge.State{Object: object, Status: status, Detail: c.Message}, nil
	}
	return false, converge.State{
		Object: object,
		Status: "no " + condType + " condition yet",
	}, nil
}

// ConditionProbe wraps CheckCondition as a converge.Probe.
func ConditionProbe(r k3s.Runner, resource, namespace, condType string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		return CheckCondition(ctx, r, resource, namespace, condType)
	}
}

// CRDsEstablishedProbe reports whether every named CustomResourceDefinition
// is Established, naming the first that is not.
func CRDsEstablishedProbe(r k3s.Runner, names []string) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		for _, name := range names {
			done, state, err := CheckCondition(ctx, r, "crd/"+name, "", "Established")
			if err != nil || !done {
				return false, state, err
			}
		}
		return true, converge.State{
			Object: "custom resource definitions",
			Status: fmt.Sprintf("%d/%d Established", len(names), len(names)),
		}, nil
	}
}
