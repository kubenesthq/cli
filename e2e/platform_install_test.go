//go:build e2e

// The kn-7k8 wave gate, run against a REAL host (no mocks — workspace rule §1)
// and a REAL control plane. Not a green unit test: a Hetzner node from
// `./scripts/ephemeral-env.sh up --profile host`, a backend and hub from
// `--profile local`, and the actual thirteen stages.
//
// What it asserts, which is the gate itself:
//
//	single-server core install completes UNDER FIFTEEN MINUTES
//	all five acceptance checks green (stage 13 runs them; this asserts it passed)
//	idempotent on re-run — a second identical run converges and changes nothing
//	failure injection names the failing COMPONENT, not "install failed"
//	uninstall leaves a known-clean machine
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000
//	export KUBENEST_CLI_TOKEN=knp_...
//	cd kubenest-cli && go test -tags e2e -v -timeout 90m ./e2e/ -run TestPlatformInstallGate
package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/uninstall"
)

// Budget is install.mdx's fifteen minutes. It is a TARGET asserted by the
// release tests, not the installer's deadline: exceeding it is a defect to
// fix, not a number to revise upward. The deadline is
// limits.timeouts.install-total and lives in the bundle.
const Budget = 15 * time.Minute

type gateEnv struct {
	server       string
	sshUser      string
	sshKey       string
	controlPlane string
	token        string
	bundle       string
	cluster      string
	// storageDevice is the lab volume. The lab node ships a blank attached
	// volume and no kubenest-vg, which is install.mdx's Option 2 — the
	// installer creates the volume group, and uninstall may remove it.
	storageDevice string
}

func gateEnvironment(t *testing.T) gateEnv {
	t.Helper()
	env := gateEnv{
		server:       envOr("KUBENEST_LAB_SERVER_IP", os.Getenv("KUBENEST_LAB_NODE1_IP")),
		sshUser:      envOr("KUBENEST_LAB_SSH_USER", "ubuntu"),
		sshKey:       os.Getenv("KUBENEST_LAB_SSH_KEY"),
		controlPlane: os.Getenv("KUBENEST_CONTROL_PLANE"),
		token:        os.Getenv("KUBENEST_CLI_TOKEN"),
		bundle:       envOr("KUBENEST_BUNDLE", "1.0"),
		cluster:      envOr("KUBENEST_GATE_CLUSTER", "gate-single-server"),

		storageDevice: os.Getenv("KUBENEST_LAB_NODE1_STORAGE_DEVICE"),
	}
	if env.server == "" {
		t.Skip("KUBENEST_LAB_SERVER_IP / KUBENEST_LAB_NODE1_IP not set: run ./scripts/ephemeral-env.sh up --profile host and source lab/hetzner/.lab-env.sh")
	}
	if env.controlPlane == "" || env.token == "" {
		t.Skip("KUBENEST_CONTROL_PLANE and KUBENEST_CLI_TOKEN not set: the install registers the cluster and will not run without a control plane")
	}
	return env
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// session builds one install run against the lab host. journalDir isolates
// the gate's journals from the operator's real ones.
func session(t *testing.T, env gateEnv, journalPath string, bundle *manifest.Manifest, opts install.Options) (*install.Session, *api.Client) {
	t.Helper()
	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := install.OpenJournal(journalPath, opts.Identity())
	if err != nil {
		t.Fatal(err)
	}
	s := &install.Session{
		RunID:    install.NewRunID(),
		Opts:     opts,
		Bundle:   bundle,
		Journal:  journal,
		Reporter: reporterTo(t),
		Out:      testWriter{t},
		API:      client,
		Started:  time.Now(),
	}
	s.Emitter = install.Emitters{
		install.TextEmitter{W: testWriter{t}},
		install.NewControlPlaneEmitter(client, s),
	}
	return s, client
}

func gateOptions(env gateEnv) install.Options {
	return install.Options{
		Bundle:  env.bundle,
		Name:    env.cluster,
		Servers: []string{env.server},
		HATier:  "single-server",
		SSHUser: env.sshUser,
		SSHKey:  env.sshKey,
		// StorageDevice is deliberately NOT set here. The gate's timed
		// install uses install.mdx Option 1 — the volume group already
		// exists and the installer never touches the customer's block
		// devices. Only the failure-injection run, which starts from a blank
		// disk, passes --storage-device.
	}
}

func fetchBundle(t *testing.T, client *api.Client, version string) *manifest.Manifest {
	t.Helper()
	raw, err := client.BundleManifest(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestPlatformInstallGate is the whole gate, in order, on one host. The
// subtests share state deliberately: an install that is torn down between
// assertions is not the thing being tested.
//
// The order exercises BOTH documented storage paths on one machine, which is
// also the only order that works on a single host:
//
//  1. failure injection with --storage-device — Option 2, the installer
//     creates kubenest-vg. It fails at stage 8 by design, leaving k3s behind.
//  2. uninstall — k3s goes, the volume group stays (the default, data-safe).
//  3. the timed install with NO --storage-device — Option 1, the default
//     path, using the volume group left by step 2 exactly as a customer's
//     pre-created one.
//  4. reported in, then a second identical run.
//  5. uninstall --destroy-data — and the volume group STILL survives,
//     because by step 3 it was the customer's. That is the strongest rule on
//     the page and it is worth asserting on a real disk.
func TestPlatformInstallGate(t *testing.T) {
	env := gateEnvironment(t)
	journalPath := t.TempDir() + "/journal.json"
	ctx := context.Background()

	bootstrap, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	bundle := fetchBundle(t, bootstrap, env.bundle)

	var clusterID string

	t.Run("failure injection names the failing component", func(t *testing.T) {
		if env.storageDevice == "" {
			t.Skip("KUBENEST_LAB_NODE1_STORAGE_DEVICE not set")
		}
		// A bundle whose Velero pin does not exist. Everything else is the
		// real manifest, so the failure travels the real path: the HelmChart
		// lands, the helm-install job cannot resolve the version, and the
		// stage converges to fail.
		poisoned := fetchBundle(t, bootstrap, env.bundle)
		poisoned.Core["velero"] = "0.0.0-does-not-exist"
		// The deadline is what a failing stage waits out. Two minutes proves
		// the mechanism without spending ten on a chart that will never
		// resolve. This is the manifest doing its job — the deadline is data.
		poisoned.Limits.Timeouts["component-ready"] = 2 * time.Minute

		opts := gateOptions(env)
		opts.StorageDevice = env.storageDevice
		s, _ := session(t, env, t.TempDir()+"/poisoned.json", poisoned, opts)
		defer s.Close()

		_, err := install.Execute(ctx, s, install.Plan())
		if err == nil {
			t.Fatal("the poisoned bundle must fail")
		}
		var stageErr *install.StageError
		if !errorsAs(err, &stageErr) {
			t.Fatalf("want a *StageError, got %T: %v", err, err)
		}
		t.Logf("failure reported: stage=%s component=%s reason=%s",
			stageErr.Stage, stageErr.Component, stageErr.ReasonCode)

		if stageErr.Stage != install.StageBackup {
			t.Errorf("failed at stage %q, want platform-backup", stageErr.Stage)
		}
		if stageErr.Component != "velero" {
			t.Errorf("the failure names component %q, want velero — naming the component is the difference between this installer and one that prints \"error\"", stageErr.Component)
		}
		if !strings.Contains(err.Error(), "uninstall --confirm") {
			t.Errorf("the failure must print both supported exits:\n%v", err)
		}
		// The control plane's own record, not the CLI's error object. A CLI
		// asserting on itself proves only that it is internally consistent;
		// reading the record back proves the whole kn-w051 chain — the CLI
		// emitted, the backend persisted, and the record names the stage and
		// the component. An API error here is FATAL: a check that passes
		// when the thing it checks is unreachable is worse than no check.
		clusterID := s.Journal.Cluster.ClusterID
		if clusterID == "" {
			t.Fatal("the failed run registered no cluster, so there is no server-side record to assert against")
		}
		health, err := bootstrap.ClusterHealth(ctx, clusterID)
		if err != nil {
			t.Fatalf("reading the cluster back: %v", err)
		}
		t.Logf("control plane records the cluster as %q", health.Status)
		if health.Status != "install_failed" {
			t.Errorf("a failed install must mark the cluster install_failed, got %q — telemetry has to see it, so a support call about it is never a surprise", health.Status)
		}

		journal, err := bootstrap.InstallJournal(ctx, clusterID)
		if err != nil {
			t.Fatalf("reading the server-side install journal: %v", err)
		}
		if len(journal) == 0 {
			t.Fatal("the server-side install journal is empty: the stage events never reached the control plane")
		}
		last := journal[len(journal)-1]
		t.Logf("server journal last entry: stage=%s component=%s status=%s reason_code=%s",
			last.Stage, last.Component, last.Status, last.ReasonCode)
		if last.Stage != install.StageBackup {
			t.Errorf("server journal's last entry is stage %q, want platform-backup", last.Stage)
		}
		if last.Component != "velero" {
			t.Errorf("server journal names component %q, want velero", last.Component)
		}
		if last.Status != api.StageFailed {
			t.Errorf("server journal records status %q, want failed", last.Status)
		}
		if last.ReasonCode != "PLATFORM_BACKUP_FAILED" {
			t.Errorf("server journal records reason_code %q, want PLATFORM_BACKUP_FAILED", last.ReasonCode)
		}
	})

	t.Run("uninstall after a failed install leaves the data", func(t *testing.T) {
		nodes := connectNodes(t, env)
		if err := uninstall.Run(ctx, uninstall.Options{Nodes: nodes, Out: testWriter{t}}); err != nil {
			t.Fatal(err)
		}
		assertHost(t, ctx, nodes[0], map[string]string{
			"k3s binary is gone":        "absent",
			"platform state is gone":    "absent",
			"the volume group survives": "present",
		})
	})

	t.Run("install completes under the fifteen-minute budget", func(t *testing.T) {
		// No --storage-device: kubenest-vg already exists, which is exactly
		// install.mdx Option 1 — the customer created it and the installer
		// never touches their block devices.
		s, _ := session(t, env, journalPath, bundle, gateOptions(env))
		defer s.Close()

		start := time.Now()
		result, err := install.Execute(ctx, s, install.Plan())
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("install failed after %s:\n%v", elapsed.Round(time.Second), err)
		}
		clusterID = s.Journal.Cluster.ClusterID

		t.Logf("install completed in %s (budget %s)", elapsed.Round(time.Second), Budget)
		if len(result.Ran) != 13 {
			t.Errorf("ran %d stages, want all thirteen: %v", len(result.Ran), result.Ran)
		}
		if elapsed > Budget {
			t.Errorf("install took %s, over the %s budget — that is a defect in the installer, not a number to revise upward",
				elapsed.Round(time.Second), Budget)
		}
	})

	t.Run("the cluster has reported in", func(t *testing.T) {
		if clusterID == "" {
			t.Skip("install did not complete")
		}
		health, err := bootstrap.ClusterHealth(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("cluster status=%s last_heartbeat=%v", health.Status, health.LastHeartbeat)
		if health.LastHeartbeat == nil {
			t.Error("no fleet-telemetry heartbeat: stage 13 asserted one arrived, so a difference here means it regressed after the install")
		}
		if health.Status == "install_failed" || health.Status == "error" {
			t.Errorf("cluster status is %q after a successful install", health.Status)
		}
	})

	t.Run("re-running the identical command converges", func(t *testing.T) {
		if clusterID == "" {
			t.Skip("install did not complete")
		}
		s, _ := session(t, env, journalPath, bundle, gateOptions(env))
		defer s.Close()

		start := time.Now()
		result, err := install.Execute(ctx, s, install.Plan())
		if err != nil {
			t.Fatalf("a second identical run must converge, not fail:\n%v", err)
		}
		t.Logf("re-run completed in %s: ran %v, skipped %v",
			time.Since(start).Round(time.Second), result.Ran, result.Skipped)

		// Exactly the three always-run stages re-run. Everything else is
		// skipped from the journal — that is what makes resume deterministic
		// rather than a hope that every component is idempotent.
		want := []string{install.StagePreflight, install.StageRegister, install.StageVerify}
		if strings.Join(result.Ran, ",") != strings.Join(want, ",") {
			t.Errorf("re-run executed %v, want only %v", result.Ran, want)
		}
		if len(result.Skipped) != 10 {
			t.Errorf("re-run skipped %d stages, want 10: %v", len(result.Skipped), result.Skipped)
		}
	})

	t.Run("uninstall leaves a known-clean machine", func(t *testing.T) {
		nodes := connectNodes(t, env)
		// --destroy-data, and the volume group STILL survives: this install
		// used a volume group the machine already had, and one the installer
		// did not create is never removed, on either path.
		if err := uninstall.Run(ctx, uninstall.Options{
			Nodes:       nodes,
			DestroyData: true,
			Ownership:   storage.CustomerCreated,
			Out:         testWriter{t},
		}); err != nil {
			t.Fatal(err)
		}
		assertHost(t, ctx, nodes[0], map[string]string{
			"k3s binary is gone":        "absent",
			"k3s service is gone":       "absent",
			"platform state is gone":    "absent",
			"the volume group survives": "present",
		})
	})
}

// assertHost runs the named host checks and compares their one-word answers.
func assertHost(t *testing.T, ctx context.Context, node uninstall.Node, want map[string]string) {
	t.Helper()
	commands := map[string]string{
		"k3s binary is gone":        "command -v k3s >/dev/null 2>&1 && echo present || echo absent",
		"k3s service is gone":       "systemctl list-units --all --no-legend k3s.service | grep -q k3s && echo present || echo absent",
		"platform state is gone":    "test -d /var/lib/rancher/k3s && echo present || echo absent",
		"the volume group survives": "sudo -n vgs kubenest-vg >/dev/null 2>&1 && echo present || echo absent",
	}
	for name, expected := range want {
		res, err := node.Runner.Run(ctx, commands[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := strings.TrimSpace(res.Stdout); got != expected {
			t.Errorf("%s: got %q, want %q", name, got, expected)
		}
	}
}

// connectNodes opens SSH connections for the checks that run outside the
// installer's own session.
func connectNodes(t *testing.T, env gateEnv) []uninstall.Node {
	t.Helper()
	opts := sshx.Options{
		User:           env.sshUser,
		KeyPath:        env.sshKey,
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
		DialTimeout:    15 * time.Second,
	}
	endpoint, err := sshx.Resolve(env.server, opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshx.Dial(context.Background(), endpoint, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return []uninstall.Node{{Address: env.server, Role: uninstall.RoleServer, Runner: client}}
}

func reporterTo(t *testing.T) converge.Reporter {
	return converge.NewTextReporter(testWriter{t})
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
