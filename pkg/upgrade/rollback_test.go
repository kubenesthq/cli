package upgrade

import (
	"context"
	"io"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
)

func journalWith(t *testing.T, stageNames ...string) *stages.Journal {
	t.Helper()
	j, err := stages.OpenJournal(t.TempDir()+"/upgrade.json",
		stages.Identity{Kind: Kind, Cluster: "prod-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range stageNames {
		if err := j.Append(stages.Entry{Stage: name, Status: stages.StatusStarted}); err != nil {
			t.Fatal(err)
		}
	}
	return j
}

// THE decision the ordering buys: a failure before the kubernetes stage costs
// seconds, and one after it costs a datastore restore. Everything else about
// rollback follows from which side of that line the failure fell on.
func TestTheMechanismFollowsFromWhereItFailed(t *testing.T) {
	from := parseManifest(t, "bundle: \"1.0\"\ncore: {traefik: 41.2.0, cert-manager: v1.21.1}\nlimits: {timeouts: {node-ready: 5m}}\n")
	to := parseManifest(t, "bundle: \"1.1\"\ncore: {traefik: 42.0.0, cert-manager: v1.22.0}\nlimits: {timeouts: {node-ready: 5m}}\n")
	rec := record{FromBundle: "1.0", ToBundle: "1.1", Snapshot: "pre-upgrade-1-1"}

	t.Run("stopped before anything moved", func(t *testing.T) {
		plan := PlanRollback(journalWith(t, StagePreflight, StageBackup), from, to, rec)
		if plan.Mechanism != MechanismNothing {
			t.Errorf("mechanism = %s, want nothing to undo", plan.Mechanism)
		}
	})

	t.Run("components moved, kubernetes never started", func(t *testing.T) {
		plan := PlanRollback(journalWith(t, StagePreflight, StageBackup, StageComponents), from, to, rec)
		if plan.Mechanism != MechanismComponents {
			t.Fatalf("mechanism = %s, want a component revert", plan.Mechanism)
		}
		report := plan.String()
		for _, want := range []string{"traefik 42.0.0 → 41.2.0", "no data implications", "seconds"} {
			if !strings.Contains(report, want) {
				t.Errorf("the plan does not mention %q:\n%s", want, report)
			}
		}
		assertTheHostStepIsLeftInPlace(t, report)
	})

	t.Run("kubernetes started", func(t *testing.T) {
		plan := PlanRollback(journalWith(t, StagePreflight, StageBackup, StageComponents, StageKubernetes), from, to, rec)
		if plan.Mechanism != MechanismRestore {
			t.Fatalf("mechanism = %s, want a datastore restore", plan.Mechanism)
		}
		report := plan.String()
		// The report must say the three things that surprise people.
		for _, want := range []string{
			"does not downgrade",
			"service interruption",
			"PERSISTENT VOLUMES ARE NOT TOUCHED",
			"PersistentVolumeClaim created during the upgrade window",
			"pre-upgrade-1-1",
		} {
			if !strings.Contains(report, want) {
				t.Errorf("the plan does not mention %q:\n%s", want, report)
			}
		}
		assertTheHostStepIsLeftInPlace(t, report)
	})
}

// A rollback never undoes the host step, and the plan SAYS so: reverting the
// APT drop-in would restore Ubuntu's own random install time and its
// self-chosen reboot, which is the behaviour the host step replaces (T7.3).
// The operator is told what was left in place rather than discovering it.
func assertTheHostStepIsLeftInPlace(t *testing.T, report string) {
	t.Helper()
	for _, want := range []string{"/etc/apt/apt.conf.d", "auto-reboot=false", "NOT undone"} {
		if !strings.Contains(report, want) {
			t.Errorf("the plan must name what it leaves in place (%q):\n%s", want, report)
		}
	}
}

// A restore with no snapshot is refused rather than attempted: there is
// nothing to go back to, and saying so is more useful than failing partway.
func TestRestoringWithoutASnapshotIsRefused(t *testing.T) {
	err := RestoreSnapshot(nil, nil, "")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "nothing to restore to") {
		t.Errorf("the refusal must say why: %v", err)
	}
}

// A failed cluster-reset must report k3s's own reason. k3s logs "Starting k3s"
// first and its fatal line last, and lab up (2026-09-28) showed an operator
// nothing but "Starting k3s v1.35.7+k3s1" from a restore that had failed.
func TestAFailedDatastoreRestoreNamesK3sReason(t *testing.T) {
	runner := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if strings.Contains(command, "ls -1 "+snapshotDir) {
			return sshx.Result{Stdout: "pre-upgrade-1-2-20260928t092620-kubenest-lab-up-1-1790587580\n"}, nil
		}
		if strings.Contains(command, "--cluster-reset") {
			return sshx.Result{ExitCode: 1, Stderr: strings.Join([]string{
				`time="2026-09-28T09:27:29Z" level=info msg="Starting k3s v1.35.7+k3s1 (cd43afc7)"`,
				`time="2026-09-28T09:27:29Z" level=info msg="Managed etcd cluster bootstrap already complete and initialized"`,
				`time="2026-09-28T09:27:30Z" level=fatal msg="starting kubernetes: preparing server: bootstrap data already found and encrypted with different token"`,
			}, "\n")}, nil
		}
		return sshx.Result{}, nil
	}}
	err := RestoreSnapshot(context.Background(), runner, "pre-upgrade-1-2-20260928t092620")
	if err == nil {
		t.Fatal("a cluster-reset that exited 1 without resetting was reported as a success")
	}
	if !strings.Contains(err.Error(), "encrypted with different token") {
		t.Errorf("the error does not carry k3s's reason, only: %v", err)
	}
	if ranCommand(runner, "rm -f") || ranCommand(runner, "systemctl start k3s") {
		t.Errorf("a failed cluster-reset still went on to touch the manifest files or start k3s: %v", runner.Commands())
	}
}

// k3s applies every file in its auto-deploy directory when the server starts,
// and the upgrade leaves its two Plan manifests there. The datastore restore
// does not remove them — the snapshot was taken before they existed — but the
// files are still on disk, so the first server start after the restore hands
// them back to k3s's deploy controller and the system-upgrade controller
// finishes the upgrade the operator just rolled back. Lab up, 2026-09-28: the
// server came back on bundle 1.1 at 11:31:00, the server Plan was re-created
// at 11:31:15, the server was drained at 11:31:28 and k3s was back on bundle
// 1.2 at 11:31:48 — outside any maintenance window, with no operation record
// and no lock.
//
// So a restore removes both manifest files BETWEEN the reset and the start,
// and this test asserts the order: stop, cluster-reset, remove, start.
// Removing them after the start would already have replayed the upgrade.
func TestARestoreRemovesTheUpgradePlanManifestsBeforeItStartsK3s(t *testing.T) {
	wrote := "pre-upgrade-1-2-20260928t100311-kubenest-lab-up-1-1790589791"
	runner := snapshotHost(wrote)
	if err := RestoreSnapshot(context.Background(), runner, "pre-upgrade-1-2-20260928t100311"); err != nil {
		t.Fatalf("the restore failed: %v", err)
	}

	commands := runner.Commands()
	at := -1
	for _, step := range []string{"systemctl stop k3s", "--cluster-reset", "rm -f", "systemctl start k3s"} {
		next := -1
		for i := at + 1; i < len(commands); i++ {
			if strings.Contains(commands[i], step) {
				next = i
				break
			}
		}
		if next < 0 {
			t.Fatalf("the restore never ran %q after its step at position %d; it ran:\n%s",
				step, at, strings.Join(commands, "\n"))
		}
		at = next
	}

	// Both files, built from the names the upgrade writes them with, in the
	// directory k3s auto-deploys from. One Plan left behind is enough to
	// re-run the upgrade on the nodes it selects.
	var removal string
	for _, command := range commands {
		if strings.Contains(command, "rm -f") {
			removal = command
			break
		}
	}
	for _, want := range []string{
		k3s.ManifestDir + "/" + serverPlan + ".yaml",
		k3s.ManifestDir + "/" + agentPlan + ".yaml",
	} {
		if !strings.Contains(removal, want) {
			t.Errorf("the removal does not name %s, so that Plan stays on disk to re-run the upgrade: %q", want, removal)
		}
	}
}

// A removal that fails stops the rollback BEFORE k3s is started.
//
// Nothing else can take those files off disk: the snapshot was taken before
// they were written, and the datastore restore does not reach the filesystem.
// Starting k3s with them in place replays the upgrade the operator just rolled
// back, which is worse than a server left stopped — a stopped server is
// recoverable by hand, a re-run upgrade is what the restore exists to undo.
func TestARestoreThatCannotRemoveThePlanManifestsDoesNotStartK3s(t *testing.T) {
	wrote := "pre-upgrade-1-2-20260928t100311-kubenest-lab-up-1-1790589791"
	host := snapshotHost(wrote)
	runner := &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
		if strings.Contains(command, "rm -f "+k3s.ManifestDir) {
			return sshx.Result{ExitCode: 1, Stderr: "rm: cannot remove: Read-only file system"}, nil
		}
		return host.Respond(command)
	}}

	err := RestoreSnapshot(context.Background(), runner, "pre-upgrade-1-2-20260928t100311")
	if err == nil {
		t.Fatal("a restore whose Plan removal failed was reported as done")
	}
	if !ranCommand(runner, "rm -f "+k3s.ManifestDir) {
		t.Fatalf("the restore never even tried to remove the Plan manifests: %v", runner.Commands())
	}
	if ranCommand(runner, "systemctl start k3s") {
		t.Errorf("k3s was started with the Plan manifests still on disk, which re-runs the upgrade the restore just undid: %v", runner.Commands())
	}
	if !strings.Contains(err.Error(), "k3s is stopped on this server") {
		t.Errorf("the failure does not tell the operator the server is stopped and how to bring it back: %v", err)
	}
}

// A revert is done when the chart REPORTS the starting bundle's version and its
// apply job succeeded, not when the component's pods are Ready: after a failed
// upgrade the previous release never stopped serving, so its pods are Ready
// before the reverted chart is even applied. Lab up, 2026-09-28: the rollback
// returned while kubenest-traefik still read 0.0.0-does-not-exist.
func TestARevertIsNotDoneUntilTheChartReportsTheStartingVersion(t *testing.T) {
	from := parseManifest(t, "bundle: \"1.1\"\ncore:\n  traefik: 41.4.0\nlimits:\n  timeouts:\n    component-ready: 2s\n")
	chartAt := func(version string) *componenttest.FakeRunner {
		return &componenttest.FakeRunner{Respond: func(command string) (sshx.Result, error) {
			switch {
			case strings.Contains(command, "get helmchart kubenest-traefik"):
				return sshx.Result{Stdout: version + " helm-install-kubenest-traefik"}, nil
			case strings.Contains(command, "get job helm-install-kubenest-traefik"):
				return sshx.Result{Stdout: "1"}, nil
			}
			return sshx.Result{}, nil
		}}
	}
	healthyAlready := coreComponent{key: "traefik", install: func(context.Context, k3s.Runner, *manifest.Manifest, converge.Reporter) error {
		return nil
	}}
	rep := converge.NewTextReporter(io.Discard)
	if err := revertComponent(context.Background(), chartAt("0.0.0-does-not-exist"), healthyAlready, "41.4.0", from, rep); err == nil {
		t.Fatal("the revert reported done while the chart still pins the failed version")
	}
	if err := revertComponent(context.Background(), chartAt("41.4.0"), healthyAlready, "41.4.0", from, rep); err != nil {
		t.Fatalf("the chart reports 41.4.0 and its job succeeded, yet the revert failed: %v", err)
	}
}
