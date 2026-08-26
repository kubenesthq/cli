package preflight_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/preflight"
)

// standaloneOptions is a healthy host with no control plane: the bundles come
// from the catalog the CLI carries.
func standaloneOptions(t *testing.T) preflight.Options {
	t.Helper()
	opts := baseOptions(t, healthyHost(nil))
	opts.Standalone = true
	return opts
}

// A standalone install has no control plane, so the control-plane check must
// not claim one is reachable — and must not fail either, because nothing is
// wrong. It reports what is true.
func TestStandalonePreflightDoesNotReportOnAControlPlaneThatDoesNotExist(t *testing.T) {
	rep, err := preflight.Run(context.Background(), standaloneOptions(t))
	if err != nil {
		t.Fatalf("a healthy host with no control plane must pass preflight: %v", err)
	}
	res, ok := outcomeOf(rep, preflight.CheckControlPlane)
	if !ok {
		t.Fatal("the control-plane check did not run at all, so its absence is invisible in the report")
	}
	if res.Outcome != preflight.Pass {
		t.Errorf("standalone control-plane check is %q: %s", res.Outcome, res.Detail)
	}
	if strings.Contains(res.Detail, "reachable,") {
		t.Errorf("the check claims a control plane was reached: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "standalone") {
		t.Errorf("the check does not say why it did not check anything: %q", res.Detail)
	}
}

// The bundle check is the SAME check in both modes. An unregistered cluster
// still may not be handed a tier or a profile its bundle does not offer —
// that would be discovered at the stage that tried to install it, on a
// customer's host.
func TestStandalonePreflightStillChecksTheRequestAgainstTheBundle(t *testing.T) {
	t.Run("unknown version names what the CLI carries, not what a control plane offers", func(t *testing.T) {
		opts := standaloneOptions(t)
		opts.BundleVersion = "9.9"
		_, err := preflight.Run(context.Background(), opts)
		if err == nil {
			t.Fatal("a bundle this CLI does not carry was accepted")
		}
		if !strings.Contains(err.Error(), "does not carry bundle") {
			t.Errorf("the refusal blames the wrong thing: %v", err)
		}
	})

	t.Run("tier the bundle does not offer", func(t *testing.T) {
		opts := standaloneOptions(t)
		opts.HATier = "ha"
		opts.Catalog = fakeCatalog{entries: []preflight.BundleEntry{{Version: "1.0", HATiers: []string{"single-server"}}}}
		_, err := preflight.Run(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "does not offer the \"ha\" tier") {
			t.Fatalf("want a tier refusal, got %v", err)
		}
	})

	t.Run("unknown profile", func(t *testing.T) {
		opts := standaloneOptions(t)
		opts.Profiles = []string{"obervability"}
		_, err := preflight.Run(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "profile named") {
			t.Fatalf("an unknown profile must be rejected in standalone too, got %v", err)
		}
	})
}

// A binary with no catalog cannot check the request at all. That is a broken
// build, not a login problem, and the fix it names has to say so.
func TestStandaloneWithNoCatalogBlamesTheBinaryNotTheLogin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog preflight.Catalog
	}{
		{"no catalog", nil},
		{"unreadable catalog", fakeCatalog{err: errors.New("embedded manifest is not valid")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := standaloneOptions(t)
			opts.Catalog = tc.catalog
			rep, err := preflight.Run(context.Background(), opts)
			if err == nil {
				t.Fatal("an install with no bundle catalog cannot be checked and must be refused")
			}
			res, ok := outcomeOf(rep, preflight.CheckBundle)
			if !ok || res.Outcome != preflight.Fail {
				t.Fatalf("the bundle check did not carry the failure: %+v", rep.Results)
			}
			if strings.Contains(res.Fix, "kubenest login") {
				t.Errorf("a standalone install was told to log in: %q", res.Fix)
			}
			if !strings.Contains(res.Fix, "CLI") {
				t.Errorf("the fix does not name the binary: %q", res.Fix)
			}
		})
	}
}

// The registered path's refusal must now offer both ways forward, because
// there are two.
func TestNoControlPlaneNamesStandaloneAsAWayOn(t *testing.T) {
	opts := baseOptions(t, healthyHost(nil))
	opts.Catalog = nil
	rep, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("a registered install with no control plane must be refused")
	}
	res, _ := outcomeOf(rep, preflight.CheckControlPlane)
	if !strings.Contains(res.Fix, "kubenest login") || !strings.Contains(res.Fix, "--standalone") {
		t.Errorf("the fix names only one of the two ways on: %q", res.Fix)
	}
}
