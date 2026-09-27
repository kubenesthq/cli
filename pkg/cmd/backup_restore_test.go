package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

// T4.1's command surface: the flag refusals, and the plan rendered from a fake
// eligibility source. The plan's decisions are pkg/backup's and are tested
// there; what is asserted here is that THIS command accepts the right flags,
// refuses the wrong combinations before it dials anything, and shows the
// operator what the run is about to do.

// restoreCmdManifest writes the bundle the command loads. It carries the
// deadlines every wait reads and T2.0's recovery-point policy.
func restoreCmdManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "platform-1.2.yaml")
	body := `bundle: "1.2"
limits:
  timeouts:
    backup: 30m
    restore-drill: 45m
    component-ready: 2m
health:
  backup:
    max-backup-age: 48h
    recovery-point-age: 26h
    max-restore-drill-age: 336h
    unconfigured-target-grace: 168h
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBackupRestoreFlagValidation is the flag surface's own refusals. Each of
// these must happen before anything is read or dialled: a flag that means a
// different thing in each mode, silently accepted, leaves an operator believing
// the run was scoped to something it never looked at.
func TestBackupRestoreFlagValidation(t *testing.T) {
	cases := []struct {
		name  string
		state restoreFlagState
		want  []string
	}{
		{
			name:  "from and latest",
			state: restoreFlagState{namespace: "payments", from: "b", latest: true},
			want:  []string{"--from", "--latest"},
		},
		{
			name:  "no backup chosen",
			state: restoreFlagState{namespace: "payments", confirm: true},
			want:  []string{"--from", "--latest"},
		},
		{
			name:  "no namespace",
			state: restoreFlagState{latest: true},
			want:  []string{"--namespace"},
		},
		{
			name:  "pvc without namespace",
			state: restoreFlagState{pvcs: []string{"data-0"}, latest: true},
			want:  []string{"--namespace"},
		},
		{
			name:  "resume and activate",
			state: restoreFlagState{resume: "abcdef01", activate: "abcdef02"},
			want:  []string{"--resume", "--activate", "mutually exclusive"},
		},
		{
			name:  "abort and resume",
			state: restoreFlagState{abort: "abcdef01", resume: "abcdef02"},
			want:  []string{"--abort", "--resume", "mutually exclusive"},
		},
		{
			name:  "resume takes no mode flags",
			state: restoreFlagState{resume: "abcdef01", latest: true, namespace: "payments"},
			want:  []string{"--resume", "--latest", "no other mode flags"},
		},
		{
			name:  "activate takes no mode flags",
			state: restoreFlagState{activate: "abcdef01", from: "daily", namespace: "payments"},
			want:  []string{"--activate", "--from"},
		},
		{
			name:  "replace is mode one",
			state: restoreFlagState{namespace: "payments", latest: true, pvcs: []string{"data-0"}, replace: true},
			want:  []string{"--replace", "mode 1"},
		},
		{
			name:  "include-jobs is mode one",
			state: restoreFlagState{namespace: "payments", latest: true, pvcs: []string{"data-0"}, includeJobs: true},
			want:  []string{"--include-jobs", "mode 1"},
		},
		{
			name:  "both sides of activation",
			state: restoreFlagState{activate: "abcdef01", keepRestored: true, keepDesired: true},
			want:  []string{"--keep-restored", "--keep-desired", "mutually exclusive"},
		},
		{
			name:  "keep-desired without activate",
			state: restoreFlagState{namespace: "payments", latest: true, keepDesired: true},
			want:  []string{"--keep-desired", "--activate"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.state.validate()
			if err == nil {
				t.Fatalf("%+v must be refused", c.state)
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}

	// The combinations that must be ACCEPTED, so a refusal that swallowed a
	// legitimate run fails here.
	ok := []restoreFlagState{
		{namespace: "payments", latest: true, replace: true, confirm: true},
		{namespace: "payments", from: "daily-1", acceptDataAge: true},
		{namespace: "payments", pvcs: []string{"data-0", "data-1"}, latest: true},
		{resume: "abcdef01"},
		{abort: "abcdef01"},
		{activate: "abcdef01", namespace: "payments", keepDesired: true},
		{activate: "abcdef01", keepRestored: true},
	}
	for _, state := range ok {
		if err := state.validate(); err != nil {
			t.Errorf("%+v must be accepted, got %v", state, err)
		}
	}
}

// TestBackupRestoreReachesTheTransport proves the command's validation is not
// its whole behaviour: past it, the run dials the server it was given and the
// refusal comes from the transport rather than from a stub.
func TestBackupRestoreReachesTheTransport(t *testing.T) {
	missingKey := filepath.Join(t.TempDir(), "no-such-key")
	err := runBackupRestore(t,
		"--cluster", "prod-1",
		"--namespace", "payments",
		"--latest",
		"--confirm",
		"--server", "10.0.1.10",
		"--ssh-key", missingKey,
		"--bundle-manifest", restoreCmdManifest(t))
	if err == nil {
		t.Fatal("a run whose SSH key does not exist cannot have connected to anything")
	}
	if !strings.Contains(err.Error(), missingKey) {
		t.Errorf("the run stopped somewhere other than the transport it needs: %v", err)
	}
}

// TestBackupRestoreRefusesAnIneligibleBackupByTheVolumeItIsMissing: the
// refusal has to name the piece, because "it is not eligible" is not something
// an operator can act on.
func TestBackupRestoreRefusesAnIneligibleBackupByTheVolumeItIsMissing(t *testing.T) {
	facts := backup.BackupFacts{
		Name:                 "daily-new",
		Phase:                "Completed",
		CaptureStartedAt:     atCmd(-2 * time.Hour),
		CompletedAt:          atCmd(-time.Hour),
		StorageLocation:      "default",
		StorageLocationPhase: "Available",
		Expected:             []backup.VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0"}},
		Missing:              []backup.VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0"}},
	}
	out, err := runRestoreWithFakes(t, []backup.BackupFacts{facts}, restoreFlagState{
		namespace: "payments",
		from:      "daily-new",
		confirm:   true,
	})
	if err == nil {
		t.Fatalf("an ineligible backup must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "payments/data-0") {
		t.Errorf("the refusal does not name the missing volume: %v", err)
	}
}

// TestBackupRestorePlanRefusesAnOverPolicyBackupWithoutAcceptance: the run
// prints the best available data and then refuses, naming the age, the policy
// and the flag that accepts it.
func TestBackupRestorePlanRefusesAnOverPolicyBackupWithoutAcceptance(t *testing.T) {
	facts := backup.BackupFacts{
		Name:                 "daily-old",
		Phase:                "Completed",
		CaptureStartedAt:     atCmd(-40 * time.Hour),
		CompletedAt:          atCmd(-39 * time.Hour),
		StorageLocation:      "default",
		StorageLocationPhase: "Available",
		ConsistencyMethod:    "uncoordinated-copy",
		Consistency:          "uncoordinated copy",
		Expected:             []backup.VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0"}},
	}
	out, err := runRestoreWithFakes(t, []backup.BackupFacts{facts}, restoreFlagState{
		namespace: "payments",
		from:      "daily-old",
		replace:   true,
		confirm:   true,
	})
	if err == nil {
		t.Fatalf("a backup older than the policy must need --accept-data-age:\n%s", out)
	}
	for _, want := range []string{"--accept-data-age", "40h0m0s", "26h0m0s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not state %q: %v", want, err)
		}
	}
	// The best available data is PRINTED before the refusal: an operator cannot
	// decide about an age they were never shown.
	for _, want := range []string{"daily-old", "40h0m0s", "complete:", "uncoordinated copy", "uid-data-0"} {
		if !strings.Contains(out, want) {
			t.Errorf("the printed plan does not state %q:\n%s", want, out)
		}
	}
}

// TestBackupRestorePlanPrintsTheBackupItChose: the happy path at the flag
// surface. It stops at the confirmation prompt on purpose — that is the last
// thing before the operation lock, and what this test is about is the plan the
// operator is shown.
func TestBackupRestorePlanPrintsTheBackupItChose(t *testing.T) {
	newer := backup.BackupFacts{
		Name:                 "daily-new",
		Phase:                "Completed",
		CaptureStartedAt:     atCmd(-3 * time.Hour),
		CompletedAt:          atCmd(-2 * time.Hour),
		StorageLocation:      "default",
		StorageLocationPhase: "Available",
		Expected:             []backup.VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0"}},
		Failed:               []backup.VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0", Detail: "PodVolumeBackup reached Failed"}},
	}
	older := backup.BackupFacts{
		Name:                 "daily-old",
		Phase:                "Completed",
		CaptureStartedAt:     atCmd(-5 * time.Hour),
		CompletedAt:          atCmd(-4 * time.Hour),
		StorageLocation:      "default",
		StorageLocationPhase: "Available",
		Expected:             []backup.VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0"}},
	}
	out, err := runRestoreWithFakesAndInput(t, []backup.BackupFacts{newer, older}, restoreFlagState{
		namespace: "payments",
		latest:    true,
		replace:   true,
	}, "")
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("a run with no confirmation must stop there, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "daily-old") {
		t.Errorf("the plan does not name the backup it chose:\n%s", out)
	}
	if !strings.Contains(out, "daily-new") || !strings.Contains(out, "PodVolumeBackup reached Failed") {
		t.Errorf("--latest passed over an ineligible backup without saying why:\n%s", out)
	}
	if !strings.Contains(out, "5h0m0s") {
		t.Errorf("the plan does not print the chosen backup's conservative data age:\n%s", out)
	}
}

// runBackupRestore drives the real command tree, so the flag surface and the
// cobra wiring are the ones an operator gets.
func runBackupRestore(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRootCommand()
	root.SetArgs(append([]string{"backup", "restore"}, args...))
	root.SetOut(io.Discard)
	return root.Execute()
}

func atCmd(offset time.Duration) string {
	return time.Now().UTC().Add(offset).Format(time.RFC3339)
}

// runRestoreWithFakes drives the command's own run with a fake eligibility
// source and a fake cluster, and returns what it printed.
func runRestoreWithFakes(t *testing.T, facts []backup.BackupFacts, state restoreFlagState) (string, error) {
	return runRestoreWithFakesAndInput(t, facts, state, "yes\n")
}

// runRestoreWithFakesAndInput is the same with the confirmation typed on stdin:
// an empty input is how a test stops a run at the prompt.
func runRestoreWithFakesAndInput(t *testing.T, facts []backup.BackupFacts, state restoreFlagState, input string) (string, error) {
	t.Helper()
	bundle, err := manifest.Load(restoreCmdManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	b := &backupRestore{
		out:   &out,
		in:    strings.NewReader(input),
		state: state,
		f:     state.options(),
	}
	b.f.Bundle = bundle
	b.f.Cluster = "prod-1"
	b.open = func(context.Context) (backup.RestoreDeps, io.Closer, error) {
		source := &stubBackups{facts: facts}
		return backup.RestoreDeps{
			Cluster: &stubCluster{},
			Backups: source,
		}, nil, nil
	}
	err = b.run(context.Background())
	return out.String(), err
}

// stubBackups is the eligibility source at the command's surface.
type stubBackups struct {
	facts []backup.BackupFacts
}

func (s *stubBackups) TerminalBackups(_ context.Context, _ string) ([]backup.BackupFacts, error) {
	return append([]backup.BackupFacts(nil), s.facts...), nil
}

func (s *stubBackups) NamedBackup(_ context.Context, _, name string) (backup.BackupFacts, error) {
	for _, facts := range s.facts {
		if facts.Name == name {
			return facts, nil
		}
	}
	return backup.BackupFacts{}, fmt.Errorf("%w: %s", backup.ErrNoBackup, name)
}

// stubCluster answers the reads a plan needs and refuses every change, so a
// test at the command's surface cannot silently depend on a part of the run it
// never set up.
type stubCluster struct{}

func (s *stubCluster) WithRunner(k3s.Runner) backup.RestoreCluster { return s }

func (s *stubCluster) Namespace(context.Context, string) (*backup.NamespaceState, error) {
	return &backup.NamespaceState{Name: "payments", UID: "uid-ns"}, nil
}

func (s *stubCluster) Claims(context.Context, string) ([]backup.VolumeRef, error) {
	return []backup.VolumeRef{{Namespace: "payments", Name: "data-0", UID: "uid-data-0"}}, nil
}

func (s *stubCluster) Workloads(context.Context, string) ([]backup.WorkloadState, error) {
	return nil, nil
}

func (s *stubCluster) Pods(context.Context, string) ([]backup.PodState, error) { return nil, nil }

func (s *stubCluster) CronJobs(context.Context, string) ([]backup.CronJobState, error) {
	return nil, nil
}

func (s *stubCluster) Applications(context.Context, string) ([]backup.ApplicationState, error) {
	return nil, nil
}

func (s *stubCluster) Jobs(context.Context, string) ([]backup.JobState, error) { return nil, nil }

func (s *stubCluster) ProjectHold(context.Context, string) (*backup.ProjectHold, error) {
	return nil, nil
}

func (s *stubCluster) OperatorChart(context.Context) (string, error) { return "", nil }

func (s *stubCluster) DrillRestore(context.Context) (*backup.DrillRestore, error) { return nil, nil }

func (s *stubCluster) VolumeRestores(context.Context, string) ([]backup.VolumeRestoreState, error) {
	return nil, nil
}

func (s *stubCluster) PodVolumeBackups(context.Context, string) ([]backup.BackupVolumeState, error) {
	return nil, nil
}

func (s *stubCluster) RestoreOutcome(context.Context, string) (*backup.RestoreOutcome, error) {
	return &backup.RestoreOutcome{Phase: "Completed"}, nil
}

func (s *stubCluster) ClaimBinding(context.Context, string, string) (*backup.ClaimBinding, error) {
	return nil, nil
}

func (s *stubCluster) NodeReady(context.Context, string) (bool, error) { return false, nil }

func (s *stubCluster) ControllerOwner(context.Context, string, string, string) (*backup.OwnerRef, error) {
	return nil, nil
}

func (s *stubCluster) AnnotateProject(context.Context, string, string, string) error {
	return errStubChanged
}

func (s *stubCluster) ClearProjectAnnotation(context.Context, string, string) error {
	return errStubChanged
}

func (s *stubCluster) ScaleWorkload(context.Context, string, string, string, int32) error {
	return errStubChanged
}

func (s *stubCluster) DeleteNamespace(context.Context, string) error { return errStubChanged }

func (s *stubCluster) DeleteClaim(context.Context, string, string) error { return errStubChanged }

func (s *stubCluster) SuspendCronJob(context.Context, string, string, bool) error {
	return errStubChanged
}

func (s *stubCluster) SuspendJob(context.Context, string, string, bool) error {
	return errStubChanged
}

func (s *stubCluster) CreateBackup(context.Context, string, []byte) error { return errStubChanged }

func (s *stubCluster) CreateRestore(context.Context, string, []byte) error { return errStubChanged }

func (s *stubCluster) Apply(context.Context, string, []byte) error { return errStubChanged }

func (s *stubCluster) Delete(context.Context, string) error { return errStubChanged }

var errStubChanged = fmt.Errorf("the test did not expect the command to change anything")
