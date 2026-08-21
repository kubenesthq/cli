package install_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
)

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
				Run: func(ctx context.Context, s *install.Session) error {
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

			body, readErr := os.ReadFile(s.Journal.Path())
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
		Run: func(ctx context.Context, s *install.Session) error {
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
	for _, stage := range install.Plan() {
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
func TestPlanMatchesTheStageContract(t *testing.T) {
	plan := install.Plan()
	if len(plan) != len(install.StageNames) {
		t.Fatalf("plan has %d stages, want %d", len(plan), len(install.StageNames))
	}
	alwaysRun := map[string]bool{
		install.StagePreflight: true,
		install.StageRegister:  true,
		install.StageVerify:    true,
	}
	for i, stage := range plan {
		if stage.Name != install.StageNames[i] {
			t.Errorf("stage %d is %q, want %q", i+1, stage.Name, install.StageNames[i])
		}
		if stage.Run == nil {
			t.Errorf("stage %s has no implementation", stage.Name)
		}
		if stage.AlwaysRun != alwaysRun[stage.Name] {
			t.Errorf("stage %s AlwaysRun = %v, want %v", stage.Name, stage.AlwaysRun, alwaysRun[stage.Name])
		}
	}
}
