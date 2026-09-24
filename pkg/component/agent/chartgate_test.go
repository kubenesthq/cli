package agent_test

import (
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/agent"
)

// The defect this gate exists for: under kn-cjqw OPTION A (2026-09-10) the
// control plane NEVER creates a cluster's workload Argo CD Applications, and
// the operator does — from chart 2.6.5 on. Below that pin the operator does
// not own them, helm accepts kubenest.workloadApplications and discards it
// (no chart in this line ships a values.schema.json), the install goes green,
// and nothing anywhere creates the Applications. The cluster is installed and
// deploys nothing.
//
// Measured, not inferred: op3 0ba157a is where the operator took ownership of
// the workload's Application, 5c965a8 made it survive `helm upgrade
// --reuse-values` without bumping the chart version, and 2.6.5 (op3 f767d97)
// is the first released tag containing both — checked with
// `git merge-base --is-ancestor`. platform-0.9 pins 2.2.0, which is the pin a
// customer still installing today would get.
func TestChartRefusesAPinWhoseOperatorDoesNotOwnTheApplications(t *testing.T) {
	for _, version := range []string{"2.2.0", "2.6.4"} {
		_, err := agent.Chart(bundleWithAgent(t, version), creds(true), agent.ValuesOptions{})
		if err == nil {
			t.Fatalf("kubenest-agent %s has no operator that creates the workload Applications and must be refused", version)
		}
		// The message has to name the pin and the fix, or the reader cannot act.
		for _, want := range []string{version, "2.6.5", "workload Argo CD Applications"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal for %s must name %q: %v", version, want, err)
			}
		}
	}
}

// Positive control, with AND without a repo credential. Without this the
// refusal above would look just as correct if Chart refused every bundle,
// which would break every install rather than the ones that cannot work.
func TestChartAcceptsTheChartsThatOwnTheApplications(t *testing.T) {
	for _, version := range []string{"2.6.5", "2.6.16", "3.0.0"} {
		for _, withRepo := range []bool{false, true} {
			chart, err := agent.Chart(bundleWithAgent(t, version), creds(withRepo), agent.ValuesOptions{})
			if err != nil {
				t.Errorf("kubenest-agent %s creates the workload Applications (repo credential: %v) and must be accepted: %v", version, withRepo, err)
				continue
			}
			if chart.Version != version {
				t.Errorf("chart version is %q, want the bundle pin %q", chart.Version, version)
			}
		}
	}
}

// An unreadable pin must refuse, not be treated as new enough. A gate that
// defaults an unparseable version to "supported" fails open, which is the
// shape of defect this whole change is about.
func TestAnUnreadablePinIsRefusedRatherThanAssumedNewEnough(t *testing.T) {
	_, err := agent.Chart(bundleWithAgent(t, "latest"), creds(true), agent.ValuesOptions{})
	if err == nil {
		t.Fatal("an unparseable chart pin was treated as creating the workload Applications")
	}
	if !strings.Contains(err.Error(), "latest") {
		t.Errorf("the refusal must name the pin it could not read: %v", err)
	}
}
