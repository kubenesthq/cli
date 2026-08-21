//go:build e2e

// The wave-3 gate for kn-fuo, on a REAL cluster with a REAL workload running.
//
// What it asserts, which is the gate:
//
//	a bundle upgrade completes with ZERO DOWNTIME for a replicated deployment
//	a forced mid-upgrade failure rolls back
//
// The transition is 0.9 → 1.0, which differs in exactly one pin: the
// Kubernetes version. That is deliberate — it is the sharpest possible test,
// because every node drains and returns, and the point of no return is
// genuinely crossed rather than stepped around.
//
// Run from the umbrella workspace with a TWO-node lab (zero downtime is not
// available on one node: when its only node drains, everything on it
// restarts, and upgrades.mdx says so plainly):
//
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=...
//	cd kubenest-cli && go test -tags e2e -v -timeout 90m ./e2e/ -run TestUpgradeGate
package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/backup"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/uninstall"
	"kubenest.io/cli/pkg/upgrade"
)

// availabilityWorkload is the thing the gate actually measures: a replicated
// deployment, spread across nodes, reachable through a Service.
//
// The spread constraint is what makes zero downtime possible at all — two
// replicas on one node both die when it drains. whenUnsatisfiable is
// ScheduleAnyway rather than DoNotSchedule so a drained replica can land
// anywhere during the upgrade rather than staying Pending, which would be an
// outage caused by the test's own scheduling rules.
const availabilityWorkload = `apiVersion: v1
kind: Namespace
metadata:
  name: gate-availability
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: always-up
  namespace: gate-availability
spec:
  replicas: 2
  selector:
    matchLabels: {app: always-up}
  template:
    metadata:
      labels: {app: always-up}
    spec:
      terminationGracePeriodSeconds: 1
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: kubernetes.io/hostname
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels: {app: always-up}
      containers:
        - name: whoami
          image: traefik/whoami:v1.10.2
          ports: [{containerPort: 80}]
          readinessProbe:
            httpGet: {path: /, port: 80}
            periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata:
  name: always-up
  namespace: gate-availability
spec:
  type: NodePort
  selector: {app: always-up}
  ports:
    - port: 80
      targetPort: 80
      nodePort: 30080
`

// availabilityProbe polls the workload continuously from a node, recording
// every failure. It runs ON the cluster rather than from this machine so the
// measurement is of the workload's availability rather than of the lab's
// internet link.
type availabilityProbe struct {
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	attempts int
	failures []string
}

func pollWorkload(t *testing.T, runner k3s.Runner, target string) *availabilityProbe {
	t.Helper()
	p := &availabilityProbe{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			select {
			case <-p.stop:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			res, err := runner.Run(ctx, fmt.Sprintf(
				`curl -s -o /dev/null -w '%%{http_code}' --max-time 3 http://%s/`, target))
			cancel()
			p.mu.Lock()
			p.attempts++
			switch {
			case err != nil:
				// The SSH hop itself failed: on the single-server tier the
				// control-plane node's API is briefly away, but SSH is not,
				// so this is recorded rather than excused.
				p.failures = append(p.failures, fmt.Sprintf("%s probe error: %v", time.Now().Format("15:04:05"), err))
			case strings.TrimSpace(res.Stdout) != "200":
				p.failures = append(p.failures, fmt.Sprintf("%s HTTP %q", time.Now().Format("15:04:05"), strings.TrimSpace(res.Stdout)))
			}
			p.mu.Unlock()
			time.Sleep(time.Second)
		}
	}()
	return p
}

func (p *availabilityProbe) result() (attempts int, failures []string) {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts, append([]string(nil), p.failures...)
}

type upgradeEnv struct {
	gateEnv
	agent string
}

func upgradeEnvironment(t *testing.T) upgradeEnv {
	t.Helper()
	base := gateEnvironment(t)
	agent := os.Getenv("KUBENEST_LAB_NODE2_IP")
	if agent == "" {
		t.Skip("KUBENEST_LAB_NODE2_IP not set: zero downtime needs two nodes — on one node, everything on it restarts when it drains")
	}
	return upgradeEnv{gateEnv: base, agent: agent}
}

func TestUpgradeGate(t *testing.T) {
	env := upgradeEnvironment(t)
	ctx := context.Background()

	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}
	const from, to = "0.9", "1.0"

	// A cluster installed at 0.9, with an agent node, so a drain has
	// somewhere to put the workload.
	installOpts := install.Options{
		Bundle: from, Name: env.cluster, HATier: "single-server",
		Servers: []string{env.server}, Agents: []string{env.agent},
		SSHUser: env.sshUser, SSHKey: env.sshKey,
		StorageDevice: env.storageDevice,
	}
	journalPath := t.TempDir() + "/install.json"

	t.Run("install the cluster at the previous bundle", func(t *testing.T) {
		bundle := fetchBundle(t, client, from)
		s, _ := session(t, env.gateEnv, journalPath, bundle, installOpts)
		defer s.Close()
		if _, err := install.Execute(ctx, s, install.Plan(s)); err != nil {
			t.Fatalf("installing at %s: %v", from, err)
		}
		t.Logf("installed at bundle %s", from)
	})

	server := connectNodes(t, env.gateEnv)[0]

	t.Run("a replicated workload is running and reachable", func(t *testing.T) {
		if err := kubectlApplyDoc(ctx, server.Runner, availabilityWorkload); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			out, err := k3s.Kubectl(ctx, server.Runner,
				"get deployment always-up -n gate-availability -o jsonpath='{.status.readyReplicas}'")
			if err == nil && strings.Trim(strings.TrimSpace(out), "'") == "2" {
				t.Log("both replicas ready")
				return
			}
			time.Sleep(5 * time.Second)
		}
		t.Fatal("the workload never became ready, so there is nothing to measure")
	})

	t.Run("a restore drill has passed", func(t *testing.T) {
		// The upgrade refuses without fresh evidence that restore works, and
		// it is right to: rollback partly depends on restore, so an untested
		// restore is not a rollback plan. kn-f9lm produces this evidence on
		// a schedule; until that lands, the lab writes a real result object
		// which the real reader reads — fixture data in a real cluster,
		// not a stub in the code path.
		result := fmt.Sprintf(`{"status":"passed","completed_at":%q,"backup":"gate-fixture","duration_seconds":42}`,
			time.Now().UTC().Format(time.RFC3339))
		// The object, namespace and key are pkg/backup's — the package that
		// writes them for real — so this fixture cannot drift from what the
		// drill actually produces.
		doc := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  %s: '%s'
`, backup.DrillResultName, backup.Namespace, backup.DrillResultDataKey, result)
		if err := kubectlApplyDoc(ctx, server.Runner, doc); err != nil {
			t.Fatal(err)
		}
		drills := upgrade.InClusterDrills{Runner: server.Runner}
		got, err := drills.LastRestoreDrill(ctx)
		if err != nil {
			t.Fatalf("the gate must be able to read the evidence it refuses without: %v", err)
		}
		if got.Status != "passed" {
			t.Fatalf("read back status %q", got.Status)
		}
	})

	var upgraded bool

	t.Run("the upgrade completes with zero downtime for the replicated workload", func(t *testing.T) {
		// The readiness gate requires every node to have been Ready for
		// limits.timeouts.node-ready before an upgrade starts, so a cluster
		// installed a minute ago cannot be upgraded — deliberately: a node
		// flapping in and out of Ready passes a single sample and then fails
		// mid-drain. The gate is right, so the test waits it out rather than
		// weakening it.
		waitForNodeDwell(t, ctx, server.Runner, fetchBundle(t, client, to))

		probe := pollWorkload(t, server.Runner, env.server+":30080")

		s := upgradeSession(t, env, client, from, to, nil)
		defer s.Close()
		start := time.Now()
		result, err := stages.Execute(ctx, s, upgrade.Plan(s))
		elapsed := time.Since(start)

		attempts, failures := probe.result()
		t.Logf("upgrade took %s; the workload was polled %d times during it", elapsed.Round(time.Second), attempts)
		if err != nil {
			t.Fatalf("the upgrade failed: %v", err)
		}
		upgraded = true
		t.Logf("ran %v", result.Ran)

		if attempts < 30 {
			t.Errorf("only %d probes in %s — too few to claim anything about availability", attempts, elapsed)
		}
		if len(failures) > 0 {
			t.Errorf("THE WORKLOAD WAS UNAVAILABLE during the upgrade: %d of %d probes failed:\n  %s",
				len(failures), attempts, strings.Join(failures, "\n  "))
		}
	})

	t.Run("every node is on the new Kubernetes version", func(t *testing.T) {
		if !upgraded {
			t.Skip("the upgrade did not complete")
		}
		target := fetchBundle(t, client, to)
		want, err := target.Core.Version("k3s")
		if err != nil {
			t.Fatal(err)
		}
		out, err := k3s.Kubectl(ctx, server.Runner, `get nodes -o jsonpath='{.items[*].status.nodeInfo.kubeletVersion}'`)
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
		t.Logf("both nodes on %s", want)
	})

	t.Run("the control plane records the new bundle", func(t *testing.T) {
		if !upgraded {
			t.Skip("the upgrade did not complete")
		}
		clusterID := clusterIDFor(t, ctx, client, env.cluster)
		record, err := client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		if record.BundleVersion != to {
			t.Errorf("the record says %s, want %s — nothing in the day-2 story is trustworthy if this drifts", record.BundleVersion, to)
		}
	})

	t.Run("a forced mid-upgrade failure rolls back", func(t *testing.T) {
		if !upgraded {
			t.Skip("the first upgrade did not complete")
		}
		// Go back to 0.9 with a poisoned component pin, so the upgrade fails
		// in the platform-components stage — before the point of no return,
		// which is where the rollback is a component revert.
		poisoned := fetchBundle(t, client, from)
		poisoned.Core["traefik"] = "0.0.0-does-not-exist"
		poisoned.Limits.Timeouts["component-ready"] = 2 * time.Minute

		s := upgradeSession(t, env, client, to, from, poisoned)
		defer s.Close()

		_, err := stages.Execute(ctx, s, upgrade.Plan(s))
		if err == nil {
			t.Fatal("a bundle pinning traefik to a version that does not exist must fail")
		}
		var stageErr *stages.StageError
		if !errorsAs(err, &stageErr) {
			t.Fatalf("want a *StageError, got %T: %v", err, err)
		}
		t.Logf("failed as designed: stage=%s component=%s", stageErr.Stage, stageErr.Component)
		if stageErr.Stage != upgrade.StageComponents {
			t.Errorf("failed at %q, want %s", stageErr.Stage, upgrade.StageComponents)
		}
		if stageErr.Component != "traefik" {
			t.Errorf("the failure names %q, want traefik", stageErr.Component)
		}

		// The rollback: before the point of no return, so a component revert.
		plan := s.RollbackPlan()
		t.Logf("rollback plan:\n%s", plan)
		if plan.Mechanism != upgrade.MechanismComponents {
			t.Fatalf("mechanism is %s; a failure before the kubernetes stage must revert components, not restore the datastore", plan.Mechanism)
		}
		if err := s.Rollback(ctx, plan); err != nil {
			t.Fatalf("rolling back: %v", err)
		}

		// And the cluster is back where it was: the workload still serving,
		// and the record saying so.
		out, err := k3s.Kubectl(ctx, server.Runner,
			"get deployment always-up -n gate-availability -o jsonpath='{.status.readyReplicas}'")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Trim(strings.TrimSpace(out), "'"); got != "2" {
			t.Errorf("after the rollback the workload has %q ready replicas, want 2", got)
		}
		clusterID := clusterIDFor(t, ctx, client, env.cluster)
		record, err := client.BundleRecord(ctx, clusterID)
		if err != nil {
			t.Fatal(err)
		}
		if record.BundleVersion != to {
			t.Errorf("after a rollback the record says %s, want %s — a record that claims a version the cluster is not on is worse than no record",
				record.BundleVersion, to)
		}
	})

	t.Run("uninstall leaves a known-clean machine", func(t *testing.T) {
		nodes := connectAllNodes(t, env)
		if err := uninstallAll(ctx, t, nodes); err != nil {
			t.Fatal(err)
		}
	})
}

// upgradeSession builds an upgrade session against the lab cluster. `to` is
// the version being moved to; override is a doctored manifest for injection.
func upgradeSession(t *testing.T, env upgradeEnv, client *api.Client, from, to string, override *manifest.Manifest) *upgrade.Session {
	t.Helper()
	ctx := context.Background()
	clusterID := clusterIDFor(t, ctx, client, env.cluster)

	recorded, err := upgrade.LoadRecord(ctx, client, clusterID)
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
	}
	journal, err := stages.OpenJournal(t.TempDir()+"/upgrade.json", opts.Identity(recorded.BundleVersion))
	if err != nil {
		t.Fatal(err)
	}
	journal.ClusterID = clusterID

	s := &upgrade.Session{
		ID: stages.NewRunID(), Opts: opts,
		From: fromBundle, To: toBundle,
		Jnl: journal, Reporter: converge.NewTextReporter(testWriter{t}),
		Out: testWriter{t}, API: client, Cluster: recorded,
		// The restore-drill gate reads real evidence from the cluster
		// (kn-f9lm). Until that lands, the gate would refuse every upgrade,
		// so the lab writes a real result object that the real reader reads
		// — fixture data in a real cluster, not a stub in the code path.
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

// waitForNodeDwell waits until every node has been Ready for the bundle's
// node-ready window, which is what the readiness gate requires.
//
// It reads JSON and parses it here rather than asking kubectl for a jsonpath:
// a jsonpath containing spaces, quotes, brackets and a * has to survive a
// shell, and one that does not survive it fails as "no output" — which is
// indistinguishable from "no nodes are ready yet" and waits out the whole
// window before saying so.
func waitForNodeDwell(t *testing.T, ctx context.Context, r k3s.Runner, bundle *manifest.Manifest) {
	t.Helper()
	dwell, err := bundle.Limits.Timeouts.For("node-ready")
	if err != nil {
		t.Fatal(err)
	}
	type nodeList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type               string    `json:"type"`
					Status             string    `json:"status"`
					LastTransitionTime time.Time `json:"lastTransitionTime"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}

	deadline := time.Now().Add(dwell + 5*time.Minute)
	for time.Now().Before(deadline) {
		out, err := k3s.Kubectl(ctx, r, "get nodes -o json")
		if err == nil {
			var nodes nodeList
			if err := json.Unmarshal([]byte(out), &nodes); err == nil && len(nodes.Items) > 0 {
				steady := true
				for _, n := range nodes.Items {
					for _, c := range n.Status.Conditions {
						if c.Type != "Ready" {
							continue
						}
						if c.Status != "True" || time.Since(c.LastTransitionTime) < dwell {
							steady = false
						}
					}
				}
				if steady {
					t.Logf("every node has been Ready for at least %s", dwell)
					return
				}
			}
		}
		time.Sleep(20 * time.Second)
	}
	t.Fatalf("nodes never settled for %s", dwell)
}

func clusterIDFor(t *testing.T, ctx context.Context, client *api.Client, name string) string {
	t.Helper()
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range orgs {
		clusters, err := client.ListOrgClusters(ctx, org.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range clusters {
			if c.Name == name {
				return c.ID
			}
		}
	}
	t.Fatalf("no cluster named %q", name)
	return ""
}

func kubectlApplyDoc(ctx context.Context, r k3s.Runner, doc string) error {
	encoded := base64Encode(doc)
	res, err := r.Run(ctx, fmt.Sprintf("printf '%%s' %s | base64 -d | sudo -n k3s kubectl apply -f -", encoded))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("applying: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func base64Encode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// connectAllNodes opens connections to every lab node for the teardown check.
func connectAllNodes(t *testing.T, env upgradeEnv) []uninstall.Node {
	t.Helper()
	opts := sshx.Options{
		User:           env.sshUser,
		KeyPath:        env.sshKey,
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
		DialTimeout:    15 * time.Second,
	}
	var nodes []uninstall.Node
	for i, address := range []string{env.server, env.agent} {
		endpoint, err := sshx.Resolve(address, opts)
		if err != nil {
			t.Fatal(err)
		}
		client, err := sshx.Dial(context.Background(), endpoint, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		role := uninstall.RoleServer
		if i > 0 {
			role = uninstall.RoleAgent
		}
		nodes = append(nodes, uninstall.Node{Address: address, Role: role, Runner: client})
	}
	return nodes
}

func uninstallAll(ctx context.Context, t *testing.T, nodes []uninstall.Node) error {
	t.Helper()
	if err := uninstall.Run(ctx, uninstall.Options{Nodes: nodes, Out: testWriter{t}}); err != nil {
		return err
	}
	for _, node := range nodes {
		assertHost(t, ctx, node, map[string]string{
			"k3s binary is gone":     "absent",
			"platform state is gone": "absent",
		})
	}
	return nil
}
