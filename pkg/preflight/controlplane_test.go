package preflight_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/preflight"
)

// controlPlaneInstallOptions is a healthy host for `platform install
// --control-plane`: the control plane does not exist yet, and the bundles come
// from the catalog the CLI carries.
func controlPlaneInstallOptions(t *testing.T) preflight.Options {
	t.Helper()
	opts := baseOptions(t, healthyHost(nil))
	opts.ControlPlaneInstall = true
	return opts
}

// The install is about to CREATE the control plane, so the control-plane check
// must not claim one is reachable — and must not fail either, because nothing
// is wrong. It reports what is true.
func TestControlPlaneInstallDoesNotReportOnAControlPlaneItIsCreating(t *testing.T) {
	rep, err := preflight.Run(context.Background(), controlPlaneInstallOptions(t))
	if err != nil {
		t.Fatalf("a healthy host that will host the control plane must pass preflight: %v", err)
	}
	res, ok := outcomeOf(rep, preflight.CheckControlPlane)
	if !ok {
		t.Fatal("the control-plane check did not run at all, so its absence is invisible in the report")
	}
	if res.Outcome != preflight.Pass {
		t.Errorf("control-plane-install check is %q: %s", res.Outcome, res.Detail)
	}
	if strings.Contains(res.Detail, "reachable,") {
		t.Errorf("the check claims a control plane was reached: %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "creates the control plane") {
		t.Errorf("the check does not say why it did not check anything: %q", res.Detail)
	}
}

// The bundle check is the SAME check in both modes. A cluster that is about to
// host its own control plane still may not be handed a tier or a profile its
// bundle does not offer — that would be discovered at the stage that tried to
// install it, on a customer's host.
func TestControlPlaneInstallStillChecksTheRequestAgainstTheBundle(t *testing.T) {
	t.Run("unknown version names what the CLI carries, not what a control plane offers", func(t *testing.T) {
		opts := controlPlaneInstallOptions(t)
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
		opts := controlPlaneInstallOptions(t)
		opts.HATier = "ha"
		opts.Catalog = fakeCatalog{entries: []preflight.BundleEntry{{Version: "1.0", HATiers: []string{"single-server"}}}}
		_, err := preflight.Run(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "does not offer the \"ha\" tier") {
			t.Fatalf("want a tier refusal, got %v", err)
		}
	})

	t.Run("unknown profile", func(t *testing.T) {
		opts := controlPlaneInstallOptions(t)
		opts.Profiles = []string{"obervability"}
		_, err := preflight.Run(context.Background(), opts)
		if err == nil || !strings.Contains(err.Error(), "profile named") {
			t.Fatalf("an unknown profile must be rejected here too, got %v", err)
		}
	})
}

// A binary with no catalog cannot check the request at all. That is a broken
// build, not a login problem, and the fix it names has to say so.
func TestControlPlaneInstallWithNoCatalogBlamesTheBinaryNotTheLogin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog preflight.Catalog
	}{
		{"no catalog", nil},
		{"unreadable catalog", fakeCatalog{err: errors.New("embedded manifest is not valid")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := controlPlaneInstallOptions(t)
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
				t.Errorf("an install that creates its own control plane was told to log in: %q", res.Fix)
			}
			if !strings.Contains(res.Fix, "CLI") {
				t.Errorf("the fix does not name the binary: %q", res.Fix)
			}
		})
	}
}

// The ordinary install's refusal must name both ways on, because there are
// two: log in to a control plane that already exists, or create one here.
func TestNoControlPlaneNamesControlPlaneInstallAsAWayOn(t *testing.T) {
	opts := baseOptions(t, healthyHost(nil))
	opts.Catalog = nil
	rep, err := preflight.Run(context.Background(), opts)
	if err == nil {
		t.Fatal("an install that registers with a control plane must refuse when none is configured")
	}
	res, _ := outcomeOf(rep, preflight.CheckControlPlane)
	if !strings.Contains(res.Fix, "kubenest login") || !strings.Contains(res.Fix, "--control-plane") {
		t.Errorf("the fix names only one of the two ways on: %q", res.Fix)
	}
}
