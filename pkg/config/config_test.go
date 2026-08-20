package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kubenest", "config.json")

	want := &Config{
		ControlPlaneURL: "https://api.example.com",
		Token:           "tok-123",
		UserEmail:       "op@example.com",
	}
	if err := saveTo(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if *got != *want {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadMissingFileIsEmptyConfig(t *testing.T) {
	got, err := loadFrom(filepath.Join(t.TempDir(), "nope", "config.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if *got != (Config{}) {
		t.Errorf("expected empty config, got %+v", got)
	}
}

func TestOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	path := filepath.Join(t.TempDir(), ".kubenest", "config.json")
	if err := saveTo(path, &Config{Token: "secret"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %o, want 700", perm)
	}
}

func TestSaveTightensExistingLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := filepath.Join(t.TempDir(), ".kubenest")
	path := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := saveTo(path, &Config{Token: "secret"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	fi, _ := os.Stat(path)
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("pre-existing file left at %o, want 600", perm)
	}
	di, _ := os.Stat(dir)
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("pre-existing dir left at %o, want 700", perm)
	}
}

func TestLegacyAPIURLMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	legacy := []byte(`{"api_url": "https://api.old.example.com", "token": "t", "team_uuid": "ignored"}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.ControlPlaneURL != "https://api.old.example.com" {
		t.Errorf("legacy api_url not migrated, got %q", got.ControlPlaneURL)
	}
	if got.Token != "t" {
		t.Errorf("token lost in migration, got %q", got.Token)
	}
}
