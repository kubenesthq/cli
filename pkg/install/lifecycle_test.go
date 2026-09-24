package install_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
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
// A registered install runs install.mdx's thirteen, with register second: the
// cluster must exist in the control plane before anything is written to a
// machine. A --control-plane install runs fourteen, with the control-plane
// stage between platform-day2 and register — the control plane has to be up
// and logged in to before the cluster hosting it can be registered through it.
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
		install.StageAgent,
		install.StageProfiles,
		install.StageRecord,
		install.StageVerify,
	}
	if install.StageControlPlane == "" {
		t.Fatal("the control-plane stage has no name, so no journal or wire record can carry it")
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
