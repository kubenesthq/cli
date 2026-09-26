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
//	  reports the recorded stage and finishes;
//	the code serving afterwards is the code the chart pins
//	  after steps (b), (c) and (e) the backend Deployment runs the backend
//	  image this binary's chart names, and the control plane reports a build
//	  whose prefix is that image's tag — which an upgrade that inherited the
//	  fence's image pin cannot do (kn-t70-control-plane-version-identity-4xso.4).
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

	"gopkg.in/yaml.v3"

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

// cpLaptopClient builds the client the installing laptop's own state describes:
// the control plane's URL and CA, and its token, out of the home tree cpRunCLI
// was given. Each "laptop" in this gate has its own copy, so a read through it is
// a read an operator could make.
func cpLaptopClient(t *testing.T, home string) *api.Client {
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
	for path, into := range map[string]any{
		filepath.Join(home, ".kubenest", "config.json"):      &config,
		filepath.Join(home, ".kubenest", "credentials.json"): &creds,
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the install's laptop state: %v", err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	token := creds.Tokens[config.URL].Token
	if config.URL == "" || token == "" {
		t.Fatalf("the install's laptop state has no control-plane URL or token (url=%q), so the control plane cannot be read the way an operator reads it", config.URL)
	}
	if config.CA == "" {
		t.Fatalf("the install's laptop state carries no control-plane CA, so a read of it would not verify the control plane's certificate")
	}
	client, err := api.New(config.URL, api.WithToken(token), api.WithCACert([]byte(config.CA)))
	if err != nil {
		t.Fatal(err)
	}
	return client
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

// cpChartBackendImage reads the backend image the chart THIS BINARY carries pins,
// as the chart's own helper renders it (templates/_helpers.tpl "kubenest.image":
// repository@digest when a digest is set, repository:tag otherwise), and that
// image's tag.
//
// IT IS READ FROM THE EMBEDDED ARCHIVE because that archive is what an upgrade
// applies, so this reference is exactly "the code this run is moving to" — and it
// is what tells an upgrade that moved from one that only said it did.
func cpChartBackendImage(t *testing.T) (image, tag string) {
	t.Helper()
	body, err := controlplane.ChartFile("values.yaml")
	if err != nil {
		t.Fatalf("reading the chart this binary carries: %v", err)
	}
	var doc struct {
		Backend struct {
			Image struct {
				Repository string `yaml:"repository"`
				Tag        string `yaml:"tag"`
				Digest     string `yaml:"digest"`
			} `yaml:"image"`
		} `yaml:"backend"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the chart's values are not readable YAML: %v", err)
	}
	pin := doc.Backend.Image
	switch {
	case pin.Repository == "":
		t.Fatalf("the chart this binary carries pins no backend repository: %+v", pin)
	case pin.Digest != "":
		return pin.Repository + "@" + pin.Digest, pin.Tag
	case pin.Tag != "":
		return pin.Repository + ":" + pin.Tag, pin.Tag
	}
	t.Fatalf("the chart this binary carries pins neither a tag nor a digest for the backend: %+v", pin)
	return "", ""
}

// cpAssertTheUpgradeRolled asserts that the code serving after an upgrade is the
// code the chart this binary carries pins.
//
// THE HARNESS HALF OF kn-t70-control-plane-version-identity-4xso.4. A run that
// read its base values off the cluster and kept the backend.image pin the fence
// stage (or a failed migration's restore) had written there re-applied the OLD
// image through its stop apply, its migration Job, its chart stage and its
// unfence apply, and lowered the fence over it while printing that it had
// upgraded. The CLI's own validation could not see it — the image its expectations
// compared against came out of those same pinned values, so no build was demanded
// and only the era floor was left, which the old code behind the fence also meets.
//
// TWO FACTS ABOUT THE SAME THING, and both are needed: the Deployment runs the
// image the chart pins, and the control plane reports a build whose prefix is that
// image's tag. The tag is short while APP_VERSION carries the whole commit sha, so
// the prefix is the tie the CLI's own validation uses too.
func cpAssertTheUpgradeRolled(t *testing.T, ctx context.Context, server k3s.Runner, home, when string) {
	t.Helper()
	wantImage, wantTag := cpChartBackendImage(t)
	if got := cpBackendImage(t, ctx, server); got != wantImage {
		t.Errorf("after %s the backend Deployment runs %s, want the image the chart this binary carries pins (%s): the run did not move the code", when, got, wantImage)
	}
	version, err := cpLaptopClient(t, home).ControlPlaneVersion(ctx)
	if err != nil {
		t.Fatalf("reading the control plane's version after %s: %v", when, err)
	}
	if version.Build == "" {
		t.Errorf("after %s the control plane reports no build stamp, so nothing can say which image is serving", when)
	}
	if wantTag != "" && !strings.HasPrefix(version.Build, wantTag) {
		t.Errorf("after %s the control plane reports build %q, which does not start with the chart's backend tag %q: an image this run did not apply is the one serving", when, version.Build, wantTag)
	}
}

// cpBackendReplicas reads the backend Deployment's DESIRED replica count. Zero
// is the state the migration stage's stop apply leaves behind, and the state
// step (d) has to interrupt in.
//
// IT RETURNS ITS ERROR rather than failing the test, because the poll that uses
// it runs on a watch callback's goroutine, where t.Fatalf is not allowed.
func cpBackendReplicas(ctx context.Context, server k3s.Runner) (int32, error) {
	out, err := k3s.Kubectl(ctx, server, "get deployment "+controlplane.ReleaseName+"-backend"+
		" -n "+controlplane.Namespace+" -o jsonpath={.spec.replicas}")
	if err != nil {
		return -1, err
	}
	var replicas int32
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &replicas); err != nil {
		return -1, fmt.Errorf("the backend's replica count %q is not a number", strings.TrimSpace(out))
	}
	return replicas, nil
}

// cpMigrationJobState describes the migration Job's own verdict: the conditions
// the Job controller writes, or the counts before it writes one. "not found" is
// an observation, not an error, like cpMigrationJob's.
func cpMigrationJobState(ctx context.Context, server k3s.Runner) (string, error) {
	out, err := k3s.Kubectl(ctx, server, "get job "+controlplane.MigrationJobName+
		" -n "+controlplane.Namespace+" -o json")
	if err != nil {
		return "not found", nil
	}
	var job struct {
		Status struct {
			Active     int32 `json:"active"`
			Succeeded  int32 `json:"succeeded"`
			Failed     int32 `json:"failed"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &job); err != nil {
		return "", fmt.Errorf("the migration Job is not readable JSON: %w", err)
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		if condition.Type == "Complete" || condition.Type == "Failed" {
			return condition.Type, nil
		}
	}
	switch {
	case job.Status.Active > 0:
		return fmt.Sprintf("running (%d pod(s) active)", job.Status.Active), nil
	case job.Status.Succeeded > 0:
		return fmt.Sprintf("succeeded (%d)", job.Status.Succeeded), nil
	case job.Status.Failed > 0:
		return fmt.Sprintf("failed (%d, no verdict yet)", job.Status.Failed), nil
	}
	return "running (no verdict yet)", nil
}

// cpWaitForTheBackendStopped waits until the state step (d) exists to interrupt
// in is ON THE CLUSTER: the migration stage's stop apply has taken the backend to
// zero replicas AND the migration Job exists. It returns a description of what it
// last saw.
//
// IT RETURNS RATHER THAN FAILING, because it runs on the watch callback's
// goroutine (see cpSetApplicationLogin for the same rule). The caller cancels the run
// either way: an arm that never reached the state must not let the upgrade finish.
func cpWaitForTheBackendStopped(ctx context.Context, server k3s.Runner, within time.Duration) (string, error) {
	deadline := time.Now().Add(within)
	last := "nothing read yet"
	for {
		replicas, err := cpBackendReplicas(ctx, server)
		if err != nil {
			last = fmt.Sprintf("the backend Deployment could not be read: %v", err)
		} else {
			job, _ := cpMigrationJobState(ctx, server)
			last = fmt.Sprintf("backend spec.replicas = %d, migration Job %s", replicas, job)
			if replicas == 0 && job != "not found" {
				return last, nil
			}
		}
		if time.Now().After(deadline) {
			return last, fmt.Errorf("waited %s for the migration stage to stop the backend and create its Job; last observation: %s", within, last)
		}
		time.Sleep(time.Second)
	}
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
	// AND THE CODE THAT RAN IS THE CODE THIS BINARY PINS, which the comparison
	// above cannot say on its own: it only says the image moved, and the whole
	// defect this gate now guards against is a run that DID move the image and
	// then moved it back to the previous candidate's through the values it read
	// off the cluster (kn-t70-control-plane-version-identity-4xso.4).
	cpAssertTheUpgradeRolled(t, ctx, server, home, "the upgrade in step (b)")
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
	client := cpLaptopClient(t, home)
	var journal struct {
		ClusterID string `json:"cluster_id"`
	}
	journalPath := filepath.Join(home, ".kubenest", "journals", name+".json")
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("reading the install's laptop state: %v", err)
	}
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatalf("%s: %v", journalPath, err)
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
	// The image the restored control plane must be running afterwards: it is
	// the previous candidate's, and the failed migration's chart is the current
	// one, so the two are different values.
	previousImage := cpBackendImage(t, ctx, server)
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
		t.Log("the checkpoint is eligible; denying the migration its database")
		scaleErr = cpSetApplicationLogin(ctx, server, false)
	})
	err := cpRunCLI(t, watcher, home, cpUpgradeArgs(env, name)...)
	select {
	case <-fired:
	default:
		t.Errorf("the run never reached the checkpoint, so the migration was never denied its database:\n%s", varOut.String())
	}
	if scaleErr != nil {
		t.Fatalf("denying the migration its database: %v", scaleErr)
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
	// THE PREVIOUS CHART IS WHAT RUNS, at the previous image, with the fence
	// still up. The migration applied the CURRENT chart with
	// `backend.replicas: 0` and the new image (the chart has ONE backend image
	// for the Job and the Deployment), so the restore is what puts the old code
	// back — and the plan's step (e) is exactly that sentence
	// (kn-t70-control-plane-version-identity-4xso.2).
	if after := cpBackendImage(t, ctx, server); after != previousImage {
		t.Errorf("the backend runs %s after a failed migration, want the previous chart's %s: the control plane must run the code its schema matches", after, previousImage)
	}
	// IT IS NOT READY YET, and that is CORRECT: PostgreSQL is still down, which
	// is why the migration failed. Readiness is asserted below, after the
	// database is restored.
	if ready := cpBackendReady(t, ctx, server); ready != 0 {
		t.Errorf("the failed migration left %d backend replica(s) Ready with the database down", ready)
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
	// AND THE ADVERTISED WAY ON HAS TO WORK. The failure text promises "fix what
	// the error names, then run the identical command again", and this is that
	// sentence being kept: with the database back, the re-run completes and
	// lowers the fence itself. It is also the harness assertion for
	// kn-t70-control-plane-version-identity-4xso.1 — the re-run's version check
	// reads the FENCE's own 503, so it reaches the backend through the node and,
	// while the backend is still at zero replicas, falls back to the version
	// this operation recorded.
	if err := cpSetApplicationLogin(ctx, server, true); err != nil {
		t.Fatalf("giving the database back: %v", err)
	}
	// AND NOW IT IS READY, still behind the fence: the restored backend was
	// waiting for exactly this.
	cpWaitFor(t, 5*time.Minute, 5*time.Second, "the restored backend to become Ready behind the fence", func() (bool, string) {
		if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceUp {
			return false, "the fence is " + string(report.State)
		}
		ready := cpBackendReady(t, ctx, server)
		return ready >= 1, fmt.Sprintf("%d backend replica(s) Ready", ready)
	})
	rerun := cpLaptopHome(t, installingHome)
	var rerunOut strings.Builder
	if err := cpRunCLI(t, &rerunOut, rerun, cpUpgradeArgs(env, name)...); err != nil {
		t.Fatalf("the identical command after a failed migration did not succeed; an operator whose fix worked must be able to run it again: %v\n%s", err, rerunOut.String())
	}
	t.Log(rerunOut.String())
	if report := cpFenceState(t, ctx, server); report.State != controlplane.FenceDown {
		t.Errorf("the re-run finished with the fence %q", report.State)
	}
	// AND THE RE-RUN REALLY UPGRADED, not only reported that it did. The re-run
	// starts from values the RESTORE pinned to the previous image, which is
	// kn-t70-control-plane-version-identity-4xso.4's second arm: run 19's pass of
	// this step did not prove the new code rolled, because the CLI's own
	// validation read its "declared" image out of those same values.
	cpAssertTheUpgradeRolled(t, ctx, server, rerun, "the identical re-run in step (e)")
	if _, revision, found := cpMigrationJob(t, ctx, server); !found || revision == "" {
		t.Error("no migration Job at an install revision is on the cluster after the re-run: the record of the schema step is gone")
	}
}

// cpAssertResumeFromASecondLaptop is (c) and (d): interrupt the run at a
// recorded step, then finish it from a genuinely fresh process state.
//
// THE SECOND LAPTOP IS A SECOND HOME, and a home is not a directory: it is the
// config, the credential and the cluster journal the installing laptop left
// behind. A bare t.TempDir() has none of them, cannot reach the control plane,

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
	// AND IT FINISHED THE UPGRADE, not only the command. This is the arm
	// kn-t70-control-plane-version-identity-4xso.4 is about: the resumed run takes
	// its base values off the cluster, where the FIRST run's fence apply had just
	// pinned the previous candidate's image, so a run that kept that pin re-applied
	// the old code through every later stage and lowered the fence over it.
	cpAssertTheUpgradeRolled(t, ctx, server, second, "the resumed run in step (c)")

	// (d) Interrupt DURING THE MIGRATION and finish from a third laptop, with the
	// backend STOPPED behind the fence and the migration Job on the cluster: the
	// state kn-t70-control-plane-version-identity-4xso.3 exists for, where nothing
	// is left running to bring the control plane back and the Job's outcome is the
	// one thing a successor has to establish rather than guess.
	//
	// THE INTERRUPT IS PLACED BY WHAT IS ON THE CLUSTER, not by the stage's own
	// header. Run 22 cancelled on that header and landed BEFORE the stop apply was
	// submitted — the run said "this action was not submitted: it could not be
	// recorded first ... context canceled" — so the backend was never stopped and
	// the resume never exercised the recovery at all. The run is therefore
	// cancelled only once the state is observable, and what the successor will
	// find is read and logged before the resume is started.
	cpResetToThePreviousCandidate(t, ctx, env, server, values)
	third := cpLaptopHome(t, installingHome)
	runCtx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	var thirdOut strings.Builder
	var (
		stoppedMu  sync.Mutex
		stoppedAt  string
		stoppedErr error
	)
	stopAtMigration := newCPWatchWriter(&thirdOut, controlplane.StageMigration, func() {
		go func() {
			// NOTHING IS CANCELLED HERE YET: the stage's header says the migration
			// stage started, not that the backend has been stopped for it.
			t.Log("the migration stage has started; waiting for the backend to be stopped behind the fence")
			seen, err := cpWaitForTheBackendStopped(ctx, server, cpMigrationStopWithin)
			stoppedMu.Lock()
			stoppedAt, stoppedErr = seen, err
			stoppedMu.Unlock()
			// CANCEL EITHER WAY: a run that was never stopped must not be left to
			// finish the whole upgrade.
			cancel2()
		}()
	})
	interrupted2 := make(chan struct{})
	go func() {
		defer close(interrupted2)
		_ = cpRunCLIWithContext(t, runCtx2, stopAtMigration, third, cpUpgradeArgs(env, name)...)
	}()
	<-interrupted2
	stoppedMu.Lock()
	seenState, seenErr := stoppedAt, stoppedErr
	stoppedMu.Unlock()
	if seenErr != nil {
		t.Fatalf("the migration stage never reached the state this arm exists to interrupt: %v", seenErr)
	}
	t.Log(thirdOut.String())

	// WHAT THE SUCCESSOR FINDS, read after the interrupt rather than assumed. The
	// backend MUST be at zero replicas or the interrupt landed somewhere else and
	// this arm proves nothing about the recovery.
	replicas, err := cpBackendReplicas(ctx, server)
	if err != nil {
		t.Fatalf("reading the backend's replica count after the interrupt: %v", err)
	}
	jobState, err := cpMigrationJobState(ctx, server)
	if err != nil {
		t.Fatalf("reading the migration Job's state after the interrupt: %v", err)
	}
	t.Logf("the interrupted run stopped with the state a successor must recover: %s; the successor finds backend spec.replicas = %d and migration Job %s", seenState, replicas, jobState)
	if replicas != 0 {
		t.Fatalf("the backend is at %d replica(s) after the interrupt, so this arm never reached the stopped state it exists to test", replicas)
	}

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
	// AND IT FOUND THE STOPPED BACKEND AND BROUGHT ONE BACK, which is the whole
	// point of this arm: the recovery's own line, whose opening is stable — what
	// it found, which code it brought back and the readiness after it follow.
	if !strings.Contains(out2.String(), "the fence is up and the backend is stopped at zero replicas with") {
		t.Errorf("the resumed run never reported the recovery this arm exists for, so it did not find a stopped backend behind the fence:\n%s", out2.String())
	}
	cpAssertTheUpgradeRolled(t, ctx, server, fourth, "the run resumed during the migration in step (d)")
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
// cpMigrationStopWithin bounds how long step (d) waits for the migration stage to
// stop the backend and create its Job. It is generous on purpose: the wait starts
// when the stage's header prints and the stop apply follows within seconds, but a
// slow node must not be reported as a failed arm.
const cpMigrationStopWithin = 5 * time.Minute

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

// cpApplicationRole is the chart's postgresql.auth.username: the role the
// backend, the checkpoint and the migration Job all log in as.
const cpApplicationRole = "kubenest"

// cpSetApplicationLogin denies or restores the application role's LOGIN, which
// is how step (e) takes the database away from the migration.
//
// IT DOES NOT SCALE POSTGRESQL DOWN. The migration stage's first apply is a
// helm upgrade, and helm's three-way merge puts the StatefulSet's replicas back
// to the chart's 1. Runs 23 and 24 measured it: PostgreSQL came back before the
// migration Job connected, the migration succeeded, and the arm had no failure
// to recover from; the earlier passes had won that race. A role attribute is
// database state, which no chart apply touches, and a refused login fails the
// Job at once rather than at a timeout.
//
// It returns the error rather than failing the test, because the arm calls it
// FROM A CALLBACK that runs on the run's own goroutine, and t.Fatalf outside the
// test goroutine is not allowed. The superuser password is read inside the pod
// from the file the chart mounts, so it never reaches this process.
func cpSetApplicationLogin(ctx context.Context, server k3s.Runner, allowed bool) error {
	attribute := "NOLOGIN"
	if allowed {
		attribute = "LOGIN"
	}
	_, err := k3s.Kubectl(ctx, server, "exec "+controlplane.ReleaseName+"-postgresql-0 -n "+controlplane.Namespace+
		` -- sh -c 'PGPASSWORD="$(cat "$POSTGRES_POSTGRES_PASSWORD_FILE")" psql -h 127.0.0.1 -U postgres -v ON_ERROR_STOP=1 -tAc "ALTER ROLE `+
		cpApplicationRole+" "+attribute+`"'`)
	return err
}
