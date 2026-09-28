package install_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/uninstall"
)

// The install the flag exists for: three hosts that each have their own data
// volume, so the stable by-id path — which contains the volume's serial — is
// different on every one of them.
const (
	nodeA = "10.0.3.10"
	nodeB = "10.0.3.11"
	nodeC = "10.0.3.12"

	volumeA = "/dev/disk/by-id/scsi-0HC_Volume_101"
	volumeB = "/dev/disk/by-id/scsi-0HC_Volume_102"
	volumeC = "/dev/disk/by-id/scsi-0HC_Volume_103"
)

// volumeGroupNode is one fake connection: it answers the volume-group probe
// the way a host with no kubenest-vg answers it, so the storage stage goes on
// to create the volume group on the device it was given for that host. Every
// command is recorded, which is how a test sees which device each node's
// runner was asked to use.
func volumeGroupNode(device string) *componenttest.FakeRunner {
	return &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch command {
		case "sudo -n vgs " + storage.VolumeGroup + " --noheadings -o vg_free --units b --nosuffix":
			return sshx.Result{ExitCode: 5, Stderr: "Volume group \"" + storage.VolumeGroup + "\" not found\n"}, nil
		case "test -b " + device:
			return sshx.Result{}, nil
		case "sudo -n blkid -p -o export " + device:
			// Verified on a real host (kn-bkwa): blank means empty output, exit 2.
			return sshx.Result{ExitCode: 2}, nil
		case "sudo -n pvcreate " + device, "sudo -n vgcreate " + storage.VolumeGroup + " " + device:
			return sshx.Result{}, nil
		}
		return sshx.Result{ExitCode: 127, Stderr: "unexpected command: " + command}, nil
	}}
}

func threeNodeInstall(opts install.Options) install.Options {
	opts.Bundle = "1.0"
	opts.Name = "prod-1"
	opts.HATier = "single-server"
	opts.Servers = []string{nodeA}
	opts.Agents = []string{nodeB, nodeC}
	return opts
}

func perHostDevices() storage.Devices {
	return storage.Devices{PerHost: map[string]string{nodeA: volumeA, nodeB: volumeB, nodeC: volumeC}}
}

// installSession is the session the plan would run, with one fake connection
// per node in the order the operator gave them.
func installSession(t *testing.T, opts install.Options, runners map[string]*componenttest.FakeRunner) *install.Session {
	t.Helper()
	s := sessionWithOpts(t, opts, &recorder{})
	for _, server := range opts.Servers {
		s.Nodes = append(s.Nodes, install.Node{Address: server, Role: install.RoleServer, Runner: runners[server]})
	}
	for _, agent := range opts.Agents {
		s.Nodes = append(s.Nodes, install.Node{Address: agent, Role: install.RoleAgent, Runner: runners[agent]})
	}
	return s
}

// runStage drives one stage out of the plan the installer would actually run,
// so what a test exercises is the wiring rather than a stage function called by
// name.
func runStage(t *testing.T, s *install.Session, name string) error {
	t.Helper()
	for _, stage := range install.Plan(s) {
		if stage.Name == name {
			return stage.Run(context.Background())
		}
	}
	t.Fatalf("the plan has no %q stage", name)
	return nil
}

// askedToCreate reports whether this node's runner was asked to build
// kubenest-vg on device.
func askedToCreate(runner *componenttest.FakeRunner, device string) bool {
	commands := runner.Commands()
	return slices.Contains(commands, "sudo -n pvcreate "+device) &&
		slices.Contains(commands, "sudo -n vgcreate "+storage.VolumeGroup+" "+device)
}

// The acceptance: a three-node install whose --storage-device values name one
// device per node creates each node's volume group on THAT node's device. One
// value applied to every node — today's behaviour — would send the first
// host's by-id path to the other two, which do not have that volume.
func TestStorageStageCreatesTheVolumeGroupOnEachNodesOwnDevice(t *testing.T) {
	opts := threeNodeInstall(install.Options{StorageDevices: perHostDevices()})
	runners := map[string]*componenttest.FakeRunner{
		nodeA: volumeGroupNode(volumeA),
		nodeB: volumeGroupNode(volumeB),
		nodeC: volumeGroupNode(volumeC),
	}
	s := installSession(t, opts, runners)

	// The test bundle pins no openebs-lvm-localpv, so the stage stops right
	// after the volume-group step this test observes. Everything asserted
	// below happens before that.
	if err := runStage(t, s, install.StageStorage); err == nil {
		t.Fatal("the storage stage ran to the end, which this test bundle cannot support: it pins no openebs-lvm-localpv")
	}

	want := map[string]string{nodeA: volumeA, nodeB: volumeB, nodeC: volumeC}
	for host, device := range want {
		if !askedToCreate(runners[host], device) {
			t.Errorf("node %s was asked to build %s on the wrong device.\ncommands:\n%s",
				host, storage.VolumeGroup, strings.Join(runners[host].Commands(), "\n"))
		}
		for other, otherDevice := range want {
			if other == host {
				continue
			}
			if slices.Contains(runners[host].Commands(), "sudo -n pvcreate "+otherDevice) {
				t.Errorf("node %s was asked to build %s on %s, which belongs to %s: the by-id path names one host's volume, so it must never be applied to another",
					host, storage.VolumeGroup, otherDevice, other)
			}
		}
	}
}

// The single form keeps its meaning: one device, every node. A cluster whose
// nodes share a device path (or a test host) must keep working.
func TestStorageStageUsesTheSingleDeviceOnEveryNode(t *testing.T) {
	opts := threeNodeInstall(install.Options{StorageDevices: storage.Devices{All: volumeA}})
	runners := map[string]*componenttest.FakeRunner{
		nodeA: volumeGroupNode(volumeA),
		nodeB: volumeGroupNode(volumeA),
		nodeC: volumeGroupNode(volumeA),
	}
	s := installSession(t, opts, runners)

	if err := runStage(t, s, install.StageStorage); err == nil {
		t.Fatal("the storage stage ran to the end, which this test bundle cannot support: it pins no openebs-lvm-localpv")
	}
	for host, runner := range runners {
		if !askedToCreate(runner, volumeA) {
			t.Errorf("node %s was not asked to build %s on %s, the one device named for every node.\ncommands:\n%s",
				host, storage.VolumeGroup, volumeA, strings.Join(runner.Commands(), "\n"))
		}
	}
}

// A per-host mapping that leaves a node out names no device for it, and
// Option 1's meaning — "the volume group is the operator's" — is not something
// the request said. It is refused before any host is touched, so no runner
// receives a command.
func TestANodeWithNoDeviceIsRefusedBeforeAnyWrite(t *testing.T) {
	// A per-host set has to name every node; this one names A and C and
	// forgets B, which is what an operator does after adding an --agent.
	opts := threeNodeInstall(install.Options{StorageDevices: storage.Devices{PerHost: map[string]string{
		nodeA: volumeA,
		nodeC: volumeC,
	}}})
	runners := map[string]*componenttest.FakeRunner{
		nodeA: volumeGroupNode(volumeA),
		nodeB: volumeGroupNode(volumeB),
		nodeC: volumeGroupNode(volumeC),
	}

	// The stage that touches block devices refuses the request itself.
	err := runStage(t, installSession(t, opts, runners), install.StageStorage)
	if err == nil {
		t.Fatalf("a mapping that names no device for %s was accepted: %s would get Option 1's treatment, which the request never said", nodeB, nodeB)
	}
	if !strings.Contains(err.Error(), nodeB) {
		t.Errorf("refusal %q does not name %s, the node it is about", err, nodeB)
	}
	for host, runner := range runners {
		if commands := runner.Commands(); len(commands) != 0 {
			t.Errorf("node %s was asked to run %v; a request that names no device for %s has to be refused before any host is touched",
				host, commands, nodeB)
		}
	}

	// And preflight, the first stage of every plan, refuses the same request
	// before it dials anything — so a real install never reaches a host. The
	// refusal has to be the REQUEST being refused: an unreachable host is
	// reported as an SSH failure, which names the node too.
	preflightRunners := map[string]*componenttest.FakeRunner{
		nodeA: volumeGroupNode(volumeA),
		nodeB: volumeGroupNode(volumeB),
		nodeC: volumeGroupNode(volumeC),
	}
	preflightErr := runStage(t, installSession(t, opts, preflightRunners), install.StagePreflight)
	if preflightErr == nil {
		t.Fatalf("preflight accepted a mapping that names no device for %s", nodeB)
	}
	if !strings.Contains(preflightErr.Error(), nodeB) {
		t.Errorf("preflight refusal %q does not name %s", preflightErr, nodeB)
	}
	if !strings.Contains(preflightErr.Error(), "--storage-device") {
		t.Errorf("preflight refusal %q does not name --storage-device, so it is not the request being refused: an SSH failure names the node as well", preflightErr)
	}
	for host, runner := range preflightRunners {
		if commands := runner.Commands(); len(commands) != 0 {
			t.Errorf("preflight reached node %s (%v) before refusing a mapping that names no device for %s", host, commands, nodeB)
		}
	}
}

// oldShapeJournal is a journal as a CLI before kn-hku7 wrote it: the install
// record carries ONE device under "storage_device", which meant "this device
// on every node". The bytes are written out rather than produced by this CLI's
// encoder, because the point is to read what the old one left on disk.
const oldShapeJournal = `{
  "identity": {"kind": "install", "cluster": "prod-1", "fields": {"--storage-device": "/dev/disk/by-id/scsi-0HC_Volume_101"}},
  "cluster_id": "cluster-1",
  "entries": [{"stage": "platform-preflight", "status": "completed", "at": "2026-09-01T10:00:00Z"}],
  "state": {"volume_group_ownership": "installer-created", "storage_device": "/dev/disk/by-id/scsi-0HC_Volume_101"}
}`

// readOldJournal writes those bytes where a journal lives and reads them back
// the way uninstall does.
func readOldJournal(t *testing.T) install.Record {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prod-1.json")
	if err := os.WriteFile(path, []byte(oldShapeJournal), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := install.ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	record, err := install.Recorded(journal)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// A journal written before the flag took a HOST=DEV form still says which
// device the installer created kubenest-vg on. The old key meant the same
// device on every node, and it has to read as exactly that: a record its own
// reader cannot understand is a record every later command loses.
func TestAnOldShapedRecordStillNamesItsDeviceOnEveryNode(t *testing.T) {
	record := readOldJournal(t)
	for _, node := range []string{nodeA, nodeB, nodeC} {
		if got := record.Devices.For(node); got != volumeA {
			t.Errorf("an old journal gives %s the device %q, want %q: the old key meant one device for every node", node, got, volumeA)
		}
	}
	if record.Ownership != storage.InstallerCreated {
		t.Errorf("ownership = %q, want installer-created: the migration is about the device, not a rewrite of the record", record.Ownership)
	}

	// Writing stays new-shape: nothing re-emits the old key.
	body, err := json.Marshal(install.Record{Devices: record.Devices, Ownership: record.Ownership})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"storage_device"`) {
		t.Errorf("a record written now still carries the pre-kn-hku7 key: %s", body)
	}
	if !strings.Contains(string(body), `"storage_devices"`) {
		t.Errorf("a record written now does not carry the per-node mapping: %s", body)
	}
}

// The consequence that made the old shape matter: `platform uninstall
// --destroy-data` releases the device behind kubenest-vg, and the journal is
// the only thing that can tell it which device. An install made before this
// change must still have its device released — on every node, which is what
// the old key meant.
func TestAnOldShapedRecordReleasesItsDeviceOnEveryNode(t *testing.T) {
	record := readOldJournal(t)

	// One platform volume in kubenest-vg, as uninstall's listing sees it.
	respond := func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "lvs --noheadings") {
			return sshx.Result{Stdout: "  pvc-abc\n"}, nil
		}
		return sshx.Result{}, nil
	}
	serverFake := &componenttest.FakeRunner{Respond: respond}
	agentFake := &componenttest.FakeRunner{Respond: respond}

	if err := uninstall.Run(context.Background(), uninstall.Options{
		Nodes: []uninstall.Node{
			{Address: nodeA, Role: uninstall.RoleServer, Runner: serverFake},
			{Address: nodeB, Role: uninstall.RoleAgent, Runner: agentFake},
		},
		DestroyData: true,
		Ownership:   record.Ownership,
		Devices:     record.Devices,
	}); err != nil {
		t.Fatal(err)
	}
	for host, runner := range map[string]*componenttest.FakeRunner{nodeA: serverFake, nodeB: agentFake} {
		commands := runner.Commands()
		if !slices.Contains(commands, "sudo -n pvremove -y "+volumeA) {
			t.Errorf("node %s did not release %s, the device its journal recorded:\n%s",
				host, volumeA, strings.Join(commands, "\n"))
		}
		if !slices.Contains(commands, "sudo -n vgremove -y kubenest-vg") {
			t.Errorf("node %s did not remove %s:\n%s", host, storage.VolumeGroup, strings.Join(commands, "\n"))
		}
	}
}

// A resume re-runs the IDENTICAL command, and the device mapping is part of
// what identical means: continuing with a different per-node mapping would
// keep building on the devices the first run chose while reporting the ones
// this run passed. Reordering the same mapping is still the same install.
func TestResumeRefusesAChangedDeviceMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.json")
	first := threeNodeInstall(install.Options{StorageDevices: perHostDevices()})
	journal, err := install.OpenJournal(path, first.Identity())
	if err != nil {
		t.Fatal(err)
	}
	// A completed stage is what binds the journal to its identity: before
	// that nothing was done under it and a different request starts clean.
	if err := journal.Append(install.Entry{
		Stage: install.StagePreflight, Status: install.StatusCompleted, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// One node's device changed: B now points at A's volume.
	changed := threeNodeInstall(install.Options{StorageDevices: storage.Devices{PerHost: map[string]string{
		nodeA: volumeA,
		nodeB: volumeA,
		nodeC: volumeC,
	}}})
	_, err = install.OpenJournal(path, changed.Identity())
	if err == nil {
		t.Fatal("a resume that changed one node's device was accepted, so the install would keep using the first run's mapping while claiming the new one")
	}
	if !strings.Contains(err.Error(), "--storage-device") {
		t.Errorf("refusal %q does not name --storage-device, the option that changed", err)
	}

	// The same mapping, written in another order, is the same install.
	reordered, err := storage.ParseDevices(
		[]string{nodeC + "=" + volumeC, nodeB + "=" + volumeB, nodeA + "=" + volumeA},
		[]string{nodeA, nodeB, nodeC})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.OpenJournal(path, threeNodeInstall(install.Options{StorageDevices: reordered}).Identity()); err != nil {
		t.Errorf("the identical mapping written in another order was refused: %v", err)
	}
}

// Uninstall has no flags for the storage device: it reads the mapping back out
// of the journal to release each node's own device. That read has to give back
// what the install recorded.
func TestTheJournalKeepsThePerNodeMappingForUninstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.json")
	opts := threeNodeInstall(install.Options{StorageDevices: perHostDevices()})
	journal, err := install.OpenJournal(path, opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.SetState(install.Record{Devices: perHostDevices(), Ownership: storage.InstallerCreated}); err != nil {
		t.Fatal(err)
	}
	record, err := install.Recorded(journal)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{nodeA: volumeA, nodeB: volumeB, nodeC: volumeC} {
		if got := record.Devices.For(host); got != want {
			t.Errorf("the journal gives %s the device %q, want %q", host, got, want)
		}
	}
	if record.Ownership != storage.InstallerCreated {
		t.Errorf("ownership = %q, want installer-created", record.Ownership)
	}
}
