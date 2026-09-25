package install_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/recoverykit"
	"kubenest.io/cli/pkg/sshx"
)

// recorder captures the wire events, which is what the console and the
// failure-injection gate actually read.
type recorder struct {
	events []install.Event
	err    error
}

func (r *recorder) Emit(_ context.Context, e install.Event) error {
	r.events = append(r.events, e)
	return r.err
}

func newSession(t *testing.T, rec *recorder) *install.Session {
	t.Helper()
	return sessionWithOpts(t, install.Options{
		Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server",
	}, rec)
}

// sessionWithOpts is newSession for a test that cares which install MODE it is
// building, because the plan and the resume identity differ between them.
func sessionWithOpts(t *testing.T, opts install.Options, rec *recorder) *install.Session {
	t.Helper()
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\nlimits:\n  timeouts:\n    install-total: 30m\n"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := install.OpenJournal(filepath.Join(t.TempDir(), "journal.json"), opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	return &install.Session{ID: "run-1", Opts: opts, Bundle: m, Jnl: j, Emit: rec, Out: io.Discard}
}

// The failed event and the failed journal entry must name the component that
// actually broke, not the one its stage lists first. platform-networking
// installs the Gateway API CRDs AND Traefik; platform-day2 installs
// system-upgrade-controller AND kured.
func TestFailedStageNamesTheActualComponent(t *testing.T) {
	cases := []struct {
		name     string
		stage    string
		declared string
		tagged   string
		wantJSON string
	}{
		{"gateway-api inside platform-networking", install.StageNetworking, "traefik", "gateway-api", "gateway-api"},
		{"kured inside platform-day2", install.StageDay2, "system-upgrade-controller", "kured", "kured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{}
			s := newSession(t, rec)
			table := []install.Stage{{
				Name:      c.stage,
				Component: c.declared,
				Run: func(ctx context.Context) error {
					return install.NewComponentError(c.tagged, errors.New("helm-install job never completed"))
				},
			}}

			_, err := install.Execute(context.Background(), s, table)
			if err == nil {
				t.Fatal("want a failure")
			}
			var stageErr *install.StageError
			if !errors.As(err, &stageErr) {
				t.Fatalf("want a *StageError, got %T", err)
			}
			if stageErr.Component != c.wantJSON {
				t.Errorf("StageError names %q, want %q", stageErr.Component, c.wantJSON)
			}

			last := rec.events[len(rec.events)-1]
			if last.Status != install.StatusFailed {
				t.Fatalf("last event is %s", last.Status)
			}
			if last.Component != c.wantJSON {
				t.Errorf("failed event names component %q, want %q — the record is what a failure-injection run matches against", last.Component, c.wantJSON)
			}

			body, readErr := os.ReadFile(s.Jnl.Path())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(body), `"component": "`+c.wantJSON+`"`) {
				t.Errorf("the journal's failed entry does not name %q:\n%s", c.wantJSON, body)
			}
		})
	}
}

// An untagged failure still names something true: the stage's own component.
func TestUntaggedFailureFallsBackToTheStageComponent(t *testing.T) {
	rec := &recorder{}
	s := newSession(t, rec)
	table := []install.Stage{{
		Name:      install.StageStorage,
		Component: "openebs-lvm-localpv",
		Run: func(ctx context.Context) error {
			return errors.New("volume group kubenest-vg not found")
		},
	}}
	if _, err := install.Execute(context.Background(), s, table); err == nil {
		t.Fatal("want a failure")
	}
	last := rec.events[len(rec.events)-1]
	if last.Component != "openebs-lvm-localpv" {
		t.Errorf("component = %q, want the stage's declared one", last.Component)
	}
}

// Every component name the plan can emit must be a key in the SHIPPED bundle
// manifest. The contract says component is the exact manifest core key, and a
// typo here is a wrong record discovered on a customer's cluster rather than
// a failing test.
func TestEveryPlanComponentIsAManifestKey(t *testing.T) {
	real := filepath.Join("..", "..", "..", "kubenest-contracts", "bundles", "platform-1.0.yaml")
	if _, err := os.Stat(real); err != nil {
		t.Skipf("contracts checkout not present: %v", err)
	}
	m, err := manifest.Load(real)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range install.Plan(newSession(t, &recorder{})) {
		if stage.Component == "" {
			continue
		}
		if _, err := m.Core.Version(stage.Component); err != nil {
			t.Errorf("stage %s declares component %q, which the shipped bundle does not pin: %v",
				stage.Name, stage.Component, err)
		}
	}
	// The names the plan tags failures with, which never appear in the stage
	// table but do reach the record.
	for _, key := range install.TaggedComponents() {
		if _, err := m.Core.Version(key); err != nil {
			t.Errorf("the plan can report component %q, which the shipped bundle does not pin: %v", key, err)
		}
	}
}

// The plan is the contract's stage list, in order, fully wired.
//
// A registered install runs fifteen stages, with register second: the cluster
// must exist in the control plane before anything is written to a machine. A
// --control-plane install runs sixteen, with the control-plane stage between
// platform-day2 and register — the control plane has to be up and logged in to
// before the cluster hosting it can be registered through it.
func TestPlanMatchesTheStageContract(t *testing.T) {
	registered := []string{
		install.StagePreflight,
		install.StageRegister,
		install.StageK3sServer,
		install.StageK3sAgents,
		install.StageNetworking,
		install.StageCerts,
		install.StageStorage,
		install.StageBackup,
		install.StageDay2,
		install.StageRecoveryKit,
		install.StageBackupTarget,
		install.StageAgent,
		install.StageProfiles,
		install.StageRecord,
		install.StageVerify,
	}
	if install.StageControlPlane == "" {
		t.Fatal("the control-plane stage has no name, so no journal or wire record can carry it")
	}
	if install.StageRecoveryKit == "" {
		t.Fatal("the recovery-kit stage has no name, so no journal or wire record can carry it")
	}
	if install.StageBackupTarget == "" {
		t.Fatal("the backup-target stage has no name, so no journal or wire record can carry it")
	}
	controlPlane := []string{
		install.StagePreflight,
		install.StageK3sServer,
		install.StageK3sAgents,
		install.StageNetworking,
		install.StageCerts,
		install.StageStorage,
		install.StageBackup,
		install.StageDay2,
		install.StageControlPlane,
		install.StageRegister,
		install.StageRecoveryKit,
		install.StageBackupTarget,
		install.StageAgent,
		install.StageProfiles,
		install.StageRecord,
		install.StageVerify,
	}
	if len(install.StageNames) != len(registered) {
		t.Fatalf("StageNames has %d stages, the registered install has %d", len(install.StageNames), len(registered))
	}
	for i, name := range registered {
		if install.StageNames[i] != name {
			t.Fatalf("StageNames[%d] is %q, the registered plan wants %q", i, install.StageNames[i], name)
		}
	}

	for _, tc := range []struct {
		name      string
		opts      install.Options
		want      []string
		alwaysRun map[string]bool
	}{
		{
			name: "registered",
			opts: install.Options{Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server"},
			want: registered,
			alwaysRun: map[string]bool{
				install.StagePreflight: true,
				install.StageRegister:  true,
				install.StageVerify:    true,
			},
		},
		{
			name: "control-plane",
			opts: install.Options{
				Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server",
				ControlPlaneInstall: true, Domain: "10-0-1-10.sslip.io", AdminEmail: "admin@10-0-1-10.sslip.io",
			},
			want: controlPlane,
			alwaysRun: map[string]bool{
				install.StagePreflight:    true,
				install.StageControlPlane: true,
				install.StageRegister:     true,
				install.StageVerify:       true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := install.Plan(sessionWithOpts(t, tc.opts, &recorder{}))
			if len(plan) != len(tc.want) {
				t.Fatalf("plan has %d stages, want %d", len(plan), len(tc.want))
			}
			for i, stage := range plan {
				if stage.Name != tc.want[i] {
					t.Errorf("stage %d is %q, want %q", i+1, stage.Name, tc.want[i])
				}
				if stage.Run == nil {
					t.Errorf("stage %s has no implementation", stage.Name)
				}
				if stage.AlwaysRun != tc.alwaysRun[stage.Name] {
					t.Errorf("stage %s AlwaysRun = %v, want %v", stage.Name, stage.AlwaysRun, tc.alwaysRun[stage.Name])
				}
			}
		})
	}
}

// A journal belongs to one install mode. Resuming a registered journal as a
// control-plane install (or the reverse) would skip the stages that make the
// mode true — a registered install's register already ran, a control-plane
// install's has not — so the identity has to carry which one this is.
func TestIdentityRefusesAResumeInTheOtherMode(t *testing.T) {
	base := install.Options{
		Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server",
		Domain: "10-0-1-10.sslip.io",
	}
	other := base
	other.ControlPlaneInstall = true

	registered, controlPlane := base.Identity(), other.Identity()
	if registered.Fields["mode"] == controlPlane.Fields["mode"] {
		t.Errorf("both modes identify as %q, so a resume cannot tell them apart", registered.Fields["mode"])
	}
	if diff := registered.Differences(controlPlane); len(diff) == 0 {
		t.Error("the two modes compare as the same install, so a resume in the other mode would be allowed")
	}
	if registered.Fields["domain"] != controlPlane.Fields["domain"] {
		t.Errorf("the domain is part of the request in both modes, so it must be compared in both: %q vs %q",
			registered.Fields["domain"], controlPlane.Fields["domain"])
	}
}

// fakeNodeRunner answers every command with an empty success. It exists so a
// test can put a server node in the session and drive a stage out of the plan
// without SSH: the stages under test here decide before any command's output
// matters.
type fakeNodeRunner struct{ commands []string }

func (f *fakeNodeRunner) Run(_ context.Context, command string) (sshx.Result, error) {
	f.commands = append(f.commands, command)
	return sshx.Result{}, nil
}

func (f *fakeNodeRunner) RunInput(_ context.Context, command string, _ io.Reader) (sshx.Result, error) {
	f.commands = append(f.commands, command)
	return sshx.Result{}, nil
}

// driveStage builds the session for one install mode, attaches a server node,
// finds the named stage in the plan the installer would actually run, and runs
// it. Driving the plan rather than calling a stage function is the point: what
// is being checked is the ORDER the plan produces.
func driveStage(t *testing.T, opts install.Options, stageName string) (*install.Session, error) {
	t.Helper()
	s := sessionWithOpts(t, opts, &recorder{})
	s.Nodes = []install.Node{{Address: "10.0.1.10", Role: install.RoleServer, Runner: &fakeNodeRunner{}}}
	for _, stage := range install.Plan(s) {
		if stage.Name == stageName {
			return s, stage.Run(context.Background())
		}
	}
	t.Fatalf("the plan has no %q stage, so nothing in the install does what that stage is for", stageName)
	return nil, nil
}

// No backup or checkpoint may be configurable before the recovery kit is
// written and verified.
//
// A bucket holding a cluster's backups while the only copy of the repository
// password that opens them is on a laptop that no longer exists is the failure
// this ordering removes, and it is invisible: every backup succeeds, the
// bucket fills, and the loss is discovered on the day someone needs it.
//
// The test does two things, because ordering by name alone would pass for a
// plan that had simply stopped configuring the target at all: it asserts where
// the stage that can produce an upload sits, and it DRIVES that stage out of
// the plan and requires it to refuse while no kit is in place.
func TestNoBackupConfigurationBeforeTheKitStage(t *testing.T) {
	modes := []struct {
		name string
		opts install.Options
	}{
		{"registered", install.Options{Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server"}},
		{"control-plane", install.Options{
			Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server",
			ControlPlaneInstall: true, Domain: "10-0-1-10.sslip.io", AdminEmail: "admin@10-0-1-10.sslip.io",
		}},
	}

	positions := func(t *testing.T, names []string) map[string]int {
		t.Helper()
		at := map[string]int{}
		for i, name := range names {
			at[name] = i
		}
		return at
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			plan := install.Plan(sessionWithOpts(t, mode.opts, &recorder{}))
			names := make([]string, 0, len(plan))
			for _, stage := range plan {
				names = append(names, stage.Name)
			}
			at := positions(t, names)

			kitAt, hasKit := at[install.StageRecoveryKit]
			if !hasKit {
				t.Fatalf("the plan has no %q stage: nothing writes the kit, so a backup can exist whose repository password is nowhere but a laptop",
					install.StageRecoveryKit)
			}
			targetAt, hasTarget := at[install.StageBackupTarget]
			if !hasTarget {
				t.Fatalf("the plan has no %q stage: either the target is configured somewhere unnamed, or it is never configured at all",
					install.StageBackupTarget)
			}
			if targetAt <= kitAt {
				t.Fatalf("%s is stage %d and %s is stage %d: the stage that first allows an upload is ordered at or before the one that writes the kit, so a backup can exist before the password that opens it is anywhere but this machine",
					install.StageBackupTarget, targetAt+1, install.StageRecoveryKit, kitAt+1)
			}

			// Velero itself may precede the kit, and must: installed and
			// unconfigured it produces nothing, and it is what CREATES the
			// per-cluster repository password the kit has to carry.
			veleroAt, hasVelero := at[install.StageBackup]
			if !hasVelero {
				t.Fatalf("the plan has no %q stage", install.StageBackup)
			}
			if veleroAt > kitAt {
				t.Errorf("%s is ordered after %s: the kit has to carry the repository password this stage creates",
					install.StageBackup, install.StageRecoveryKit)
			}
		})
	}

	// StageNames carries the same guarantee: it is this ordering's vocabulary
	// for the journal and the wire, so a plan that was reordered without it
	// would report stages in an order the install does not run.
	stageNames := positions(t, install.StageNames)
	kitAt, hasKit := stageNames[install.StageRecoveryKit]
	targetAt, hasTarget := stageNames[install.StageBackupTarget]
	if !hasKit || !hasTarget {
		t.Fatalf("StageNames does not name both %q and %q", install.StageRecoveryKit, install.StageBackupTarget)
	}
	if targetAt <= kitAt {
		t.Fatalf("StageNames puts %s at %d and %s at %d", install.StageBackupTarget, targetAt+1, install.StageRecoveryKit, kitAt+1)
	}

	// The runtime guard, driven from the plan itself: with a target that can
	// produce an upload and no kit in place, the stage must refuse.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBENEST_BACKUP_ACCESS_KEY_ID", "AKTEST")
	t.Setenv("KUBENEST_BACKUP_SECRET_ACCESS_KEY", "sekret")
	target := "s3://kubenest-backups/prod-1?endpoint=127.0.0.1:1&region=main"
	guarded := install.Options{
		Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server",
		BackupTarget: target,
	}
	_, err := driveStage(t, guarded, install.StageBackupTarget)
	if err == nil {
		t.Fatalf("%s configured a backup target with no recovery kit in place: an upload could then exist whose repository password is nowhere but this machine", install.StageBackupTarget)
	}
	if !strings.Contains(err.Error(), install.StageRecoveryKit) {
		t.Errorf("the refusal must name the stage that writes the kit first, got %v", err)
	}

	// The positive control: with a kit written locally, the stage gets past
	// the guard. Without this the check above would pass just as well against
	// a stage that refused everything, which would prove nothing about
	// ordering.
	s := sessionWithOpts(t, guarded, &recorder{})
	s.Jnl.ClusterID = "cluster-1"
	kit, err := recoverykit.New(
		recoverykit.Binding{Kind: recoverykit.KindCluster, InstanceID: "inst-1", OrganisationID: "org-1", ClusterID: "cluster-1"},
		"20260925T100000Z-abcdef", recoverykit.Location{Endpoint: "127.0.0.1:1", Bucket: "kubenest-backups", Region: "main", Prefix: "prod-1"},
		"velero-default-kopia",
		map[string]string{recoverykit.KeyVeleroRepoPassword: "repo-pw", recoverykit.KeyK3sJoinToken: "K10aaa::server:bbb"},
		time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := recoverykit.GenerateFleetKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.SealTo(fleet.Recipient()); err != nil {
		t.Fatal(err)
	}
	doc, err := kit.Document()
	if err != nil {
		t.Fatal(err)
	}
	path, err := recoverykit.LocalPath("cluster-1", recoverykit.KindCluster, kit.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}

	s.Nodes = []install.Node{{Address: "10.0.1.10", Role: install.RoleServer, Runner: &fakeNodeRunner{}}}
	for _, stage := range install.Plan(s) {
		if stage.Name != install.StageBackupTarget {
			continue
		}
		passed := stage.Run(context.Background())
		if passed != nil && strings.Contains(passed.Error(), "refusing to configure a backup target") {
			t.Errorf("the guard refused even with a kit written locally, so it is not a guard but a wall: %v", passed)
		}
	}

	// And the kit stage itself must be a no-op, not a failure, when there is
	// no target: a cluster with no bucket has no kit to write and no backup to
	// open.
	untargeted := install.Options{Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server"}
	if _, err := driveStage(t, untargeted, install.StageRecoveryKit); err != nil {
		t.Errorf("with no --backup-target there is no S3 location for a kit and no backup to open it; the stage must say so and continue, got %v", err)
	}
}
