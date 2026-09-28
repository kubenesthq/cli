//go:build e2e

// S2's WORKLOAD HALF (kn-1krv, plan task S2-workload): the 1.1 -> 1.2 upgrade of
// a two-node workload cluster on REAL hosts, and the half of the rollback story
// that had never run outside unit tests.
//
// What it asserts, which is the gate:
//
//	`platform upgrade --to <to>` OUTSIDE the window is refused and names the
//	  next opening in local time AND UTC
//	a hop the catalog does not offer is refused and names the bundle to step
//	  through first (kn-mtpf's adjacency rule)
//	a component failure BEFORE the point of no return rolls back and puts the
//	  starting bundle's pins back
//	a failure AFTER the point of no return is recovered by the documented
//	  disaster-recovery procedure: `platform rollback` chooses the datastore
//	  restore, refuses to run without an acknowledgement, restores the stage-2
//	  snapshot, and what Kubernetes state was written since the snapshot is gone
//	after those two failures the cluster is still on the starting bundle, and a
//	  `--wait` run holds NOTHING while it waits, starts when the window opens,
//	  and completes the clean hop
//	the two-replica workload, measured from OUTSIDE at a stated rate with
//	  retries off, stays inside its limits for the whole upgrade, reported
//	  SEPARATELY from the Kubernetes API's own reading
//
// BUNDLE 1.2 DOES NOT EXIST IN THE CATALOG YET. T7.2 adds it, with a newer k3s
// patch release, and this gate is that transition's acceptance. Both bundles are
// therefore parameters: KUBENEST_UPGRADE_FROM_BUNDLE (default 1.1) and
// KUBENEST_UPGRADE_TO_BUNDLE (default 1.2). When the catalog does not serve the
// target, or serves one whose k3s pin does not move, the test SKIPS and says
// exactly what is missing rather than passing vacuously: the Kubernetes stage IS
// the point of no return, and an upgrade that steps around it proves nothing
// about the path this bead exists for.
//
// Run from the umbrella workspace with a THREE-host lab: a control plane (any
// profile that serves the bundles; the control plane's own upgrade is T7.0's
// half) and a two-node workload cluster whose SERVER is
// KUBENEST_LAB_SERVER_IP/KUBENEST_LAB_NODE1_IP and whose AGENT is
// KUBENEST_LAB_NODE2_IP:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=...
//	cd kubenest-cli && go test -tags e2e -v -timeout 180m ./e2e/ -run TestWorkloadUpgrade11To12
//
// The FIXTURE is installed by this test at `from` unless a cluster with the same
// name is already registered at that bundle. A hardware run that wants the
// released-to-candidate fidelity the bead names should install the fixture with
// the RELEASED 1.1 CLI and set KUBENEST_UPGRADE_SKIP_INSTALL=1; this test then
// adopts it instead of installing over it.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/upgrade"
	"kubenest.io/cli/pkg/window"
)

// w12Env is the fixture this gate needs beyond upgradeEnvironment's.
type w12Env struct {
	upgradeEnv
	// from and to are the bundles the hop is between. They are read from the
	// environment because 1.2 is not in the catalog yet.
	from, to string
	// skipInstall says the fixture was installed OUTSIDE this test, with the
	// released CLI, which is the fidelity a released-to-candidate hop wants.
	skipInstall bool
	// zone is the window's IANA zone, so a refusal's local and UTC halves are
	// two different renderings of one instant.
	zone *time.Location
}

func w12Environment(t *testing.T) w12Env {
	t.Helper()
	env := w12Env{
		upgradeEnv:  upgradeEnvironment(t),
		from:        envOr("KUBENEST_UPGRADE_FROM_BUNDLE", "1.1"),
		to:          envOr("KUBENEST_UPGRADE_TO_BUNDLE", "1.2"),
		skipInstall: os.Getenv("KUBENEST_UPGRADE_SKIP_INSTALL") != "",
	}
	zone := envOr("KUBENEST_GATE_WINDOW_TZ", "Asia/Kolkata")
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("KUBENEST_GATE_WINDOW_TZ=%q is not an IANA name: %v", zone, err)
	}
	env.zone = loc
	return env
}

// w12CatalogGate refuses — by SKIPPING, with the reason — the runs this gate
// cannot make a real claim about. The precondition is T7.2's: bundle 1.2 moves
// k3s to a newer patch release, or the Kubernetes stage and its recovery path
// are never exercised.
func w12CatalogGate(t *testing.T, ctx context.Context, client *api.Client, env w12Env) (*manifest.Manifest, *manifest.Manifest) {
	t.Helper()
	bundles, err := client.ListBundles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	serves := map[string]bool{}
	for _, bundle := range bundles {
		serves[bundle.Version] = true
	}
	if !serves[env.from] {
		t.Fatalf("the catalog does not serve the starting bundle %s, so the fixture this gate expects cannot be installed", env.from)
	}
	if !serves[env.to] {
		t.Skipf("the catalog does not serve bundle %s yet: T7.2 adds it (with the newer k3s patch release the S2 precondition names) and this gate is that transition's acceptance. Set KUBENEST_UPGRADE_TO_BUNDLE to a candidate the catalog serves, or re-run once T7.2 lands", env.to)
	}
	fromBundle := fetchBundle(t, client, env.from)
	toBundle := fetchBundle(t, client, env.to)
	fromK3s, err := fromBundle.Core.Version("k3s")
	if err != nil {
		t.Fatalf("bundle %s pins no k3s: %v", env.from, err)
	}
	toK3s, err := toBundle.Core.Version("k3s")
	if err != nil {
		t.Fatalf("bundle %s pins no k3s: %v", env.to, err)
	}
	if fromK3s == toK3s {
		t.Skipf("bundle %s pins the same k3s as %s (%s), so the Kubernetes stage would step around the point of no return: the S2 precondition is that %s moves k3s to a newer patch release (plan 7.10)", env.to, env.from, fromK3s, env.to)
	}
	t.Logf("[%s] %s → %s moves Kubernetes %s → %s", w12Now(), env.from, env.to, fromK3s, toK3s)
	return fromBundle, toBundle
}

func w12Now() string { return time.Now().Format(time.RFC3339) }

// w12Home gives the CLI a HOME of its own, logged in to the lab's control
// plane. It matters twice: the upgrade journal the CLI writes lives under it,
// and the post-point-of-no-return branch runs `platform rollback` as a SECOND
// process that must find that journal.
func w12Home(t *testing.T, env w12Env) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&config.Config{
		ControlPlaneURL: env.controlPlane,
		ControlPlaneCA:  string(env.controlPlaneCA),
	}); err != nil {
		t.Fatal(err)
	}
	creds := &config.Credentials{Tokens: map[string]config.StoredCredential{}}
	creds.Set(env.controlPlane, env.token)
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatal(err)
	}
}

// w12RunCLI runs the REAL command tree, so the flags, the refusals and the
// recorded steps are the ones an operator gets.
func w12RunCLI(out io.Writer, args ...string) error {
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// w12UpgradeArgs is the workload upgrade's command line. The node list is given
// explicitly because this runs from a laptop that holds no install journal for
// the fixture.
func w12UpgradeArgs(env w12Env, target string, extra ...string) []string {
	args := []string{"platform", "upgrade", "--cluster", env.cluster, "--to", target,
		"--server", env.server, "--agent", env.agent,
		"--ssh-user", env.sshUser}
	if env.sshKey != "" {
		args = append(args, "--ssh-key", env.sshKey)
	}
	return append(args, extra...)
}

func w12RollbackArgs(env w12Env, extra ...string) []string {
	// --to is the bundle the interrupted upgrade was moving to. The rollback
	// reads the run's journal, and the journal's identity includes it: without
	// the flag the identity does not match and the resume is refused.
	args := []string{"platform", "rollback", "--cluster", env.cluster, "--to", env.to,
		"--server", env.server, "--agent", env.agent,
		"--ssh-user", env.sshUser}
	if env.sshKey != "" {
		args = append(args, "--ssh-key", env.sshKey)
	}
	return append(args, extra...)
}

// w12SetWindow stores a window through the REAL set-window command.
func w12SetWindow(t *testing.T, env w12Env, days []string, start, end, zone string) string {
	t.Helper()
	var out bytes.Buffer
	if err := w12RunCLI(&out, "cluster", "set-window", "--cluster", env.cluster,
		"--days", strings.Join(days, ","), "--start", start, "--end", end, "--timezone", zone); err != nil {
		t.Fatalf("set-window %v %s-%s %s: %v\n%s", days, start, end, zone, err, out.String())
	}
	return out.String()
}

// w12StoreOpenWindow stores a window that is SHUT now and opens about two
// minutes from now for six hours, listing all seven days so a midnight crossing
// is legal. It returns the opening instant.
func w12StoreOpenWindow(t *testing.T, env w12Env) time.Time {
	t.Helper()
	start := time.Now().In(env.zone).Add(2 * time.Minute).Truncate(time.Minute)
	end := start.Add(6 * time.Hour)
	w12SetWindow(t, env, weekdays(), start.Format("15:04"), end.Format("15:04"), env.zone.String())
	return start
}

// w12Session is the in-process upgrade session the failure branches need: only a
// doctored manifest can inject a failure, and the journal path is the CLI's own
// so that `platform rollback`, run as a real second process, finds it.
func w12Session(t *testing.T, env w12Env, client *api.Client, from, to string, override *manifest.Manifest, journalPath string) *upgrade.Session {
	t.Helper()
	ctx := context.Background()
	clusterID := clusterIDFor(t, ctx, client, env.cluster)

	records := upgrade.ControlPlaneRecords{Client: client, ClusterID: clusterID}
	recorded, err := records.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fromBundle := fetchBundle(t, client, from)
	toBundle := override
	if toBundle == nil {
		toBundle = fetchBundle(t, client, to)
	}

	opts := upgrade.Options{
		Cluster: env.cluster, To: to,
		Servers: []string{env.server}, Agents: []string{env.agent},
		SSHUser: env.sshUser, SSHKey: env.sshKey,
		// The failure branches are about rollback, not about the window; the
		// window's own gate is T3.1's, and this gate's --wait run is below.
		BypassWindow: true,
	}
	journal, err := stages.OpenJournal(journalPath, opts.Identity(recorded.BundleVersion))
	if err != nil {
		t.Fatal(err)
	}
	journal.ClusterID = clusterID

	s := &upgrade.Session{
		ID: stages.NewRunID(), Opts: opts,
		From: fromBundle, To: toBundle,
		Jnl: journal, Reporter: reporterTo(t),
		Out: testWriter{t}, API: client, Cluster: recorded, Records: records,
		// The restore-drill gate reads real evidence from the cluster, so the
		// fixture writes a real result object the real reader reads.
		Drills: upgrade.InClusterDrills{Runner: connectNodes(t, env.gateEnv)[0].Runner},
	}
	s.Emit = stages.Emitters{
		stages.TextEmitter{W: testWriter{t}},
		stages.NewControlPlaneEmitter(client, func() string { return journal.ClusterID }),
	}
	if err := s.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

// w12UpgradeJournalPath is where the CLI keeps this cluster's upgrade journal,
// under the HOME this test owns.
func w12UpgradeJournalPath(t *testing.T, env w12Env) string {
	t.Helper()
	path, err := upgrade.JournalPath(env.cluster)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// w12ApplyDrillEvidence writes the real restore-drill result object the
// upgrade's pre-flight reads. It uses pkg/backup's own names, so it cannot
// drift from what a drill actually produces.
func w12ApplyDrillEvidence(t *testing.T, ctx context.Context, r k3s.Runner) {
	t.Helper()
	result := fmt.Sprintf(`{"status":"passed","completed_at":%q,"backup":"gate-fixture","duration_seconds":42}`,
		time.Now().UTC().Format(time.RFC3339))
	doc := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  %s: '%s'
`, backup.DrillResultName, backup.Namespace, backup.DrillResultDataKey, result)
	if err := kubectlApplyDoc(ctx, r, doc); err != nil {
		t.Fatal(err)
	}
	got, err := (upgrade.InClusterDrills{Runner: r}).LastRestoreDrill(ctx)
	if err != nil {
		t.Fatalf("the gate must be able to read the evidence it refuses without: %v", err)
	}
	if got.Status != "passed" {
		t.Fatalf("read back drill status %q", got.Status)
	}
}

// w12LiveRecord reads the cluster's operation record, or nil when it holds
// none: the record IS the lock, so its absence is "nothing was held".
func w12LiveRecord(t *testing.T, ctx context.Context, r k3s.Runner) *operation.Stored {
	t.Helper()
	store := &operation.Store{Runner: r, Operator: "gate@e2e"}
	live, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("reading the operation record: %v", err)
	}
	return live
}

// w12HelmChartVersion reads one core component's pinned chart version from the
// cluster, which is where "the starting bundle's pins are back" is observable.
func w12HelmChartVersion(t *testing.T, ctx context.Context, r k3s.Runner, component string) string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, fmt.Sprintf(`get helmchart %s -n kube-system -o jsonpath='{.spec.version}'`, component))
	if err != nil {
		t.Fatalf("reading the %s HelmChart: %v", component, err)
	}
	return strings.Trim(strings.TrimSpace(out), "'")
}

// w12WaitForKubectl waits until the cluster's API answers again, which a
// datastore restore interrupts: k3s stops, restores and comes back.
func w12WaitForKubectl(t *testing.T, ctx context.Context, r k3s.Runner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := k3s.Kubectl(ctx, r, "get nodes --no-headers"); err == nil {
			return
		}
		if ctx.Err() != nil {
			t.Fatal("the test's deadline passed while waiting for the cluster to answer after the restore")
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatal("the cluster did not answer within 10 minutes of the datastore restore")
}

// w12HoldWatch records whether the cluster's operation record or kured's lock
// was held at any point, with the timestamps it observed. It is deliberately
// tolerant of read errors: a transient failure is not a lock.
type w12HoldWatch struct {
	mu     sync.Mutex
	heldAt []time.Time
	stop   chan struct{}
	done   chan struct{}
}

func w12StartHoldWatch(ctx context.Context, client *api.Client, runner k3s.Runner, clusterID string) (*w12HoldWatch, func()) {
	w := &w12HoldWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			held := false
			if live, err := client.GetClusterOperation(ctx, clusterID); err == nil && live != nil {
				held = true
			}
			store := &operation.Store{Runner: runner, Operator: "gate@e2e"}
			if live, err := store.Current(ctx); err == nil && live != nil {
				held = true
			}
			if held {
				w.mu.Lock()
				w.heldAt = append(w.heldAt, time.Now())
				w.mu.Unlock()
			}
		}
	}()
	return w, func() {
		close(w.stop)
		<-w.done
	}
}

func (w *w12HoldWatch) firstHeld() (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.heldAt) == 0 {
		return time.Time{}, false
	}
	return w.heldAt[0], true
}

func TestWorkloadUpgrade11To12(t *testing.T) {
	env := w12Environment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()

	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	_, toBundle := w12CatalogGate(t, ctx, client, env)
	w12Home(t, env)

	installedHere := false
	t.Run("the two-node workload cluster is on the starting bundle", func(t *testing.T) {
		_, existing := w12ClusterID(ctx, client, env.cluster)
		if existing != "" {
			record, err := client.BundleRecord(ctx, existing)
			if err != nil {
				t.Fatal(err)
			}
			if record.BundleVersion != env.from {
				t.Skipf("the registered cluster %s is on bundle %s, not %s: this gate proves the %s → %s hop and cannot pose it from another starting point",
					env.cluster, record.BundleVersion, env.from, env.from, env.to)
			}
			t.Logf("[%s] adopting the registered cluster %s, installed outside this test at bundle %s (the released-CLI fixture the bead asks for)", w12Now(), env.cluster, env.from)
			return
		}
		if env.skipInstall {
			t.Fatalf("KUBENEST_UPGRADE_SKIP_INSTALL is set but no cluster named %s is registered: install the fixture with the released %s CLI and register it, or unset the variable", env.cluster, env.from)
		}
		// Each host's by-id path carries its own volume's serial, so on real
		// hosts the two nodes name two different devices (kn-hku7). Without
		// the agent's, the one device is every node's, which suits a volume
		// group created by hand (no device at all) or identical kernel names.
		devices := storage.Devices{All: env.storageDevice}
		if agentDevice := os.Getenv("KUBENEST_LAB_NODE2_STORAGE_DEVICE"); env.storageDevice != "" && agentDevice != "" {
			devices = storage.Devices{PerHost: map[string]string{env.server: env.storageDevice, env.agent: agentDevice}}
		}
		opts := install.Options{
			Bundle: env.from, Name: env.cluster, HATier: "single-server",
			Servers: []string{env.server}, Agents: []string{env.agent},
			SSHUser: env.sshUser, SSHKey: env.sshKey,
			StorageDevices: devices,
		}
		s, _ := session(t, env.gateEnv, t.TempDir()+"/install.json", fetchBundle(t, client, env.from), opts)
		defer s.Close()
		if _, err := install.Execute(ctx, s, install.Plan(s)); err != nil {
			t.Fatalf("installing the fixture at %s: %v", env.from, err)
		}
		installedHere = true
		t.Logf("[%s] installed %s at bundle %s (a hardware run wanting the released-to-candidate fidelity should install this with the released %s CLI and set KUBENEST_UPGRADE_SKIP_INSTALL=1)",
			w12Now(), env.cluster, env.from, env.from)
	})
	if t.Failed() {
		return
	}

	_, clusterID := w12ClusterID(ctx, client, env.cluster)
	if clusterID == "" {
		t.Fatalf("the cluster %s is not registered to the control plane", env.cluster)
	}
	server := connectAllNodes(t, env.upgradeEnv)[0].Runner
	t.Cleanup(func() {
		if !installedHere {
			return
		}
		if os.Getenv("KUBENEST_GATE_KEEP") != "" {
			t.Logf("[%s] KUBENEST_GATE_KEEP is set: leaving the cluster up for diagnosis", w12Now())
			return
		}
		if err := uninstallAll(context.Background(), t, connectAllNodes(t, env.upgradeEnv)); err != nil {
			t.Logf("[%s] uninstall: %v", w12Now(), err)
		}
	})

	// The workload whose availability the gate measures: two replicas spread
	// over both nodes, reachable through a NodePort.
	if err := kubectlApplyDoc(ctx, server, availabilityWorkload); err != nil {
		t.Fatalf("deploying the availability workload: %v", err)
	}
	w12WaitForWorkload(t, ctx, server)
	w12ApplyDrillEvidence(t, ctx, server)

	// The readiness gate requires every node Ready for limits.timeouts
	// node-ready before an upgrade starts, so the dwell is waited out here
	// rather than weakened — the same reason TestUpgradeGate waits it.
	waitForNodeDwell(t, ctx, server, toBundle)

	// Step 1a: outside the window the upgrade is refused and names the next
	// opening in local time AND UTC. The window is stored through the real
	// command and it is closed now.
	t.Run("the upgrade outside the window is refused and names the next opening in local time and UTC", func(t *testing.T) {
		today := time.Now().In(env.zone).Weekday()
		w12SetWindow(t, env, []string{window.Name((today + 1) % 7)}, "02:00", "03:00", env.zone.String())
		stored, err := client.MaintenanceWindow(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := window.Parse(window.Spec{Days: stored.Window.Days, Start: stored.Window.Start,
			End: stored.Window.End, Timezone: stored.Window.Timezone})
		if err != nil {
			t.Fatalf("the stored window is not one the CLI can represent: %v", err)
		}
		at, ok := parsed.NextOpen(time.Now())
		if !ok {
			t.Fatal("the stored window has no opening within the next week")
		}
		local := at.In(env.zone).Format("Mon 2 Jan 15:04 MST")
		utc := at.UTC().Format("Mon 2 Jan 15:04 MST")

		var out bytes.Buffer
		err = w12RunCLI(&out, w12UpgradeArgs(env, env.to)...)
		if err == nil {
			t.Fatalf("the upgrade was NOT refused outside the window:\n%s", out.String())
		}
		refusal := err.Error() + out.String()
		if !strings.Contains(refusal, "outside the maintenance window") {
			t.Errorf("the refusal does not say now is outside the window:\n%s", refusal)
		}
		for _, want := range []string{local, utc} {
			if !strings.Contains(refusal, want) {
				t.Errorf("the refusal does not name the next opening as %q: it must name it in local time AND UTC:\n%s", want, refusal)
			}
		}
		if !strings.Contains(refusal, "--wait") {
			t.Errorf("the refusal does not name --wait as the way to hold for the window:\n%s", refusal)
		}
		if live := w12LiveRecord(t, ctx, server); live != nil {
			t.Errorf("a refused run left the operation lock held: %s", live.Record.Request.Kind)
		}
		t.Logf("[%s] refused; the next opening is %s (%s)", w12Now(), local, utc)
	})

	// Planted negative: a hop of more than one bundle is refused and names the
	// bundle to step through first (kn-mtpf's adjacency rule). Only the
	// PRE-FLIGHT stage is run, so a missing check cannot turn this test into a
	// real upgrade: pre-flight changes nothing, and the refusal is where the
	// whole run would have stopped.
	t.Run("a hop the catalog does not offer is refused and names the bundle to step through first", func(t *testing.T) {
		if _, err := client.BundleManifest(ctx, "1.0"); err != nil {
			t.Skipf("the catalog does not serve bundle 1.0 any more (%v), so the 1.0 → %s hop cannot be posed", err, env.to)
		}
		s := w12Session(t, env, client, "1.0", env.to, nil, t.TempDir()+"/adjacency.json")
		defer s.Close()
		plan := upgrade.Plan(s)
		if len(plan) == 0 || plan[0].Name != upgrade.StagePreflight {
			t.Fatalf("the plan's first stage is %v, want %s", plan, upgrade.StagePreflight)
		}
		runErr := plan[0].Run(ctx)
		if runErr == nil {
			t.Fatalf("a 1.0 → %s hop passed pre-flight, so the bundle path is listed rather than enforced: kn-mtpf's adjacency rule is missing", env.to)
		}
		refusal := runErr.Error()
		if !strings.Contains(refusal, env.from) {
			t.Errorf("the refusal does not name %s, the bundle to step through first:\n%s", env.from, refusal)
		}
		t.Logf("[%s] the 1.0 → %s hop was refused:\n%s", w12Now(), env.to, refusal)
	})

	// Step 3: a component failure BEFORE the Kubernetes stage rolls back, and
	// the rollback puts the starting bundle's pins back. The poisoned bundle is
	// a real forward manifest whose traefik pin does not exist, so the failure
	// lands in platform-components — before the point of no return, where a
	// rollback is a Helm revert.
	t.Run("a component failure before the point of no return rolls back and restores the starting bundle's pins", func(t *testing.T) {
		poisoned := fetchBundle(t, client, env.to)
		poisoned.Core["traefik"] = "0.0.0-does-not-exist"
		poisoned.Limits.Timeouts["component-ready"] = 2 * time.Minute

		s := w12Session(t, env, client, env.from, env.to, poisoned, w12UpgradeJournalPath(t, env))
		defer s.Close()
		_, runErr := stages.Execute(ctx, s, upgrade.Plan(s))
		if runErr == nil {
			t.Fatal("a bundle pinning traefik to a version that does not exist must fail")
		}
		var stageErr *stages.StageError
		if !errorsAs(runErr, &stageErr) {
			t.Fatalf("want a *StageError, got %T: %v", runErr, runErr)
		}
		if stageErr.Stage != upgrade.StageComponents {
			t.Errorf("failed at %q, want %s", stageErr.Stage, upgrade.StageComponents)
		}
		plan := s.RollbackPlan()
		t.Logf("[%s] rollback plan:\n%s", w12Now(), plan)
		if plan.Mechanism != upgrade.MechanismComponents {
			t.Fatalf("mechanism is %s; a failure before the kubernetes stage must revert components, not restore the datastore", plan.Mechanism)
		}
		if len(plan.Components) == 0 {
			t.Fatal("the plan would revert no component, so \"the pins are back\" cannot be asserted from it")
		}
		if err := s.Rollback(ctx, plan); err != nil {
			t.Fatalf("rolling back: %v", err)
		}
		// Observable on the cluster: the components are at the versions the
		// STARTING bundle pins.
		fromBundleNow := fetchBundle(t, client, env.from)
		for _, component := range []string{"traefik", "cert-manager", "kured"} {
			want, err := fromBundleNow.Core.Version(component)
			if err != nil {
				continue
			}
			if got := w12HelmChartVersion(t, ctx, server, component); got != want {
				t.Errorf("%s is pinned at %s after the rollback, want the starting bundle's %s", component, got, want)
			}
		}
		record, err := client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		if record.BundleVersion != env.from {
			t.Errorf("after the component rollback the record says %s, want %s", record.BundleVersion, env.from)
		}
		t.Logf("[%s] the component rollback put the %s pins back", w12Now(), env.from)
	})

	// Step 4: a failure AFTER the point of no return. The stage that takes the
	// cluster past it is the kubernetes stage, so the failure is injected
	// THERE: the target's real k3s pin is kept — the Plan is written and
	// system-upgrade-controller starts working — and its per-node deadline is
	// made impossible, so the stage starts and never converges. From there the
	// only way back is the datastore snapshot, which is the whole claim this
	// branch exists to make.
	t.Run("a failure after the point of no return is recovered by the datastore restore", func(t *testing.T) {
		poisoned := fetchBundle(t, client, env.to)
		poisoned.Limits.Timeouts["upgrade-per-node"] = time.Second

		s := w12Session(t, env, client, env.from, env.to, poisoned, w12UpgradeJournalPath(t, env))
		defer s.Close()
		_, runErr := stages.Execute(ctx, s, upgrade.Plan(s))
		if runErr == nil {
			t.Fatal("a kubernetes stage with a one-second per-node deadline must fail")
		}
		var stageErr *stages.StageError
		if !errorsAs(runErr, &stageErr) {
			t.Fatalf("want a *StageError, got %T: %v", runErr, runErr)
		}
		if stageErr.Stage != upgrade.StageKubernetes {
			t.Fatalf("failed at %q, want %s: this branch is the post-point-of-no-return one", stageErr.Stage, upgrade.StageKubernetes)
		}
		plan := s.RollbackPlan()
		if plan.Mechanism != upgrade.MechanismRestore {
			t.Fatalf("PlanRollback chose %s; once the kubernetes stage has started only a datastore restore goes back", plan.Mechanism)
		}
		if plan.Snapshot == "" || plan.SnapshotAt.IsZero() {
			t.Fatalf("the plan names snapshot %q taken %v: the stage-2 snapshot is what the recovery restores", plan.Snapshot, plan.SnapshotAt)
		}
		t.Logf("[%s] failed at the point of no return; the recovery plan restores snapshot %s taken %s",
			w12Now(), plan.Snapshot, plan.SnapshotAt.Format(time.RFC3339))

		// Kubernetes state written SINCE the snapshot: a ConfigMap created now.
		// The restore must lose it, which is the observable form of "what was
		// written since the snapshot".
		marker := fmt.Sprintf("gate-after-snapshot-%d", time.Now().Unix())
		if err := kubectlApplyDoc(ctx, server, fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: default
data:
  note: written after the stage-2 snapshot
`, marker)); err != nil {
			t.Fatalf("writing the post-snapshot marker: %v", err)
		}

		// WITHOUT the acknowledgement the restore must not proceed: it
		// interrupts service, and an expensive recovery that runs without being
		// asked for is worse than one that is refused.
		var out bytes.Buffer
		refuseErr := w12RunCLI(&out, w12RollbackArgs(env)...)
		if refuseErr == nil {
			t.Fatalf("the datastore restore proceeded with no acknowledgement:\n%s", out.String())
		}
		if !strings.Contains(refuseErr.Error()+out.String(), "--confirm") {
			t.Errorf("the refusal does not name the acknowledgement that unlocks the restore: %v\n%s", refuseErr, out.String())
		}
		t.Logf("[%s] the restore refused without --confirm: %v", w12Now(), refuseErr)

		// WITH the acknowledgement: the documented disaster-recovery procedure.
		var confirm bytes.Buffer
		started := time.Now()
		runErr = w12RunCLI(&confirm, w12RollbackArgs(env, "--confirm")...)
		recovery := time.Since(started)
		if runErr != nil {
			t.Fatalf("the datastore restore failed after %s: %v\n%s", recovery.Round(time.Second), runErr, confirm.String())
		}
		report := confirm.String()
		if !strings.Contains(report, "RESTORING THE DATASTORE SNAPSHOT") {
			t.Errorf("the run does not report which mechanism it used:\n%s", report)
		}
		if !strings.Contains(report, plan.Snapshot) {
			t.Errorf("the report does not name the snapshot it restored (%s):\n%s", plan.Snapshot, report)
		}
		if !strings.Contains(report, plan.SnapshotAt.Format(time.RFC3339)) {
			t.Errorf("the report does not name when the snapshot was taken (%s):\n%s", plan.SnapshotAt.Format(time.RFC3339), report)
		}
		t.Logf("[%s] RECOVERY TIME %s (snapshot %s taken %s)", w12Now(), recovery.Round(time.Second), plan.Snapshot, plan.SnapshotAt.Format(time.RFC3339))

		w12WaitForKubectl(t, ctx, server)
		if res, err := server.Run(ctx, fmt.Sprintf("sudo -n k3s kubectl get configmap %s -n default -o name", marker)); err == nil && res.ExitCode == 0 {
			t.Errorf("the marker ConfigMap %s survived the datastore restore, so the cluster was NOT returned to the snapshot", marker)
		} else {
			t.Logf("[%s] the marker ConfigMap written after the snapshot is gone, as the restore reported", w12Now())
		}
		record, err := client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		if record.BundleVersion != env.from {
			t.Errorf("the record says %s after the recovery, want %s: a record that claims a version the cluster is not on is worse than no record", record.BundleVersion, env.from)
		}
		// A datastore restore rolls back Kubernetes objects, not data: the
		// workload's Deployment is pre-snapshot and must be back.
		out2, err := k3s.Kubectl(ctx, server, "get deployment always-up -n gate-availability -o jsonpath='{.status.readyReplicas}'")
		if err != nil {
			t.Fatalf("the workload is gone after the datastore restore: %v", err)
		}
		if got := strings.Trim(strings.TrimSpace(out2), "'"); got != "2" {
			t.Errorf("after the recovery the workload has %q ready replicas, want 2", got)
		}
	})

	// Steps 1b and 2: a `--wait` run from before the window opens, holding
	// nothing while it waits, performing the clean hop, with the workload
	// measured throughout.
	t.Run("with --wait it holds nothing while it waits, starts at the opening, and the workload never left its limits", func(t *testing.T) {
		opening := w12StoreOpenWindow(t, env)
		if wait := time.Until(opening); wait < 30*time.Second || wait > 3*time.Minute {
			t.Fatalf("the window opens in %s, want about two minutes", wait.Round(time.Second))
		}
		watch, stopWatch := w12StartHoldWatch(ctx, client, server, clusterID)
		defer stopWatch()

		probe := pollWorkload(t, server, []string{env.server + ":30080", env.agent + ":30080"})
		writer := &t31StampingWriter{}
		started := time.Now()
		runErr := w12RunCLI(writer, w12UpgradeArgs(env, env.to, "--wait")...)
		elapsed := time.Since(started)
		attempts, failures := probe.result()
		stopWatch()

		if writer.enteredAt.IsZero() {
			t.Fatalf("the --wait run never reported entering the window:\n%s", writer.String())
		}
		if early := opening.Sub(writer.enteredAt); early > 5*time.Second {
			t.Errorf("the run reported `inside` %s before the window opened", early.Round(time.Second))
		}
		if late := writer.enteredAt.Sub(opening); late > 5*time.Minute {
			t.Errorf("the run entered the window %s after the opening", late.Round(time.Second))
		}
		if first, ok := watch.firstHeld(); ok && first.Before(opening.Add(-5*time.Second)) {
			t.Errorf("the operation record was held at %s, %s before the window opened: a --wait run must hold NOTHING while it waits",
				first.Format(time.RFC3339), opening.Sub(first).Round(time.Second))
		}
		if !strings.Contains(writer.String(), "Nothing is held while this waits") {
			t.Errorf("the run does not say it holds nothing while waiting:\n%s", writer.String())
		}
		if runErr != nil {
			t.Fatalf("the clean %s → %s upgrade failed after %s:\n%v\n%s", env.from, env.to, elapsed.Round(time.Second), runErr, writer.String())
		}
		t.Logf("[%s] the %s → %s upgrade completed in %s", w12Now(), env.from, env.to, elapsed.Round(time.Second))

		record, err := client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		if record.BundleVersion != env.to {
			t.Errorf("the record says %s after the upgrade, want %s: nothing in the day-2 story is trustworthy if this drifts", record.BundleVersion, env.to)
		}
		want, err := toBundle.Core.Version("k3s")
		if err != nil {
			t.Fatal(err)
		}
		out, err := k3s.Kubectl(ctx, server, `get nodes -o jsonpath='{.items[*].status.nodeInfo.kubeletVersion}'`)
		if err != nil {
			t.Fatal(err)
		}
		versions := strings.Fields(strings.Trim(out, "'"))
		if len(versions) != 2 {
			t.Fatalf("expected two nodes, got %v", versions)
		}
		for _, got := range versions {
			if got != want {
				t.Errorf("a node runs %s, want %s", got, want)
			}
		}

		// The workload measurement, and the Kubernetes API's own reading
		// SEPARATELY: a node answering its own API server is not the same claim
		// as a customer reaching the workload, and merging them hides which one
		// failed.
		t.Logf("[%s] the outside probe polled both nodes' NodePort once a second, retries off: %d attempts", w12Now(), attempts)
		if attempts < 30 {
			t.Errorf("only %d probes in %s: too few to claim anything about availability", attempts, elapsed.Round(time.Second))
		}
		if len(failures) > 0 {
			t.Errorf("THE WORKLOAD WAS UNAVAILABLE during the upgrade: %d of %d probes failed:\n  %s",
				len(failures), attempts, strings.Join(failures, "\n  "))
		}
		apiOut, err := k3s.Kubectl(ctx, server, "get deployment always-up -n gate-availability -o jsonpath='{.status.readyReplicas}'")
		if err != nil {
			t.Fatalf("reading the workload from the Kubernetes API: %v", err)
		}
		t.Logf("[%s] the Kubernetes API separately reports %q ready replicas", w12Now(), strings.Trim(strings.TrimSpace(apiOut), "'"))
	})
}

// w12ClusterID resolves a cluster name to its control-plane id, reporting
// "not registered" rather than failing.
func w12ClusterID(ctx context.Context, client *api.Client, name string) (string, string) {
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		return "", ""
	}
	for _, org := range orgs {
		clusters, err := client.ListOrgClusters(ctx, org.ID)
		if err != nil {
			return "", ""
		}
		for _, c := range clusters {
			if c.Name == name {
				return org.ID, c.ID
			}
		}
	}
	return "", ""
}

// w12WaitForWorkload waits until both replicas report ready, so "the workload
// exists" is a fact before anything measures it.
func w12WaitForWorkload(t *testing.T, ctx context.Context, r k3s.Runner) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := k3s.Kubectl(ctx, r,
			"get deployment always-up -n gate-availability -o jsonpath='{.status.readyReplicas}'")
		if err == nil && strings.Trim(strings.TrimSpace(out), "'") == "2" {
			t.Logf("[%s] both replicas of the availability workload are ready", w12Now())
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatal("the workload never became ready, so there is nothing to measure")
}
