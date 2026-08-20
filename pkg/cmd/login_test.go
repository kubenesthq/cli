package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/config"
)

// isolateHome points the config/credentials paths at a temp dir.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	return home
}

func TestLoginTokenStdinStoresCredential(t *testing.T) {
	home := isolateHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s — --token-stdin must not call the control plane", r.URL.Path)
	}))
	defer srv.Close()

	root := NewRootCommand()
	root.SetArgs([]string{"login", "--control-plane", srv.URL, "--token-stdin"})
	root.SetIn(strings.NewReader("knp_from_console\n"))
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v", err)
	}

	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if tok := creds.TokenFor(srv.URL); tok != "knp_from_console" {
		t.Errorf("stored token = %q", tok)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(home, ".kubenest", "credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("credentials mode = %o, want 600", perm)
		}
	}

	// The control plane is remembered for later commands.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlPlaneURL != srv.URL {
		t.Errorf("control plane not remembered: %q", cfg.ControlPlaneURL)
	}
}

func TestLoginDeviceFlowPrintsCodeAndStoresToken(t *testing.T) {
	isolateHome(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/cli/device":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-1",
				"user_code":        "ABCD-EFGH",
				"verification_uri": "https://console.example.com/cli-authorize",
				"expires_in":       900,
				"interval":         1,
			})
		case "/api/v1/auth/cli/device/token":
			json.NewEncoder(w).Encode(map[string]string{"token": "knp_device_flow"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	root := NewRootCommand()
	root.SetArgs([]string{"login", "--control-plane", srv.URL})
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("login: %v", err)
	}

	// The human needs the code and where to enter it.
	for _, want := range []string{"ABCD-EFGH", "console.example.com/cli-authorize"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("login output missing %q:\n%s", want, out.String())
		}
	}
	// The token must never be printed.
	if strings.Contains(out.String(), "knp_device_flow") {
		t.Errorf("login printed the token:\n%s", out.String())
	}

	creds, err := config.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if tok := creds.TokenFor(srv.URL); tok != "knp_device_flow" {
		t.Errorf("stored token = %q", tok)
	}
}

func TestLogoutDeletesLocalCredentialAndSaysTokenSurvives(t *testing.T) {
	isolateHome(t)

	creds := &config.Credentials{}
	creds.Set("https://api.example.com", "knp_x")
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(&config.Config{ControlPlaneURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}

	root := NewRootCommand()
	root.SetArgs([]string{"logout"})
	var out bytes.Buffer
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	after, _ := config.LoadCredentials()
	if tok := after.TokenFor("https://api.example.com"); tok != "" {
		t.Errorf("token still stored after logout: %q", tok)
	}
	if !strings.Contains(out.String(), "revoke") {
		t.Errorf("logout must say the token survives until revoked in the console:\n%s", out.String())
	}
}
