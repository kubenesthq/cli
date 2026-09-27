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
//	export KUBENEST_ADMIN_EMAIL=… KUBENEST_ADMIN_PASSWORD=…   # to create the fixture's project
//	export KUBENEST_GATE_CLUSTER=…          # the cluster's name as the control plane knows it; the fixture's project is created on it (the default, gate-single-server, is not a three-node lab)
//	export KUBENEST_BUNDLE=…                # the bundle the lab installed (the default is 1.0)
//	export KUBENEST_BACKUP_ACCESS_KEY_ID=… KUBENEST_BACKUP_SECRET_ACCESS_KEY=…
//	cd kubenest-cli && go test -tags e2e -run TestRestoreVolume -v -timeout 4h ./e2e/
//
// The fixture's namespace is a PROJECT this gate creates through the control
// plane (createProjectNamespace, shared with S4), not a bare Namespace: mode 2's
// step 2 writes its reconcile pause on the Project named after the namespace and
// refuses when nothing can acknowledge it.
//
// The gate skips, naming that command, when only one node is present.
package e2e

import (
	"context"
	"encoding/json"
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

// s5Project is the fixture PROJECT, and each fixture gets its own workload name
// inside its namespace so the two subtests cannot see each other's volumes.
const s5Project = "e2e-restore-volumes"

// s5Namespace is the namespace the control plane gave that project. The
// OPERATOR creates it, not the gate: s5Open fills this in from the control
// plane's answer, and every call both gates hanging off this fixture make names
// it. It is deliberately not a constant — the platform, not the gate, decides a
// project's namespace, and a name derived from the project's name would be a
// guess about a value the control plane owns.
var s5Namespace string

// s5PendingSettle is how long the replacement pod must have been Pending before
// the gate believes it is stranded rather than still being scheduled.
const s5PendingSettle = 30 * time.Second

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
	// The fixture namespace is the PROJECT's, created the way the console
	// creates one: mode 2's pause has to land on the Project named after it, and
	// a namespace written by hand has no Project behind it (hardware,
	// 2026-09-27).
	s5Namespace = createProjectNamespace(t, env, controlPlane, env.cluster, s5Project, lab)
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

// kubectlStatus runs kubectl and returns its error rather than failing the
// test, for the callers that poll: a missing claim or namespace is an
// observation on the way to the answer, not a failure.
func (l *s5Lab) kubectlStatus(args string) (string, error) {
	return k3s.Kubectl(context.Background(), l.nodes["node1"], args)
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
		replicas := lab.replicas(workload)

		// THE NODE LOSS, as the API sees a node that is not coming back: its
		// k3s-agent stops, the Pod it held goes, and the ReplicaSet's
		// replacement mounts the claims it cannot reach.
		//
		// THE CLAIMS AND THE DEPLOYMENT STAY, because mode 2 does those steps
		// itself: step 1 finds the mounting workloads through their PODS,
		// step 3 scales those workloads to zero and step 4 deletes the stranded
		// claims (plan 7.5 mode 2). The first version of this fixture copied
		// probe P5's raw-Velero steps and scaled the Deployment, force-deleted
		// the Pods and deleted the claims first, and on hardware (2026-09-27)
		// the command refused with `claim e2e-restore-volumes/dead-a is not in
		// the namespace, so there is nothing to refill`.
		lab.stopAgent(agent)
		// The node comes back when this arm ends: the next arm needs a whole
		// cluster, and on hardware (2026-09-27) its backup could not reach the
		// store while this agent was still down.
		t.Cleanup(func() { lab.startAgent(agent) })
		lab.waitFor(5*time.Minute, "the agent to go NotReady", func() (bool, string) {
			out, err := k3s.Kubectl(context.Background(), lab.nodes["node1"], "get node "+agent+" -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}'")
			if err != nil {
				return false, err.Error()
			}
			return strings.TrimSpace(out) != "True", strings.TrimSpace(out)
		})
		lab.kubectl("-n " + s5Namespace + " delete pod " + lab.podOnNode(workload, agent) + " --grace-period=0 --force")
		lab.waitFor(5*time.Minute, "the ReplicaSet's replacement pod to stay Pending on the stranded claims", lab.pendingHolder(workload))
		before := lab.claimIdentities(workload+"-a", workload+"-b")

		var out strings.Builder
		err := lab.runCLI(&out, append([]string{"backup", "restore",
			"--namespace", s5Namespace,
			"--pvc", workload + "-a", "--pvc", workload + "-b",
			"--latest", "--confirm"}, lab.verbArgs()...)...)
		if err != nil {
			t.Fatalf("the volume restore failed: %v\n%s", err, out.String())
		}
		operationID := s4OperationID(out.String())
		if operationID == "" {
			t.Fatalf("the run printed no operation id:\n%s", out.String())
		}
		// Step 3 of mode 2 scales the mounting workload to zero and step 7
		// keeps it there until activation, so nothing of the workload runs
		// before its data is back. (The fixture no longer scales it; this is
		// the product's own postcondition.)
		if got := lab.replicas(workload); got != "0" {
			t.Errorf("the run left %s at %s replicas, want it held at 0 until activation (plan 7.5 mode 2, step 3)", workload, got)
		}
		// THE REFILL IS THE CLAIMS BEING NEW OBJECTS ON FRESH VOLUMES. The gate
		// no longer deletes the claims, so their phase proves nothing here: they
		// were Bound before the restore too, to the volumes the dead node held.
		// Step 4 deletes each named claim and Velero provisions a volume that
		// never belonged to that node, so both the claim's UID and its bound
		// volume must change.
		lab.waitFor(10*time.Minute, "both named claims replaced by fresh volumes", func() (bool, string) {
			for claim, was := range before {
				now, err := lab.claimIdentity(claim)
				if err != nil {
					return false, err.Error()
				}
				if now.uid == was.uid {
					return false, claim + " still has uid " + was.uid + ": it was not deleted and refilled"
				}
				if now.volume == "" {
					return false, claim + " is bound to no volume"
				}
				if now.volume == was.volume {
					return false, claim + " kept the volume " + was.volume + " it held on the dead node"
				}
			}
			return true, "both claims are new objects bound to fresh volumes"
		})

		// Activation brings the workload back to its recorded replica count,
		// and the workload's OWN pod then reads the backup's data — the same
		// thing P5 measured.
		if err := lab.runCLI(&out, append([]string{"backup", "restore", "--activate", operationID, "--keep-desired"}, lab.verbArgs()...)...); err != nil {
			t.Fatalf("activation failed: %v\n%s", err, out.String())
		}
		if got := lab.replicas(workload); got != replicas {
			t.Errorf("activation put %s at %s replicas, want the recorded %s", workload, got, replicas)
		}
		lab.waitFor(5*time.Minute, "the workload's own pod to run on the refilled volumes", func() (bool, string) {
			return lab.podRunning(workload), lab.podState(workload)
		})
		if got := lab.readClaims(workload); !strings.Contains(got, "dead-a-"+tag) || !strings.Contains(got, "dead-b-"+tag) {
			t.Errorf("the workload reads %q, want the backup's dead-a-%s and dead-b-%s", got, tag, tag)
		}
		// The fresh volumes are on a LIVE node: the pod that mounts them runs,
		// and it is not on the node that died.
		if node := strings.TrimSpace(lab.kubectl("-n " + s5Namespace + " get pods -l app=" + workload + " -o jsonpath={.items[0].spec.nodeName}")); node == agent || node == "" {
			t.Errorf("the refilled workload runs on %q, want a live node other than the dead %s", node, agent)
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
		namedBefore := lab.claimIdentityOrFatal(workload + "-a")
		unnamedBefore := lab.claimIdentityOrFatal(workload + "-b")
		replicas := lab.replicas(workload)

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
		// THE NAMED CLAIM IS THE ONE THE MODE MUST REPLACE, and the gate does
		// not delete it: step 4 does (plan 7.5 mode 2). Without this the
		// "reads the backup's data" check below would pass on a restore that
		// did nothing at all, because sel-a's file has not changed since the
		// backup was taken.
		namedAfter := lab.claimIdentityOrFatal(workload + "-a")
		if namedAfter.uid == namedBefore.uid {
			t.Errorf("the NAMED claim is still uid %s: it was not deleted and refilled, so the data below is the live node's, not the backup's", namedBefore.uid)
		}
		if namedAfter.volume == "" || namedAfter.volume == namedBefore.volume {
			t.Errorf("the NAMED claim is bound to %q, want a fresh volume and not the %s it held", namedAfter.volume, namedBefore.volume)
		}
		// The UNNAMED claim of the same Pod is the invariant this mode exists
		// for: neither its object nor its volume may be disturbed, and its
		// newer bytes must survive.
		unnamedAfter := lab.claimIdentityOrFatal(workload + "-b")
		if unnamedAfter.uid != unnamedBefore.uid || unnamedAfter.volume != unnamedBefore.volume {
			t.Errorf("the UNNAMED claim was replaced (uid %s -> %s, volume %s -> %s): the restore disturbed a volume it was not asked to touch",
				unnamedBefore.uid, unnamedAfter.uid, unnamedBefore.volume, unnamedAfter.volume)
		}
		if got := lab.replicas(workload); got != "0" {
			t.Errorf("the run left %s at %s replicas, want it held at 0 until activation (plan 7.5 mode 2, step 3)", workload, got)
		}
		if err := lab.runCLI(&out, append([]string{"backup", "restore", "--activate", operationID, "--keep-desired"}, lab.verbArgs()...)...); err != nil {
			t.Fatalf("activation failed: %v\n%s", err, out.String())
		}
		if got := lab.replicas(workload); got != replicas {
			t.Errorf("activation put %s at %s replicas, want the recorded %s", workload, got, replicas)
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
	before := lab.claimIdentities(workload+"-a", workload+"-b")
	lab.backupNow()
	restoresBefore, err := lab.volumeRestores()
	if err != nil {
		t.Fatalf("listing the velero Restores before the run: %v", err)
	}

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
	// the next step is the restore itself. What must not exist yet is THIS RUN's
	// Restore — not every Restore in the velero namespace: the command does not
	// delete the Restore objects it creates, and the arm before this one leaves
	// its own behind, so "velero has no Restore" is a condition that never comes
	// back.
	lab.waitFor(10*time.Minute, "the claims to be deleted before the Restore is created", func() (bool, string) {
		now, err := lab.volumeRestores()
		if err != nil {
			return false, err.Error()
		}
		if len(now) != len(restoresBefore) {
			return false, "the Restore already exists; the window is gone"
		}
		claims := strings.TrimSpace(lab.kubectl("-n " + s5Namespace + " get pvc " + workload + "-a " + workload + "-b -o name --ignore-not-found"))
		return claims == "", "the claims are gone and no Restore of this run exists yet"
	})
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the killed run reported success")
	}
	pause := strings.TrimSpace(lab.kubectl("get project " + s5Namespace + " -n kubenest-system -o jsonpath={.metadata.annotations.kubenest\\.io/reconcile-paused}"))
	if pause == "" {
		t.Fatal("the interrupted run left no pause behind")
	}
	scaledBefore := strings.TrimSpace(lab.kubectl("-n " + s5Namespace + " get deployment " + workload + " -o jsonpath={.spec.replicas}"))

	var resumed strings.Builder
	if err := lab.runCLI(&resumed, append([]string{"backup", "restore", "--resume", pause}, lab.verbArgs()...)...); err != nil {
		t.Fatalf("--resume failed: %v\n%s", err, resumed.String())
	}
	// The resume finished the data restore: the claims the killed run deleted
	// come back as NEW objects bound to fresh volumes. The claim is the
	// postcondition, not the run's own wording.
	lab.waitFor(10*time.Minute, "the resumed claims refilled and bound to fresh volumes", func() (bool, string) {
		for claim, was := range before {
			now, err := lab.claimIdentity(claim)
			if err != nil {
				return false, err.Error()
			}
			if now.uid == was.uid {
				return false, claim + " is still uid " + was.uid
			}
			if now.volume == "" || now.volume == was.volume {
				return false, claim + " is bound to " + now.volume
			}
		}
		return true, "both claims are new objects bound to fresh volumes"
	})
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

// startAgent starts k3s-agent again on the node stopAgent took away.
func (l *s5Lab) startAgent(node string) {
	l.t.Helper()
	for name, runner := range l.nodes {
		if name == "node1" {
			continue
		}
		out, err := runner.Run(context.Background(), "hostname")
		if err != nil || strings.TrimSpace(out.Stdout) != node {
			continue
		}
		res, err := runner.Run(context.Background(), "sudo -n systemctl start k3s-agent")
		if err != nil || res.ExitCode != 0 {
			l.t.Errorf("starting k3s-agent on %s (%s) again: %v exit %d", node, name, err, res.ExitCode)
			return
		}
		l.t.Logf("k3s-agent started on %s (%s) again", node, name)
		return
	}
	l.t.Errorf("no lab node's SSH session reports the hostname %s, so its agent was not started again", node)
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

// replicas is the workload's desired replica count: mode 2 records it before
// scaling the workload to zero (step 3) and puts it back at activation (step 7),
// so the gate reads it itself rather than trusting either run's prose.
func (l *s5Lab) replicas(workload string) string {
	return strings.TrimSpace(l.kubectl("-n " + s5Namespace + " get deployment " + workload + " -o jsonpath={.spec.replicas}"))
}

// podOnNode names the pod a workload has on one node.
func (l *s5Lab) podOnNode(workload, node string) string {
	l.t.Helper()
	pod := strings.TrimSpace(l.kubectl("-n " + s5Namespace + " get pods -l app=" + workload + " --field-selector spec.nodeName=" + node + " -o jsonpath={.items[0].metadata.name}"))
	if pod == "" {
		l.t.Fatalf("%s has no pod on %s, so that node's loss cannot be simulated", workload, node)
	}
	return pod
}

// pendingHolder is what a dead node leaves behind, and the state mode 2's step 1
// needs to see: the pod is still there, it mounts the claims the node took away,
// and it stays Pending because nothing can give it those volumes.
//
// THE REPLACEMENT POD IS WHY THE FIXTURE DOES NOT SCALE ANYTHING. The gate used
// to delete the pod and the claims and scale the Deployment to zero, which is
// what P5's raw-Velero probe had to do to Velero directly; through the command
// it is step 1's input and steps 3-4's job. On hardware (2026-09-27) that
// fixture made the command refuse with "claim .../dead-a is not in the
// namespace, so there is nothing to refill".
//
// The pod list is read as JSON rather than through a path expression, the way
// the product reads cluster state: the mounts are a list of objects, and a
// rendering that silently yields nothing would look like a pod that mounts
// nothing instead of failing here.
func (l *s5Lab) pendingHolder(workload string) func() (bool, string) {
	return func() (bool, string) {
		out, err := l.kubectlStatus("-n " + s5Namespace + " get pods -l app=" + workload + " -o json")
		if err != nil {
			return false, err.Error()
		}
		var list struct {
			Items []struct {
				Metadata struct {
					Name              string `json:"name"`
					CreationTimestamp string `json:"creationTimestamp"`
				} `json:"metadata"`
				Spec struct {
					Volumes []struct {
						PersistentVolumeClaim *struct {
							ClaimName string `json:"claimName"`
						} `json:"persistentVolumeClaim"`
					} `json:"volumes"`
				} `json:"spec"`
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return false, err.Error()
		}
		if len(list.Items) != 1 {
			return false, fmt.Sprintf("the workload has %d pods", len(list.Items))
		}
		pod := list.Items[0]
		if pod.Status.Phase != "Pending" {
			return false, pod.Metadata.Name + " is " + pod.Status.Phase
		}
		mounted := map[string]bool{}
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				mounted[volume.PersistentVolumeClaim.ClaimName] = true
			}
		}
		for _, claim := range []string{workload + "-a", workload + "-b"} {
			if !mounted[claim] {
				return false, pod.Metadata.Name + " does not mount " + claim
			}
		}
		at, err := time.Parse(time.RFC3339, pod.Metadata.CreationTimestamp)
		if err != nil {
			return false, pod.Metadata.Name + " was created at " + pod.Metadata.CreationTimestamp + ", which does not parse"
		}
		if age := time.Since(at); age < s5PendingSettle {
			return false, fmt.Sprintf("%s has been Pending for %s of %s", pod.Metadata.Name, age.Round(time.Second), s5PendingSettle)
		}
		return true, fmt.Sprintf("%s has been Pending on the stranded claims for %s", pod.Metadata.Name, time.Since(at).Round(time.Second))
	}
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

// claimIdentity is a claim's UID and the volume it is bound to.
//
// IT IS THE OBSERVABLE A REFILL HAS TO MOVE, now that the gate no longer
// deletes the claims itself: mode 2's step 4 deletes each named claim and Velero
// provisions a volume that never belonged to the dead node, so a claim that
// still has its old UID or its old volume was not refilled — and its Bound
// phase says nothing, because it was Bound before the restore too (probe P5:
// claims that kept their volume name stayed Pending for ever).
type claimIdentity struct {
	uid    string
	volume string
}

func (l *s5Lab) claimIdentity(claim string) (claimIdentity, error) {
	out, err := l.kubectlStatus("-n " + s5Namespace + " get persistentvolumeclaim " + claim + " -o jsonpath='{.metadata.uid} {.spec.volumeName}'")
	if err != nil {
		return claimIdentity{}, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return claimIdentity{}, fmt.Errorf("claim %s reads %q, want a uid and a volume name", claim, strings.TrimSpace(out))
	}
	return claimIdentity{uid: fields[0], volume: fields[1]}, nil
}

func (l *s5Lab) claimIdentities(claims ...string) map[string]claimIdentity {
	l.t.Helper()
	out := map[string]claimIdentity{}
	for _, claim := range claims {
		identity, err := l.claimIdentity(claim)
		if err != nil {
			l.t.Fatalf("reading %s before the restore: %v", claim, err)
		}
		out[claim] = identity
	}
	return out
}

// claimIdentityOrFatal is claimIdentity where the claim MUST be there: the
// caller reads it to compare with one it read earlier, so a missing claim is a
// failure rather than an observation.
func (l *s5Lab) claimIdentityOrFatal(claim string) claimIdentity {
	l.t.Helper()
	identity, err := l.claimIdentity(claim)
	if err != nil {
		l.t.Fatalf("reading %s: %v", claim, err)
	}
	return identity
}

// volumeRestores names the mode-2 Restore objects in the velero namespace —
// "kubenest-restore-volumes-<operation>" (the drill's are
// "kubenest-restore-drill-…" and mode 1's are "kubenest-restore-<operation>",
// so the prefix is this mode's own). The resume gate uses the count to tell
// whether the run it killed had reached its Restore: the command never deletes
// these, so the answer has to be a difference from a baseline.
func (l *s5Lab) volumeRestores() ([]string, error) {
	out, err := l.kubectlStatus("get restores.velero.io -n velero -o jsonpath='{range .items[*]}{.metadata.name}{\"\\n\"}{end}'")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name := strings.TrimSpace(line); strings.HasPrefix(name, "kubenest-restore-volumes-") {
			names = append(names, name)
		}
	}
	return names, nil
}

// s5NamespaceDocument is the bare Namespace document S5's node gate (T5.5,
// node_lifecycle_test.go) still applies before its workloads. This gate does not
// call it: s5Open has already made the fixture namespace a PROJECT, which is
// what mode 2's pause needs, so applying this on top is only a merge into a
// namespace the operator owns. It stays until that gate's fixture moves onto
// the project too.
func s5NamespaceDocument() string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata: {name: %s}\n", s5Namespace)
}

// s5WorkloadDocument is a Deployment with two claims, pinned to one node. Both
// claims carry the workload's selector labels, which the mode REQUIRES before it
// deletes anything (Velero's selector has to match the claim for it to come
// back), and the unnamed one of the two keeps its current contents because the
// restore's modifier strips it from the restored pod — not because the selector
// misses it.
//
// The namespace is s5Namespace, the one the control plane gave the fixture's
// project: the operator creates it, so no caller passes a name of its own. both
// is T5.5's node gate's argument and decides nothing here: the mode needs BOTH
// claims to carry the workload's selector labels, so the two documents are the
// same either way.
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
