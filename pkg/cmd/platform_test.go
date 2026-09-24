package cmd

import (
	"bytes"
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

// The help example is a first action a reader can copy. It must show the
// command that builds a first cluster — the one that also installs the
// control plane — and the command that adds a later cluster to it. It must
// also not offer a component profile the current CLI cannot install.
func TestPlatformInstallHelpShowsTheFirstClusterPath(t *testing.T) {
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"platform", "install", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("platform install help: %v", err)
	}

	help := output.String()
	for _, want := range []string{
		// The first cluster: the control plane comes with it.
		// The bundle version on the next line is deliberately not asserted: it
		// moves with every release, and pinning it here only breaks this test
		// again on the next one.
		"kubenest platform install \\\n    --control-plane \\\n    --bundle",
		// A later cluster: registered with the control plane already logged in to,
		// so it names a bundle and no control plane.
		"kubenest platform install \\\n    --bundle",
		"--admin-email",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("install help does not show %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "--profile observability") {
		t.Errorf("install help advertises an unbuilt profile:\n%s", help)
	}
}

// The install help lists every flag the command accepts, so the flag surface
// can be read off the text an operator reads. Asserted both ways round: a
// name that came back — a removed switch, or a second spelling of one — shows
// up here as a name the surface does not have, and a name that disappeared
// shows up as a missing one.
func TestPlatformInstallHelpListsExactlyItsFlagSurface(t *testing.T) {
	root := NewRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"platform", "install", "--help"})

	if err := root.Execute(); err != nil {
		t.Fatalf("platform install help: %v", err)
	}

	want := map[string]bool{
		"admin-email": true, "agent": true, "backup-target": true, "bundle": true,
		"control-plane": true, "domain": true, "ha": true, "help": true, "name": true,
		"org": true, "profile": true, "server": true, "ssh-key": true, "ssh-user": true,
		"storage-device": true,
	}
	got := map[string]bool{}
	inFlags := false
	for _, line := range strings.Split(output.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "Flags:":
			inFlags = true
			continue
		case !inFlags, trimmed == "":
			continue
		case !strings.HasPrefix(trimmed, "-"):
			inFlags = false
			continue
		}
		for _, field := range strings.Fields(line) {
			field = strings.TrimSuffix(field, ",")
			if strings.HasPrefix(field, "--") {
				got[strings.TrimPrefix(field, "--")] = true
				break
			}
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("install help does not list --%s", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("install help lists --%s, which the install surface does not have", name)
		}
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
// it stops before it touches a host. The refusal has to name both ways on: a
// first cluster installs the control plane, and any other cluster is added
// against a login.
func TestInstallWithNowhereToRegisterNamesBothWaysOn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "install",
		"--bundle", "1.0", "--name", "x", "--server", "10.0.0.1", "--ha", "single-server"})
	err := root.Execute()
	if err == nil {
		t.Fatal("install with no control plane configured and no --control-plane must refuse")
	}
	if !strings.Contains(err.Error(), "kubenest login") {
		t.Errorf("the refusal must name the fix, got: %v", err)
	}
	if !strings.Contains(err.Error(), "kubenest platform install --control-plane") {
		t.Errorf("the refusal does not offer the other way on, got: %v", err)
	}
}

// The control plane being installed has exactly one organization, so --org
// has nothing to choose between. Two instructions that contradict each other
// are answered by asking, not by picking one.
func TestControlPlaneAndOrgAreMutuallyExclusive(t *testing.T) {
	f := InstallFlags{
		Bundle: "1.0", Name: "prod-1", Servers: []string{"10.0.1.10"},
		HATier: "single-server", ControlPlane: true, Org: "acme",
	}
	err := f.Validate()
	if err == nil {
		t.Fatal("--control-plane with --org must be refused: that control plane has one organization")
	}
	if !strings.Contains(err.Error(), "--org") || !strings.Contains(err.Error(), "--control-plane") {
		t.Errorf("the refusal does not name both flags: %v", err)
	}
}

// --domain and --admin-email describe the control plane's own serving address
// and its administrator. Neither means anything on a cluster being added to a
// fleet, and silently ignoring them would leave the operator believing they
// had configured something.
func TestControlPlaneOnlyFlagsNeedControlPlane(t *testing.T) {
	base := func() InstallFlags {
		return InstallFlags{
			Bundle: "1.0", Name: "prod-2", Servers: []string{"10.0.1.10"},
			HATier: "single-server",
		}
	}
	f := base()
	f.Domain = "kubenest.example.com"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "--domain") {
		t.Errorf("--domain without --control-plane must be refused, got %v", err)
	}
	f = base()
	f.AdminEmail = "ops@example.com"
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "--admin-email") {
		t.Errorf("--admin-email without --control-plane must be refused, got %v", err)
	}
}

// The generated domain is what the console, the API and the hub are served
// under, and sslip.io answers <address>.sslip.io with that address: a first
// cluster therefore needs no DNS arranged before it can serve its console.
func TestControlPlaneDomainDefaultsToTheFirstServer(t *testing.T) {
	f := InstallFlags{Servers: []string{"10.0.1.10", "10.0.1.11"}}
	if got := f.controlPlaneDomain(); got != "10.0.1.10.sslip.io" {
		t.Errorf("default domain = %q, want the first server's address under sslip.io", got)
	}
	if got := f.controlPlaneAdminEmail(f.controlPlaneDomain()); got != "admin@10.0.1.10.sslip.io" {
		t.Errorf("default admin = %q", got)
	}

	f.Domain = "kubenest.example.com"
	if got := f.controlPlaneDomain(); got != "kubenest.example.com" {
		t.Errorf("--domain must win over the derived name, got %q", got)
	}
	f.AdminEmail = "ops@example.com"
	if got := f.controlPlaneAdminEmail(f.Domain); got != "ops@example.com" {
		t.Errorf("--admin-email must win over the derived account, got %q", got)
	}
}

// The load-bearing property of --control-plane at the command surface: the
// bundle manifest comes from this binary, not from a control plane. Asking
// for a version the binary does not carry must fail on THAT, having never
// looked for a control plane or a login.
func TestControlPlaneReadsTheBundleFromTheBinaryNotAControlPlane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := NewRootCommand()
	root.SetArgs([]string{"platform", "install", "--control-plane",
		"--bundle", "99.9", "--name", "x", "--server", "10.0.0.1", "--ha", "single-server"})
	err := root.Execute()
	if err == nil {
		t.Fatal("a bundle this CLI does not carry must be refused")
	}
	if strings.Contains(err.Error(), "kubenest login") || strings.Contains(err.Error(), "no control plane configured") {
		t.Errorf("a --control-plane install went looking for a control plane to log in to: %v", err)
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
