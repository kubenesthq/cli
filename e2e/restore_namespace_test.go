//go:build e2e

// S4's gate: "I deleted the payments namespace" (PLAN 7.5, kn-x0wv).
//
// THE FIXTURE IS ONE LAB HOST with the platform reconcilers (the operator and
// Argo CD) running normally and a configured backup target. The scenario is the
// everyday restore, and what it checks cannot be checked anywhere else: that a
// namespace comes back with its objects and its volume's bytes, that the
// reconcilers cannot undo the restore while it runs, that nothing runs by
// surprise before activation, and that an interrupted restore is resumable and
// NEVER activates.
//
// WHAT A HARDWARE RUN NEEDS, IN FULL:
//
//	./scripts/ephemeral-env.sh up --profile host --nodes 1
//	source lab/hetzner/.lab-env.sh                 # KUBENEST_LAB_NODE1_IP etc.
//	export KUBENEST_CONTROL_PLANE=… KUBENEST_CLI_TOKEN=…   # a control plane
//	export KUBENEST_BACKUP_ACCESS_KEY_ID=… KUBENEST_BACKUP_SECRET_ACCESS_KEY=…
//	export KUBENEST_E2E_SENTINEL_HOST=<an address the LAB can reach on this workstation>
//	cd kubenest-cli && go test -tags e2e -run TestRestoreNamespaceScenarioS4 -v -timeout 4h ./e2e/
//
// The sentinel is a real HTTP receiver bound on this workstation: the CronJob
// and the positive-control Job call it over the lab network, which is the only
// way to prove that "nothing ran before activation" is a fact about the cluster
// rather than about the test's own bookkeeping.
package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
)

// s4Project is the project the gate creates through the control plane. Its
// NAMESPACE comes back from the control plane rather than being derived here.
const s4Project = "e2e-restore-ns"

// s4SentinelPath is the path every sentinel call uses.
const s4SentinelPath = "/sentinel"

// s4Sentinel is a real HTTP receiver with a positive control.
type s4Sentinel struct {
	listener net.Listener
	server   *http.Server
	address  string
	mu       sync.Mutex
	hits     []string
}

// s4StartSentinel starts the receiver the cluster's workloads call. advertised
// is KUBENEST_E2E_SENTINEL_HOST, the address the LAB reaches this workstation
// at. When it names a port, the receiver listens on that port, because the
// tunnel or firewall rule the lab uses was opened for it before the run;
// without one it takes any free port and the URL carries it. The receiver used
// to take a random port while the URL named only the host, so no pod could
// ever reach it.
func s4StartSentinel(t *testing.T, advertised string) *s4Sentinel {
	t.Helper()
	listen := "0.0.0.0:0"
	_, port, splitErr := net.SplitHostPort(advertised)
	if splitErr == nil {
		listen = net.JoinHostPort("0.0.0.0", port)
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatalf("binding the sentinel receiver on %s: %v", listen, err)
	}
	s := &s4Sentinel{listener: listener, address: advertised}
	if splitErr != nil {
		s.address = net.JoinHostPort(advertised, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	}
	s.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != s4SentinelPath {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.hits = append(s.hits, time.Now().UTC().Format(time.RFC3339))
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = s.server.Serve(listener) }()
	t.Cleanup(func() { _ = s.server.Close() })
	return s
}

func (s *s4Sentinel) url() string {
	return fmt.Sprintf("http://%s%s", s.address, s4SentinelPath)
}

func (s *s4Sentinel) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hits)
}

// s4Cluster is the gate's handle on the lab: the SSH transport every kubectl
// call goes through, and the control plane the project is created on.
type s4Cluster struct {
	t       *testing.T
	env     gateEnv
	runner  *sshx.Client
	bundle  string
	cluster string
	client  *api.Client
}

func s4Open(t *testing.T, env gateEnv) *s4Cluster {
	t.Helper()
	ep, err := sshx.Resolve(env.server, sshx.Options{User: env.sshUser, KeyPath: env.sshKey})
	if err != nil {
		t.Fatalf("resolving %s: %v", env.server, err)
	}
	runner, err := sshx.Dial(context.Background(), ep, sshx.Options{KeyPath: env.sshKey})
	if err != nil {
		t.Fatalf("dialling %s: %v", env.server, err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatalf("building the control-plane client: %v", err)
	}
	// The bundle the CLUSTER runs, fetched from the control plane: an e2e run
	// that used a local file would be testing a manifest this cluster is not
	// installed from.
	bundlePath := s4BundleManifest(t, env, client)
	return &s4Cluster{t: t, env: env, runner: runner, bundle: bundlePath, cluster: env.cluster, client: client}
}

func s4BundleManifest(t *testing.T, env gateEnv, client *api.Client) string {
	t.Helper()
	raw, err := client.BundleManifest(context.Background(), env.bundle)
	if err != nil {
		t.Fatalf("fetching bundle %s from the control plane: %v", env.bundle, err)
	}
	path := t.TempDir() + "/platform-" + env.bundle + ".yaml"
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (c *s4Cluster) kubectl(args string) string {
	c.t.Helper()
	out, err := k3s.Kubectl(context.Background(), c.runner, args)
	if err != nil {
		c.t.Fatalf("kubectl %s: %v", args, err)
	}
	return out
}

func (c *s4Cluster) apply(doc string) {
	c.t.Helper()
	res, err := c.runner.RunInput(context.Background(), "sudo -n k3s kubectl apply -f -", strings.NewReader(doc))
	if err != nil {
		c.t.Fatalf("applying a document: %v", err)
	}
	if res.ExitCode != 0 {
		c.t.Fatalf("applying a document: exit %d: %s", res.ExitCode, firstLineOfE2E(res.Stderr))
	}
}

func firstLineOfE2E(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// waitFor polls one condition to a deadline, reporting the last observation.
func (c *s4Cluster) waitFor(within time.Duration, what string, check func() (bool, string)) {
	c.t.Helper()
	deadline := time.Now().Add(within)
	last := "not observed yet"
	for {
		ok, detail := check()
		if ok {
			c.t.Logf("%s: %s", what, detail)
			return
		}
		last = detail
		if time.Now().After(deadline) {
			c.t.Fatalf("%s did not happen within %s: %s", what, within, last)
		}
		time.Sleep(5 * time.Second)
	}
}

// createProject creates the project THROUGH THE CONTROL PLANE, which is what
// makes the namespace the reconcilers' to create: the gate is "I deleted the
// payments namespace", and the namespace has to be one the platform put there.
// sessionClient signs in as the administrator for the one call that needs a
// user session. POST /api/v1/projects accepts no CLI token, because projects
// belong to the console, so the fixture's project is created the way the
// console creates one. On hardware (2026-09-27) the gate's CLI token was
// refused there.
func (c *s4Cluster) sessionClient() *api.Client {
	c.t.Helper()
	email, password := os.Getenv("KUBENEST_ADMIN_EMAIL"), os.Getenv("KUBENEST_ADMIN_PASSWORD")
	if email == "" || password == "" {
		c.t.Skip("KUBENEST_ADMIN_EMAIL and KUBENEST_ADMIN_PASSWORD are not set: creating the fixture's project needs a user session (POST /api/v1/projects accepts no CLI token)")
	}
	anonymous, err := api.New(c.env.controlPlane)
	if err != nil {
		c.t.Fatalf("building a client for the administrator's login: %v", err)
	}
	token, err := anonymous.PasswordLogin(context.Background(), email, password)
	if err != nil {
		c.t.Fatalf("signing in as %s to create the fixture's project: %v", email, err)
	}
	session, err := api.New(c.env.controlPlane, api.WithToken(token))
	if err != nil {
		c.t.Fatalf("building the administrator's session client: %v", err)
	}
	return session
}

func (c *s4Cluster) createProject(name string) string {
	c.t.Helper()
	orgs, err := c.client.ListOrgs(context.Background())
	if err != nil || len(orgs) == 0 {
		c.t.Fatalf("listing organisations: %v (%d orgs)", err, len(orgs))
	}
	var clusterID string
	for _, org := range orgs {
		clusters, err := c.client.ListOrgClusters(context.Background(), org.ID)
		if err != nil {
			c.t.Fatalf("listing %s's clusters: %v", org.Name, err)
		}
		for _, cluster := range clusters {
			if cluster.Name == c.cluster {
				clusterID = cluster.ID
			}
		}
	}
	if clusterID == "" {
		c.t.Fatalf("the control plane has no cluster named %s", c.cluster)
	}
	project, err := c.sessionClient().CreateProject(context.Background(), clusterID, name)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
		c.t.Fatalf("creating project %s through the control plane: %v", name, err)
	}
	if project == nil || project.Namespace == "" {
		c.t.Fatalf("the control plane returned no namespace for project %s", name)
	}
	// The operator reconciles the Project CR into a namespace; the gate waits
	// for the namespace the control plane named.
	c.waitFor(10*time.Minute, "the project's namespace to appear", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get namespace "+project.Namespace+" -o name")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) != "", project.Namespace + " exists"
	})
	return project.Namespace
}

// runCLI runs the real command tree, so the gate tears nothing down and
// rebuilds it: it drives the verb an operator types.
func (c *s4Cluster) runCLI(out io.Writer, args ...string) error {
	c.t.Helper()
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// runCLIWithContext is the same with a context the gate can cancel mid-restore.
func (c *s4Cluster) runCLIWithContext(ctx context.Context, out io.Writer, args ...string) error {
	c.t.Helper()
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

// verbArgs is the transport every restore call carries: the server the CLI
// reaches the cluster through, and the bundle it reads its deadlines from.
func (c *s4Cluster) verbArgs(extra ...string) []string {
	args := []string{
		"--cluster", c.cluster,
		"--server", c.env.server,
		"--ssh-user", c.env.sshUser,
		"--ssh-key", c.env.sshKey,
		"--bundle-manifest", c.bundle,
	}
	return append(args, extra...)
}

// TestRestoreNamespaceScenarioS4 is the scenario gate.
func TestRestoreNamespaceScenarioS4(t *testing.T) {
	env := gateEnvironment(t)
	sentinelHost := os.Getenv("KUBENEST_E2E_SENTINEL_HOST")
	if sentinelHost == "" {
		t.Skip("KUBENEST_E2E_SENTINEL_HOST is not set: the due CronJob and the positive-control Job have to reach an HTTP receiver on this workstation, so a run without it cannot prove that nothing ran before activation")
	}
	sentinel := s4StartSentinel(t, sentinelHost)
	c := s4Open(t, env)

	namespace := c.createProject(s4Project)
	// A RUN FROM A PREVIOUS ATTEMPT MUST NOT BE MISTAKEN FOR THIS ONE: the
	// restore verb refuses to start while another operation holds the record,
	// which is the lock working, and a stale record would fail the gate for the
	// wrong reason.
	c.kubectl("delete configmap kubenest-operation -n kube-system --ignore-not-found")

	const proofPath = "/data/proof.txt"
	proof := fmt.Sprintf("s4-%d", time.Now().UnixNano())
	c.apply(s4WorkloadDocument(namespace, proof))
	c.waitFor(5*time.Minute, "the workload's PVC and pod", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get pods -n "+namespace+" -l app=s4-web -o jsonpath={.items[0].status.phase}")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) == "Running", strings.TrimSpace(out)
	})
	written := c.waitForProof(namespace, proofPath)

	// THE POSITIVE CONTROL: the sentinel path is proven to work BEFORE the
	// assertion that it stays silent means anything.
	before := sentinel.count()
	c.apply(s4SentinelProbeJob(namespace, sentinel.url()))
	c.waitFor(5*time.Minute, "the sentinel positive control", func() (bool, string) {
		if sentinel.count() > before {
			return true, "the receiver saw the probe Job's call"
		}
		return false, "the probe Job has not called the receiver yet"
	})

	// A due CronJob and a Job that WOULD call the sentinel if they ran.
	c.apply(s4CronJobDocument(namespace, sentinel.url()))
	c.apply(s4JobDocument(namespace, sentinel.url()))

	var backupOut strings.Builder
	if err := c.runCLI(&backupOut, append([]string{"backup", "now"}, c.verbArgs()...)...); err != nil {
		t.Fatalf("backup now failed: %v\n%s", err, backupOut.String())
	}
	backup := s4BackupName(backupOut.String())
	if backup == "" {
		t.Fatalf("backup now did not name the backup it took:\n%s", backupOut.String())
	}
	t.Logf("backup %s taken", backup)

	// The deletion the scenario starts from, then the reconcilers' own
	// re-creation of the namespace — with empty claims, which is the ordinary
	// state `--replace` exists for.
	c.kubectl("delete namespace " + namespace + " --wait=false")
	c.waitFor(10*time.Minute, "the reconcilers to recreate the namespace", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get namespace "+namespace+" -o name")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) != "", "the namespace is back"
	})
	c.waitFor(10*time.Minute, "the recreated namespace to hold an empty claim", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get persistentvolumeclaim -n "+namespace+" -o jsonpath={.items[*].status.phase}")
		if err != nil {
			return false, err.Error()
		}
		phases := strings.Fields(out)
		return len(phases) > 0, strings.Join(phases, ",")
	})

	hitsBeforeRestore := sentinel.count()

	var plan strings.Builder
	if err := c.runCLI(&plan, append([]string{"backup", "restore", "--namespace", namespace, "--latest", "--replace", "--confirm"}, c.verbArgs()...)...); err != nil {
		t.Fatalf("the restore failed: %v\n%s", err, plan.String())
	}
	for _, want := range []string{"Restore plan for namespace " + namespace, "data age:", "coverage:", "consistency:", "discarded:", "restored — awaiting activation"} {
		if !strings.Contains(plan.String(), want) {
			t.Errorf("the run does not print %q:\n%s", want, plan.String())
		}
	}
	operationID := s4OperationID(plan.String())
	if operationID == "" {
		t.Fatalf("the run printed no operation id, so nothing can be activated:\n%s", plan.String())
	}

	// THE PROJECT IS PAUSED and the pause names this operation.
	pause := c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath={.metadata.annotations.kubenest\\.io/reconcile-paused}")
	if strings.TrimSpace(pause) != operationID {
		t.Errorf("the project's pause annotation is %q, want the operation %s", pause, operationID)
	}
	// The data is back: same objects, same file bytes.
	c.waitFor(10*time.Minute, "the restored claim to be bound", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get persistentvolumeclaim -n "+namespace+" -o jsonpath={.items[*].status.phase}")
		if err != nil {
			return false, err.Error()
		}
		phases := strings.Fields(out)
		for _, phase := range phases {
			if phase != "Bound" {
				return false, strings.Join(phases, ",")
			}
		}
		return len(phases) > 0, strings.Join(phases, ",")
	})
	if restored := c.sha256OfProof(namespace, proofPath); restored != written {
		t.Errorf("the restored file's SHA-256 is %s, want the %s written before the backup", restored, written)
	}
	// The Job is NOT restored, and the CronJob is suspended.
	if jobs := strings.TrimSpace(c.kubectl("get jobs -n " + namespace + " -o name")); jobs != "" {
		t.Errorf("a Job came back with the restore: %s", jobs)
	}
	if suspend := strings.TrimSpace(c.kubectl("get cronjob s4-sentinel -n " + namespace + " -o jsonpath={.spec.suspend}")); suspend != "true" {
		t.Errorf("the restored CronJob's suspend is %q, want true", suspend)
	}

	// NEITHER RECONCILER RESTART CHANGES THE HOLD: the pause is an annotation
	// on the Project, which lives outside the namespace.
	s4Restart(t, c, "kubenest-system", "kubenest-operator")
	s4Restart(t, c, "argocd", "argocd-application-controller")
	c.waitFor(2*time.Minute, "the operator to come back", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get deployment kubenest-operator -n kubenest-system -o jsonpath={.status.availableReplicas}")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0", strings.TrimSpace(out)
	})
	if pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath={.metadata.annotations.kubenest\\.io/reconcile-paused}")); pause != operationID {
		t.Errorf("a reconciler restart lifted the pause (annotation %q)", pause)
	}
	if suspend := strings.TrimSpace(c.kubectl("get cronjob s4-sentinel -n " + namespace + " -o jsonpath={.spec.suspend}")); suspend != "true" {
		t.Errorf("a reconciler restart un-suspended the CronJob (%q)", suspend)
	}

	// ACTIVATION shows what may run, then lifts the pause.
	var activation strings.Builder
	if err := c.runCLI(&activation, append([]string{"backup", "restore", "--activate", operationID, "--keep-desired"}, c.verbArgs()...)...); err != nil {
		t.Fatalf("activation failed: %v\n%s", err, activation.String())
	}
	if !strings.Contains(activation.String(), "CronJob s4-sentinel") {
		t.Errorf("activation does not show which scheduled work will start:\n%s", activation.String())
	}
	if pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath={.metadata.annotations.kubenest\\.io/reconcile-paused}")); pause != "" {
		t.Errorf("activation left the pause in place (%q)", pause)
	}
	if suspend := strings.TrimSpace(c.kubectl("get cronjob s4-sentinel -n " + namespace + " -o jsonpath={.spec.suspend}")); suspend != "false" {
		t.Errorf("activation left the CronJob suspended (%q)", suspend)
	}
	// THE SENTINEL MUST STILL BE SILENT: a Job restored by --include-jobs (not
	// used here) or a CronJob that ran before activation would have called it.
	if got := sentinel.count(); got != hitsBeforeRestore {
		// A positive control is one hit; anything more is a Job or CronJob that
		// ran when it should not have.
		t.Errorf("the sentinel receiver saw %d call(s) between the restore and activation, want none", got-hitsBeforeRestore)
	}

	t.Run("an interrupted restore resumes without activating", func(t *testing.T) {
		s4InterruptedResume(t, c, namespace, proof, proofPath)
	})
}

// s4InterruptedResume kills the CLI mid-restore and finishes with --resume.
func s4InterruptedResume(t *testing.T, c *s4Cluster, namespace, proof, proofPath string) {
	t.Helper()
	if err := c.runCLI(io.Discard, append([]string{"backup", "now"}, c.verbArgs()...)...); err != nil {
		t.Fatalf("backup now before the interrupted run failed: %v", err)
	}
	c.kubectl("delete namespace " + namespace + " --wait=false")
	c.waitFor(10*time.Minute, "the namespace to be recreated", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get namespace "+namespace+" -o name")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) != "", "the namespace is back"
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- c.runCLIWithContext(ctx, &out, append([]string{"backup", "restore", "--namespace", namespace, "--latest", "--replace", "--confirm"}, c.verbArgs()...)...)
	}()
	// KILLED MID-RESTORE, once the Velero Restore exists: that is the window
	// between "the namespace is gone" and "the data is back".
	c.waitFor(20*time.Minute, "the Velero Restore to be created", func() (bool, string) {
		restores := strings.TrimSpace(c.kubectl("get restores.velero.io -n velero -o name"))
		return restores != "", restores
	})
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("the killed run reported success: a cancelled restore cannot have finished")
	}
	t.Logf("the interrupted run stopped with: %v", err)

	// THE PAUSE IS STILL IN PLACE, and it names the interrupted operation.
	pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath={.metadata.annotations.kubenest\\.io/reconcile-paused}"))
	if pause == "" {
		t.Fatalf("the interrupted run left no pause behind:\n%s", out.String())
	}
	operationID := pause

	// The record is what `kubenest health` shows the operation by, and it is
	// the record a second laptop resumes.
	record := c.kubectl("get configmap kubenest-operation -n kube-system -o jsonpath={.data.record\\.json}")
	if !strings.Contains(record, operationID) {
		t.Errorf("the live operation record does not name %s", operationID)
	}

	var resumed strings.Builder
	if err := c.runCLI(&resumed, append([]string{"backup", "restore", "--resume", operationID}, c.verbArgs()...)...); err != nil {
		t.Fatalf("--resume failed: %v\n%s", err, resumed.String())
	}
	if !strings.Contains(resumed.String(), "restored — awaiting activation") {
		t.Errorf("the resume did not finish the data restore:\n%s", resumed.String())
	}
	// A RESUME NEVER ACTIVATES.
	if pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath={.metadata.annotations.kubenest\\.io/reconcile-paused}")); pause != operationID {
		t.Errorf("--resume lifted the pause (annotation %q)", pause)
	}
	if suspend := strings.TrimSpace(c.kubectl("get cronjob s4-sentinel -n " + namespace + " -o jsonpath={.spec.suspend}")); suspend != "true" {
		t.Errorf("--resume un-suspended the CronJob (%q): activating is not resuming", suspend)
	}
	if restored := c.sha256OfProof(namespace, proofPath); restored != "" {
		t.Logf("restored proof file after the resume: %s (%s)", restored, proof)
	}
}

// s4Restart restarts one deployment by name, if the cluster has it: an install
// without Argo CD has no argocd namespace, and that is not a gate failure.
func s4Restart(t *testing.T, c *s4Cluster, namespace, name string) {
	t.Helper()
	if _, err := k3s.Kubectl(context.Background(), c.runner, "get deployment "+name+" -n "+namespace+" -o name"); err != nil {
		t.Logf("%s/%s is not on this cluster, so there is nothing to restart", namespace, name)
		return
	}
	if _, err := k3s.Kubectl(context.Background(), c.runner, "rollout restart deployment "+name+" -n "+namespace); err != nil {
		t.Fatalf("restarting %s/%s: %v", namespace, name, err)
	}
	if _, err := k3s.Kubectl(context.Background(), c.runner, "rollout status deployment "+name+" -n "+namespace+" --timeout=5m"); err != nil {
		t.Fatalf("waiting for %s/%s to come back: %v", namespace, name, err)
	}
}

// s4OperationID reads the operation id out of the run's own output.
func s4OperationID(out string) string {
	const prefix = "--activate "
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, prefix); i >= 0 {
			rest := line[i+len(prefix):]
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return ""
}

func s4BackupName(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "backup ") && strings.Contains(line, " completed on ") {
			return strings.Fields(line)[1]
		}
	}
	return ""
}

// s4WorkloadDocument is the fixture: a Deployment with a claim holding a known
// file, pinned to the one lab node.
func s4WorkloadDocument(namespace, proof string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: s4-data, namespace: %[1]s}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: s4-web, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: s4-web}}
  template:
    metadata: {labels: {app: s4-web}}
    spec:
      containers:
        - name: web
          image: busybox:1.36
          command: [sh, -c, "echo %[2]s > /data/proof.txt && sync && sleep 1000000"]
          volumeMounts: [{name: data, mountPath: /data}]
      volumes:
        - {name: data, persistentVolumeClaim: {claimName: s4-data}}
`, namespace, proof)
}

// s4JobDocument is a Job that would call the sentinel at once.
func s4JobDocument(namespace, url string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata: {name: s4-once, namespace: %[1]s}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: call
          image: busybox:1.36
          command: [wget, -q, -O, /dev/null, "%[2]s"]
`, namespace, url)
}

// s4CronJobDocument is a CronJob that is due every minute.
func s4CronJobDocument(namespace, url string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: CronJob
metadata: {name: s4-sentinel, namespace: %[1]s}
spec:
  schedule: "* * * * *"
  suspend: false
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: call
              image: busybox:1.36
              command: [wget, -q, -O, /dev/null, "%[2]s"]
`, namespace, url)
}

// s4SentinelProbeJob is the POSITIVE CONTROL: one Job that calls the sentinel
// on purpose, so a later silence is evidence about the cluster rather than
// about the receiver never having worked.
func s4SentinelProbeJob(namespace, url string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata: {name: s4-sentinel-probe, namespace: %[1]s}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: probe
          image: busybox:1.36
          command: [wget, -q, -O, /dev/null, "%[2]s"]
`, namespace, url)
}

// waitForProof reads the known file's digest through the pod that mounts it,
// waiting for the workload to be there. The digest is what makes "the same
// bytes came back" a fact rather than a hope.
func (c *s4Cluster) waitForProof(namespace, path string) string {
	c.t.Helper()
	var digest string
	c.waitFor(5*time.Minute, "the fixture's file to be readable", func() (bool, string) {
		digest = c.sha256OfProof(namespace, path)
		if digest == "" {
			return false, "no digest yet (the workload may still be starting)"
		}
		return true, digest
	})
	return digest
}

// sha256OfProof reads the file's digest through the pod that mounts it, or ""
// when the pod or the file is not there yet.
func (c *s4Cluster) sha256OfProof(namespace, path string) string {
	c.t.Helper()
	out, err := k3s.Kubectl(context.Background(), c.runner, "get pods -n "+namespace+" -l app=s4-web -o jsonpath={.items[0].metadata.name}")
	if err != nil {
		return ""
	}
	pod := strings.TrimSpace(out)
	if pod == "" {
		return ""
	}
	out, err = k3s.Kubectl(context.Background(), c.runner,
		fmt.Sprintf("-n %s exec %s -- sha256sum %s", namespace, pod, path))
	if err != nil {
		c.t.Logf("reading %s through pod %s/%s: %v", path, namespace, pod, err)
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
