package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func runCluster(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestRotateTokenRequiresACluster(t *testing.T) {
	_, err := runCluster(t, "cluster", "rotate-token")
	if err == nil {
		t.Fatal("rotate-token ran with no --cluster")
	}
	if !strings.Contains(err.Error(), "--cluster is required") {
		t.Errorf("error %q does not say which flag is missing", err)
	}
}

// THE HELP TEXT IS PART OF THE FIX, not decoration. The defect this command
// exists to prevent is someone treating rotation as routine hygiene, and the
// only thing standing between an operator and a fleet-wide outage is that the
// command says what it does before they run it.
func TestRotateTokenHelpSaysItDisconnectsTheCluster(t *testing.T) {
	out, err := runCluster(t, "cluster", "rotate-token", "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	// Case-insensitive on purpose: the test is about what the help SAYS, and
	// pinning the casing would fail on a rewording that kept the meaning.
	lower := strings.ToLower(out)
	for _, want := range []string{"disconnected", "leaked", "re-run"} {
		if !strings.Contains(lower, want) {
			t.Errorf("help does not mention %q:\n%s", want, out)
		}
	}
}

func TestClusterGroupCarriesRotateToken(t *testing.T) {
	out, err := runCluster(t, "cluster", "--help")
	if err != nil {
		t.Fatalf("cluster --help: %v", err)
	}
	if !strings.Contains(out, "rotate-token") {
		t.Errorf("rotate-token is not listed under cluster:\n%s", out)
	}
}
