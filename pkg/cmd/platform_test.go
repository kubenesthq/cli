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

func TestSkeletonCommandsFailLoudly(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "install",
		"--bundle", "1.0", "--name", "x", "--server", "10.0.0.1", "--ha", "single-server"})
	err := root.Execute()
	if err == nil {
		t.Fatal("a skeleton command must exit non-zero, not pretend success")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("skeleton error should say the command is not available yet, got: %v", err)
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
