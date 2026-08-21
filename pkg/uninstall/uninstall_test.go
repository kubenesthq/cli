package uninstall_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/uninstall"
)

func node(t *testing.T, role uninstall.Role, respond func(string) (sshx.Result, error)) (uninstall.Node, *componenttest.FakeRunner) {
	t.Helper()
	fake := &componenttest.FakeRunner{Respond: respond}
	return uninstall.Node{Address: "10.0.1.10", Role: role, Runner: fake}, fake
}

func withVolumes(names ...string) func(string) (sshx.Result, error) {
	return func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "lvs --noheadings") {
			return sshx.Result{Stdout: "  " + strings.Join(names, "\n  ") + "\n"}, nil
		}
		return sshx.Result{}, nil
	}
}

func ranAny(cmds []string, substr string) bool {
	for _, c := range cmds {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// The default: k3s and everything the installer placed goes, data stays.
func TestDefaultRemovesK3sAndKeepsData(t *testing.T) {
	n, fake := node(t, uninstall.RoleServer, withVolumes("pvc-abc", "pvc-def"))
	var out bytes.Buffer
	if err := uninstall.Run(context.Background(), uninstall.Options{
		Nodes: []uninstall.Node{n}, Out: &out,
	}); err != nil {
		t.Fatal(err)
	}
	cmds := fake.Commands()
	if !ranAny(cmds, "k3s-uninstall.sh") {
		t.Error("k3s was not removed")
	}
	if ranAny(cmds, "lvremove") || ranAny(cmds, "vgremove") {
		t.Fatalf("uninstall destroyed data without --destroy-data:\n%s", strings.Join(cmds, "\n"))
	}
	if !strings.Contains(out.String(), "--destroy-data") {
		t.Errorf("the operator must be told the volumes were kept and how to remove them:\n%s", out.String())
	}
}

// --destroy-data removes the volumes, and removes the volume group only when
// the installer created it.
func TestDestroyDataRemovesVolumesAndAnInstallerCreatedGroup(t *testing.T) {
	n, fake := node(t, uninstall.RoleServer, withVolumes("pvc-abc"))
	if err := uninstall.Run(context.Background(), uninstall.Options{
		Nodes:       []uninstall.Node{n},
		DestroyData: true,
		Ownership:   storage.InstallerCreated,
		Device:      "/dev/disk/by-id/scsi-0HC_Volume_123",
	}); err != nil {
		t.Fatal(err)
	}
	cmds := fake.Commands()
	for _, want := range []string{"lvremove -y kubenest-vg/pvc-abc", "vgremove -y kubenest-vg", "pvremove -y /dev/disk/by-id/scsi-0HC_Volume_123"} {
		if !ranAny(cmds, want) {
			t.Errorf("missing %q:\n%s", want, strings.Join(cmds, "\n"))
		}
	}
}

// THE rule that protects a customer's disk: a volume group they created is
// never removed, even with --destroy-data.
func TestCustomerCreatedVolumeGroupIsNeverRemoved(t *testing.T) {
	n, fake := node(t, uninstall.RoleServer, withVolumes("pvc-abc"))
	var out bytes.Buffer
	if err := uninstall.Run(context.Background(), uninstall.Options{
		Nodes:       []uninstall.Node{n},
		DestroyData: true,
		Ownership:   storage.CustomerCreated,
		Out:         &out,
	}); err != nil {
		t.Fatal(err)
	}
	cmds := fake.Commands()
	if !ranAny(cmds, "lvremove -y kubenest-vg/pvc-abc") {
		t.Error("--destroy-data must still remove the platform's volumes")
	}
	if ranAny(cmds, "vgremove") || ranAny(cmds, "pvremove") {
		t.Fatalf("removed a volume group the customer created:\n%s", strings.Join(cmds, "\n"))
	}
	if !strings.Contains(out.String(), "you created it") {
		t.Errorf("the operator must be told why it was left:\n%s", out.String())
	}
}

// "Do not know" must never resolve to "destroy".
func TestUnknownOwnershipIsTreatedAsTheCustomers(t *testing.T) {
	n, fake := node(t, uninstall.RoleServer, withVolumes("pvc-abc"))
	var out bytes.Buffer
	if err := uninstall.Run(context.Background(), uninstall.Options{
		Nodes: []uninstall.Node{n}, DestroyData: true, Out: &out,
	}); err != nil {
		t.Fatal(err)
	}
	if ranAny(fake.Commands(), "vgremove") {
		t.Fatal("removed a volume group with no record of who created it")
	}
	if !strings.Contains(out.String(), "no record") {
		t.Errorf("the operator must be told the ownership was unknown:\n%s", out.String())
	}
}

// Only the platform's own volumes are removed. A customer may keep their own
// logical volumes in a volume group they created, and --destroy-data is not
// permission to remove those.
func TestOnlyPlatformVolumesAreRemoved(t *testing.T) {
	n, fake := node(t, uninstall.RoleServer, withVolumes("pvc-abc", "customer-database", "backups"))
	if err := uninstall.Run(context.Background(), uninstall.Options{
		Nodes: []uninstall.Node{n}, DestroyData: true, Ownership: storage.CustomerCreated,
	}); err != nil {
		t.Fatal(err)
	}
	cmds := fake.Commands()
	for _, keep := range []string{"customer-database", "backups"} {
		if ranAny(cmds, keep) {
			t.Errorf("removed a logical volume the platform did not create: %s", keep)
		}
	}
}

// Uninstall has to be re-runnable, and has to work on a node where the
// install failed before k3s was ever placed.
func TestACleanNodeIsNotAnError(t *testing.T) {
	n, fake := node(t, uninstall.RoleAgent, func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "k3s-agent-uninstall.sh") {
			return sshx.Result{Stdout: "no k3s installation\n"}, nil
		}
		return sshx.Result{}, nil
	})
	if err := uninstall.Run(context.Background(), uninstall.Options{Nodes: []uninstall.Node{n}}); err != nil {
		t.Fatalf("a node with nothing on it must uninstall cleanly: %v", err)
	}
	if !ranAny(fake.Commands(), "k3s-agent-uninstall.sh") {
		t.Error("an agent must use the agent uninstall script")
	}
}

// One failing node does not abandon the rest, and the report names it.
func TestOneBadNodeDoesNotStopTheOthers(t *testing.T) {
	bad, _ := node(t, uninstall.RoleServer, func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "k3s-uninstall.sh") {
			return sshx.Result{ExitCode: 1, Stderr: "systemd: failed to stop k3s.service"}, nil
		}
		return sshx.Result{}, nil
	})
	bad.Address = "10.0.1.99"
	good, goodFake := node(t, uninstall.RoleAgent, withVolumes())

	err := uninstall.Run(context.Background(), uninstall.Options{Nodes: []uninstall.Node{bad, good}})
	if err == nil {
		t.Fatal("want an error naming the node that resisted")
	}
	if !strings.Contains(err.Error(), "10.0.1.99") {
		t.Errorf("the error must name the node:\n%v", err)
	}
	if !ranAny(goodFake.Commands(), "k3s-agent-uninstall.sh") {
		t.Error("the healthy node must still have been cleaned")
	}
}
