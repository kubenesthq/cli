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
		server:       os.Getenv("KUBENEST_LAB_SERVER_IP"),
		sshUser:      envOr("KUBENEST_LAB_SSH_USER", "ubuntu"),
		sshKey:       os.Getenv("KUBENEST_LAB_SSH_KEY"),
		controlPlane: os.Getenv("KUBENEST_CONTROL_PLANE"),
		token:        os.Getenv("KUBENEST_CLI_TOKEN"),
		bundle:       envOr("KUBENEST_BUNDLE", "1.0"),
		cluster:      envOr("KUBENEST_GATE_CLUSTER", "gate-single-server"),

		storageDevice: os.Getenv("KUBENEST_LAB_NODE1_STORAGE_DEVICE"),
	}
	if env.server == "" {
		t.Skip("KUBENEST_LAB_SERVER_IP not set: run ./scripts/ephemeral-env.sh up --profile host and source lab/hetzner/.lab-env.sh")
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
		install.ControlPlaneEmitter{Client: client, Session: s},
	}
	return s, client
}

func gateOptions(env gateEnv) install.Options {
	return install.Options{
		Bundle:        env.bundle,
		Name:          env.cluster,
		Servers:       []string{env.server},
		HATier:        "single-server",
		SSHUser:       env.sshUser,
		SSHKey:        env.sshKey,
		StorageDevice: env.storageDevice,
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

	t.Run("install completes under the fifteen-minute budget", func(t *testing.T) {
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

	t.Run("failure injection names the failing component", func(t *testing.T) {
		if clusterID == "" {
			t.Skip("install did not complete")
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

		// A fresh journal: this run must reach stage 8, not skip to it.
		opts := gateOptions(env)
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
	})

	t.Run("uninstall leaves a known-clean machine", func(t *testing.T) {
		s, _ := session(t, env, journalPath, bundle, gateOptions(env))
		defer s.Close()
		nodes := connectNodes(t, env)

		if err := uninstall.Run(ctx, uninstall.Options{
			Nodes: nodes,
			Out:   testWriter{t},
		}); err != nil {
			t.Fatal(err)
		}

		// Known-clean: no k3s binary, no k3s service, no platform state under
		// /var/lib/rancher — and the volume group still there, because
		// uninstall never destroys data by default.
		checks := []struct {
			name    string
			command string
			want    string
		}{
			{"k3s binary is gone", "command -v k3s >/dev/null 2>&1 && echo present || echo absent", "absent"},
			{"k3s service is gone", "systemctl list-units --all --no-legend k3s.service | grep -q k3s && echo present || echo absent", "absent"},
			{"platform state is gone", "test -d /var/lib/rancher/k3s && echo present || echo absent", "absent"},
			{"the volume group survives", "sudo -n vgs kubenest-vg >/dev/null 2>&1 && echo present || echo absent", "present"},
		}
		for _, c := range checks {
			res, err := nodes[0].Runner.Run(ctx, c.command)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got := strings.TrimSpace(res.Stdout); got != c.want {
				t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			}
		}
	})
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
