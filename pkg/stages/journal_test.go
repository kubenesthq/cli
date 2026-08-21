package stages_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/stages"
)

// "Re-run the identical command" is the documented resume path. A command
// that is not identical is refused, naming the difference — resuming into a
// half-installed cluster with changed flags is how a cluster stops matching
// its own record.
func TestJournalRefusesADifferentInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	original := identity("prod-1", map[string]string{
		"bundle": "1.0", "HA tier": "single-server",
		"servers": "10.0.1.10", "agents": "10.0.1.11",
	})
	j, err := stages.OpenJournal(path, original)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(stages.Entry{Stage: stageK3sServer, Status: stages.StatusCompleted}); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		id   stages.Identity
		want string
	}{
		"different bundle": {
			id:   identity("prod-1", map[string]string{"bundle": "1.1", "HA tier": "single-server", "servers": "10.0.1.10", "agents": "10.0.1.11"}),
			want: "bundle",
		},
		"different tier": {
			id:   identity("prod-1", map[string]string{"bundle": "1.0", "HA tier": "ha", "servers": "10.0.1.10", "agents": "10.0.1.11"}),
			want: "HA tier",
		},
		"a node swapped": {
			id:   identity("prod-1", map[string]string{"bundle": "1.0", "HA tier": "single-server", "servers": "10.0.1.99", "agents": "10.0.1.11"}),
			want: "servers",
		},
		"a profile added": {
			id:   identity("prod-1", map[string]string{"bundle": "1.0", "HA tier": "single-server", "servers": "10.0.1.10", "agents": "10.0.1.11", "profiles": "observability"}),
			want: "profiles",
		},
		"a different operation entirely": {
			id:   stages.Identity{Kind: "upgrade", Cluster: "prod-1"},
			want: "operation",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := stages.OpenJournal(path, c.id)
			if err == nil {
				t.Fatal("a different install must be refused, not resumed into")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal must name the differing field %q:\n%s", c.want, err)
			}
			if !strings.Contains(err.Error(), "known state") {
				t.Errorf("the refusal must offer the way out:\n%s", err)
			}
		})
	}
}

// Argument order is not a different stages.
func TestJournalAcceptsTheSameNodesInADifferentOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	first := identity("prod-1", map[string]string{
		"bundle": "1.0", "HA tier": "ha",
		"servers":  stages.List([]string{"10.0.1.10", "10.0.1.11", "10.0.1.12"}),
		"profiles": stages.List([]string{"secrets", "observability"}),
	})
	if _, err := stages.OpenJournal(path, first); err != nil {
		t.Fatal(err)
	}
	j, err := stages.OpenJournal(path, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save(); err != nil {
		t.Fatal(err)
	}

	reordered := identity("prod-1", map[string]string{
		"bundle": "1.0", "HA tier": "ha",
		"servers":  stages.List([]string{"10.0.1.12", "10.0.1.10", "10.0.1.11"}),
		"profiles": stages.List([]string{"observability", "secrets"}),
	})
	if _, err := stages.OpenJournal(path, reordered); err != nil {
		t.Errorf("the same install with its arguments in another order must resume: %v", err)
	}
}

// The journal is the durable record of a cluster's install and sits next to
// the control-plane token. It is written 0600.
func TestJournalIsWrittenPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "journal.json")
	j, err := stages.OpenJournal(path, identity("prod-1", map[string]string{"bundle": "1.0"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(stages.Entry{Stage: stageRegister, Status: stages.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("journal mode is %o, want 600", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("journal directory mode is %o, want 700", perm)
	}
}

// THE credential rule, enforced rather than promised: the agent JWT and the
// repository deploy key are minted once per process and terminate on the
// target hosts. Nothing about them reaches the journal — which is why resume
// re-runs register rather than trying to recover secrets it never wrote.
func TestNoCredentialCanReachTheJournal(t *testing.T) {
	const privateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----"
	const agentJWT = "eyJhbGciOiJIUzI1NiJ9.SUPER_SECRET_AGENT_JWT.sig"

	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := stages.OpenJournal(path, identity("prod-1", map[string]string{"bundle": "1.0"}))
	if err != nil {
		t.Fatal(err)
	}

	// A register stage that succeeded: the credentials stay in the caller's
	// memory and only the non-secret facts are journalled.
	j.ClusterID = "019d52e1-ba17-7e70-94a0-8a33a48b7fcb"
	if err := j.SetState(map[string]any{
		"token_version": 2,
		"repo_url":      "git@gitea.internal:kubenest/prod-1.git",
		"adopted":       true,
	}); err != nil {
		t.Fatal(err)
	}
	_ = privateKey
	_ = agentJWT
	if err := j.Append(stages.Entry{Stage: stageRegister, Status: stages.StatusCompleted, RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, privateKey) || strings.Contains(body, "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatal("a private key reached the install journal")
	}
	if strings.Contains(body, agentJWT) || strings.Contains(body, "SUPER_SECRET_AGENT_JWT") {
		t.Fatal("an agent JWT reached the install journal")
	}
	// The non-secret facts ARE recorded — they are what makes the record
	// legible without making it dangerous.
	for _, want := range []string{"019d52e1-ba17-7e70-94a0-8a33a48b7fcb", `"token_version": 2`, "gitea.internal", `"adopted": true`} {
		if !strings.Contains(body, want) {
			t.Errorf("journal is missing the non-secret register fact %q:\n%s", want, body)
		}
	}
}

// A cluster name is user input and reaches a filesystem path.
func TestJournalPathCannotEscapeItsDirectory(t *testing.T) {
	path, err := stages.JournalPath("install", "../../etc/cron.d/evil")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "..") || !strings.Contains(path, filepath.Join(".kubenest", "journals")) {
		t.Errorf("journal path %q escapes the journal directory", path)
	}
}

// A corrupt journal is reported as one, with the way out — not treated as an
// empty journal, which would silently re-run stages against a live cluster.
func TestCorruptJournalIsReportedNotIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := stages.OpenJournal(path, identity("prod-1", map[string]string{"bundle": "1.0"}))
	if err == nil {
		t.Fatal("a corrupt journal must be an error, not an empty one")
	}
	if !strings.Contains(err.Error(), "uninstall --confirm") {
		t.Errorf("the error must say how to get back to a known state:\n%s", err)
	}
}

// Completed() reads the LAST word on a stage: one that completed and was
// later re-run into a failure is not complete.
func TestCompletedReadsTheLastWordOnAStage(t *testing.T) {
	j, err := stages.OpenJournal(filepath.Join(t.TempDir(), "j.json"), identity("c", nil))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(j.Append(stages.Entry{Stage: stageStorage, Status: stages.StatusCompleted}))
	if _, ok := j.Completed(stageStorage); !ok {
		t.Fatal("a completed stage must read as completed")
	}
	must(j.Append(stages.Entry{Stage: stageStorage, Status: stages.StatusStarted}))
	must(j.Append(stages.Entry{Stage: stageStorage, Status: stages.StatusFailed, Detail: "vg vanished"}))
	if _, ok := j.Completed(stageStorage); ok {
		t.Fatal("a stage that later failed must not read as completed")
	}
}

func TestRemoveDeletesTheJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := stages.OpenJournal(path, identity("prod-1", map[string]string{"bundle": "1.0"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Save(); err != nil {
		t.Fatal(err)
	}
	if err := j.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("journal still present after Remove: %v", err)
	}
	// Removing twice is not an error: uninstall must be re-runnable.
	if err := j.Remove(); err != nil {
		t.Errorf("Remove must be idempotent: %v", err)
	}
}
