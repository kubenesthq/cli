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
	"encoding/json"
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
	"kubenest.io/cli/pkg/backup"
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

// hitsFrom is the time of every call from the n-th on, so a failure says WHEN
// a stray call came, which is what separates a restored CronJob from the
// recreated namespace's live one.
func (s *s4Sentinel) hitsFrom(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= len(s.hits) {
		return nil
	}
	return append([]string(nil), s.hits[n:]...)
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

// e2eFixtureCluster is what creating a gate's fixture project needs of that
// gate's cluster handle: a kubectl that reports its error instead of failing
// the test, because the creator POLLS with it, and a waitFor to poll.
type e2eFixtureCluster interface {
	kubectlStatus(args string) (string, error)
	waitFor(within time.Duration, what string, check func() (bool, string))
}

// e2eAdminSession signs in as the administrator for the one call that needs a
// user session. POST /api/v1/projects accepts no CLI token, because projects
// belong to the console, so a fixture's project is created the way the console
// creates one. On hardware (2026-09-27) the gate's CLI token was refused there.
func e2eAdminSession(t *testing.T, env gateEnv) *api.Client {
	t.Helper()
	email, password := os.Getenv("KUBENEST_ADMIN_EMAIL"), os.Getenv("KUBENEST_ADMIN_PASSWORD")
	if email == "" || password == "" {
		t.Skip("KUBENEST_ADMIN_EMAIL and KUBENEST_ADMIN_PASSWORD are not set: creating the fixture's project needs a user session (POST /api/v1/projects accepts no CLI token)")
	}
	anonymous, err := api.New(env.controlPlane)
	if err != nil {
		t.Fatalf("building a client for the administrator's login: %v", err)
	}
	token, err := anonymous.PasswordLogin(context.Background(), email, password)
	if err != nil {
		t.Fatalf("signing in as %s to create the fixture's project: %v", email, err)
	}
	session, err := api.New(env.controlPlane, api.WithToken(token))
	if err != nil {
		t.Fatalf("building the administrator's session client: %v", err)
	}
	return session
}

// createProjectNamespace creates a project THROUGH THE CONTROL PLANE and
// returns the namespace the control plane named, waiting for the operator to
// reconcile it into existence.
//
// THE PROJECT IS THE POINT, for every gate that needs one: the namespace has to
// be one the platform put there. S4's gate is "I deleted the payments
// namespace"; T4.3's mode 2 writes its reconcile pause on the Project named
// after the namespace and refuses when nothing can acknowledge it, so a bare
// Namespace object made its selective arm fail on hardware (2026-09-27) with
// `annotate project e2e-restore-volumes -n kubenest-system ...: NotFound`. The
// operator creates the project's namespace, so callers use the name that comes
// back, not one they derive.
//
// A NAME THAT ALREADY EXISTS IS NOT A FAILURE: POST refuses a duplicate, and a
// re-run — or the second gate in one package — must still learn the namespace,
// so the project is read back from the list. The namespace is only ever the
// control plane's to choose.
func createProjectNamespace(t *testing.T, env gateEnv, control *api.Client, clusterName, name string, c e2eFixtureCluster) string {
	t.Helper()
	orgs, err := control.ListOrgs(context.Background())
	if err != nil || len(orgs) == 0 {
		t.Fatalf("listing organisations: %v (%d orgs)", err, len(orgs))
	}
	var clusterID string
	for _, org := range orgs {
		clusters, err := control.ListOrgClusters(context.Background(), org.ID)
		if err != nil {
			t.Fatalf("listing %s's clusters: %v", org.Name, err)
		}
		for _, listed := range clusters {
			if listed.Name == clusterName {
				clusterID = listed.ID
			}
		}
	}
	if clusterID == "" {
		t.Fatalf("the control plane has no cluster named %s", clusterName)
	}
	session := e2eAdminSession(t, env)
	project, err := session.CreateProject(context.Background(), clusterID, name)
	if err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "already") {
			t.Fatalf("creating project %s through the control plane: %v", name, err)
		}
		project = findProject(t, session, clusterID, name)
	}
	if project == nil || project.Namespace == "" {
		t.Fatalf("the control plane returned no namespace for project %s", name)
	}
	// The operator reconciles the Project CR into a namespace; the gate waits
	// for the namespace the control plane named.
	c.waitFor(10*time.Minute, "the project's namespace to appear", func() (bool, string) {
		out, err := c.kubectlStatus("get namespace " + project.Namespace + " -o name")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) != "", project.Namespace + " exists"
	})
	return project.Namespace
}

// findProject reads one project back by name, for the duplicate case above.
func findProject(t *testing.T, session *api.Client, clusterID, name string) *api.Project {
	t.Helper()
	status, body, err := session.Get(context.Background(), "/api/v1/projects?cluster_id="+clusterID+"&items_per_page=100")
	if err != nil {
		t.Fatalf("listing the control plane's projects to read %s back: %v", name, err)
	}
	if status != http.StatusOK {
		t.Fatalf("listing the control plane's projects to read %s back: HTTP %d: %s", name, status, firstLineOfE2E(string(body)))
	}
	var page struct {
		Data []api.Project `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("parsing the control plane's project list: %v", err)
	}
	for i := range page.Data {
		if page.Data[i].Name == name {
			return &page.Data[i]
		}
	}
	t.Fatalf("the control plane refused project %s as an existing name but does not list it", name)
	return nil
}

// createProject creates the S4 fixture's project through the shared creator.
func (c *s4Cluster) createProject(name string) string {
	c.t.Helper()
	return createProjectNamespace(c.t, c.env, c.client, c.cluster, name, c)
}

// kubectlStatus runs kubectl and returns its error rather than failing the
// test: the shared project creator polls with it.
func (c *s4Cluster) kubectlStatus(args string) (string, error) {
	return k3s.Kubectl(context.Background(), c.runner, args)
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
	restoreStarted := time.Now().UTC()

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
	pause := c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath='{.metadata.annotations.kubenest\\.io/reconcile-paused}'")
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
	s4Restart(t, c, s4OperatorSelector)
	s4Restart(t, c, s4ArgoControllerSelector)
	c.waitFor(2*time.Minute, "the operator to come back", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get deployment -A -l "+s4OperatorSelector+" -o jsonpath='{.items[0].status.availableReplicas}'")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "0", strings.TrimSpace(out)
	})
	if pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath='{.metadata.annotations.kubenest\\.io/reconcile-paused}'")); pause != operationID {
		t.Errorf("a reconciler restart lifted the pause (annotation %q)", pause)
	}
	if suspend := strings.TrimSpace(c.kubectl("get cronjob s4-sentinel -n " + namespace + " -o jsonpath={.spec.suspend}")); suspend != "true" {
		t.Errorf("a reconciler restart un-suspended the CronJob (%q)", suspend)
	}

	// THE SENTINEL MUST STILL BE SILENT UNTIL ACTIVATION: a Job restored by
	// --include-jobs (not used here) or a CronJob that ran before activation
	// would have called it. The count is taken BEFORE --activate: activation
	// unsuspends the CronJob, which then runs at once, and on hardware
	// (2026-09-27) those later calls were counted as a failure.
	if got := sentinel.count(); got != hitsBeforeRestore {
		t.Errorf("the sentinel receiver saw %d call(s) between the restore and activation, want none: at %v, the restore command started at %s",
			got-hitsBeforeRestore, sentinel.hitsFrom(hitsBeforeRestore), restoreStarted.Format(time.RFC3339))
	}

	// ACTIVATION shows what may run, then lifts the pause.
	var activation strings.Builder
	if err := c.runCLI(&activation, append([]string{"backup", "restore", "--activate", operationID, "--keep-desired"}, c.verbArgs()...)...); err != nil {
		t.Fatalf("activation failed: %v\n%s", err, activation.String())
	}
	if !strings.Contains(activation.String(), "CronJob s4-sentinel") {
		t.Errorf("activation does not show which scheduled work will start:\n%s", activation.String())
	}
	if pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath='{.metadata.annotations.kubenest\\.io/reconcile-paused}'")); pause != "" {
		t.Errorf("activation left the pause in place (%q)", pause)
	}
	if suspend := strings.TrimSpace(c.kubectl("get cronjob s4-sentinel -n " + namespace + " -o jsonpath={.spec.suspend}")); suspend != "false" {
		t.Errorf("activation left the CronJob suspended (%q)", suspend)
	}

	// kn-x0wv.8: THE RESTORED POD IS THE WORKLOAD'S OWN POD, AND ACTIVATION
	// LEAVES IT. The ReplicaSet the restore brought back has replicas, so it
	// adopted the pod the restore filled as one of its own replicas; deleting it
	// at activation would restart the workload, and on a claim one node can mount
	// once (OpenEBS LVM) the replacement cannot mount it while the deleted pod's
	// mount is still going away. Velero's restore-wait init container in the
	// pod's spec is what says this pod is the RESTORED one — a pod the ReplicaSet
	// created has none — so its presence is the fix, and its absence is the
	// second pod kn-x0wv.8 is about.
	var ownPod string
	c.waitFor(5*time.Minute, "the workload's own pod to be Running after activation", func() (bool, string) {
		pod, err := c.runningWorkloadPodOf(namespace)
		if err != nil {
			return false, firstLineOfE2E(err.Error())
		}
		ownPod = pod
		return true, pod
	})
	if init := strings.TrimSpace(c.kubectl("get pod " + ownPod + " -n " + namespace + ` -o go-template='{{range .spec.initContainers}}{{.name}} {{end}}'`)); !strings.Contains(init, "restore-wait") {
		t.Errorf("the workload's pod %s carries no Velero restore-wait init container (init containers %q): the restored pod is the ReplicaSet's own replica and activation must leave it, so a pod the ReplicaSet created instead means the restored pod was lost and the workload restarted, which is kn-x0wv.8", ownPod, init)
	}
	if got := c.waitForDigestIn(namespace, ownPod, proofPath); got != written {
		t.Errorf("the workload's own pod %s reads %s = %s, want the %s written before the backup: the restored data did not survive activation", ownPod, proofPath, got, written)
	}

	t.Run("an interrupted restore resumes without activating", func(t *testing.T) {
		s4InterruptedResume(t, c, namespace, proof, proofPath)
	})

	// kn-x0wv.2: the backup taken while a rollout was in progress, whose volume
	// copy belongs to a pod whose ReplicaSet is at 0 replicas. It runs last
	// because it builds its own fixture and removes it again.
	t.Run("a backup taken mid-rollout restores the data", func(t *testing.T) {
		s4MidRollout(t, c, namespace, proof, proofPath)
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

	// The Restores that exist before this run, so the wait below is for the one
	// THIS run creates: the main arm's own Restore already exists, and on
	// hardware the wait matched it and cancelled the run before it paused.
	existing := map[string]bool{}
	for _, name := range strings.Fields(c.kubectl("get restores.velero.io -n velero -o name")) {
		existing[name] = true
	}

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
		for _, name := range strings.Fields(c.kubectl("get restores.velero.io -n velero -o name")) {
			if !existing[name] {
				return true, name
			}
		}
		return false, "no Restore this run created yet"
	})
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("the killed run reported success: a cancelled restore cannot have finished")
	}
	t.Logf("the interrupted run stopped with: %v", err)

	// THE PAUSE IS STILL IN PLACE, and it names the interrupted operation.
	pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath='{.metadata.annotations.kubenest\\.io/reconcile-paused}'"))
	if pause == "" {
		t.Fatalf("the interrupted run left no pause behind:\n%s", out.String())
	}
	operationID := pause

	// The record is what `kubenest health` shows the operation by, and it is
	// the record a second laptop resumes.
	record := c.kubectl("get configmap kubenest-operation -n kube-system -o jsonpath='{.data.record\\.json}'")
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
	if pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath='{.metadata.annotations.kubenest\\.io/reconcile-paused}'")); pause != operationID {
		t.Errorf("--resume lifted the pause (annotation %q)", pause)
	}
	if suspend := strings.TrimSpace(c.kubectl("get cronjob s4-sentinel -n " + namespace + " -o jsonpath={.spec.suspend}")); suspend != "true" {
		t.Errorf("--resume un-suspended the CronJob (%q): activating is not resuming", suspend)
	}
	if restored := c.sha256OfProof(namespace, proofPath); restored != "" {
		t.Logf("restored proof file after the resume: %s (%s)", restored, proof)
	}
}

// ---------------------------------------------------------------------------
// kn-x0wv.2: the restore of a backup taken mid-rollout.
//
// WHAT THE BACKUP HELD ON HARDWARE. On lab w3 (2026-09-27) the gate re-applied
// its Deployment while an earlier run's workload was still there, so a rollout
// from s4-web-5df4c559 to s4-web-6b4c7c8567 was in progress when the backup was
// taken. The backup held ONE PodVolumeBackup of the claim, and it named the OLD
// pod (s4-web-5df4c559-qcqqp), while the old ReplicaSet (s4-web-5df4c559) was
// held at 0 replicas. Velero restored that pod, stripped its owner references
// (pkg/restore/restore.go, resetMetadata), the ReplicaSet adopted it BY ITS
// SELECTOR and deleted it, and the PodVolumeRestore created for it never ran:
// the Restore stayed InProgress until Velero's itemOperationTimeout (4h) while
// the CLI waited its own restore timeout (2h).
//
// kn-x0wv.8: WHAT THE FIRST FIX GOT WRONG. cf35f8f renamed every restored pod
// the backup had copied a volume for, and that renamed too much. A ReplicaSet
// WITH replicas adopts its restored pod and counts it as one of its replicas;
// with the pod renamed it counted nothing, so it created a SECOND pod in the
// same second — a pod with no Velero restore-wait init container, which mounted
// the claim first, so the restored pod could never mount it (lab w1, 2026-09-28:
// "MountVolume.SetUp failed ... verifyMount: device already mounted", OpenEBS LVM
// refusing a second mount of one volume). An ORDINARY one-replica Deployment
// hung that way. The danger is only ever the ReplicaSet the backup holds at ZERO
// replicas, so the restore now renames THAT ReplicaSet's selector and template
// (backup.HeldReplicaSetHash) and leaves every pod's labels alone.
//
// WHAT THIS ARM THEREFORE EXPECTS AFTER THE RESTORE: the restored hold pod keeps
// its ORIGINAL pod-template-hash and has NO controller owner (the renamed
// ReplicaSet does not select it), and the restored ReplicaSet s4-hold-rs is at 0
// replicas with backup.HeldReplicaSetHash in its selector AND in its template.
// Activation then deletes that orphan — and leaves the workload's own pod alone.
//
// WHY THE FIXTURE CANNOT SIMPLY RACE A ROLLOUT FOR THAT STATE. Three rules of
// Velero and Kubernetes leave the live cluster almost no window in which to
// catch it:
//
//   - Velero does not back up an item that is being deleted (pkg/backup/
//     item_backupper.go, itemInclusionChecks: a deletionTimestamp fails the
//     item), so a pod held in termination has NO PodVolumeBackup and produces no
//     danger at all — a fixture that "held the old pod in termination" would
//     fail its own precondition;
//   - a PodVolumeBackup is created only for a pod whose phase is Running
//     (pkg/podvolume/backupper.go, kube.IsPodRunning);
//   - and the ReplicaSet controller claims pods and then deletes what it has
//     above its desired count in the same sync (pkg/controller/replicaset,
//     claimPods then manageReplicas), so "ReplicaSet at 0 AND its own pod still
//     Running and not yet deleted" lasts milliseconds — Velero's lists of the
//     two objects are not even atomic.
//
// So the fixture BUILDS that state and holds it. The pod's controller owner
// reference names the workload's Deployment (which manages no pods) instead of
// the ReplicaSet, and a ReplicaSet ignores an object whose controller reference
// is not its own (client-go, controller_ref_manager.ClaimObject: "Owned by
// someone else. Ignore."), so the pod stays Running beside the ReplicaSet at 0 —
// which is what w3's backup caught in the instant before that pod's own deletion
// landed. The garbage collector keeps it because the Deployment is really there,
// and the ReplicaSet still selects the pod by its labels. What the RESTORE sees
// is indistinguishable from w3's backup, because Velero strips owner references
// on restore anyway: a pod with a PodVolumeBackup, carrying the ReplicaSet's
// selector labels, and a ReplicaSet at 0 replicas that selects it.
//
// The fixture's claim (s4-hold-data) is deliberately separate from the S4
// workload's, because Velero copies a claim ONCE and skips every other pod that
// mounts it (pkg/podvolume/snapshot_tracker.go, TakenForPodVolume): a claim with
// two pods would make this arm a race, and this arm has to FAIL LOUDLY rather
// than pass without exercising the fix.
// ---------------------------------------------------------------------------
const (
	// s4HoldRS is the ReplicaSet at zero replicas; the pod's name is built from
	// it the way a ReplicaSet names its pods.
	s4HoldRS = "s4-hold-rs"
	// s4HoldPod is that pod. The fixture writes it out by hand (see the document
	// below) because no sequence of ReplicaSet operations leaves one of its own
	// pods Running beside it at zero replicas.
	s4HoldPod = "s4-hold-rs-carry"
	// s4HoldClaim is the claim whose copy the backup holds, and s4HoldVolume is
	// the pod's volume name for it, as the PodVolumeBackup and the
	// PodVolumeRestore name it.
	s4HoldClaim  = "s4-hold-data"
	s4HoldVolume = "data"
	// s4HoldHash is the pod-template-hash the fixture's ReplicaSet selects AND
	// the fixture's pod carries. The restore never touches the pod's labels: it
	// renames the SELECTOR AND TEMPLATE of the ReplicaSet, whose count is 0,
	// to backup.HeldReplicaSetHash, so that ReplicaSet selects none of the
	// restored pods — while the restored pod keeps this value.
	s4HoldHash = "kubenest-hold"
	// s4HoldMarker is written into the fixture's claim BY THE TEST, through the
	// pod, before the backup: the fixture's container never writes that name, so
	// its digest after the restore is a fact about the volume restore rather
	// than about a container that wrote the same bytes again.
	s4HoldMarker = "/data/restored-only.txt"
	// s4ProofMarker is the same idea on the S4 workload's own claim.
	s4ProofMarker = "/data/s4-mid-rollout-marker.txt"
)

// s4MidRolloutBound is how long the mid-rollout restore may take here. It is far
// below Velero's own answer to a PodVolumeRestore that cannot run
// (itemOperationTimeout, 4h) and below the CLI's own restore timeout: this arm's
// backup is a namespace-sized one, so anything near this bound is the hang
// kn-x0wv.2 describes rather than the work.
const s4MidRolloutBound = 20 * time.Minute

// s4MidRollout is kn-x0wv.2's hardware arm as kn-x0wv.8 reshaped the fix: the
// namespace is restored from a backup taken in the state a rollout leaves between
// "the old ReplicaSet is at 0" and "the old pod is gone".
//
// It runs after the arms above — it needs a live namespace and a workload — and
// it leaves the namespace as it found it: it clears what the interrupt arm left
// pending, adds its own fixture (claim, ReplicaSet at 0, held pod), and removes
// that fixture again whatever happens.
func s4MidRollout(t *testing.T, c *s4Cluster, namespace, proof, proofPath string) {
	t.Helper()
	t.Logf("mid-rollout restore of %s (the workload's proof is %s)", namespace, proof)

	// THE STATE THE ARMS ABOVE LEFT. The interrupt arm finishes with a pending
	// operation: its record makes the restore verb refuse to start, and its
	// pause keeps the reconcilers from recreating the namespace this arm deletes
	// (which is the scenario's premise). There is nothing else to clear: a pod
	// Velero restored for the workload's own claim carries the workload's OWN
	// pod-template-hash and is adopted by the ReplicaSet that came back with it,
	// so it IS the workload's own pod — it no longer marks itself as the
	// restore's, and nothing about it needs emptying before this arm runs. (The
	// gate's own body clears the record the same way.)
	c.kubectl("delete configmap kubenest-operation -n kube-system --ignore-not-found")
	if pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath='{.metadata.annotations.kubenest\\.io/reconcile-paused}'")); pause != "" {
		c.kubectl("annotate project " + namespace + " -n kubenest-system " + backup.PauseAnnotationKey + "-")
		t.Logf("cleared the pause %q the arm above left on project %s", pause, namespace)
	}

	// readRunningWorkloadPod finds the S4 workload's Running pod. There is one
	// kind of it: the restore no longer renames a pod, so a pod the restore
	// filled a volume through is the workload's own pod once its ReplicaSet
	// adopts it. What is read through it is what the claim holds.
	readRunningWorkloadPod := func() (string, string) {
		pod, err := c.runningWorkloadPodOf(namespace)
		if err != nil {
			return "", firstLineOfE2E(err.Error())
		}
		return pod, ""
	}
	waitForWorkloadPod := func(what string) string {
		var pod string
		c.waitFor(5*time.Minute, what, func() (bool, string) {
			got, detail := readRunningWorkloadPod()
			pod = got
			return got != "", detail
		})
		return pod
	}

	// THE DANGEROUS STATE, and its removal whatever happens next, so a failed
	// run does not leave it behind for the arms and gates that follow.
	if at := strings.LastIndexByte(s4HoldPod, '-'); at <= 0 || s4HoldPod[:at] != s4HoldRS {
		t.Fatalf("the fixture's pod %s is not named after its ReplicaSet %s: a pod's name is what it is walked back from to the ReplicaSet that selects it (the CLI's replicaSetOfPodName), and this arm's claim is about that ReplicaSet's recorded count", s4HoldPod, s4HoldRS)
	}
	deploymentUID := strings.TrimSpace(c.kubectl("get deployment s4-web -n " + namespace + " -o jsonpath={.metadata.uid}"))
	if deploymentUID == "" {
		t.Fatalf("deployment s4-web has no uid in %s, so the fixture cannot hold its pod out of its ReplicaSet's reach", namespace)
	}
	c.apply(s4MidRolloutHoldDocument(namespace, deploymentUID, fmt.Sprintf("s4-hold-%d", time.Now().UnixNano())))
	t.Cleanup(func() {
		for _, what := range []string{"replicaset " + s4HoldRS, "pod " + s4HoldPod, "persistentvolumeclaim " + s4HoldClaim} {
			if out, err := k3s.Kubectl(context.Background(), c.runner, "delete "+what+" -n "+namespace+" --ignore-not-found"); err != nil {
				t.Logf("removing the mid-rollout fixture's %s: %v (%s)", what, err, firstLineOfE2E(out))
			}
		}
	})

	// THE FIXTURE'S STATE, READ FROM THE CLUSTER, so the same reading can be
	// taken before and after the backup: the state must not move while the
	// backup is taken, because a backup holds one instant and a state that only
	// existed outside it proves nothing.
	const (
		holdPodShape = `{.status.phase}|{.metadata.deletionTimestamp}|{.metadata.uid}|{.metadata.labels.pod-template-hash}|{.metadata.labels.app}|{.metadata.ownerReferences[0].kind}|{.metadata.ownerReferences[0].name}`
		holdSetShape = `{.spec.replicas}|{.metadata.uid}|{.spec.selector.matchLabels.pod-template-hash}|{.spec.selector.matchLabels.app}`
	)
	type holdState struct {
		podPhase, podDeleting, podUID, podHash, podApp string
		podOwnerKind, podOwnerName                     string
		replicas, setUID, setHash, setApp              string
	}
	readHold := func() (holdState, error) {
		podOut, err := c.kubectlStatus("get pod " + s4HoldPod + " -n " + namespace + " -o jsonpath='" + holdPodShape + "'")
		if err != nil {
			return holdState{}, err
		}
		setOut, err := c.kubectlStatus("get rs " + s4HoldRS + " -n " + namespace + " -o jsonpath='" + holdSetShape + "'")
		if err != nil {
			return holdState{}, err
		}
		pod := strings.Split(strings.TrimSpace(podOut), "|")
		set := strings.Split(strings.TrimSpace(setOut), "|")
		for len(pod) < 7 {
			pod = append(pod, "")
		}
		for len(set) < 4 {
			set = append(set, "")
		}
		return holdState{pod[0], pod[1], pod[2], pod[3], pod[4], pod[5], pod[6], set[0], set[1], set[2], set[3]}, nil
	}
	assertHold := func(when string) holdState {
		s, err := readHold()
		if err != nil {
			t.Fatalf("reading the fixture's state %s: %v", when, err)
		}
		if s.podPhase != "Running" {
			t.Fatalf("the fixture's pod %s is %q %s, not Running: Velero creates a PodVolumeBackup only for a Running pod (pkg/podvolume/backupper.go), so the backup would hold no copy of its claim and this arm would prove nothing about kn-x0wv.2", s4HoldPod, s.podPhase, when)
		}
		if s.podDeleting != "" {
			t.Fatalf("the fixture's pod %s carries a deletionTimestamp %s: Velero does not back up an item that is being deleted (pkg/backup/item_backupper.go), so the backup would hold no copy of it and this arm would prove nothing", s4HoldPod, when)
		}
		if s.podHash != s4HoldHash || s.setHash != s4HoldHash || s.podApp == "" || s.podApp != s.setApp {
			t.Fatalf("ReplicaSet %s selects app=%q pod-template-hash=%q while the pod carries app=%q pod-template-hash=%q %s: the ReplicaSet would not adopt the restored pod by its selector, so the danger this arm restores into would not exist", s4HoldRS, s.setApp, s.setHash, s.podApp, s.podHash, when)
		}
		// WHY THE POD IS STILL THERE. A ReplicaSet ignores an object whose
		// controller reference is not its own, so the fixture holds the pod by
		// pointing its ownerReference at the workload's Deployment; a pod that is
		// owned by this ReplicaSet, or owned by nothing, is deleted by it as soon
		// as it is at zero (claimPods, then manageReplicas).
		if s.podOwnerKind == "" || s.podOwnerName == "" {
			t.Fatalf("the fixture's pod has no controller owner %s: a pod with the ReplicaSet's labels and no owner is adopted and deleted by ReplicaSet %s at 0 replicas, so the state would not hold long enough to be backed up", when, s4HoldRS)
		}
		if strings.EqualFold(s.podOwnerKind, "ReplicaSet") && s.podOwnerName == s4HoldRS {
			t.Fatalf("the fixture's pod %s is owned by ReplicaSet %s itself %s, which is at 0 replicas: that controller deletes what it owns in the same sync it is scaled down, so the pod would not survive to be backed up", s4HoldPod, s4HoldRS, when)
		}
		if s.replicas != "0" {
			t.Fatalf("ReplicaSet %s is at %q replica(s) %s, want 0: with the pod counted as its own, this would not be the zero-replica case the backup has to hold", s4HoldRS, s.replicas, when)
		}
		return s
	}
	c.waitFor(5*time.Minute, "the fixture's held pod to run on its claim", func() (bool, string) {
		s, err := readHold()
		if err != nil {
			return false, firstLineOfE2E(err.Error())
		}
		if s.podPhase != "Running" {
			return false, s4HoldPod + " is " + s.podPhase
		}
		return true, s4HoldPod + " is Running with pod-template-hash " + s.podHash
	})
	before := assertHold("before the backup")

	// The two markers, one per claim, written through the pods that mount them:
	// each with a value from this run, and a name no fixture container writes.
	holdMarker := fmt.Sprintf("s4-hold-volume-%d", time.Now().UnixNano())
	c.execIn(namespace, s4HoldPod, "echo "+holdMarker+" > "+s4HoldMarker+" && sync")
	holdMarkerDigest := c.waitForDigestIn(namespace, s4HoldPod, s4HoldMarker)
	proofPod := waitForWorkloadPod("the workload's own pod before the backup")
	proofMarker := fmt.Sprintf("s4-mid-rollout-volume-%d", time.Now().UnixNano())
	c.execIn(namespace, proofPod, "echo "+proofMarker+" > "+s4ProofMarker+" && sync")
	proofMarkerDigest := c.waitForDigestIn(namespace, proofPod, s4ProofMarker)
	written := c.waitForProof(namespace, proofPath)
	holdClaimUID := strings.TrimSpace(c.kubectl("get persistentvolumeclaim " + s4HoldClaim + " -n " + namespace + " -o jsonpath={.metadata.uid}"))
	if holdClaimUID == "" {
		t.Fatalf("the fixture's claim %s has no uid, so the backup's copy of it could not be told from another claim's", s4HoldClaim)
	}

	// THE BACKUP.
	var backupOut strings.Builder
	if err := c.runCLI(&backupOut, append([]string{"backup", "now"}, c.verbArgs()...)...); err != nil {
		t.Fatalf("backup now failed: %v\n%s", err, backupOut.String())
	}
	midBackup := s4BackupName(backupOut.String())
	if midBackup == "" {
		t.Fatalf("backup now did not name the backup it took:\n%s", backupOut.String())
	}
	t.Logf("mid-rollout backup %s taken", midBackup)
	after := assertHold("after the backup")
	if before.podUID != after.podUID || before.setUID != after.setUID {
		t.Fatalf("the fixture was replaced while the backup was taken (pod uid %s then %s, ReplicaSet uid %s then %s): the backup does not hold the pod and ReplicaSet this arm checked", before.podUID, after.podUID, before.setUID, after.setUID)
	}

	// THE BACKUP HAS TO HOLD THE DANGER. The backup's PodVolumeBackups are the
	// only place outside the object store that says which pods' volumes it
	// copied, and this arm's state exists only if one of them belongs to the
	// fixture's pod — the pod whose ReplicaSet the backup holds at zero replicas.
	// (The restore's own rule no longer reads this list: pkg/backup's
	// heldReplicaSetRule matches a ReplicaSet by its count, so a resumed run
	// builds one document whatever the backup holds.) A backup that does not
	// name this pod and this claim is not a run of kn-x0wv.2's case, and the arm
	// fails here rather than going green on a state it never produced.
	pvbs := c.kubectl(fmt.Sprintf(
		`get podvolumebackups.velero.io -n velero -l velero.io/backup-name=%s -o jsonpath='{range .items[*]}{.spec.pod.namespace}/{.spec.pod.name} {.spec.volume} {.metadata.labels.velero\.io/pvc-uid}{"\n"}{end}'`,
		midBackup))
	wantCopy := namespace + "/" + s4HoldPod + " " + s4HoldVolume + " " + holdClaimUID
	if !strings.Contains(pvbs, wantCopy) {
		t.Fatalf("backup %s does not hold a copy of the fixture's claim: want %q among\n%s\n\nThis arm restores a backup taken while a pod whose ReplicaSet is at 0 replicas still held a volume copy, which is what kn-x0wv.2 is about, and a backup without that copy proves nothing. Velero skips a pod that is being deleted and copies the volume only of a Running one, so the fixture's pod must be Running and not terminating at backup time.",
			midBackup, wantCopy, strings.TrimSpace(pvbs))
	}
	t.Logf("backup %s holds a PodVolumeBackup for %s (volume %s of claim %s/%s), whose ReplicaSet %s is on the cluster at 0 replicas",
		midBackup, s4HoldPod, s4HoldVolume, namespace, s4HoldClaim, s4HoldRS)

	// THE SCENARIO'S PREMISE, the same one the arms above use: the namespace is
	// deleted and the reconcilers recreate it empty, which is the state
	// --replace exists for.
	c.kubectl("delete namespace " + namespace + " --wait=false")
	c.waitFor(10*time.Minute, "the reconcilers to recreate the namespace", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get namespace "+namespace+" -o name")
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(out) != "", "the namespace is back"
	})
	c.waitFor(10*time.Minute, "the recreated namespace to hold a claim", func() (bool, string) {
		out, err := k3s.Kubectl(context.Background(), c.runner, "get persistentvolumeclaim -n "+namespace+" -o jsonpath={.items[*].status.phase}")
		if err != nil {
			return false, err.Error()
		}
		phases := strings.Fields(out)
		return len(phases) > 0, strings.Join(phases, ",")
	})

	// THE RESTORE, NAMED AND BOUNDED. Naming the backup matters: the arms above
	// and this one leave newer backups behind, and --latest would restore
	// whichever of them is newest rather than the one this arm's state is in. A
	// restore that waits out a PodVolumeRestore whose pod a zero-replica
	// ReplicaSet adopted and deleted blocks here until the deadline; the bound is
	// far below Velero's own itemOperationTimeout (4h), so the failure says what
	// happened instead of tying up the lab for hours.
	ctx, cancel := context.WithTimeout(context.Background(), s4MidRolloutBound)
	defer cancel()
	var plan strings.Builder
	began := time.Now()
	restoreErr := c.runCLIWithContext(ctx, &plan, append([]string{"backup", "restore", "--namespace", namespace, "--from", midBackup, "--replace", "--confirm"}, c.verbArgs()...)...)
	took := time.Since(began)
	if restoreErr != nil || !strings.Contains(plan.String(), "restored — awaiting activation") {
		t.Fatalf("the mid-rollout backup %s did not restore within %s: %v\n%s\n\nA PodVolumeRestore whose pod a zero-replica ReplicaSet adopted and deleted makes Velero wait its itemOperationTimeout (4h) and the CLI wait its own restore timeout, which is kn-x0wv.2; the ReplicaSet hold rule (backup.HeldReplicaSetHash, pkg/backup's heldReplicaSetRule) is what stops it, and the state the backup must hold is checked above.",
			midBackup, s4MidRolloutBound, restoreErr, plan.String())
	}
	t.Logf("the mid-rollout backup restored in %s, inside the %s bound", took.Round(time.Second), s4MidRolloutBound)
	operationID := s4OperationID(plan.String())
	if operationID == "" {
		t.Fatalf("the run printed no operation id, so nothing can be activated:\n%s", plan.String())
	}

	// THE RESTORED STATE, BEFORE ACTIVATION. THIS IS THE FIX.
	//
	// The ReplicaSet is read from the RESTORED namespace, which is the backup:
	// its being at zero is the proof that the backup held the danger, and the
	// held hash in its selector AND its template is the fix itself.
	if replicas := strings.TrimSpace(c.kubectl("get rs " + s4HoldRS + " -n " + namespace + " -o jsonpath={.spec.replicas}")); replicas != "0" {
		t.Errorf("the restored ReplicaSet %s is at %q replica(s), want 0: the backup did not hold it at zero, so this restore did not run kn-x0wv.2's case", s4HoldRS, replicas)
	}
	held := strings.TrimSpace(c.kubectl("get rs " + s4HoldRS + " -n " + namespace + " -o jsonpath='{.spec.selector.matchLabels.pod-template-hash}|{.spec.template.metadata.labels.pod-template-hash}'"))
	if held != backup.HeldReplicaSetHash+"|"+backup.HeldReplicaSetHash {
		t.Errorf("the restored ReplicaSet %s carries selector|template pod-template-hash %q, want %q in both: the restore renames the SELECTOR of a ReplicaSet at 0 replicas so it cannot adopt and delete the restored pod, and renames the TEMPLATE with it because the API refuses a ReplicaSet whose selector does not match its own template", s4HoldRS, held, backup.HeldReplicaSetHash)
	}
	// The pod Velero restored for that volume copy is ALIVE and carries the hash
	// the BACKUP held: the restore does not touch a pod's labels. Without the
	// rule, the ReplicaSet at 0 would adopt it by its selector and delete it, and
	// its PodVolumeRestore would never run.
	c.waitFor(5*time.Minute, "the restored pod to be alive with its volume filled", func() (bool, string) {
		out, err := c.kubectlStatus("get pod " + s4HoldPod + " -n " + namespace + " -o jsonpath='{.metadata.labels.pod-template-hash}|{.status.phase}|{.metadata.deletionTimestamp}'")
		if err != nil {
			return false, firstLineOfE2E(err.Error())
		}
		fields := strings.Split(strings.TrimSpace(out), "|")
		if len(fields) < 3 {
			return false, strings.TrimSpace(out)
		}
		if fields[0] != s4HoldHash {
			return false, "the restored pod carries pod-template-hash " + fields[0] + ", want the backup's own " + s4HoldHash
		}
		if fields[2] != "" {
			return false, "the restored pod is being deleted"
		}
		return fields[1] == "Running", "pod-template-hash " + fields[0] + ", phase " + fields[1]
	})
	// AND NO CONTROLLER OWNS IT, which is why activation deletes it: the renamed
	// ReplicaSet selects nothing, so the pod is the RESTORE's own and not the
	// workload's. A pod a controller had adopted would be the workload's own pod
	// and would stay at activation.
	ownerKinds, err := c.kubectlStatus("get pod " + s4HoldPod + " -n " + namespace + ` -o go-template='{{range .metadata.ownerReferences}}{{.kind}} {{end}}'`)
	if err != nil {
		t.Fatalf("reading the restored pod's owner references: %v", err)
	}
	if got := strings.TrimSpace(ownerKinds); got != "" {
		t.Errorf("the restored pod %s is owned by %s: the renamed ReplicaSet %s selects nothing, so nothing may adopt it — and a pod a controller owns is the workload's own, which activation keeps instead of cleaning up", s4HoldPod, got, s4HoldRS)
	}
	// Its PodVolumeRestore COMPLETED: the volume the danger threatened was
	// filled. (The marker's digest below is the stronger form of the same fact,
	// because no container writes that file.)
	pvrs := c.kubectl(fmt.Sprintf(`get podvolumerestores.velero.io -n velero -l velero.io/restore-name=kubenest-restore-%s -o jsonpath='{range .items[*]}{.spec.pod.name} {.spec.volume} {.status.phase}{"\n"}{end}'`, operationID))
	if want := s4HoldPod + " " + s4HoldVolume + " Completed"; !strings.Contains(pvrs, want) {
		t.Errorf("the restore's PodVolumeRestores do not include %q, so the volume copied for the pod whose ReplicaSet is at zero was not shown to be filled:\n%s", want, strings.TrimSpace(pvrs))
	}
	if got := c.waitForDigestIn(namespace, s4HoldPod, s4HoldMarker); got != holdMarkerDigest {
		t.Errorf("the restored claim's %s is %s, want the %s written before the backup: the volume the danger threatened does not hold the bytes the backup copied", s4HoldMarker, got, holdMarkerDigest)
	}
	// The workload's own pod and its claim: the ordinary half of the restore,
	// which the danger must not have disturbed. It is the pod the RESTORE filled
	// — Velero's restore-wait init container is still in its spec — because the
	// ReplicaSet that came back with it has replicas and adopted it as one of
	// them. A pod the ReplicaSet had created instead is exactly kn-x0wv.8's
	// second pod, and the claim it mounts is the one the restore needed.
	proofPod = waitForWorkloadPod("the workload's own pod on the restored claim")
	if init := strings.TrimSpace(c.kubectl("get pod " + proofPod + " -n " + namespace + ` -o go-template='{{range .spec.initContainers}}{{.name}} {{end}}'`)); !strings.Contains(init, "restore-wait") {
		t.Errorf("the workload's pod %s carries no Velero restore-wait init container (init containers %q): the pod the restore filled must be the workload's own, adopted by the ReplicaSet that came back with it, and a pod the ReplicaSet created is the second pod kn-x0wv.8 is about", proofPod, init)
	}
	if got := c.waitForDigestIn(namespace, proofPod, s4ProofMarker); got != proofMarkerDigest {
		t.Errorf("the restored claim's %s is %s, want the %s written before the backup", s4ProofMarker, got, proofMarkerDigest)
	}
	if got := c.waitForDigestIn(namespace, proofPod, proofPath); got != written {
		t.Errorf("the restored %s is %s, want the %s written before the backup", proofPath, got, written)
	}

	// ACTIVATION: the pause is lifted, the restored ORPHAN — the pod that
	// existed only to fill the volumes and that no controller owns — is deleted,
	// and the workload's own controller's pods take the claims over.
	var activation strings.Builder
	if err := c.runCLI(&activation, append([]string{"backup", "restore", "--activate", operationID, "--keep-desired"}, c.verbArgs()...)...); err != nil {
		t.Fatalf("activation failed: %v\n%s", err, activation.String())
	}
	if pause := strings.TrimSpace(c.kubectl("get project " + namespace + " -n kubenest-system -o jsonpath='{.metadata.annotations.kubenest\\.io/reconcile-paused}'")); pause != "" {
		t.Errorf("activation left the pause in place (%q)", pause)
	}
	// No restored ORPHAN is left: the fixture's pod was the restore's own, so
	// activation deleted it. (The workload's own pod is NOT one of these — a
	// controller owns it, so activation keeps it.) Activation deletes without
	// waiting, and the pod then takes its grace period to stop: on lab w1
	// (2026-09-28) a check made straight after activation still found it
	// terminating, and it was gone moments later. So the claim is that it GOES,
	// within a bound; a pod activation never deleted stays Running and fails it.
	c.waitFor(2*time.Minute, "the restored orphan "+s4HoldPod+" to be gone after activation", func() (bool, string) {
		out, err := c.kubectlStatus("get pod " + s4HoldPod + " -n " + namespace + " -o jsonpath='{.status.phase}|{.metadata.deletionTimestamp}' --ignore-not-found")
		if err != nil {
			return false, firstLineOfE2E(err.Error())
		}
		left := strings.Trim(strings.TrimSpace(out), "'")
		return left == "", "still there: phase|deletionTimestamp " + left
	})
	// The Deployment's OWN pod — a pod whose pod-template-hash matches a
	// ReplicaSet with replicas > 0 — is Running on the restored claim.
	ownPod := waitForWorkloadPod("the workload's own pod after activation")
	at := strings.LastIndexByte(ownPod, '-')
	if at <= 0 {
		t.Fatalf("the workload's pod %q carries no ReplicaSet name, so its ReplicaSet cannot be checked", ownPod)
	}
	ownSet := ownPod[:at]
	ownHash := strings.TrimSpace(c.kubectl("get pod " + ownPod + " -n " + namespace + " -o jsonpath={.metadata.labels.pod-template-hash}"))
	ownReplicas := strings.TrimSpace(c.kubectl("get rs " + ownSet + " -n " + namespace + " -o jsonpath={.spec.replicas}"))
	if ownHash == "" || ownHash == backup.HeldReplicaSetHash || ownReplicas == "" || ownReplicas == "0" {
		t.Errorf("the workload's pod %s carries pod-template-hash %q and ReplicaSet %s is at %q replica(s): want the workload's own pod, whose ReplicaSet has replicas and whose hash no held ReplicaSet carries", ownPod, ownHash, ownSet, ownReplicas)
	}
	if got := c.waitForDigestIn(namespace, ownPod, proofPath); got != written {
		t.Errorf("the workload's own pod %s reads %s = %s, want the %s written before the backup: the restored data did not survive activation", ownPod, proofPath, got, written)
	}
	if got := c.waitForDigestIn(namespace, ownPod, s4ProofMarker); got != proofMarkerDigest {
		t.Errorf("the workload's own pod %s reads %s = %s, want the %s written before the backup", ownPod, s4ProofMarker, got, proofMarkerDigest)
	}
	// WHAT IS PUT BACK: the fixture's claim, ReplicaSet and held pod are removed
	// by the cleanup above, and the S4 workload's own spec is never touched by
	// this arm — the dangerous state is this arm's own objects, so the arms and
	// gates that follow find the namespace as they left it.
	t.Logf("after activation the workload's own pod %s (pod-template-hash %s, ReplicaSet %s at %s replica(s)) is Running on the restored claim", ownPod, ownHash, ownSet, ownReplicas)
}

// s4MidRolloutHoldDocument is the dangerous state written out: the claim, the
// ReplicaSet at zero replicas that selects the pod, and the pod itself —
// Running, mounted on the claim, carrying the ReplicaSet's selector labels, and
// held out of the ReplicaSet's reach by an ownerReference to the workload's
// Deployment (deploymentUID), which manages no pods.
//
// THE POD IS WRITTEN OUT BY HAND. There is no sequence of ReplicaSet operations
// that leaves a Running pod beside its own ReplicaSet at zero replicas: the
// controller claims the pod and deletes it in the same sync, which is the
// milliseconds-wide window w3's backup caught (see the comment on s4MidRollout).
// Its name still follows the naming convention a ReplicaSet uses
// (<replicaset>-<suffix>), which is what lets a pod's name be walked back to its
// ReplicaSet (the CLI's replicaSetOfPodName), and its labels are the
// ReplicaSet's selector, which is what the restore's danger is made of.
//
// THE RESTORE LEAVES THE POD ALONE (kn-x0wv.8): what it renames is the
// ReplicaSet's selector and template, to backup.HeldReplicaSetHash, so the
// restored pod keeps s4HoldHash and nothing adopts it. The fixture is exactly
// the state the BACKUP holds; the restore is what moves the selector out of it.
//
// The verbs are: %[1]s namespace, %[2]s pod, %[3]s ReplicaSet, %[4]s claim,
// %[5]s pod-template-hash, %[6]s the value the fixture's container writes,
// %[7]s the pod's volume name, %[8]s the workload Deployment's uid.
func s4MidRolloutHoldDocument(namespace, deploymentUID, started string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: %[4]s, namespace: %[1]s}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: %[3]s, namespace: %[1]s}
spec:
  replicas: 0
  selector: {matchLabels: {app: s4-hold, pod-template-hash: %[5]s}}
  template:
    metadata: {labels: {app: s4-hold, pod-template-hash: %[5]s}}
    spec:
      containers:
        - name: hold
          image: busybox:1.36
          command: [sh, -c, "echo %[6]s > /data/started.txt && sync && sleep 1000000"]
          volumeMounts: [{name: %[7]s, mountPath: /data}]
      volumes:
        - {name: %[7]s, persistentVolumeClaim: {claimName: %[4]s}}
---
apiVersion: v1
kind: Pod
metadata:
  name: %[2]s
  namespace: %[1]s
  labels: {app: s4-hold, pod-template-hash: %[5]s}
  ownerReferences:
    - {apiVersion: apps/v1, kind: Deployment, name: s4-web, uid: %[8]s, controller: true}
spec:
  containers:
    - name: hold
      image: busybox:1.36
      command: [sh, -c, "echo %[6]s > /data/started.txt && sync && sleep 1000000"]
      volumeMounts: [{name: %[7]s, mountPath: /data}]
  volumes:
    - {name: %[7]s, persistentVolumeClaim: {claimName: %[4]s}}
`, namespace, s4HoldPod, s4HoldRS, s4HoldClaim, s4HoldHash, started, s4HoldVolume, deploymentUID)
}

// execIn runs one shell command inside a pod. The gate writes the markers into
// claims with it. The command is built from fixture-safe values (letters,
// digits, dashes, dots, slashes), so it is quoted as a whole and needs no
// escaping of its own.
func (c *s4Cluster) execIn(namespace, pod, command string) string {
	c.t.Helper()
	return c.kubectl(fmt.Sprintf("-n %s exec %s -- sh -c '%s'", namespace, pod, command))
}

// sha256InPod reads a file's digest through one NAMED pod.
func (c *s4Cluster) sha256InPod(namespace, pod, path string) string {
	c.t.Helper()
	out, err := k3s.Kubectl(context.Background(), c.runner, fmt.Sprintf("-n %s exec %s -- sha256sum %s", namespace, pod, path))
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

// waitForDigestIn is waitForProof for a named pod: for a file on a claim whose
// pods do not carry the workload's labels.
func (c *s4Cluster) waitForDigestIn(namespace, pod, path string) string {
	c.t.Helper()
	var digest string
	c.waitFor(5*time.Minute, "the file "+path+" of pod "+pod, func() (bool, string) {
		digest = c.sha256InPod(namespace, pod, path)
		if digest == "" {
			return false, "no digest yet (the pod may still be starting)"
		}
		return true, digest
	})
	return digest
}

// The reconcilers the gate restarts, by label: an install names them after its
// Helm release (operator-kubenest-operator-2-controller-manager, and Argo CD's
// application controller is a StatefulSet), so names looked up by hand were
// wrong on hardware (2026-09-27).
const (
	s4OperatorSelector       = "app.kubernetes.io/name=kubenest-operator-2,app.kubernetes.io/component=manager"
	s4ArgoControllerSelector = "app.kubernetes.io/name=argocd-application-controller"
)

// s4Restart restarts every Deployment or StatefulSet the selector matches, in
// any namespace. A cluster with none (an install without Argo CD) is not a gate
// failure.
func s4Restart(t *testing.T, c *s4Cluster, selector string) {
	t.Helper()
	out, err := k3s.Kubectl(context.Background(), c.runner, "get deployment,statefulset -A -l "+selector+` -o jsonpath='{range .items[*]}{.kind} {.metadata.namespace} {.metadata.name}{"\n"}{end}'`)
	if err != nil {
		t.Fatalf("finding the workloads labelled %s: %v", selector, err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		t.Logf("nothing labelled %s is on this cluster, so there is nothing to restart", selector)
		return
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		kind, namespace, name := strings.ToLower(fields[0]), fields[1], fields[2]
		if _, err := k3s.Kubectl(context.Background(), c.runner, "rollout restart "+kind+" "+name+" -n "+namespace); err != nil {
			t.Fatalf("restarting %s %s/%s: %v", kind, namespace, name, err)
		}
		t.Logf("restarted %s %s/%s", kind, namespace, name)
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

// runningWorkloadPodOf names the S4 workload's Running pod in namespace, or says
// why there is none. There is one kind of it: the restore no longer renames a
// pod, so a pod it filled a volume through is the workload's own pod as soon as
// its ReplicaSet adopts it, and the workload's own label is the whole filter.
func (c *s4Cluster) runningWorkloadPodOf(namespace string) (string, error) {
	out, err := c.kubectlStatus("get pods -n " + namespace + " -l app=s4-web -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.labels.pod-template-hash} {.status.phase}{\"\\n\"}{end}'")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[2] == "Running" {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no Running pod of the S4 workload: %s", strings.TrimSpace(out))
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
	return c.sha256InPod(namespace, pod, path)
}
