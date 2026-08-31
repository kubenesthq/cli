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
// Verified against the published artifacts, not inferred: chart 2.2.0 pulled
// from ghcr has zero occurrences of gitSSHPrivateKey in values.yaml and no
// GIT_SSH_* in its templates; 2.3.5 has both.
func TestARepoCredentialIsRefusedAgainstAChartThatCannotCarryIt(t *testing.T) {
	_, err := agent.Chart(bundleWithAgent(t, "2.2.0"), creds(true))
	if err == nil {
		t.Fatal("a repo credential was rendered into a chart with no gitSSHPrivateKey value")
	}
	// The message has to name both versions, or the reader cannot act on it.
	for _, want := range []string{"2.2.0", "2.3.5", "gitSSHPrivateKey"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %v", want, err)
		}
	}
}

// Positive control. Without this the refusal above would look just as correct
// if Chart refused every repo credential, which would break every install
// rather than the ones that cannot work.
func TestARepoCredentialIsAcceptedAtTheFirstChartThatCarriesIt(t *testing.T) {
	for _, version := range []string{"2.3.5", "2.4.0", "2.10.0", "3.0.0"} {
		if _, err := agent.Chart(bundleWithAgent(t, version), creds(true)); err != nil {
			t.Errorf("chart %s carries gitSSHPrivateKey and must be accepted: %v", version, err)
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
