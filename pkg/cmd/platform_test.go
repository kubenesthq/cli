package cmd

import (
	"strings"
	"testing"
)

func TestInstallFlagsValidate(t *testing.T) {
	valid := func() InstallFlags {
		return InstallFlags{
			Bundle:  "1.0",
			Name:    "prod-1",
			Servers: []string{"10.0.1.10"},
			HATier:  "single-server",
		}
	}

	if err := (&InstallFlags{}).Validate(); err == nil {
		t.Error("empty flags must not validate")
	}
	f := valid()
	if err := f.Validate(); err != nil {
		t.Errorf("valid single-server flags rejected: %v", err)
	}

	f = valid()
	f.HATier = "medium"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "single-server") {
		t.Errorf("unknown HA tier must be rejected naming the real tiers, got %v", err)
	}

	f = valid()
	f.HATier = "ha"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "three") {
		t.Errorf("ha with one server must demand three control-plane nodes, got %v", err)
	}
	f.Servers = []string{"a", "b", "c"}
	if err := f.Validate(); err != nil {
		t.Errorf("ha with three servers rejected: %v", err)
	}

	f = valid()
	f.Servers = []string{"a", "b"}
	if err := f.Validate(); err == nil {
		t.Error("single-server with two servers must be rejected")
	}
}

// Every platform command is implemented now. What is asserted here is that
// each still refuses loudly rather than pretending: a command that cannot do
// its job must exit non-zero and say why.
func TestUpgradeRefusesWithoutItsRequiredFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"platform", "upgrade"}, "--cluster is required"},
		{[]string{"platform", "upgrade", "--cluster", "prod-1"}, "--to is required"},
		{[]string{"platform", "rollback"}, "--cluster is required"},
		{[]string{"platform", "diff", "--from", "1.0"}, "--from and --to"},
		{[]string{"cluster", "set-window"}, "--cluster is required"},
	} {
		root := NewRootCommand()
		root.SetArgs(c.args)
		err := root.Execute()
		if err == nil {
			t.Fatalf("%v must refuse", c.args)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: want %q, got: %v", c.args, c.want, err)
		}
	}
}

// Install is implemented, and its first refusal is the one the page leads
// with: a control plane is required. The cluster is registered before
// anything is written to a machine, so an install with nowhere to register
// stops before it touches a host.
func TestInstallRequiresAControlPlane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "install",
		"--bundle", "1.0", "--name", "x", "--server", "10.0.0.1", "--ha", "single-server"})
	err := root.Execute()
	if err == nil {
		t.Fatal("install without a control plane must refuse")
	}
	if !strings.Contains(err.Error(), "kubenest login") {
		t.Errorf("the refusal must name the fix, got: %v", err)
	}
}

func TestUninstallDemandsConfirm(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "uninstall"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("uninstall without --confirm must refuse, got: %v", err)
	}
}
