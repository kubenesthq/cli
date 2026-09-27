package k3s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DisruptionReport is the answer to "would a drain finish?", with what was
// observed and what to do about it. It is a neutral result rather than a
// package's own gate type because TWO operations ask this question — a bundle
// upgrade before it drains nodes one at a time, and `kubenest node remove`
// before it takes a node out for good — and a second implementation of it
// would be a second answer.
type DisruptionReport struct {
	// Passed is whether a drain could complete.
	Passed bool
	// Detail is what was observed, naming the budgets that cannot move.
	Detail string
	// Fix is what to do about it. Empty when Passed.
	Fix string
}

// DrainWouldFinish asks whether a drain over the cluster's
// PodDisruptionBudgets would FINISH, which is answerable in advance — not
// whether a budget is reasonable, which is not.
//
// A budget that permits zero disruption stalls the drain until its timeout and
// leaves the cluster mid-operation, which is a strictly worse place than either
// finishing or not starting. The check is deliberately narrow: a budget
// currently allowing no disruptions AND with no slack (every pod it expects is
// needed) can never let a pod move.
func DrainWouldFinish(ctx context.Context, r Runner) DisruptionReport {
	out, err := Kubectl(ctx, r, "get poddisruptionbudgets -A -o json")
	if err != nil {
		return DisruptionReport{
			Detail: "could not read pod disruption budgets: " + err.Error(),
			Fix:    "the cluster must be readable before a node can be drained",
		}
	}
	var pdbs struct {
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
	if err := json.Unmarshal([]byte(out), &pdbs); err != nil {
		return DisruptionReport{Detail: "unparsable pod disruption budgets",
			Fix: "the cluster must be readable before a node can be drained"}
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
		return DisruptionReport{
			Detail: strings.Join(blocking, "; "),
			Fix:    "relax the budget or add a replica. A drain that can never complete holds the cluster mid-operation — the CLI never force-deletes a pod, because that is an operator's decision and not a tool's",
		}
	}
	return DisruptionReport{Passed: true,
		Detail: fmt.Sprintf("%d budget(s), none of which would block a drain indefinitely", len(pdbs.Items))}
}
