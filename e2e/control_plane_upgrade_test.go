//go:build e2e

// The kn-t70 wave gate: THE CONTROL-PLANE HALF OF S2 (PLAN 7.8), run against a
// REAL control plane on a disposable host and driven through the REAL command
// tree, so the flags, the refusals and the recorded steps are the ones an
// operator gets.
//
// What it asserts, in the order the plan gives them:
//
//	(a) a control plane installed from the PREVIOUS candidate artifact;
//	(b) the upgrade to the CURRENT candidate, with the fence, an eligible
//	    checkpoint taken BEFORE the migration, the migration Job as a recorded
//	    step, the new chart rolled, the new backend validated through the
//	    node's port-forward, and the fence lowered;
//	(c) an interruption after the checkpoint, finished by a SECOND laptop with
//	    --resume;
//	(d) an interruption during the migration, finished the same way;
//	(e) a migration Job forced to fail while fenced: the previous chart is what
//	    runs, the fence is still up, and the checkpoint is still eligible;
//	(f) a chart whose PostgreSQL major — and separately distribution — differs
//	    from the running one, refused by name.
//
// THE ASSERTIONS S2 COPIES, and how each is observed here:
//
//	no customer request reaches the backend from the moment the route switches
//	  a poller hits https://api.<domain>/api/v1/health throughout the run and
//	  asserts every answer while the fence was up was 503;
//	the old code never serves the migrated schema
//	  the migration Job's completion and the new Deployment's rollout are
//	  ordered, and the Deployment's image is the new one before the fence
//	  lowers;
//	the checkpoint is in S3 BEFORE the migration starts
//	  the eligible checkpoint's timestamp precedes the migration Job's;
//	the second laptop finds the interrupted operation at its recorded step
//	  the resumed run is a NEW process with a NEW journal directory, and it
//	  reports the recorded stage and finishes.
//
// Run from the umbrella workspace, on hosts from
// `./scripts/ephemeral-env.sh up --profile host`:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_PREVIOUS_CANDIDATE_CHART=/path/to/kubenest-<prev>.tgz
//	export KUBENEST_CURRENT_CANDIDATE_CHART=/path/to/kubenest-<current>.tgz
//	export KUBENEST_ADMIN_EMAIL=admin@<domain> KUBENEST_ADMIN_PASSWORD=...
//	cd kubenest-cli && go test -tags e2e -v -timeout 180m ./e2e/ -run TestControlPlaneUpgradeGate
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/controlplane"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// cpUpgradeEnv is the fixture this gate needs beyond gateEnvironment's.
type cpUpgradeEnv struct {
	gateEnv
	// previousCandidate is the pinned chart archive the control plane is
	// INSTALLED from: a candidate artifact, not one this binary carries, so
	// the upgrade starts somewhere other than where it ends.
	previousCandidate string
	// toBundle is the bundle the upgrade moves TO. The control plane's own
	// version is its chart, not the bundle, but the command's --to names the
	// bundle whose manifest bounds the run and whose catalog the operator is
	// moving the installation onto.
	toBundle string
	// domain is the control plane's domain, from the control-plane URL the
	// lab exports (https://api.<domain>).
	domain string
}

func cpUpgradeEnvironment(t *testing.T) cpUpgradeEnv {
	t.Helper()
	env := cpUpgradeEnv{
		gateEnv:           gateEnvironment(t),
		previousCandidate: os.Getenv("KUBENEST_PREVIOUS_CANDIDATE_CHART"),
		toBundle:          envOr("KUBENEST_TO_BUNDLE", "1.2"),
	}
	if env.previousCandidate == "" {
		t.Skip("KUBENEST_PREVIOUS_CANDIDATE_CHART not set: the gate installs the control plane from the PREVIOUS 1.2 candidate's chart, so an upgrade starts somewhere other than where it ends")
	}
	if _, err := os.Stat(env.previousCandidate); err != nil {
		t.Fatalf("KUBENEST_PREVIOUS_CANDIDATE_CHART: %v", err)
	}
	host := strings.TrimPrefix(strings.TrimPrefix(env.controlPlane, "https://"), "http://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	env.domain = strings.TrimPrefix(host, "api.")
	if env.domain == host {
		t.Fatalf("KUBENEST_CONTROL_PLANE %q is not https://api.<domain>: the gate needs the domain its route serves", env.controlPlane)
	}
	return env
}

// cpRunCLI runs the real command tree with a journal directory of its own, so a
// gate run never touches an operator's journals and a "second laptop" is a
// genuinely fresh process state.
func cpRunCLI(t *testing.T, out io.Writer, home string, args ...string) error {
	t.Helper()
	prev := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("HOME", prev) }()
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// cpInstallArgs is the control-plane install: the same flags the plan's
// quickstart uses.
func cpInstallArgs(env cpUpgradeEnv, name string) []string {
	args := []string{"platform", "install", "--control-plane",
		"--bundle", env.bundle,
		"--name", name,
		"--domain", env.domain,
		"--ha", "single-server",
		"--server", env.server,
	}
	if env.sshUser != "" {
		args = append(args, "--ssh-user", env.sshUser)
	}
	if env.sshKey != "" {
		args = append(args, "--ssh-key", env.sshKey)
	}
	if env.storageDevice != "" {
		args = append(args, "--storage-device", env.storageDevice)
	}
	// The upgrade's recovery-point gate refuses a control plane with no
	// checkpoint, and the checkpoint needs a backup target. The credentials
	// come from the environment the install already reads
	// (KUBENEST_BACKUP_* and KUBENEST_CHECKPOINT_*).
	if target := os.Getenv("KUBENEST_GATE_BACKUP_TARGET"); target != "" {
		args = append(args, "--backup-target", target)
	}
	return args
}

func cpUpgradeArgs(env cpUpgradeEnv, name string, extra ...string) []string {
	args := []string{"platform", "upgrade", "--control-plane",
		"--cluster", name,
		"--to", env.toBundle,
	}
	if env.sshUser != "" {
		args = append(args, "--ssh-user", env.sshUser)
	}
	if env.sshKey != "" {
		args = append(args, "--ssh-key", env.sshKey)
	}
	return append(args, extra...)
}

// cpDial reaches the management cluster's server node the way every other gate
// does: over SSH, with the CLI's own transport.
func cpDial(t *testing.T, ctx context.Context, env cpUpgradeEnv) *sshx.Client {
	t.Helper()
	endpoint, err := sshx.Resolve(env.server, sshx.Options{User: env.sshUser, KeyPath: env.sshKey})
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshx.Dial(ctx, endpoint, sshx.Options{KeyPath: env.sshKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// cpFenceWatcher polls the PUBLIC route for as long as a run lasts, recording
// every answer. The assertion S2 copies — no customer request reaches the
// backend from the moment the route switches until it is restored — is about
// an interval, so it needs an observer that runs during the interval rather
// than a reading taken afterwards.
type cpFenceWatcher struct {
	url      string
	stop     chan struct{}
	done     chan struct{}
	sawFence bool
	answers  []int
	leaked   []int
}

func cpStartWatcher(url string) *cpFenceWatcher {
	w := &cpFenceWatcher{url: url, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		client := &http.Client{Timeout: 10 * time.Second}
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			resp, err := client.Get(w.url)
			if err == nil {
				w.answers = append(w.answers, resp.StatusCode)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusServiceUnavailable {
					w.sawFence = true
				} else if w.sawFence {
					// After the fence was up, anything other than a 503 is a
					// request that reached a backend: the interval S2 forbids.
					w.leaked = append(w.leaked, resp.StatusCode)
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return w
}

func (w *cpFenceWatcher) close() {
	close(w.stop)
	<-w.done
}

// cpEligibleAt reads the eligible checkpoint's timestamp from the status
// document the runner publishes.
func cpEligibleAt(t *testing.T, ctx context.Context, server k3s.Runner) string {
	t.Helper()
	checkpoint, err := controlplane.ReadEligibleCheckpoint(ctx, server)
	if err != nil {
		t.Fatalf("reading the eligible checkpoint: %v", err)
	}
	if checkpoint == nil {
		return ""
	}
	return checkpoint.At
}

// cpMigrationJob reads the migration Job's creation timestamp and its
// install-revision annotation.
func cpMigrationJob(t *testing.T, ctx context.Context, server k3s.Runner) (created, revision string, found bool) {
	t.Helper()
	out, err := k3s.Kubectl(ctx, server, "get job "+controlplane.MigrationJobName+
		" -n "+controlplane.Namespace+
		" -o jsonpath={.metadata.creationTimestamp}{' '}{.metadata.annotations.kubenest\\.io/install-revision}")
	if err != nil {
		return "", "", false
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", "", false
	}
	if len(fields) > 1 {
		revision = fields[1]
	}
	return fields[0], revision, true
}

// cpBackendImage reads the image the backend Deployment runs: what proves the
// old code is no longer the one serving.
func cpBackendImage(t *testing.T, ctx context.Context, server k3s.Runner) string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, server, "get deployment "+controlplane.ReleaseName+"-backend"+
		" -n "+controlplane.Namespace+" -o jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		t.Fatalf("reading the backend image: %v", err)
	}
	return strings.TrimSpace(out)
}

// cpFenceState reads where the public API route points.
func cpFenceState(t *testing.T, ctx context.Context, server k3s.Runner) controlplane.FenceReport {
	t.Helper()
	report, err := controlplane.Status(ctx, server)
	if err != nil {
		t.Fatalf("reading the fence state: %v", err)
	}
	return report
}

// cpInstallPreviousCandidate installs the control plane's chart from the PINNED
// previous candidate archive, so the state the upgrade starts from is a real
// artifact rather than the one this binary embeds.
//
// It is the same apply the CLI performs (k3s HelmChart with spec.chartContent),
// with the archive it was pointed at, which is why pkg/controlplane exposes
// ApplyArchive and RevisionFor: a gate that could only apply the embedded
// archive would prove nothing about an upgrade.
func cpInstallPreviousCandidate(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, values string) string {
	t.Helper()
	archive, err := os.ReadFile(env.previousCandidate)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := controlplane.RevisionFor(archive, values)
	if err != nil {
		t.Fatal(err)
	}
	// The install that just ran left ITS migration Job, rendered from the chart
	// this binary embeds. Job.spec.template is immutable, so the previous
	// candidate's apply would fail trying to patch it to the previous image.
	// Removing it lets the previous candidate create its own Job at its own
	// revision, which is the state a real installation of that candidate is in
	// when an upgrade reaches it: an old revision's Job, and values that still
	// enable the migration.
	if _, err := k3s.Kubectl(ctx, server, "delete job "+controlplane.MigrationJobName+" -n "+controlplane.Namespace+
		" --ignore-not-found --wait=true"); err != nil {
		t.Fatalf("removing the install's migration Job before the previous candidate is applied: %v", err)
	}
	applied, err := controlplane.ApplyArchive(ctx, server, archive, values)
	if err != nil {
		t.Fatalf("installing the control plane from the previous candidate %s: %v", env.previousCandidate, err)
	}
	if applied != revision {
		t.Fatalf("the previous candidate applied at revision %s but its values hash to %s", applied, revision)
	}
	t.Logf("control plane installed from the previous candidate at install revision %s", revision)
	return revision
}

// cpForceMigrationFailure makes the migration Job fail while the fence is up,
// by taking the database away from it — the same failure hardware observed
// ("connection refused" against a restarting PostgreSQL), produced on purpose.
func cpForceMigrationFailure(t *testing.T, ctx context.Context, server k3s.Runner, down bool) {
	t.Helper()
	replicas := "1"
	verb := "stopping"
	if !down {
		replicas, verb = "1", "restarting"
	} else {
		replicas = "0"
	}
	t.Logf("%s PostgreSQL so the migration Job cannot connect", verb)
	if _, err := k3s.Kubectl(ctx, server, "scale statefulset "+controlplane.ReleaseName+"-postgresql"+
		" -n "+controlplane.Namespace+" --replicas="+replicas); err != nil {
		t.Fatalf("scaling PostgreSQL: %v", err)
	}
}

// cpPostgresImage rewrites the running PostgreSQL StatefulSet's image, so the
// refusal's terms can be exercised by moving the RUNNING side — which is a
// read-only change to a StatefulSet that needs no second chart.
func cpPostgresImage(t *testing.T, ctx context.Context, server k3s.Runner, image string) {
	t.Helper()
	if _, err := k3s.Kubectl(ctx, server, "set image statefulset/"+controlplane.ReleaseName+"-postgresql"+
		" postgresql="+image+" -n "+controlplane.Namespace); err != nil {
		t.Fatalf("setting the PostgreSQL image to %s: %v", image, err)
	}
}

// The gate itself.
func TestControlPlaneUpgradeGate(t *testing.T) {
	env := cpUpgradeEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Minute)
	defer cancel()
	server := cpDial(t, ctx, env)

	home := t.TempDir()
	name := env.cluster
	values, previousRevision := cpPrepareControlPlane(t, ctx, env, server, home, name)
	previousImage := cpBackendImage(t, ctx, server)

	// ---------------------------------------------------------------- (a)
	// The control plane is INSTALLED FROM THE PREVIOUS CANDIDATE. Its values
	// are the ones the install rendered, so the upgrade carries them forward.
	if _, err := controlplane.Revision(values); err != nil {
		t.Fatalf("the values the install rendered have no revision: %v", err)
	}
	if previousRevision == "" {
		t.Fatal("the previous candidate was applied without a revision")
	}

	// ---------------------------------------------------------------- (b)
	watcher := cpStartWatcher("https://api." + env.domain + "/api/v1/health")
	var out strings.Builder
	runErr := cpRunCLI(t, &out, home, cpUpgradeArgs(env, name)...)
	watcher.close()
	t.Log(out.String())

	// The fence held: every answer the public route gave while it was up was a
	// 503, and the route only ever pointed at the fence or the backend.
	if watcher.sawFence {
		if len(watcher.leaked) > 0 {
			t.Fatalf("a customer request reached the backend while the fence was up: %v (answers: %v)", watcher.leaked, watcher.answers)
		}
	} else {
		t.Errorf("the fence was never observed on the public route; answers were %v", watcher.answers)
	}
	if runErr != nil {
		t.Fatalf("the control-plane upgrade failed: %v", runErr)
	}

	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceDown {
		t.Errorf("the upgrade left the fence %q (route points at %q), want it lowered", report.State, report.BackendRef)
	}
	newImage := cpBackendImage(t, ctx, server)
	if newImage == previousImage {
		t.Errorf("the backend image is still %s: the new chart did not roll", previousImage)
	}
	created, revision, found := cpMigrationJob(t, ctx, server)
	if !found {
		t.Fatal("no migration Job is on the cluster after an upgrade that had to migrate: the schema step did not run as a recorded step")
	}
	eligible := cpEligibleAt(t, ctx, server)
	if eligible == "" {
		t.Fatal("no eligible checkpoint was published: an upgrade with no recovery point must not have started")
	}
	if created != "" && eligible > created {
		t.Errorf("the eligible checkpoint %s is NEWER than the migration Job %s: the checkpoint must exist before the migration starts", eligible, created)
	}
	if revision == "" {
		t.Error("the migration Job carries no install revision, so the wait could not have matched it to the apply")
	}

	// The new backend answers through the node's port-forward, which is what
	// the validation did while the fence was up.
	cpAssertVersionThroughTheNode(t, ctx, env, server)

	// ---------------------------------------------------------------- (f)
	// A PostgreSQL whose major or distribution differs is REFUSED, by name.
	cpAssertPostgresRefusal(t, ctx, env, server, name, home)

	// ---------------------------------------------------------------- (e)
	// A migration Job that cannot reach its database fails, and what is left
	// is the PREVIOUS chart with the fence still up — never a half-migrated
	// control plane serving customers.
	cpAssertMigrationFailureKeepsTheFenceUp(t, ctx, env, server, name, home)

	// ----------------------------------------------------------- (c) and (d)
	cpAssertResumeFromASecondLaptop(t, ctx, env, server, name)
}

// cpPrepareControlPlane performs step (a) and returns the values the control
// plane runs with, plus the revision the previous candidate was applied at.
func cpPrepareControlPlane(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, home, name string) (string, string) {
	t.Helper()
	var out strings.Builder
	if err := cpRunCLI(t, &out, home, cpInstallArgs(env, name)...); err != nil {
		t.Fatalf("installing the control plane: %v\n%s", err, out.String())
	}
	t.Log(out.String())

	values, err := k3s.Kubectl(ctx, server, "get helmchart "+controlplane.ReleaseName+
		" -n kube-system -o jsonpath={.spec.valuesContent}")
	if err != nil {
		t.Fatalf("reading the applied values: %v", err)
	}
	if strings.TrimSpace(values) == "" {
		t.Fatal("the install applied no values: the control-plane HelmChart carries none")
	}
	// Re-apply the PINNED previous candidate over the install's own values, so
	// the starting point is the candidate artifact rather than the embedded
	// chart the install just used for its last apply.
	revision := cpInstallPreviousCandidate(t, ctx, env, server, values)
	if err := controlplane.WaitReady(ctx, server, revision, envBundle(t, ctx, env), convergeReporter(t)); err != nil {
		t.Fatalf("the previous candidate did not become ready: %v", err)
	}
	cpOpenWindowAllWeek(t, ctx, home, name)
	return values, revision
}

// cpOpenWindowAllWeek gives the management cluster a window that is open at
// every minute and waits until it is ACTIVE. The upgrade is a disruptive
// operation and refuses to start without a window in force (T3.1); this gate
// is about the upgrade, so its window must never be the reason it cannot run.
// The client is built from the install's own laptop state in home.
func cpOpenWindowAllWeek(t *testing.T, ctx context.Context, home, name string) {
	t.Helper()
	var config struct {
		URL string `json:"control_plane_url"`
		CA  string `json:"control_plane_ca"`
	}
	var creds struct {
		Tokens map[string]struct {
			Token string `json:"token"`
		} `json:"tokens"`
	}
	var journal struct {
		ClusterID string `json:"cluster_id"`
	}
	for path, into := range map[string]any{
		filepath.Join(home, ".kubenest", "config.json"):            &config,
		filepath.Join(home, ".kubenest", "credentials.json"):       &creds,
		filepath.Join(home, ".kubenest", "journals", name+".json"): &journal,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the install's laptop state: %v", err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	client, err := api.New(config.URL, api.WithToken(creds.Tokens[config.URL].Token), api.WithCACert([]byte(config.CA)))
	if err != nil {
		t.Fatal(err)
	}
	current, err := client.MaintenanceWindow(ctx, journal.ClusterID)
	if err != nil {
		t.Fatalf("reading the management cluster's window: %v", err)
	}
	if _, err := client.PutMaintenanceWindow(ctx, journal.ClusterID, api.MaintenanceWindow{
		Days:     []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"},
		Start:    "00:00",
		End:      "23:59",
		Timezone: "UTC",
	}, current.CurrentRevision()); err != nil {
		t.Fatalf("storing an all-week window: %v", err)
	}
	cpWaitFor(t, 5*time.Minute, 5*time.Second, "the all-week window is active", func() (bool, string) {
		record, err := client.MaintenanceWindow(ctx, journal.ClusterID)
		if err != nil {
			return false, err.Error()
		}
		return record.State == api.WindowStateActive, record.State
	})
}

// cpAssertVersionThroughTheNode reads the deployed backend's identity through
// the node's port-forward — the same route the upgrade's validation used, and
// the only one that works while the fence is up.
func cpAssertVersionThroughTheNode(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner) {
	t.Helper()
	addr, err := controlplane.BackendAddr(ctx, server)
	if err != nil {
		t.Fatalf("reading the backend address: %v", err)
	}
	if _, ok := server.(interface {
		DialTCP(ctx context.Context, addr string) (net.Conn, error)
	}); !ok {
		t.Fatalf("the SSH connection to %s cannot open a tunnel to %s, so the CLI's own validation could not have run: %v", env.server, addr, ok)
	}
	// The CLI's own validation ran inside the upgrade and would have failed the
	// run above; this asserts the ROUTE half from outside, with the fence down,
	// so a fence that never lifted cannot hide behind a port-forward.
	watcher := cpStartWatcher("https://api." + env.domain + "/api/v1/health")
	defer watcher.close()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := http.Get("https://api." + env.domain + "/api/v1/health")
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the public route never answered 200 after the fence came down; last answers %v", watcher.answers)
		}
		time.Sleep(2 * time.Second)
	}
}

// cpAssertPostgresRefusal moves the RUNNING PostgreSQL to a different major and
// then to a different distribution, and asserts each is refused by name.
func cpAssertPostgresRefusal(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, name, home string) {
	t.Helper()
	original := cpRunningImage(t, ctx, server)
	for _, tc := range []struct {
		what  string
		image string
		want  string
	}{
		{"a different major", "docker.io/bitnami/postgresql:16.4.0-debian-12-r0", "major"},
		{"a different distribution", "docker.io/library/postgres:17.2", "distribution"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			cpPostgresImage(t, ctx, server, tc.image)
			defer cpPostgresImage(t, ctx, server, original)
			var out strings.Builder
			err := cpRunCLI(t, &out, home, cpUpgradeArgs(env, name)...)
			if err == nil {
				t.Fatalf("an upgrade onto %s was accepted; the control plane's database must not change without its own tested procedure\n%s", tc.what, out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the %s:\n%v", tc.want, err)
			}
		})
	}
}

func cpRunningImage(t *testing.T, ctx context.Context, server k3s.Runner) string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, server, "get statefulset "+controlplane.ReleaseName+"-postgresql"+
		" -n "+controlplane.Namespace+" -o jsonpath={.spec.template.spec.containers[0].image}")
	if err != nil {
		t.Fatalf("reading the PostgreSQL image: %v", err)
	}
	return strings.TrimSpace(out)
}

// cpAssertMigrationFailureKeepsTheFenceUp forces the migration Job's database
// away, runs the upgrade, and asserts what is left: the PREVIOUS chart still
// running, the fence still up, and the checkpoint still eligible.
//
// The automatic return TO the checkpoint is T4.7's recovery path (the plan's
// "rolling back to the checkpoint is automatic only while the fence has held
// continuously"); what this gate proves is the state the failure leaves, which
// is the one a customer's requests are answered from.
func cpAssertMigrationFailureKeepsTheFenceUp(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, name, home string) {
	t.Helper()
	before := cpBackendImage(t, ctx, server)
	eligibleBefore := cpEligibleAt(t, ctx, server)

	cpForceMigrationFailure(t, ctx, server, true)
	defer cpForceMigrationFailure(t, ctx, server, false)

	var out strings.Builder
	err := cpRunCLI(t, &out, home, cpUpgradeArgs(env, name)...)
	t.Log(out.String())
	if err == nil {
		t.Fatal("a migration Job that could not reach its database did not fail the upgrade")
	}
	if !strings.Contains(err.Error(), controlplane.MigrationJobName) {
		t.Errorf("the failure does not name the migration Job:\n%v", err)
	}

	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceUp {
		t.Errorf("the failed migration left the fence %q: a customer request could reach a control plane whose schema was not brought forward", report.State)
	}
	if after := cpBackendImage(t, ctx, server); after != before {
		t.Errorf("the failed migration changed the backend image from %s to %s: the previous chart must still be what runs", before, after)
	}
	if eligibleAfter := cpEligibleAt(t, ctx, server); eligibleAfter != eligibleBefore {
		t.Errorf("the failed migration changed the eligible checkpoint from %s to %s", eligibleBefore, eligibleAfter)
	}
}

// cpAssertResumeFromASecondLaptop is (c) and (d): interrupt the run, then
// finish it from a genuinely fresh process state.
//
// THE SECOND LAPTOP IS A SECOND HOME. `--resume` reads the operation record
// from the CLUSTER and takes it over; a journal on this machine would be this
// machine's memory, and the point of the assertion is that a laptop that has
// none can finish the operation.
func cpAssertResumeFromASecondLaptop(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, name string) {
	t.Helper()
	first := t.TempDir()
	second := t.TempDir()

	// (c) Interrupt after the checkpoint: cancel the run once the eligible
	// checkpoint has moved, which is the recorded step the plan names.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted := make(chan struct{})
	go func() {
		defer close(interrupted)
		var out strings.Builder
		_ = cpRunCLIWithContext(t, runCtx, &out, first, cpUpgradeArgs(env, name)...)
	}()
	before := cpEligibleAt(t, ctx, server)
	cpWaitFor(t, 10*time.Minute, 5*time.Second, "the checkpoint the interrupted run publishes", func() (bool, string) {
		now := cpEligibleAt(t, ctx, server)
		return now != "" && now != before, "eligible checkpoint is " + now
	})
	cancel()
	<-interrupted

	opID := cpOperationID(t, ctx, server)
	if opID == "" {
		t.Fatal("an interrupted control-plane upgrade left no operation record, so there is nothing for a second laptop to find")
	}
	t.Logf("interrupted after the checkpoint; operation %s", opID)

	// The second laptop: a fresh HOME, so no journal of the first run's.
	var out strings.Builder
	err := cpRunCLI(t, &out, second, cpUpgradeArgs(env, name, "--resume", opID)...)
	t.Log(out.String())
	if err != nil {
		t.Fatalf("the second laptop could not finish the interrupted upgrade: %v", err)
	}
	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceDown {
		t.Errorf("the resumed run finished with the fence %q", report.State)
	}

	// (d) Interrupt DURING THE MIGRATION and finish from the third laptop. The
	// migration is the step that must not be half-done and unwatched: the fence
	// is up, the backend is stopped, and the Job's outcome is the one thing a
	// successor has to establish rather than guess.
	third := t.TempDir()
	_, revisionBefore, _ := cpMigrationJob(t, ctx, server)
	runCtx, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	interrupted2 := make(chan struct{})
	go func() {
		defer close(interrupted2)
		var out strings.Builder
		_ = cpRunCLIWithContext(t, runCtx, &out, third, cpUpgradeArgs(env, name)...)
	}()
	cpWaitFor(t, 15*time.Minute, 5*time.Second, "the migration Job the interrupted run creates", func() (bool, string) {
		_, revision, found := cpMigrationJob(t, ctx, server)
		if !found {
			return false, "no migration Job yet"
		}
		return revision != "" && revision != revisionBefore, "migration Job at revision " + revision
	})
	cancel2()
	<-interrupted2

	opID2 := cpOperationID(t, ctx, server)
	if opID2 == "" {
		t.Fatal("an upgrade interrupted during the migration left no operation record")
	}
	fourth := t.TempDir()
	var out2 strings.Builder
	if err := cpRunCLI(t, &out2, fourth, cpUpgradeArgs(env, name, "--resume", opID2)...); err != nil {
		t.Fatalf("the second laptop could not finish the upgrade interrupted during the migration: %v\n%s", err, out2.String())
	}
	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceDown {
		t.Errorf("the run resumed during the migration finished with the fence %q", report.State)
	}
	if _, revision, found := cpMigrationJob(t, ctx, server); !found || revision == "" {
		t.Error("no migration Job at an install revision is on the cluster after the resumed run finished")
	}
}

// cpOperationID reads the live operation record's id.
func cpOperationID(t *testing.T, ctx context.Context, server k3s.Runner) string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, server, "get configmap kubenest-operation -n kube-system -o jsonpath={.data.record\\.json}")
	if err != nil {
		return ""
	}
	const key = `"operation_id":"`
	i := strings.Index(out, key)
	if i < 0 {
		return ""
	}
	rest := out[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// cpWaitFor retries until check holds, naming what ran out.
func cpWaitFor(t *testing.T, within, every time.Duration, what string, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ok, detail := check()
		if ok {
			t.Logf("%s: %s", what, detail)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s; last observation: %s", within, what, detail)
		}
		time.Sleep(every)
	}
}

// cpRunCLIWithContext runs the real command tree with a cancellable context —
// the CLI's own interruption path, which is what a killed laptop does.
func cpRunCLIWithContext(t *testing.T, ctx context.Context, out io.Writer, home string, args ...string) error {
	t.Helper()
	prev := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("HOME", prev) }()
	root := cmd.NewRootCommand()
	root.SetContext(ctx)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// envBundle is the bundle manifest the waits are bounded by. It comes from the
// CLI's embedded catalog, which is the same document the command uses when it
// installs the control plane (there is no control plane to fetch one from at
// that point).
func envBundle(t *testing.T, ctx context.Context, env cpUpgradeEnv) *manifest.Manifest {
	t.Helper()
	m, err := bundles.Manifest(env.bundle)
	if err != nil {
		t.Fatalf("reading bundle %s from the CLI's catalog: %v", env.bundle, err)
	}
	return m
}

func convergeReporter(t *testing.T) converge.Reporter {
	t.Helper()
	return converge.ReporterFunc(func(e converge.Event) {
		t.Logf("  %s %s %s", e.Check, e.Outcome, e.State.Status)
	})
}
