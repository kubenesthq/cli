package install

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// The administrator password is shown once per INSTALL, not once per run that
// happened to generate it. On real hardware the first run generated the
// password and then failed at the control-plane stage's readiness check;
// every resumed run found the Secret already there, so an install keyed on
// "generated in this run" finished without ever showing its password.
func TestTheAdminPasswordIsShownOnceAcrossTheRunsOfOneInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	opts := Options{
		Bundle: "1.0", Name: "cp-1", Servers: []string{"10.0.1.10"}, HATier: "single-server",
		ControlPlaneInstall: true, Domain: "kn.example.com", AdminEmail: "admin@kn.example.com",
	}
	const password = "correct-horse-battery"

	// One run of the install: the journal and its record are read back the
	// way the command builds a session (pkg/cmd platform_run.go).
	run := func() string {
		t.Helper()
		j, err := OpenJournal(path, opts.Identity())
		if err != nil {
			t.Fatal(err)
		}
		record, err := Recorded(j)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		s := &Session{ID: NewRunID(), Opts: opts, Jnl: j, Record: record, Out: &out}
		if err := s.showAdminPasswordOnce(password); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	if first := run(); !strings.Contains(first, password) {
		t.Fatalf("the first run to finish the stage did not show the password:\n%s", first)
	}
	if second := run(); strings.Contains(second, password) {
		t.Fatalf("a later run of the same install showed the password again:\n%s", second)
	}
}
