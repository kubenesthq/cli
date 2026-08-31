package agent_test

import (
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/agent"
)

// The defect: helm accepts values a chart does not declare and discards them.
// The chart ships no values.schema.json, so rendering gitSSHPrivateKey against
// kubenest-agent 2.2.0 — the pin platform-0.9 carries — produced a green
// install with bootstrap.gitea disabled and no Git credential of any kind.
// Neither the bundled Git server nor the external repo, discovered later by
// whoever wondered why nothing deployed.
//
// Verified against the published artifacts, not inferred: every tag from 2.2.0
// to 2.4.0 was pulled from ghcr. 2.2.0-2.3.4 have zero gitSSHPrivateKey and no
// GIT_SSH_*; 2.3.5 has both but pins appVersion dcc8d25, an operator build that
// cannot read the variable; 2.4.0 pins 8411f91, which can. No chart in the line
// ships a values.schema.json, which is why helm accepts and discards.
func TestARepoCredentialIsRefusedAgainstAChartThatCannotCarryIt(t *testing.T) {
	for _, version := range []string{"2.2.0", "2.3.0", "2.3.4", "2.3.5"} {
		_, err := agent.Chart(bundleWithAgent(t, version), creds(true))
		if err == nil {
			t.Fatalf("chart %s cannot deliver a working deploy key and must be refused", version)
		}
		// The message has to name the pin and the fix, or the reader cannot act.
		for _, want := range []string{version, "2.4.0", "gitSSHPrivateKey"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal for %s must name %q: %v", version, want, err)
			}
		}
	}
}

// 2.3.5 is the trap. It carries gitSSHPrivateKey and wires
// GIT_SSH_PRIVATE_KEY_FILE, so a check on the values surface alone passes it -
// but its appVersion dcc8d25 predates kn-gjew and reads GIT_TOKEN only. The
// key is accepted, mounted, and ignored: the same silent no-credential end
// state as 2.2.0, reached one layer down. Kept as its own test because a
// future reader lowering the constant back to 2.3.5 would still see the test
// above pass on 2.2.0 and think the gate was intact.
func TestTheChartThatCarriesTheValueButCannotReadItIsAlsoRefused(t *testing.T) {
	if _, err := agent.Chart(bundleWithAgent(t, "2.3.5"), creds(true)); err == nil {
		t.Fatal("2.3.5 accepts gitSSHPrivateKey but its operator cannot read it; it must be refused")
	}
}

// Positive control. Without this the refusal above would look just as correct
// if Chart refused every repo credential, which would break every install
// rather than the ones that cannot work.
func TestARepoCredentialIsAcceptedAtTheFirstChartThatCarriesIt(t *testing.T) {
	for _, version := range []string{"2.4.0", "2.4.1", "2.10.0", "3.0.0"} {
		if _, err := agent.Chart(bundleWithAgent(t, version), creds(true)); err != nil {
			t.Errorf("chart %s delivers a working deploy key and must be accepted: %v", version, err)
		}
	}
}

// The gate is conditional on there BEING a credential. A cluster installed
// from bundle 0.9 with no GitOps repository is a supported configuration and
// must keep working — 0.9's pin is deliberate history, not a defect to fix by
// moving it.
func TestAnOldChartIsFineWithNoRepoCredential(t *testing.T) {
	if _, err := agent.Chart(bundleWithAgent(t, "2.2.0"), creds(false)); err != nil {
		t.Errorf("bundle 0.9 without a GitOps repo must still install: %v", err)
	}
}

// An unreadable pin must refuse, not be treated as new enough. A gate that
// defaults an unparseable version to "supported" fails open, which is the
// shape of defect this whole change is about.
func TestAnUnreadablePinIsRefusedRatherThanAssumedNewEnough(t *testing.T) {
	_, err := agent.Chart(bundleWithAgent(t, "latest"), creds(true))
	if err == nil {
		t.Fatal("an unparseable chart pin was treated as carrying the deploy key values")
	}
	if !strings.Contains(err.Error(), "latest") {
		t.Errorf("the refusal must name the pin it could not read: %v", err)
	}
}
