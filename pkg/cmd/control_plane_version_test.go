package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/version"
)

// stubControlPlane is a control plane that answers the version route (or does
// not) and counts how often it was asked.
type stubControlPlane struct {
	mu       sync.Mutex
	version  int
	status   int
	body     string
	requests []string
}

func (s *stubControlPlane) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.Path)
		s.mu.Unlock()
		if r.URL.Path != "/api/v1/version" {
			http.Error(w, `{"detail": "no route"}`, http.StatusNotFound)
			return
		}
		if s.status != 0 && s.status != http.StatusOK {
			http.Error(w, s.body, s.status)
			return
		}
		body := s.body
		if body == "" {
			body = `{"contract": ` + itoa(s.version) + `, "build": "c121ed8"}`
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (s *stubControlPlane) versionRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, path := range s.requests {
		if path == "/api/v1/version" {
			n++
		}
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}

// loggedIn points this machine at the stub, with a stored token.
func loggedIn(t *testing.T, url string) {
	t.Helper()
	isolateHome(t)
	if err := config.Save(&config.Config{ControlPlaneURL: url}); err != nil {
		t.Fatal(err)
	}
	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	creds.Set(url, "knp_tok")
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatal(err)
	}
}

// runCommand executes one command path from the real tree.
func runCommand(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRootCommand()
	root.SetArgs(args)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root.Execute()
}

// A control plane below the CLI's floor is refused with the control-plane
// upgrade named as the fix, at the command that first assumed an API this CLI
// understands.
func TestAControlPlaneBelowTheFloorIsRefusedWithTheUpgradeNamed(t *testing.T) {
	required, ok := version.RequiredEra()
	if !ok {
		t.Fatal("pkg/version records no floor, so nothing can be refused")
	}
	stub := &stubControlPlane{version: required.Era - 1}
	url := stub.start(t)
	loggedIn(t, url)

	err := runCommand(t, "platform", "upgrade", "--cluster", "prod-1", "--to", "1.1")
	if err == nil {
		t.Fatal("a control plane below the floor was accepted")
	}
	if !strings.Contains(err.Error(), "platform upgrade --control-plane") {
		t.Errorf("the refusal does not name the control-plane upgrade as the fix: %v", err)
	}
	// It refused BEFORE doing any work: the only request made was the version
	// read the check itself needs.
	if got := stub.versionRequests(); got != 1 {
		t.Errorf("the version route was read %d time(s), want exactly 1 before the refusal", got)
	}
}

// A control plane that does not serve the version route at all is CANNOT TELL,
// and the command must not be refused for it.
//
// The population a refusal would condemn includes every control plane built
// before the contract counter — including ones that already carry the behaviour
// an era was minted for.
func TestAnOlderControlPlaneAnswering404IsNotReportedAsHavingTheBehaviour(t *testing.T) {
	stub := &stubControlPlane{status: http.StatusNotFound, body: `{"detail": "Not Found"}`}
	url := stub.start(t)
	loggedIn(t, url)

	err := runCommand(t, "platform", "upgrade", "--cluster", "prod-1", "--to", "1.1")
	if err != nil && strings.Contains(err.Error(), "contract era") {
		t.Fatalf("a 404 at the version route was read as a capability refusal: %v", err)
	}
	// It DID ask, and then carried on past the check: the failure it reaches is
	// about the cluster's record, not about a behaviour the control plane
	// cannot be shown to lack.
	if got := stub.versionRequests(); got == 0 {
		t.Error("the version route was never read, so this test does not exercise the 404 path")
	}
	// The proof that "cannot tell" let the command through: it went on to ask
	// the control plane for something else. A 404 turned into an error stops
	// the command at the version read, and nothing follows it.
	stub.mu.Lock()
	requests := append([]string(nil), stub.requests...)
	stub.mu.Unlock()
	carriedOn := false
	for i, path := range requests {
		if path == "/api/v1/version" && i < len(requests)-1 {
			carriedOn = true
		}
	}
	if !carriedOn {
		t.Errorf("the command stopped at the version read (requests: %v): a 404 there is 'cannot tell', not a refusal", requests)
	}
}

// The commands that do not need a live version endpoint must not call it.
//
// SIGNING IN IS FIRST: `--token-stdin` exists for a console-created token and
// has to work with the control plane unreachable, fenced or being restored.
// `recovery-kit check` is the other half, and it checks compatibility against
// the recovery set's manifest — a document the operator holds.
func TestTheCommandsThatDoNotNeedALiveVersionEndpointDoNotCallIt(t *testing.T) {
	stub := &stubControlPlane{version: 99}
	url := stub.start(t)
	loggedIn(t, url)

	// --token-stdin: the login must store the token and reach nothing.
	root := NewRootCommand()
	root.SetArgs([]string{"login", "--control-plane", url, "--token-stdin"})
	root.SetIn(strings.NewReader("knp_from_console\n"))
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("login --token-stdin: %v", err)
	}
	if got := stub.versionRequests(); got != 0 {
		t.Errorf("login --token-stdin read the version route %d time(s); a token the operator already holds must be storable with the control plane unreachable", got)
	}

	// recovery-kit check: a local command. It may fail for a missing kit or a
	// missing fleet key; what it must not do is ask the control plane's
	// version.
	if err := runCommand(t, "recovery-kit", "check"); err != nil {
		t.Logf("recovery-kit check failed, which is fine: %v", err)
	}
	if got := stub.versionRequests(); got != 0 {
		t.Errorf("recovery-kit check read the version route %d time(s)", got)
	}

	// And the classification the wiring consults agrees with what was just
	// observed, for every leaf of the real command tree.
	tree := NewRootCommand()
	for _, path := range leafPaths(tree) {
		switch path {
		case "login", "platform install", "platform upgrade", "backup now":
			if !needsTheControlPlane(path) {
				t.Errorf("%s needs the control plane but is not classified as such", path)
			}
		case "recovery-kit check", "platform rollback", "platform restore", "cluster list":
			if needsTheControlPlane(path) {
				t.Errorf("%s must not consult the live version endpoint: recovery and resuming check compatibility against the recovery set's manifest", path)
			}
		}
	}
}

// leafPaths returns every full command path in a tree.
func leafPaths(root *cobra.Command) []string {
	var out []string
	var walk func(cmd *cobra.Command, prefix string)
	walk = func(cmd *cobra.Command, prefix string) {
		path := strings.TrimSpace(prefix + " " + cmd.Name())
		if !cmd.HasSubCommands() || cmd.RunE != nil || cmd.Run != nil {
			out = append(out, path)
		}
		for _, child := range cmd.Commands() {
			walk(child, path)
		}
	}
	for _, child := range root.Commands() {
		walk(child, "")
	}
	return out
}
