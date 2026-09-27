//go:build e2e

// T4.3's gate: one workload's stranded volumes, restored in place, on a real
// three-node lab (PLAN 7.5 mode 2, kn-t43).
//
// THE THREE-NODE FIXTURE IS THE POINT. A local volume is pinned to the node
// that hosts it, so "the node died and its volumes are gone" cannot be
// constructed on one machine: the gate needs a server to keep serving and an
// agent to lose. The two fixtures are probe P5's, replayed through the command
// an operator would type:
//
//	dead-node   a pod's two claims both lived on the agent; the agent is
//	            stopped, the claims are stranded, and the restore refills both
//	            on the live server.
//	selective   a pod mounts two claims on a LIVE node; newer data is written
//	            into the one that is NOT named, only the other is destroyed and
//	            refilled, and the unnamed one must keep its newer bytes.
//
// WHAT A HARDWARE RUN NEEDS, IN FULL:
//
//	./scripts/ephemeral-env.sh up --profile host --nodes 3
//	source lab/hetzner/.lab-env.sh          # KUBENEST_LAB_NODE1_IP/2_IP/3_IP, SSH key
//	export KUBENEST_CONTROL_PLANE=… KUBENEST_CLI_TOKEN=…      # a control plane
//	export KUBENEST_BACKUP_ACCESS_KEY_ID=… KUBENEST_BACKUP_SECRET_ACCESS_KEY=…
//	cd kubenest-cli && go test -tags e2e -run TestRestoreVolume -v -timeout 4h ./e2e/
//
// The gate skips, naming that command, when only one node is present.
package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
)

// s5Namespace is the fixture namespace, and each fixture gets its own workload
// name inside it so the two subtests cannot see each other's volumes.
const s5Namespace = "e2e-restore-volumes"

// s5Lab is the gate's handle on the three-node lab. Every kubectl call goes
// through node 1 (a server is the only node that runs kubectl), and the
// node-level steps go through the node they are about.
type s5Lab struct {
	t       *testing.T
	env     gateEnv
	nodes   map[string]*sshx.Client
	bundle  string
	cluster string
}

func s5Open(t *testing.T, env gateEnv) *s5Lab {
	t.Helper()
	node2 := os.Getenv("KUBENEST_LAB_NODE2_IP")
	node3 := os.Getenv("KUBENEST_LAB_NODE3_IP")
	if env.server == "" || node2 == "" || node3 == "" {
		t.Skipf("this gate needs three nodes and the lab has %s; run ./scripts/ephemeral-env.sh up --profile host --nodes 3 and source lab/hetzner/.lab-env.sh",
			strings.Join(s5Present(env.server, node2, node3), ", "))
	}
	controlPlane, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatalf("building the control-plane client: %v", err)
	}
	bundlePath := s4BundleManifest(t, env, controlPlane)
	lab := &s5Lab{t: t, env: env, nodes: map[string]*sshx.Client{}, bundle: bundlePath, cluster: env.cluster}
	for name, address := range map[string]string{"node1": env.server, "node2": node2, "node3": node3} {
		ep, err := sshx.Resolve(address, sshx.Options{User: env.sshUser, KeyPath: env.sshKey})
		if err != nil {
			t.Fatalf("resolving %s: %v", address, err)
		}
		runner, err := sshx.Dial(context.Background(), ep, sshx.Options{KeyPath: env.sshKey})
		if err != nil {
			t.Fatalf("dialling %s: %v", address, err)
		}
		lab.nodes[name] = runner
		t.Cleanup(func() { _ = runner.Close() })
	}
	return lab
}

func s5Present(server, node2, node3 string) []string {
	var present []string
	for name, address := range map[string]string{"node1": server, "node2": node2, "node3": node3} {
		if address != "" {
			present = append(present, name)
		}
	}
	return present
}

func (l *s5Lab) kubectl(args string) string {
	l.t.Helper()
	out, err := k3s.Kubectl(context.Background(), l.nodes["node1"], args)
	if err != nil {
		l.t.Fatalf("kubectl %s: %v", args, err)
	}
	return out
}

func (l *s5Lab) apply(doc string) {
	l.t.Helper()
	res, err := l.nodes["node1"].RunInput(context.Background(), "sudo -n k3s kubectl apply -f -", strings.NewReader(doc))
	if err != nil {
		l.t.Fatalf("applying a document: %v", err)
	}
	if res.ExitCode != 0 {
		l.t.Fatalf("applying a document: exit %d: %s", res.ExitCode, firstLineOfE2E(res.Stderr))
	}
}

func (l *s5Lab) host(node, command string) {
	l.t.Helper()
	res, err := l.nodes[node].Run(context.Background(), command)
	if err != nil {
		l.t.Fatalf("%s: %s: %v", node, command, err)
	}
	if res.ExitCode != 0 {
		l.t.Fatalf("%s: %s: exit %d: %s", node, command, res.ExitCode, firstLineOfE2E(res.Stderr))
	}
}

func (l *s5Lab) waitFor(within time.Duration, what string, check func() (bool, string)) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	last := "not observed yet"
	for {
		ok, detail := check()
		if ok {
			l.t.Logf("%s: %s", what, detail)
			return
		}
		last = detail
		if time.Now().After(deadline) {
			l.t.Fatalf("%s did not happen within %s: %s", what, within, last)
		}
		time.Sleep(5 * time.Second)
	}
}

func (l *s5Lab) runCLI(out io.Writer, args ...string) error {
	l.t.Helper()
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

func (l *s5Lab) verbArgs(extra ...string) []string {
	args := []string{
		"--cluster", l.cluster,
		"--server", l.env.server,
		"--ssh-user", l.env.sshUser,
		"--ssh-key", l.env.sshKey,
		"--bundle-manifest", l.bundle,
	}
	return append(args, extra...)
}

// s5NodeNames maps the lab's three addresses to their cluster node names, by
// provider address — the same mapping `node replace` needs.
func (l *s5Lab) nodeNames() map[string]string {
	l.t.Helper()
	out := l.kubectl("get nodes -o jsonpath='{range .items[*]}{.metadata.name}{\" \"}{.status.addresses[?(@.type==\"InternalIP\")].address}{\"\\n\"}{end}'")
	names := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			names[fields[1]] = fields[0]
		}
	}
	return names
}

func (l *s5Lab) agentNode() string {
	l.t.Helper()
	out := strings.TrimSpace(l.kubectl("get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath={.items[0].metadata.name}"))
	if out == "" {
		l.t.Fatal("the lab has no agent node, so the dead-node fixture cannot be built")
	}
	return out
}

func (l *s5Lab) serverNode() string {
	l.t.Helper()
	return strings.TrimSpace(l.kubectl("get nodes -l node-role.kubernetes.io/control-plane -o jsonpath={.items[0].metadata.name}"))
}

// TestRestoreVolumeDeadNodeFixture is probe P5's two fixtures, through the
// command.
func TestRestoreVolumeDeadNodeFixture(t *testing.T) {
	env := gateEnvironment(t)
	lab := s5Open(t, env)
	lab.apply(s5NamespaceDocument())
	lab.kubectl("delete configmap kubenest-operation -n kube-system --ignore-not-found")

	t.Run("dead-node", func(t *testing.T) {
		agent := lab.agentNode()
		workload := "dead"
		tag := fmt.Sprintf("%d", time.Now().UnixNano())
		// Both claims are placed on the agent by pinning the workload there.
		lab.apply(s5WorkloadDocument(workload, map[string]string{"kubernetes.io/hostname": agent}, true))
		lab.waitFor(5*time.Minute, "the fixture's pod on the agent", func() (bool, string) {
			return lab.podRunning(workload), lab.podState(workload)
		})
		lab.writeClaim(workload, "/a", "dead-a-"+tag)
		lab.writeClaim(workload, "/b", "dead-b-"+tag)

		lab.backupNow()
		// The node loss: k3s-agent stopped, the pods force-deleted, and the
		// claims deleted — which is what a dead node's volumes look like from
		// the API.
		lab.kubectl("scale deployment " + workload + " --replicas=0")
		lab.stopAgent(agent)
		lab.waitFor(5*time.Minute, "the agent to go NotReady", func() (bool, string) {
			out, err := k3s.Kubectl(context.Background(), lab.nodes["node1"], "get node "+agent+" -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'")
			if err != nil {
				return false, err.Error()
			}
			return strings.TrimSpace(out) != "True", strings.TrimSpace(out)
		})
		lab.kubectl("-n " + s5Namespace + " delete pod -l app=" + workload + " --grace-period=0 --force --ignore-not-found")
		lab.kubectl("-n " + s5Namespace + " delete pvc " + workload + "-a " + workload + "-b --wait=false")
		lab.waitFor(3*time.Minute, "the dead node's claims to be gone", func() (bool, string) {
			out, err := k3s.Kubectl(context.Background(), lab.nodes["node1"], "-n "+s5Namespace+" get pvc -o name")
			if err != nil {
				return false, err.Error()
			}
			return strings.TrimSpace(out) == "", out
		})

		var out strings.Builder
		err := lab.runCLI(&out, append([]string{"backup", "restore",
			"--namespace", s5Namespace,
			"--pvc", workload + "-a", "--pvc", workload + "-b",
			"--latest", "--confirm"}, lab.verbArgs()...)...)
		if err != nil {
			t.Fatalf("the volume restore failed: %v\n%s", err, out.String())
		}
		for _, want := range []string{"Volume restore plan", "keeps its current contents", "restored — awaiting activation", "held at 0 replicas"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("the run does not say %q:\n%s", want, out.String())
			}
		}
		operationID := s4OperationID(out.String())
		if operationID == "" {
			t.Fatalf("the run printed no operation id:\n%s", out.String())
		}
		// Every named claim is Bound, on a node that is Ready.
		lab.waitFor(10*time.Minute, "both refilled claims to be Bound", func() (bool, string) {
			out, err := k3s.Kubectl(context.Background(), lab.nodes["node1"], "-n "+s5Namespace+" get pvc -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{\" \"}{end}'")
			if err != nil {
				return false, err.Error()
			}
			phases := strings.Fields(out)
			if len(phases) != 2 {
				return false, out
			}
			for _, phase := range phases {
				if !strings.HasSuffix(phase, "=Bound") {
					return false, out
				}
			}
			return true, out
		})

		// Activation brings the workload back, and the workload's OWN pod then
		// reads the backup's data — the same thing P5 measured.
		if err := lab.runCLI(&out, append([]string{"backup", "restore", "--activate", operationID, "--keep-desired"}, lab.verbArgs()...)...); err != nil {
			t.Fatalf("activation failed: %v\n%s", err, out.String())
		}
		lab.waitFor(5*time.Minute, "the workload's own pod to run on the refilled volumes", func() (bool, string) {
			return lab.podRunning(workload), lab.podState(workload)
		})
		if got := lab.readClaims(workload); !strings.Contains(got, "dead-a-"+tag) || !strings.Contains(got, "dead-b-"+tag) {
			t.Errorf("the workload reads %q, want the backup's dead-a-%s and dead-b-%s", got, tag, tag)
		}
		if node := strings.TrimSpace(lab.kubectl("-n " + s5Namespace + " get pods -l app=" + workload + " -o jsonpath={.items[0].spec.nodeName}")); node == agent {
			t.Errorf("the refilled workload landed back on the dead node %s", agent)
		}
	})

	t.Run("selective", func(t *testing.T) {
		server := lab.serverNode()
		workload := "sel"
		tag := fmt.Sprintf("%d", time.Now().UnixNano())
		lab.apply(s5WorkloadDocument(workload, map[string]string{"kubernetes.io/hostname": server}, false))
		lab.waitFor(5*time.Minute, "the selective fixture's pod", func() (bool, string) {
			return lab.podRunning(workload), lab.podState(workload)
		})
		lab.writeClaim(workload, "/a", "sel-a-v1-"+tag)
		lab.writeClaim(workload, "/b", "sel-b-v1-"+tag)
		unnamedBefore := lab.claimUID(workload + "-b")

		lab.backupNow()
		// NEWER data into the claim that will NOT be named.
		lab.writeClaim(workload, "/b", "sel-b-v2-NEWER-"+tag)

		var out strings.Builder
		err := lab.runCLI(&out, append([]string{"backup", "restore",
			"--namespace", s5Namespace,
			"--pvc", workload + "-a",
			"--latest", "--confirm"}, lab.verbArgs()...)...)
		if err != nil {
			// A PartiallyFailed restore is EXPECTED here — one error per volume
			// the strategic patch removed — and the command tolerates exactly
			// that. Any other failure is the gate's.
			t.Fatalf("the selective restore failed: %v\n%s", err, out.String())
		}
		operationID := s4OperationID(out.String())
		if operationID == "" {
			t.Fatalf("the run printed no operation id:\n%s", out.String())
		}
		if uid := lab.claimUID(workload + "-b"); uid != unnamedBefore {
			t.Errorf("the UNNAMED claim was replaced (uid %s -> %s): the restore disturbed a volume it was not asked to touch", unnamedBefore, uid)
		}
		if err := lab.runCLI(&out, append([]string{"backup", "restore", "--activate", operationID, "--keep-desired"}, lab.verbArgs()...)...); err != nil {
			t.Fatalf("activation failed: %v\n%s", err, out.String())
		}
		lab.waitFor(5*time.Minute, "the selective workload's own pod", func() (bool, string) {
			return lab.podRunning(workload), lab.podState(workload)
		})
		got := lab.readClaims(workload)
		if !strings.Contains(got, "sel-a-v1-"+tag) {
			t.Errorf("the named claim reads %q, want the backup's sel-a-v1-%s", got, tag)
		}
		if !strings.Contains(got, "sel-b-v2-NEWER-"+tag) {
			t.Errorf("the UNNAMED claim reads %q, want its newer sel-b-v2-NEWER-%s: this is the data-loss bug the mode exists to prevent", got, tag)
		}
	})
}

// TestRestoreVolumeResume kills the CLI between the scale-to-zero and the
// Restore, then finishes with --resume.
func TestRestoreVolumeResume(t *testing.T) {
	env := gateEnvironment(t)
	lab := s5Open(t, env)
	lab.apply(s5NamespaceDocument())
	lab.kubectl("delete configmap kubenest-operation -n kube-system --ignore-not-found")

	server := lab.serverNode()
	workload := "resume"
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	lab.apply(s5WorkloadDocument(workload, map[string]string{"kubernetes.io/hostname": server}, false))
	lab.waitFor(5*time.Minute, "the resume fixture's pod", func() (bool, string) {
		return lab.podRunning(workload), lab.podState(workload)
	})
	lab.writeClaim(workload, "/a", "resume-a-"+tag)
	lab.writeClaim(workload, "/b", "resume-b-"+tag)
	lab.backupNow()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		root := cmd.NewRootCommand()
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append([]string{"backup", "restore",
			"--namespace", s5Namespace,
			"--pvc", workload + "-a", "--pvc", workload + "-b",
			"--latest", "--confirm"}, lab.verbArgs()...))
		done <- root.ExecuteContext(ctx)
	}()
	// THE KILL WINDOW: the workloads are at zero and the claims are deleted, so
	// the next step is the restore itself.
	lab.waitFor(10*time.Minute, "the claims to be deleted before the Restore is created", func() (bool, string) {
		restores := strings.TrimSpace(lab.kubectl("get restores.velero.io -n velero -o name --no-headers 2>/dev/null"))
		claims := strings.TrimSpace(lab.kubectl("-n " + s5Namespace + " get pvc " + workload + "-a " + workload + "-b -o name --ignore-not-found"))
		if restores != "" {
			return false, "the Restore already exists; the window is gone"
		}
		return claims == "", "the claims are gone and no Restore exists yet"
	})
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the killed run reported success")
	}
	pause := strings.TrimSpace(lab.kubectl("get project " + s5Namespace + " -n kubenest-system -o jsonpath={.metadata.annotations.kubenest\\.io/reconcile-paused}"))
	if pause == "" {
		t.Fatal("the interrupted run left no pause behind")
	}
	scaledBefore := lab.kubectl("-n " + s5Namespace + " get deployment " + workload + " -o jsonpath={.spec.replicas}")

	var resumed strings.Builder
	if err := lab.runCLI(&resumed, append([]string{"backup", "restore", "--resume", pause}, lab.verbArgs()...)...); err != nil {
		t.Fatalf("--resume failed: %v\n%s", err, resumed.String())
	}
	if !strings.Contains(resumed.String(), "restored — awaiting activation") {
		t.Errorf("the resume did not finish the data restore:\n%s", resumed.String())
	}
	if got := strings.TrimSpace(lab.kubectl("-n " + s5Namespace + " get deployment " + workload + " -o jsonpath={.spec.replicas}")); got != scaledBefore {
		t.Errorf("the resume changed the workload's replicas (%s -> %s): it continues a restore, it does not scale anything back", scaledBefore, got)
	}
	if now := strings.TrimSpace(lab.kubectl("get project " + s5Namespace + " -n kubenest-system -o jsonpath={.metadata.annotations.kubenest\\.io/reconcile-paused}")); now != pause {
		t.Errorf("--resume lifted the pause (annotation %q)", now)
	}
	operationID := pause
	if err := lab.runCLI(&resumed, append([]string{"backup", "restore", "--activate", operationID, "--keep-desired"}, lab.verbArgs()...)...); err != nil {
		t.Fatalf("activation failed: %v\n%s", err, resumed.String())
	}
	lab.waitFor(5*time.Minute, "the resumed workload's own pod", func() (bool, string) {
		return lab.podRunning(workload), lab.podState(workload)
	})
	if got := lab.readClaims(workload); !strings.Contains(got, "resume-a-"+tag) || !strings.Contains(got, "resume-b-"+tag) {
		t.Errorf("the resumed workload reads %q, want the backup's resume-a-%s and resume-b-%s", got, tag, tag)
	}
}

// --- fixture helpers ---

// stopAgent takes the agent away the way a dead node is: its k3s-agent unit
// stops, so its volumes are unreachable and its Node goes NotReady. The stop
// happens on the agent's OWN host, found by the hostname k3s names the node
// with, because the API server that could tell us which host that is runs on
// the node we are about to take away.
func (l *s5Lab) stopAgent(node string) {
	l.t.Helper()
	for name, runner := range l.nodes {
		if name == "node1" {
			continue
		}
		out, err := runner.Run(context.Background(), "hostname")
		if err != nil {
			continue
		}
		if strings.TrimSpace(out.Stdout) != node {
			continue
		}
		res, err := runner.Run(context.Background(), "sudo -n systemctl stop k3s-agent")
		if err != nil || res.ExitCode != 0 {
			l.t.Fatalf("stopping k3s-agent on %s (%s): %v exit %d", node, name, err, res.ExitCode)
		}
		l.t.Logf("k3s-agent stopped on %s (%s)", node, name)
		return
	}
	l.t.Fatalf("no lab node's SSH session reports the hostname %s, so the agent could not be taken away", node)
}

func (l *s5Lab) backupNow() {
	l.t.Helper()
	var out strings.Builder
	if err := l.runCLI(&out, append([]string{"backup", "now"}, l.verbArgs()...)...); err != nil {
		l.t.Fatalf("backup now failed: %v\n%s", err, out.String())
	}
	if s4BackupName(out.String()) == "" {
		l.t.Fatalf("backup now named no backup:\n%s", out.String())
	}
}

func (l *s5Lab) podState(workload string) string {
	out, err := k3s.Kubectl(context.Background(), l.nodes["node1"], "-n "+s5Namespace+" get pods -l app="+workload+" -o jsonpath={.items[0].status.phase}")
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(out)
}

func (l *s5Lab) podRunning(workload string) bool {
	out, err := k3s.Kubectl(context.Background(), l.nodes["node1"], "-n "+s5Namespace+" get pods -l app="+workload+" -o jsonpath='{range .items[*]}{.status.phase}{\" \"}{end}'")
	if err != nil {
		return false
	}
	phases := strings.Fields(out)
	return len(phases) == 1 && phases[0] == "Running"
}

// writeClaim writes one file into the claim mounted at the given path.
func (l *s5Lab) writeClaim(workload, path, content string) {
	l.t.Helper()
	pod := strings.TrimSpace(l.kubectl("-n " + s5Namespace + " get pods -l app=" + workload + " -o jsonpath={.items[0].metadata.name}"))
	if pod == "" {
		l.t.Fatalf("no pod to write %s through for %s", path, workload)
	}
	if _, err := k3s.Kubectl(context.Background(), l.nodes["node1"],
		fmt.Sprintf("-n %s exec %s -- sh -c 'echo %s > %s/v.txt && sync'", s5Namespace, pod, content, path)); err != nil {
		l.t.Fatalf("writing %s: %v", path, err)
	}
}

// readClaims reads both claims' files through the workload's own pod.
func (l *s5Lab) readClaims(workload string) string {
	l.t.Helper()
	pod := strings.TrimSpace(l.kubectl("-n " + s5Namespace + " get pods -l app=" + workload + " -o jsonpath={.items[0].metadata.name}"))
	if pod == "" {
		return ""
	}
	out, err := k3s.Kubectl(context.Background(), l.nodes["node1"],
		fmt.Sprintf("-n %s exec %s -- sh -c 'echo a=$(cat /a/v.txt) b=$(cat /b/v.txt)'", s5Namespace, pod))
	if err != nil {
		l.t.Logf("reading the claims through %s: %v", pod, err)
		return ""
	}
	return strings.TrimSpace(out)
}

func (l *s5Lab) claimUID(name string) string {
	out, err := k3s.Kubectl(context.Background(), l.nodes["node1"], "-n "+s5Namespace+" get pvc "+name+" -o jsonpath={.metadata.uid}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func s5NamespaceDocument() string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata: {name: %s}\n", s5Namespace)
}

// s5WorkloadDocument is a Deployment with two claims, pinned to one node. The
// second claim is labelled differently from the first ONLY in the fixture's
// own labels, which the restore's selector uses: both carry the workload's
// selector labels, which is what the mode checks before it deletes anything.
func s5WorkloadDocument(name string, nodeSelector map[string]string, both bool) string {
	selector := "app: " + name
	nodeLine := ""
	if len(nodeSelector) > 0 {
		nodeLine = "      nodeSelector:\n"
		for key, value := range nodeSelector {
			nodeLine += fmt.Sprintf("        %s: %s\n", key, value)
		}
	}
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: %[1]s-a, namespace: %[2]s, labels: {%[3]s}}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: %[1]s-b, namespace: %[2]s, labels: {%[3]s}}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: %[1]s, namespace: %[2]s}
spec:
  replicas: 1
  selector: {matchLabels: {%[3]s}}
  template:
    metadata: {labels: {%[3]s}}
    spec:
%[4]s      containers:
        - name: app
          image: busybox:1.36
          command: [sh, -c, "sleep 1000000"]
          volumeMounts: [{name: a, mountPath: /a}, {name: b, mountPath: /b}]
      volumes:
        - {name: a, persistentVolumeClaim: {claimName: %[1]s-a}}
        - {name: b, persistentVolumeClaim: {claimName: %[1]s-b}}
`, name, s5Namespace, selector, nodeLine)
}
