package upgrade

import (
	"context"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

// snapshotHost is a server whose snapshots directory holds files: it answers
// the snapshot save, the directory listing, the service stop and start, the
// cluster-reset and Velero's (empty) storage-location list.
func snapshotHost(files ...string) *componenttest.FakeRunner {
	return &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		switch {
		case strings.Contains(command, "ls -1 "+snapshotDir):
			return sshx.Result{Stdout: strings.Join(files, "\n") + "\n"}, nil
		case strings.Contains(command, "--cluster-reset"):
			if !strings.Contains(command, "--cluster-reset-restore-path="+snapshotDir+"/") {
				return sshx.Result{ExitCode: 1, Stderr: "unexpected restore path: " + command}, nil
			}
			name := command[strings.LastIndex(command, "/")+1:]
			for _, f := range files {
				if f == name {
					return sshx.Result{Stderr: `level=info msg="Managed etcd cluster membership has been reset"`}, nil
				}
			}
			return sshx.Result{ExitCode: 1, Stderr: `level=fatal msg="etcd: snapshot path does not exist: ` + snapshotDir + "/" + name + `"`}, nil
		}
		return sshx.Result{}, nil
	}}
}

func ranCommand(r *componenttest.FakeRunner, fragment string) bool {
	for _, c := range r.Commands() {
		if strings.Contains(c, fragment) {
			return true
		}
	}
	return false
}

// k3s names the file <name>-<node>-<unix time>, and the rollback restores the
// file. Lab up, 2026-09-28: the upgrade recorded the name it asked for, and
// the post-point-of-no-return rollback failed on "snapshot path does not
// exist".
func TestTheUpgradeRecordsTheSnapshotFileK3sWrote(t *testing.T) {
	wrote := "pre-upgrade-1-2-20260928t100311-kubenest-lab-up-1-1790589791"
	runner := snapshotHost("etcd-snapshot-kubenest-lab-up-1-1790580000", wrote)
	j, err := stages.OpenJournal(t.TempDir()+"/upgrade.json", stages.Identity{Kind: Kind, Cluster: "prod-1"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 28, 10, 3, 11, 0, time.UTC)
	s := &Session{
		Jnl:   j,
		Opts:  Options{Cluster: "prod-1", To: "1.2", Now: func() time.Time { return at }},
		Nodes: []Node{{Address: "10.0.1.10", Server: true, Runner: runner}},
	}
	if err := stageBackup(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.Record.Snapshot != wrote {
		t.Fatalf("the upgrade recorded snapshot %q; k3s wrote %q, and a rollback restores what is on disk", s.Record.Snapshot, wrote)
	}
}

// A record written before the fix holds the bare name. The restore still finds
// the one file k3s wrote for it.
func TestARollbackRestoresTheFileABareRecordedNameNamed(t *testing.T) {
	wrote := "pre-upgrade-1-2-20260928t100311-kubenest-lab-up-1-1790589791"
	runner := snapshotHost(wrote)
	if err := RestoreSnapshot(context.Background(), runner, "pre-upgrade-1-2-20260928t100311"); err != nil {
		t.Fatalf("the restore could not find the file k3s wrote for the recorded name: %v", err)
	}
	if !ranCommand(runner, "--cluster-reset-restore-path="+snapshotDir+"/"+wrote) {
		t.Errorf("the restore did not restore %s: %v", wrote, runner.Commands())
	}
}

// A snapshot that is not on disk is refused while k3s still runs: stopping the
// cluster for a restore that cannot happen is an outage for nothing.
func TestARollbackWithNoSnapshotOnDiskIsRefusedWithoutStoppingK3s(t *testing.T) {
	runner := snapshotHost("etcd-snapshot-kubenest-lab-up-1-1790580000")
	err := RestoreSnapshot(context.Background(), runner, "pre-upgrade-1-2-20260928t100311")
	if err == nil {
		t.Fatal("a restore of a snapshot that is not on disk was reported as done")
	}
	if ranCommand(runner, "systemctl stop k3s") {
		t.Errorf("k3s was stopped for a restore whose snapshot is not on disk: %v", runner.Commands())
	}
	if !strings.Contains(err.Error(), "pre-upgrade-1-2-20260928t100311") {
		t.Errorf("the refusal does not name the snapshot it looked for: %v", err)
	}
}

// Two files for one name cannot be told apart, so neither is restored.
func TestARollbackRefusesTwoSnapshotsForOneName(t *testing.T) {
	runner := snapshotHost(
		"pre-upgrade-1-2-20260928t100311-kubenest-lab-up-1-1790589791",
		"pre-upgrade-1-2-20260928t100311-kubenest-lab-up-2-1790589792",
	)
	if err := RestoreSnapshot(context.Background(), runner, "pre-upgrade-1-2-20260928t100311"); err == nil {
		t.Fatal("a name matching two snapshot files was restored anyway")
	}
	if ranCommand(runner, "systemctl stop k3s") {
		t.Errorf("k3s was stopped although the snapshot to restore was ambiguous: %v", runner.Commands())
	}
}
