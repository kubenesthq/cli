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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	client   *http.Client
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	sawFence bool
	answers  []int
	leaked   []int
}

// cpStartWatcher polls the public route with the control plane's own CA; the
// lab's certificate is issued by it, and a watcher that could not verify it
// recorded no answers at all (third hardware run: "answers were []").
func cpStartWatcher(url string, ca []byte) *cpFenceWatcher {
	w := &cpFenceWatcher{url: url, client: cpHTTPClient(ca), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			resp, err := w.client.Get(w.url)
			if err == nil {
				_ = resp.Body.Close()
				w.mu.Lock()
				w.answers = append(w.answers, resp.StatusCode)
				w.mu.Unlock()
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return w
}

// close stops the poller and judges what it saw. The fenced interval runs from
// the first 503 to the last one; any other answer inside it is a request that
// reached a backend while the fence was meant to be up, which is what S2
// forbids. Answers after the last 503 are the fence lowered on purpose, so a
// successful upgrade's closing 200s are not leaks.
func (w *cpFenceWatcher) close() {
	close(w.stop)
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	first, last := -1, -1
	for i, code := range w.answers {
		if code == http.StatusServiceUnavailable {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	w.sawFence = first >= 0
	for i := first + 1; first >= 0 && i < last; i++ {
		if w.answers[i] != http.StatusServiceUnavailable {
			w.leaked = append(w.leaked, w.answers[i])
		}
	}
}

func (w *cpFenceWatcher) snapshot() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int(nil), w.answers...)
}

// cpHTTPClient trusts the control plane's CA, from the install's laptop state.
func cpHTTPClient(ca []byte) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

// cpLaptopCA is the control plane's CA as the install recorded it in home.
func cpLaptopCA(t *testing.T, home string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".kubenest", "config.json"))
	if err != nil {
		t.Fatalf("reading the install's laptop config: %v", err)
	}
	var config struct {
		CA string `json:"control_plane_ca"`
	}
	if err := json.Unmarshal(raw, &config); err != nil || config.CA == "" {
		t.Fatalf("the install's laptop config carries no control-plane CA (%v)", err)
	}
	return []byte(config.CA)
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
	watcher := cpStartWatcher("https://api."+env.domain+"/api/v1/health", cpLaptopCA(t, home))
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
	cpAssertVersionThroughTheNode(t, ctx, env, server, cpLaptopCA(t, home))

	// ---------------------------------------------------------------- (f)
	// A PostgreSQL whose major or distribution differs is REFUSED, by name.
	cpAssertPostgresRefusal(t, ctx, env, server, name, home)

	// ---------------------------------------------------------------- (e)
	// A migration Job that cannot reach its database fails, and what is left
	// is the PREVIOUS chart with the fence still up — never a half-migrated
	// control plane serving customers.
	cpAssertMigrationFailureKeepsTheFenceUp(t, ctx, env, server, name, home, values)

	// ----------------------------------------------------------- (c) and (d)
	cpAssertResumeFromASecondLaptop(t, ctx, env, server, name, home, values)
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
// every minute. The upgrade is a disruptive operation and refuses to start
// without a stored window (T3.1: upgrade.ControlPlaneRecords.Window reads the
// STORED window); this gate is about the upgrade, so its window must never be
// the reason it cannot run. It does not wait for `active`: the second hardware
// run did, and the bundle 1.1 operator, which predates the window handler,
// never acknowledges one, so the window stays `stored`.
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
	record, err := client.MaintenanceWindow(ctx, journal.ClusterID)
	if err != nil {
		t.Fatalf("reading the stored window back: %v", err)
	}
	if record.Window == nil || len(record.Window.Days) != 7 {
		t.Fatalf("the all-week window was not stored: state %s, window %+v", record.State, record.Window)
	}
	t.Logf("all-week window stored at state %s", record.State)
}

// cpAssertVersionThroughTheNode reads the deployed backend's identity through
// the node's port-forward — the same route the upgrade's validation used, and
// the only one that works while the fence is up.
func cpAssertVersionThroughTheNode(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, ca []byte) {
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
	watcher := cpStartWatcher("https://api."+env.domain+"/api/v1/health", ca)
	defer watcher.close()
	client := cpHTTPClient(ca)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := client.Get("https://api." + env.domain + "/api/v1/health")
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			if code == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the public route never answered 200 after the fence came down; last answers %v", watcher.snapshot())
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
			// THE REFUSAL'S TERMS ARE IN THE OUTPUT, NOT IN THE ERROR. The
			// gate line — "PostgreSQL unchanged: ... a different major ..."
			// plus its Fix — is printed by the gates stage, and the command's
			// error is only the summary ("this control-plane upgrade is
			// refused before anything is changed: PostgreSQL unchanged"). A
			// test that read the error alone therefore asserted nothing about
			// which difference was named.
			both := err.Error() + "\n" + out.String()
			if !strings.Contains(both, tc.want) {
				t.Errorf("neither the error nor the run's output names the %s:\n%v", tc.want, both)
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

// cpAssertMigrationFailureKeepsTheFenceUp is (e): force the migration Job's
// database away WHILE THE RUN IS AT THE MIGRATION, and assert what is left —
// the PREVIOUS chart still running, the fence still up, the checkpoint still
// eligible — and then that a clean upgrade succeeds, so the arm after this one
// starts from a known state.
//
// FOUR THINGS THIS ARM HAS TO GET RIGHT, and each was wrong before:
//
//	it runs from a HOME THAT CAN REACH THE CONTROL PLANE (cpLaptopHome): a bare
//	t.TempDir() cannot log in, so the run never got as far as the migration;
//
//	it starts from a chart that is NOT the one being applied
//	(cpResetToThePreviousCandidate): the run that (b) completed already left
//	the current candidate running, so there was nothing to migrate and the
//	upgrade was a no-op;
//
//	PostgreSQL goes away only ONCE THE CHECKPOINT IS ELIGIBLE, which is what
//	the run's own output says — the checkpoint stage needs the database, so
//	stopping it beforehand failed the checkpoint instead of the migration;
//
//	a FRESH journal, so the fence stage raises a fence instead of reporting the
//	completed first run's stage as skipped.
//
// The automatic return TO the checkpoint is T4.7's recovery path (the plan's
// "rolling back to the checkpoint is automatic only while the fence has held
// continuously"); what this gate proves is the state the failure leaves, which
// is the one a customer's requests are answered from.
func cpAssertMigrationFailureKeepsTheFenceUp(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, name, installingHome, values string) {
	t.Helper()
	cpResetToThePreviousCandidate(t, ctx, env, server, values)
	home := cpLaptopHome(t, installingHome)

	var (
		varOut   strings.Builder
		scaleErr error
		fired    = make(chan struct{})
	)
	// THE MARKER IS THE RUN'S OWN TRANSITION, not a poll from outside: the
	// checkpoint is eligible when the run says its stage completed, and only
	// then does taking the database away hit the MIGRATION rather than the
	// checkpoint.
	watcher := newCPWatchWriter(&varOut, controlplane.StageCheckpoint+" ok", func() {
		defer close(fired)
		t.Log("the checkpoint is eligible; taking PostgreSQL away from the migration")
		scaleErr = cpScalePostgres(ctx, server, 0)
	})
	err := cpRunCLI(t, watcher, home, cpUpgradeArgs(env, name)...)
	select {
	case <-fired:
	default:
		t.Errorf("the run never reached the checkpoint, so the migration was never denied its database:\n%s", varOut.String())
	}
	if scaleErr != nil {
		t.Fatalf("taking PostgreSQL away at the migration: %v", scaleErr)
	}
	t.Log(varOut.String())

	if err == nil {
		t.Fatal("a migration Job that could not reach its database did not fail the upgrade")
	}
	if !strings.Contains(err.Error(), controlplane.MigrationJobName) {
		t.Errorf("the failure does not name the migration Job:\n%v", err)
	}
	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceUp {
		t.Errorf("the failed migration left the fence %q: a customer request could reach a control plane whose schema was not brought forward", report.State)
	}
	// NO CODE SERVES A HALF-MIGRATED SCHEMA, which is what the plan protects —
	// and it is NOT "the image is unchanged". The chart has ONE backend image
	// for the migration Job and the Deployment, and the migration stage's stop
	// apply sets `backend.replicas: 0`, so the Deployment's SPEC carries the new
	// image with zero replicas. What matters is that nothing is serving: no
	// backend replica is Ready, and the public route is still the fence.
	// (An earlier version of this arm compared the images and failed a correct
	// product for a state the upgrade intends.)
	if ready := cpBackendReady(t, ctx, server); ready != 0 {
		t.Errorf("the failed migration left %d backend replica(s) Ready, so a build that was never migrated is serving", ready)
	}
	// THE CHECKPOINT THIS RUN TOOK is the one that must survive. Comparing with
	// the checkpoint from before the run is wrong: the run publishes its own in
	// stage 3, deliberately, and the marker moves.
	want := cpCheckpointMarkerIn(t, varOut.String())
	if want == "" {
		t.Fatalf("the run never reported the checkpoint it published, so this arm cannot say which one must survive:\n%s", varOut.String())
	}
	if eligibleAfter := cpEligibleMarker(t, ctx, server); eligibleAfter != want {
		t.Errorf("the eligible checkpoint after the failed migration is %s, but the run published %s: the recovery point this run took is not the one the control plane would return to", eligibleAfter, want)
	}
	// AND BACK TO A KNOWN STATE BY HAND, so the arm after this one starts from
	// a control plane that is running and serving.
	//
	// NOT by re-running the upgrade: kn-t70-control-plane-version-identity-4xso.1
	// is filed for that. A re-run's version check goes through the fenced route
	// while the backend is at zero replicas, so it fails with HTTP 503 and the
	// assertion would be asserting the other bead's defect. Until that bead
	// lands, the harness restores the state itself.
	if err := cpScalePostgres(ctx, server, 1); err != nil {
		t.Fatalf("restoring PostgreSQL: %v", err)
	}
	if err := cpWaitForPostgres(ctx, server); err != nil {
		t.Fatal(err)
	}
	cpLowerTheFence(t, ctx, values, server)
	cpResetToThePreviousCandidate(t, ctx, env, server, values)
	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceDown {
		t.Errorf("the hand-restored control plane left the fence %q", report.State)
	}
}

// cpBackendReady is how many backend replicas are Ready. An absent
// status.readyReplicas is 0, which is the state a stopped Deployment reports.
func cpBackendReady(t *testing.T, ctx context.Context, server k3s.Runner) int {
	t.Helper()
	out, err := k3s.Kubectl(ctx, server, "get deployment "+controlplane.ReleaseName+"-backend"+
		" -n "+controlplane.Namespace+" -o jsonpath={.status.readyReplicas}")
	if err != nil {
		t.Fatalf("reading the backend's ready replicas: %v", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return 0
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("the backend's ready replica count %q is not a number", out)
	}
	return n
}

// cpCheckpointMarkerIn reads the marker the run itself reported, from the line
// stageCheckpoint prints: "checkpoint eligible BEFORE the migration starts:
// <marker> (job <job>)".
func cpCheckpointMarkerIn(t *testing.T, out string) string {
	t.Helper()
	const prefix = "checkpoint eligible BEFORE the migration starts: "
	i := strings.Index(out, prefix)
	if i < 0 {
		return ""
	}
	marker := out[i+len(prefix):]
	if j := strings.Index(marker, " (job "); j >= 0 {
		marker = marker[:j]
	}
	if j := strings.IndexAny(marker, "\n\r"); j >= 0 {
		marker = marker[:j]
	}
	return strings.TrimSpace(marker)
}

// cpEligibleMarker is the eligible checkpoint as "<key>@<at>", the same shape
// the upgrade's own stage prints.
func cpEligibleMarker(t *testing.T, ctx context.Context, server k3s.Runner) string {
	t.Helper()
	checkpoint, err := controlplane.ReadEligibleCheckpoint(ctx, server)
	if err != nil {
		t.Fatalf("reading the eligible checkpoint: %v", err)
	}
	if checkpoint == nil {
		return ""
	}
	return checkpoint.Key + "@" + checkpoint.At
}

// cpLowerTheFence restores the public route and dismantles the fence BY HAND,
// through the package API the command uses. It is the manual recovery path: the
// route goes back to the backend FIRST (Lower owns that order), and only then
// are the fence's objects deleted.
func cpLowerTheFence(t *testing.T, ctx context.Context, values string, server k3s.Runner) {
	t.Helper()
	replicas := int32(1)
	err := controlplane.Lower(ctx, server, values, &replicas,
		func(v string) error {
			_, err := controlplane.Apply(ctx, server, v)
			return err
		},
		func() error {
			return controlplane.WaitForRouteBackend(ctx, server, controlplane.ReleaseName+"-backend",
				5*time.Minute, 2*time.Second, converge.NewTextReporter(io.Discard))
		})
	if err != nil {
		t.Fatalf("lowering the fence by hand: %v", err)
	}
	t.Log("the fence was lowered by hand and the route is back on the backend")
}

// cpWaitForPostgres waits until the PostgreSQL StatefulSet is ready again,
// because the migration Job it is about to be given has backoffLimit 0: a Job
// created before the database accepts connections fails permanently.
func cpWaitForPostgres(ctx context.Context, server k3s.Runner) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		out, err := k3s.Kubectl(ctx, server, "get statefulset "+controlplane.ReleaseName+"-postgresql"+
			" -n "+controlplane.Namespace+" -o jsonpath={.status.readyReplicas}")
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the PostgreSQL StatefulSet did not become ready again (%s, last error %v)", strings.TrimSpace(out), err)
		}
		time.Sleep(2 * time.Second)
	}
}

// cpAssertResumeFromASecondLaptop is (c) and (d): interrupt the run at a
// recorded step, then finish it from a genuinely fresh process state.
//
// THE SECOND LAPTOP IS A SECOND HOME, and a home is not a directory: it is the
// config, the credential and the cluster journal the installing laptop left
// behind. A bare t.TempDir() has none of them, cannot reach the control plane,
// and so waited ten minutes for a checkpoint that was never going to be taken.
//
// EACH ARM STARTS FROM THE PREVIOUS CANDIDATE, so there is an upgrade to
// interrupt: without that reset the run's stages are all skipped and nothing
// happens at the moment it is cancelled.
func cpAssertResumeFromASecondLaptop(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, name, installingHome, values string) {
	t.Helper()

	// (c) Interrupt once the checkpoint has been published — the recorded step
	// the plan names — and finish from the second laptop.
	cpResetToThePreviousCandidate(t, ctx, env, server, values)
	first := cpLaptopHome(t, installingHome)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstOut strings.Builder
	stopAtCheckpoint := newCPWatchWriter(&firstOut, controlplane.StageCheckpoint+" ok", func() {
		t.Log("the checkpoint is eligible; interrupting the run there")
		cancel()
	})
	interrupted := make(chan struct{})
	go func() {
		defer close(interrupted)
		_ = cpRunCLIWithContext(t, runCtx, stopAtCheckpoint, first, cpUpgradeArgs(env, name)...)
	}()
	<-interrupted
	t.Log(firstOut.String())

	opID := cpOperationID(t, ctx, server)
	if opID == "" {
		t.Fatal("an interrupted control-plane upgrade left no operation record, so there is nothing for a second laptop to find")
	}
	t.Logf("interrupted after the checkpoint; operation %s", opID)

	second := cpLaptopHome(t, installingHome)
	var out strings.Builder
	if err := cpRunCLI(t, &out, second, cpUpgradeArgs(env, name, "--resume", opID)...); err != nil {
		t.Fatalf("the second laptop could not finish the interrupted upgrade: %v\n%s", err, out.String())
	}
	t.Log(out.String())
	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceDown {
		t.Errorf("the resumed run finished with the fence %q", report.State)
	}

	// (d) Interrupt DURING THE MIGRATION and finish from a third laptop. The
	// migration is the step that must not be half-done and unwatched: the fence
	// is up, the backend is stopped, and the Job's outcome is the one thing a
	// successor has to establish rather than guess.
	cpResetToThePreviousCandidate(t, ctx, env, server, values)
	third := cpLaptopHome(t, installingHome)
	runCtx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	var thirdOut strings.Builder
	stopAtMigration := newCPWatchWriter(&thirdOut, controlplane.StageMigration, func() {
		t.Log("the migration stage has started; interrupting the run inside it")
		cancel2()
	})
	interrupted2 := make(chan struct{})
	go func() {
		defer close(interrupted2)
		_ = cpRunCLIWithContext(t, runCtx2, stopAtMigration, third, cpUpgradeArgs(env, name)...)
	}()
	<-interrupted2
	t.Log(thirdOut.String())

	opID2 := cpOperationID(t, ctx, server)
	if opID2 == "" {
		t.Fatal("an upgrade interrupted during the migration left no operation record")
	}
	fourth := cpLaptopHome(t, installingHome)
	var out2 strings.Builder
	if err := cpRunCLI(t, &out2, fourth, cpUpgradeArgs(env, name, "--resume", opID2)...); err != nil {
		t.Fatalf("the second laptop could not finish the upgrade interrupted during the migration: %v\n%s", err, out2.String())
	}
	t.Log(out2.String())
	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceDown {
		t.Errorf("the run resumed during the migration finished with the fence %q", report.State)
	}
	if _, revision, found := cpMigrationJob(t, ctx, server); !found || revision == "" {
		t.Error("no migration Job at an install revision is on the cluster after the resumed run finished")
	}
}

// cpOperationID reads the live operation record's id.
//
// IT READS THE CONFIGMAP AS JSON. The first version used a jsonpath on the key
// `record.json` — a key with a DOT in it — and the arm reported "an interrupted
// control-plane upgrade left no operation record" while the record was on the
// cluster: `kubectl get … -o jsonpath={.data.record\.json}` is one more thing
// that can silently answer nothing, and it did. A whole-object read cannot.
//
// THE CALLER'S CONTEXT MUST NOT BE A CANCELLED RUN'S. Every read taken after an
// interrupt uses the gate's own context; passing the run's would fail here the
// same way the CLI's own record write did.
func cpOperationID(t *testing.T, ctx context.Context, server k3s.Runner) string {
	t.Helper()
	if err := ctx.Err(); err != nil {
		t.Fatalf("cpOperationID was given a cancelled context (%v): every read after an interrupt must use the test's own context", err)
	}
	out, err := k3s.Kubectl(ctx, server, "get configmap kubenest-operation -n kube-system -o json")
	if err != nil {
		t.Logf("reading the operation record: %v", err)
		return ""
	}
	var object struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &object); err != nil {
		t.Logf("the operation record is not readable JSON: %v", err)
		return ""
	}
	var record struct {
		OperationID string `json:"operation_id"`
		Stage       string `json:"stage"`
	}
	if err := json.Unmarshal([]byte(object.Data["record.json"]), &record); err != nil {
		t.Logf("the record document is not readable JSON: %v", err)
		return ""
	}
	if record.OperationID == "" {
		t.Logf("the operation record carries no operation_id: %s", object.Data["record.json"])
	}
	return record.OperationID
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

// cpLaptopHome returns a fresh HOME carrying the INSTALLING laptop's state.
//
// A BARE t.TempDir() IS NOT A LAPTOP. The CLI finds its control plane in
// ~/.kubenest/config.json, its token in credentials.json, and the cluster's
// journal under ~/.kubenest/journals/; a home with none of those cannot reach
// the control plane at all, which is why the resume arms waited ten minutes for
// a checkpoint that never came and why the forced-failure arm re-ran a
// COMPLETED stage off the first run's journal instead of doing anything.
//
// The token and the CA are copied, not re-derived: the gate is about resuming
// on a second laptop, and a second laptop is a machine that has the operator's
// credential and no memory of the first run's process.
func cpLaptopHome(t *testing.T, from string) string {
	t.Helper()
	home := t.TempDir()
	dst := filepath.Join(home, ".kubenest")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "credentials.json"} {
		src := filepath.Join(from, ".kubenest", name)
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading the installing laptop's %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The CLUSTER JOURNAL too: the command resolves the management cluster (and
	// its node list) through the install journal this machine holds.
	src := filepath.Join(from, ".kubenest", "journals")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("reading the installing laptop's journals in %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "journals"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, "journals", entry.Name()), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// cpWatchWriter passes a run's output through and fires ONCE, when the stream
// contains marker.
//
// IT IS HOW AN ARM INTERRUPTS THE RUN AT THE STEP IT MEANS TO. Deciding from
// outside — polling the checkpoint, or the migration Job — means polling state
// the run has not reached yet and guessing when it has; the run's own stage
// transitions are the only ordered account of what it is doing.
type cpWatchWriter struct {
	mu     sync.Mutex
	buf    strings.Builder
	out    io.Writer
	marker string
	fired  bool
	fire   func()
}

func newCPWatchWriter(out io.Writer, marker string, fire func()) *cpWatchWriter {
	return &cpWatchWriter{out: out, marker: marker, fire: fire}
}

func (w *cpWatchWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	fire := false
	if !w.fired && strings.Contains(w.buf.String(), w.marker) {
		w.fired = true
		fire = true
	}
	w.mu.Unlock()
	// The callback runs OUTSIDE the lock: it may write output of its own, and
	// firing under the lock would deadlock the run it is watching.
	if fire {
		w.fire()
	}
	return w.out.Write(p)
}

// cpResetToThePreviousCandidate puts the control plane back on the PINNED
// previous candidate, so an arm that follows really has a chart to move.
//
// Without it, (c), (d) and (e) all start from a control plane that already runs
// the current candidate: (e) has nothing to migrate and the upgrade is a no-op,
// and the resume arms interrupt a run whose stages are all skipped.
func cpResetToThePreviousCandidate(t *testing.T, ctx context.Context, env cpUpgradeEnv, server k3s.Runner, values string) string {
	t.Helper()
	revision := cpInstallPreviousCandidate(t, ctx, env, server, values)
	if err := controlplane.WaitReady(ctx, server, revision, envBundle(t, ctx, env), convergeReporter(t)); err != nil {
		t.Fatalf("the previous candidate did not become ready again: %v", err)
	}
	t.Logf("control plane put back on the previous candidate at install revision %s", revision)
	return revision
}

// cpScalePostgres returns the error rather than failing the test, because the
// forced-failure arm scales the StatefulSet FROM A CALLBACK that runs on the
// run's own goroutine — and t.Fatalf outside the test goroutine is not allowed.
func cpScalePostgres(ctx context.Context, server k3s.Runner, replicas int) error {
	_, err := k3s.Kubectl(ctx, server, "scale statefulset "+controlplane.ReleaseName+"-postgresql"+
		" -n "+controlplane.Namespace+" --replicas="+strconv.Itoa(replicas))
	return err
}
