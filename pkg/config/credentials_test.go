package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCredentialsRoundTripKeyedByControlPlane(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kubenest", "credentials.json")

	c := &Credentials{}
	c.Set("https://api.prod.example.com", "knp_prod")
	c.Set("https://api.staging.example.com/", "knp_staging") // trailing slash normalizes
	if err := saveCredentialsTo(path, c); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := loadCredentialsFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if tok := got.TokenFor("https://api.prod.example.com"); tok != "knp_prod" {
		t.Errorf("prod token = %q", tok)
	}
	// Lookup normalizes the same way storage did.
	if tok := got.TokenFor("https://api.staging.example.com"); tok != "knp_staging" {
		t.Errorf("staging token = %q — trailing slash must not split identities", tok)
	}
	if tok := got.TokenFor("https://api.other.example.com"); tok != "" {
		t.Errorf("unknown control plane returned %q, want empty", tok)
	}

	got.Delete("https://api.prod.example.com/")
	if tok := got.TokenFor("https://api.prod.example.com"); tok != "" {
		t.Errorf("deleted token still present: %q", tok)
	}
}

func TestCredentialsFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	path := filepath.Join(t.TempDir(), ".kubenest", "credentials.json")
	c := &Credentials{}
	c.Set("https://api.example.com", "knp_secret")
	if err := saveCredentialsTo(path, c); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials file mode = %o, want 600 — the token exists nowhere else", perm)
	}
}

func TestCredentialsMissingFileIsEmptyStore(t *testing.T) {
	got, err := loadCredentialsFrom(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tokens) != 0 {
		t.Errorf("expected empty store, got %+v", got)
	}
}
