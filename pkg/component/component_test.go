package component_test

import (
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
)

func respondJSON(json string) func(string) (sshx.Result, error) {
	return func(string) (sshx.Result, error) {
		return sshx.Result{Stdout: json}, nil
	}
}

func TestCheckConditionTrue(t *testing.T) {
	r := &componenttest.FakeRunner{Respond: respondJSON(
		`{"status": {"conditions": [{"type": "Programmed", "status": "True"}]}}`)}

	done, state, err := component.CheckCondition(context.Background(), r,
		"gateway/kubenest-gateway", "kubenest-system", "Programmed")
	if err != nil || !done {
		t.Fatalf("done=%v err=%v, want condition met", done, err)
	}
	if !strings.Contains(state.Object, "kubenest-gateway") {
		t.Errorf("state object = %q", state.Object)
	}

	cmds := r.Commands()
	if len(cmds) != 1 || !strings.Contains(cmds[0], "get gateway/kubenest-gateway -o json -n kubenest-system") {
		t.Errorf("kubectl invocation = %v", cmds)
	}
}

// A False condition is a converging observation carrying the reason and the
// fix-shaped message — the text an operator acts on.
func TestCheckConditionFalseCarriesReasonAndMessage(t *testing.T) {
	r := &componenttest.FakeRunner{Respond: respondJSON(
		`{"status": {"conditions": [{"type": "Programmed", "status": "False",
		  "reason": "Invalid", "message": "secret kubenest-gateway-default-cert not found"}]}}`)}

	done, state, err := component.CheckCondition(context.Background(), r,
		"gateway/kubenest-gateway", "kubenest-system", "Programmed")
	if err != nil || done {
		t.Fatalf("done=%v err=%v, want not-done observation", done, err)
	}
	if !strings.Contains(state.Status, "Invalid") {
		t.Errorf("status %q should carry the reason", state.Status)
	}
	if !strings.Contains(state.Detail, "not found") {
		t.Errorf("detail %q should carry the message", state.Detail)
	}
}

func TestCheckConditionAbsentObjectIsObservationNotVerdict(t *testing.T) {
	r := &componenttest.FakeRunner{Respond: func(string) (sshx.Result, error) {
		return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): gateways.gateway.networking.k8s.io "kubenest-gateway" not found`}, nil
	}}
	done, state, err := component.CheckCondition(context.Background(), r,
		"gateway/kubenest-gateway", "kubenest-system", "Programmed")
	if done {
		t.Fatal("an absent object must not satisfy a condition")
	}
	if err == nil {
		t.Fatal("absence should surface as a probe error (converge treats it as transient)")
	}
	if state.Status != "not found yet" {
		t.Errorf("state = %+v", state)
	}
}

func TestCRDsEstablishedNamesTheStraggler(t *testing.T) {
	r := &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "httproutes") {
			return sshx.Result{Stdout: `{"status": {"conditions": [{"type": "Established", "status": "False", "reason": "Installing"}]}}`}, nil
		}
		return sshx.Result{Stdout: `{"status": {"conditions": [{"type": "Established", "status": "True"}]}}`}, nil
	}}

	probe := component.CRDsEstablishedProbe(r, []string{
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
	})
	done, state, err := probe(context.Background())
	if err != nil || done {
		t.Fatalf("done=%v err=%v, want converging", done, err)
	}
	if !strings.Contains(state.Object, "httproutes") {
		t.Errorf("state should name the CRD that is not Established, got %+v", state)
	}
}

func TestCRDsEstablishedAllDone(t *testing.T) {
	r := &componenttest.FakeRunner{Respond: respondJSON(
		`{"status": {"conditions": [{"type": "Established", "status": "True"}]}}`)}
	probe := component.CRDsEstablishedProbe(r, []string{"a.x.io", "b.x.io"})
	done, state, err := probe(context.Background())
	if err != nil || !done {
		t.Fatalf("done=%v err=%v state=%+v", done, err, state)
	}
	if !strings.Contains(state.Status, "2/2") {
		t.Errorf("status = %q", state.Status)
	}
}
