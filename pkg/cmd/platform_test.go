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

	f = valid()
	f.Standalone = true
	if err := f.Validate(); err != nil {
		t.Errorf("a standalone install needs no control-plane flags and must validate: %v", err)
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

// An install that is going to register has to have somewhere to register, and
// it stops before it touches a host. Since kn-l827 there are two ways on from
// here rather than one, and the refusal has to name both — a customer who
// wanted a standalone cluster should not have to find that flag elsewhere.
func TestInstallWithNowhereToRegisterNamesBothWaysOn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "install",
		"--bundle", "1.0", "--name", "x", "--server", "10.0.0.1", "--ha", "single-server"})
	err := root.Execute()
	if err == nil {
		t.Fatal("install without a control plane and without --standalone must refuse")
	}
	if !strings.Contains(err.Error(), "kubenest login") {
		t.Errorf("the refusal must name the fix, got: %v", err)
	}
	if !strings.Contains(err.Error(), "--standalone") {
		t.Errorf("the refusal does not offer the other way on, got: %v", err)
	}
}

// --standalone must not silently coexist with a flag that only means anything
// when registering. Two instructions that contradict each other are answered
// by asking, not by picking one.
func TestStandaloneAndOrgAreMutuallyExclusive(t *testing.T) {
	f := InstallFlags{
		Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"},
		HATier: "single-server", Standalone: true, Org: "acme",
	}
	err := f.Validate()
	if err == nil {
		t.Fatal("--standalone with --org must be refused: there is no organization to register under")
	}
	if !strings.Contains(err.Error(), "--org") || !strings.Contains(err.Error(), "--standalone") {
		t.Errorf("the refusal does not name both flags: %v", err)
	}
}

// The load-bearing property of standalone mode at the command surface: the
// bundle manifest comes from this binary, not from a control plane. Asking
// for a version the binary does not carry must fail on THAT, having never
// looked for a control plane or a login.
func TestStandaloneReadsTheBundleFromTheBinaryNotAControlPlane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "install", "--standalone",
		"--bundle", "99.9", "--name", "x", "--server", "10.0.0.1", "--ha", "single-server"})
	err := root.Execute()
	if err == nil {
		t.Fatal("a bundle this CLI does not carry must be refused")
	}
	if strings.Contains(err.Error(), "kubenest login") || strings.Contains(err.Error(), "control plane") {
		t.Errorf("a standalone install went looking for a control plane: %v", err)
	}
	if !strings.Contains(err.Error(), "does not carry bundle") {
		t.Errorf("the refusal blames the wrong thing: %v", err)
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
