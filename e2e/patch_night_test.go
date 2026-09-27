//go:build e2e

// T3.7's gate: S3, the patch night (PLAN-CLOSE-THE-GAP-2026-09.md v6.1, section
// 4, "S3 — Patch night"), on REAL Ubuntu hosts.
//
// What it asserts, which is the gate:
//
//	the window the CLI stores opens fifteen minutes later, and while it is shut
//	  NO node reboots even with the reboot-required marker present
//	while a NEW window is applying every node is held with
//	  kubenest.io/auto-reboot=false, and kured is not even running there
//	when the window opens the two agents reboot ONE AT A TIME (never two nodes
//	  cordoned or NotReady at once), each boot ID changes and its pending state
//	  clears
//	the workload survives the agent reboots inside its predeclared limits,
//	  reported separately from the Kubernetes API's own reading
//	no NODE_NOT_READY alert fires for a planned reboot
//	the server is NOT rebooted automatically: it stays held, refuses without an
//	  explicit --confirm, and `kubenest node reboot --confirm --wait` takes it
//	  down inside the window and waits from OUTSIDE over SSH
//	lane A's single-server server goes through the same manual verb
//
// The automatic path is the one this gate is written for because probe P1
// (kn-t33, run on real hardware 2026-09-25) answered YES: kured honours the
// lock, and kubenest.io/auto-reboot=false stops it on the node. A lab that
// takes P1's fallback fails the agent-reboot subtest rather than passing
// silently, which is correct: the fallback changes the product's promise.
//
// Run from the umbrella workspace with a THREE-node lab plus the lane-A
// single-server cluster:
//
//	./scripts/ephemeral-env.sh up --profile host --nodes 3 --name <name>
//	source lab/hetzner/.lab-env.sh
//	export KUBENEST_CONTROL_PLANE=http://localhost:8000 KUBENEST_CLI_TOKEN=...
//	export KUBENEST_LAB_NODE1_IP=... KUBENEST_LAB_NODE2_IP=... KUBENEST_LAB_NODE3_IP=...
//	cd kubenest-cli && go test -tags e2e -v -timeout 300m ./e2e/ -run TestS3PatchNightGate
//
// k3d cannot run this at all: no systemd, no block devices, no kernel to
// reboot, and no kured. The three-node cluster is installed here (server =
// node1, agents = node2 and node3) at KUBENEST_PATCH_NIGHT_BUNDLE; the cluster
// gateEnvironment names (KUBENEST_GATE_CLUSTER) is used only as lane A, and
// that half skips when lane A is not registered rather than installing a
// second single-server cluster.
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/cmd"
	"kubenest.io/cli/pkg/config"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/uninstall"
	"kubenest.io/cli/pkg/window"
)

// pnEnv is the fixture S3 needs on top of gateEnvironment's.
type pnEnv struct {
	gateEnv
	// agent1 and agent2 are the two agent hosts. They reboot by themselves when
	// the window opens; the server does not.
	agent1, agent2 string
	// patchCluster is the THREE-node cluster this gate installs and then
	// patches. It is deliberately not gateEnvironment's cluster, which is lane
	// A's single-server fixture.
	patchCluster string
	// bundle is what the three-node cluster is installed at. os_patching and
	// the node reporter are bundle 1.2's, so an older bundle cannot produce the
	// evidence this gate asserts.
	bundle string
	// zone is the window's IANA zone. A zone with a real offset makes a
	// refusal's local and UTC halves two renderings of one instant rather than
	// the same string twice.
	zone *time.Location
	// outside is how long the shut-window observation runs: the scenario's ten
	// minutes, two report intervals plus slack.
	outside time.Duration
}

func pnEnvironment(t *testing.T) pnEnv {
	t.Helper()
	base := gateEnvironment(t)
	env := pnEnv{
		gateEnv:      base,
		agent1:       os.Getenv("KUBENEST_LAB_NODE2_IP"),
		agent2:       os.Getenv("KUBENEST_LAB_NODE3_IP"),
		patchCluster: envOr("KUBENEST_PATCH_NIGHT_CLUSTER", "gate-patch-night"),
		bundle:       envOr("KUBENEST_PATCH_NIGHT_BUNDLE", base.bundle),
	}
	if env.agent1 == "" || env.agent2 == "" {
		t.Skip("KUBENEST_LAB_NODE2_IP and KUBENEST_LAB_NODE3_IP are both required: S3 is one server plus two agents, and one-at-a-time serialisation cannot be observed on fewer nodes")
	}
	zone := envOr("KUBENEST_GATE_WINDOW_TZ", "Asia/Kolkata")
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatalf("KUBENEST_GATE_WINDOW_TZ=%q is not an IANA name: %v", zone, err)
	}
	env.zone = loc
	minutes := 10
	if raw := os.Getenv("KUBENEST_PATCH_NIGHT_OUTSIDE_MINUTES"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("KUBENEST_PATCH_NIGHT_OUTSIDE_MINUTES=%q is not a positive number of minutes", raw)
		}
		minutes = parsed
	}
	env.outside = time.Duration(minutes) * time.Minute
	return env
}

// pnHome gives the CLI a HOME of its own, logged in to the lab's control plane,
// so the flags, the config file and the refusals are the ones an operator gets
// and no operator journal is touched.
func pnHome(t *testing.T, env pnEnv) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := config.Save(&config.Config{
		ControlPlaneURL: env.controlPlane,
		ControlPlaneCA:  string(env.controlPlaneCA),
	}); err != nil {
		t.Fatal(err)
	}
	creds := &config.Credentials{Tokens: map[string]config.StoredCredential{}}
	creds.Set(env.controlPlane, env.token)
	if err := config.SaveCredentials(creds); err != nil {
		t.Fatal(err)
	}
}

// pnRunCLI runs the REAL command tree, so set-window's validation, the refusal
// wording and node reboot's flags are the ones an operator gets.
func pnRunCLI(out io.Writer, args ...string) error {
	root := cmd.NewRootCommand()
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	return root.Execute()
}

// pnNow is the one timestamp format every log line here uses, so a -v
// transcript says WHEN each observation was made.
func pnNow() string { return time.Now().Format(time.RFC3339) }

// --- hosts -----------------------------------------------------------------
//
// Every remote read goes over SSH through the CLI's own transport or through
// k3s.Kubectl, never a local kubeconfig (AGENTS.md: "never point a local
// kubeconfig at a provisioned or customer cluster. Use SSH to the host").

// pnNode is one lab host and the role it plays.
type pnNode struct {
	address string
	agent   bool
	runner  k3s.Runner
}

func pnConnect(t *testing.T, env pnEnv) []pnNode {
	t.Helper()
	opts := sshx.Options{
		User:           env.sshUser,
		KeyPath:        env.sshKey,
		KnownHostsPath: t.TempDir() + "/known_hosts",
		DialTimeout:    15 * time.Second,
	}
	var nodes []pnNode
	for i, address := range []string{env.server, env.agent1, env.agent2} {
		endpoint, err := sshx.Resolve(address, opts)
		if err != nil {
			t.Fatal(err)
		}
		client, err := sshx.Dial(context.Background(), endpoint, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		nodes = append(nodes, pnNode{address: address, agent: i > 0, runner: client})
	}
	return nodes
}

// pnDial is one SSH connection to an address the control plane recorded, which
// is how the lane-A half reaches a host this test was not told about.
func pnDial(t *testing.T, env pnEnv, address, user string) k3s.Runner {
	t.Helper()
	opts := sshx.Options{
		User:           user,
		KeyPath:        env.sshKey,
		KnownHostsPath: t.TempDir() + "/known_hosts",
		DialTimeout:    15 * time.Second,
	}
	endpoint, err := sshx.Resolve(address, opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshx.Dial(context.Background(), endpoint, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func pnHostRun(t *testing.T, ctx context.Context, r k3s.Runner, command string) string {
	t.Helper()
	res, err := r.Run(ctx, command)
	if err != nil {
		t.Fatalf("running %q: %v", command, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("running %q: exit %d: %s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout)
}

// pnBootID is the host's boot id, read from the kernel rather than from
// Kubernetes, so a k3s-agent restart cannot be mistaken for a reboot and vice
// versa.
func pnBootID(t *testing.T, ctx context.Context, r k3s.Runner) string {
	t.Helper()
	return pnHostRun(t, ctx, r, "cat /proc/sys/kernel/random/boot_id")
}

// pnBootIDOrEmpty is pnBootID for a host that may be DOWN, which a reboot
// makes it: the SSH read fails then, and an expected absence is not a failure.
// An empty answer means "not observed", and a caller comparing boot ids must
// treat it as "keep waiting" rather than as a value.
func pnBootIDOrEmpty(ctx context.Context, r k3s.Runner) string {
	res, err := r.Run(ctx, "cat /proc/sys/kernel/random/boot_id")
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// pnRebootPending is Ubuntu's own marker. The operator's whole os_patching
// story is derived from it, so it is the ground truth here.
func pnRebootPending(t *testing.T, ctx context.Context, r k3s.Runner) bool {
	t.Helper()
	res, err := r.Run(ctx, "test -f /var/run/reboot-required && echo yes || echo no")
	if err != nil {
		t.Fatalf("reading the reboot marker: %v", err)
	}
	return strings.TrimSpace(res.Stdout) == "yes"
}

// pnTouchMarker creates Ubuntu's reboot-required marker. It needs nothing from
// apt, which is exactly why it is the deterministic probe of the reboot path.
func pnTouchMarker(t *testing.T, ctx context.Context, r k3s.Runner) {
	t.Helper()
	pnHostRun(t, ctx, r, "sudo -n touch /var/run/reboot-required")
}

// pnUnattendedDryRun reports whether this host has updates unattended-upgrades
// would apply, which is how the bead says to choose the node that takes the
// real-update path.
func pnUnattendedDryRun(t *testing.T, ctx context.Context, r k3s.Runner) (bool, string) {
	t.Helper()
	// Refresh the package lists first: a host with pending security updates must
	// not be missed because nobody ran apt-get update since the image was built.
	if res, err := r.Run(ctx, "sudo -n apt-get update -qq"); err != nil {
		t.Fatalf("apt-get update: %v", err)
	} else if res.ExitCode != 0 {
		t.Fatalf("apt-get update: exit %d: %s", res.ExitCode, pnFirstLine(res.Stderr))
	}
	res, err := r.Run(ctx, "sudo -n unattended-upgrade --dry-run 2>&1 | tail -20")
	if err != nil {
		t.Fatalf("unattended-upgrade --dry-run: %v", err)
	}
	text := res.Stdout + res.Stderr
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(line), "packages that will be upgraded") {
			continue
		}
		_, list, _ := strings.Cut(line, ":")
		if strings.TrimSpace(list) != "" {
			return true, line
		}
	}
	return false, text
}

// pnApplyUnattended runs the real unattended-upgrades, whose success branch is
// what writes Ubuntu's marker.
func pnApplyUnattended(t *testing.T, ctx context.Context, r k3s.Runner) {
	t.Helper()
	res, err := r.Run(ctx, "sudo -n unattended-upgrade -d 2>&1 | tail -5")
	if err != nil {
		t.Fatalf("unattended-upgrade: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("unattended-upgrade: exit %d: %s", res.ExitCode, pnFirstLine(res.Stderr))
	}
}

func pnFirstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// --- Kubernetes reads from a surviving node --------------------------------

// pnNodeList is the slice of `kubectl get nodes -o json` this gate needs.
type pnNodeList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			NodeInfo struct {
				BootID string `json:"bootID"`
			} `json:"nodeInfo"`
		} `json:"status"`
	} `json:"items"`
}

// pnNodeState is one node as Kubernetes sees it, which is the OUTSIDE view the
// serialisation claim is made from: kured's cordon is a spec change and its
// reboot is the kubelet going NotReady, and both are visible on the API server
// of a node that is still up.
type pnNodeState struct {
	Name             string
	Unschedulable    bool
	Ready            bool
	BootID           string
	HasAutoRebootKey bool
	AutoRebootLabel  string
}

func pnReadNodes(t *testing.T, ctx context.Context, r k3s.Runner) map[string]pnNodeState {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, "get nodes -o json")
	if err != nil {
		return nil
	}
	var list pnNodeList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil
	}
	states := map[string]pnNodeState{}
	for _, item := range list.Items {
		state := pnNodeState{Name: item.Metadata.Name, Unschedulable: item.Spec.Unschedulable,
			BootID: item.Status.NodeInfo.BootID}
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Ready" {
				state.Ready = condition.Status == "True"
			}
		}
		if value, ok := item.Metadata.Labels["kubenest.io/auto-reboot"]; ok {
			state.HasAutoRebootKey, state.AutoRebootLabel = true, value
		}
		states[state.Name] = state
	}
	return states
}

func pnDisruptive(states map[string]pnNodeState) []string {
	var out []string
	for name, state := range states {
		if state.Unschedulable || !state.Ready {
			out = append(out, name)
		}
	}
	return out
}

// pnHelmChartConfigValues reads the kured values the operator wrote, which is
// the object the refused crossing window must leave untouched.
func pnHelmChartConfigValues(t *testing.T, ctx context.Context, r k3s.Runner) string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, `get helmchartconfig kured -n kube-system -o jsonpath='{.spec.valuesContent}'`)
	if err != nil {
		return ""
	}
	return out
}

// pnKuredPodNodes reports which nodes currently run a kured pod. kured's
// DaemonSet has an affinity excluding held nodes, so this is the strongest
// outside view that the hold is in force: kured cannot reboot a node it is not
// running on.
func pnKuredPodNodes(t *testing.T, ctx context.Context, r k3s.Runner) []string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, `get pods -n kube-system -l app.kubernetes.io/name=kured -o json`)
	if err != nil {
		return nil
	}
	var list struct {
		Items []struct {
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil
	}
	var nodes []string
	for _, item := range list.Items {
		nodes = append(nodes, item.Spec.NodeName)
	}
	return nodes
}

// --- the window -------------------------------------------------------------

// pnSetWindow stores a window through the REAL `kubenest cluster set-window`,
// which is where T3.4's agreement rules are enforced. It returns the run's
// output so the caller can assert on what the operator was told.
func pnSetWindow(t *testing.T, env pnEnv, cluster string, days []string, start, end, zone string) string {
	t.Helper()
	var out bytes.Buffer
	err := pnRunCLI(&out, "cluster", "set-window", "--cluster", cluster,
		"--days", strings.Join(days, ","), "--start", start, "--end", end, "--timezone", zone)
	if err != nil {
		t.Fatalf("kubenest cluster set-window failed: %v\n%s", err, out.String())
	}
	return out.String()
}

// pnWindowRecord reads back what the control plane stored, because that is the
// window every gate computes from (the backend canonicalizes the day list).
func pnWindowRecord(t *testing.T, ctx context.Context, client *api.Client, clusterID string) api.MaintenanceWindowRecord {
	t.Helper()
	record, err := client.MaintenanceWindow(ctx, clusterID)
	if err != nil {
		t.Fatalf("reading the stored maintenance window: %v", err)
	}
	if record.Window == nil {
		t.Fatal("the control plane stored no window")
	}
	return record
}

// pnWindowOpening is when the stored window next opens, from the window the
// control plane holds rather than from the numbers this test typed.
func pnWindowOpening(t *testing.T, record api.MaintenanceWindowRecord, now time.Time) time.Time {
	t.Helper()
	parsed, err := window.Parse(window.Spec{
		Days: record.Window.Days, Start: record.Window.Start, End: record.Window.End, Timezone: record.Window.Timezone,
	})
	if err != nil {
		t.Fatalf("the stored window is not one the CLI can represent: %v", err)
	}
	at, ok := parsed.NextOpen(now)
	if !ok {
		t.Fatal("the stored window has no opening within the next week")
	}
	return at
}

// --- the control plane's own surfaces ---------------------------------------

// pnCheck is one health check as GET /api/v1/clusters/<id>/health reports it.
type pnCheck struct {
	Check      string         `json:"check"`
	Status     string         `json:"status"`
	ReasonCode string         `json:"reason_code"`
	Message    string         `json:"message"`
	Detail     map[string]any `json:"detail"`
}

type pnHealthView struct {
	Status     string     `json:"status"`
	ExitCode   int        `json:"exit_code"`
	Checks     []pnCheck  `json:"checks"`
	ReceivedAt *time.Time `json:"received_at"`
}

func (h pnHealthView) check(name string) (pnCheck, bool) {
	for _, c := range h.Checks {
		if c.Check == name {
			return c, true
		}
	}
	return pnCheck{}, false
}

// pnAlert mirrors the alerts route's items: "no NODE_NOT_READY alert fired" is
// read from the alert record, not from a check's own message.
type pnAlert struct {
	ClusterID   string    `json:"cluster_id"`
	Check       string    `json:"check"`
	ReasonCode  string    `json:"reason_code"`
	Status      string    `json:"status"`
	Message     string    `json:"message"`
	State       string    `json:"state"`
	FirstSeenAt time.Time `json:"first_seen_at"`
}

// pnHTTPClient trusts the control plane's CA when the lab exported one and the
// system roots otherwise, so the same helper works against a local control
// plane and a self-hosted one.
func pnHTTPClient(ca []byte) *http.Client {
	if len(ca) == 0 {
		return &http.Client{Timeout: 20 * time.Second}
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(ca)
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
}

// pnGetJSON is the read `kubenest health --json` (T2.9) will make once that
// command lands. Until then this gate reads the endpoint with the same bearer
// token, which is the fallback the bead names.
func pnGetJSON(t *testing.T, ctx context.Context, env pnEnv, path string, out any) {
	t.Helper()
	if err := pnGetJSONQuiet(ctx, env, path, out); err != nil {
		t.Fatalf("%v", err)
	}
}

// pnGetJSONQuiet is pnGetJSON for a poller: a read that fails is a sample that
// was not taken, not a failure, and a goroutine must not call t.Fatal.
func pnGetJSONQuiet(ctx context.Context, env pnEnv, path string, out any) error {
	base := strings.TrimSuffix(env.controlPlane, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Accept", "application/json")
	resp, err := pnHTTPClient(env.controlPlaneCA).Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s returned something that is not the documented JSON: %w", path, err)
	}
	return nil
}

func pnClusterHealth(t *testing.T, ctx context.Context, env pnEnv, clusterID string) pnHealthView {
	t.Helper()
	var view pnHealthView
	pnGetJSON(t, ctx, env, "/api/v1/clusters/"+url.PathEscape(clusterID)+"/health", &view)
	return view
}

func pnAlerts(t *testing.T, ctx context.Context, env pnEnv, orgID string) []pnAlert {
	t.Helper()
	var list struct {
		Items []pnAlert `json:"items"`
	}
	pnGetJSON(t, ctx, env, "/api/v1/orgs/"+url.PathEscape(orgID)+"/alerts", &list)
	return list.Items
}

// pnOrgIDAndCluster resolves the org and cluster ids. Unlike clusterIDFor it
// reports "not registered" rather than failing, because lane A's absence is a
// skip and not a defect.
func pnOrgIDAndCluster(ctx context.Context, client *api.Client, name string) (string, string) {
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		return "", ""
	}
	for _, org := range orgs {
		clusters, err := client.ListOrgClusters(ctx, org.ID)
		if err != nil {
			return "", ""
		}
		for _, c := range clusters {
			if c.Name == name {
				return org.ID, c.ID
			}
		}
	}
	return "", ""
}

// --- the operator's own patch evidence --------------------------------------

// pnPatchNode is one entry of the os_patching health detail. `pending_days` is
// present exactly when that node has a reboot pending, which is how "health
// shows the pending state" is read from the surface.
type pnPatchNode struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	PendingDay float64 `json:"pending_days"`
	Reason     string  `json:"reason"`
}

func pnPatchDetail(t *testing.T, view pnHealthView) ([]pnPatchNode, pnCheck) {
	t.Helper()
	check, ok := view.check("os_patching")
	if !ok {
		t.Fatalf("the health surface reports no os_patching check, so the patch evidence this gate asserts does not exist (cluster state %q, %d checks)", view.Status, len(view.Checks))
	}
	if check.Status == "unsupported" {
		t.Fatalf("this cluster's bundle does not declare os_patching (status %q): S3's evidence cannot exist without the T3.6 producer, so install the patch-night cluster at a bundle that ships it (KUBENEST_PATCH_NIGHT_BUNDLE)", check.Status)
	}
	raw, _ := check.Detail["nodes"].([]any)
	nodes := make([]pnPatchNode, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		node := pnPatchNode{Name: fmt.Sprint(entry["name"]), Status: fmt.Sprint(entry["status"]),
			Reason: fmt.Sprint(entry["reason"])}
		if days, ok := entry["pending_days"].(float64); ok {
			node.PendingDay = days
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 {
		t.Fatalf("the os_patching check carries no per-node evidence (status %q, reason %q), so which host needs a reboot cannot be read: the T3.6 producer is the prerequisite", check.Status, check.ReasonCode)
	}
	return nodes, check
}

// pnBookkeeping is the operator's own per-node record: when each node's reboot
// was FIRST seen pending, on its current boot ID. It lives in a ConfigMap in
// the operator's namespace so an operator restart cannot reset a weeks-old age
// to zero, and it is the only place the first-seen instant exists.
type pnBookkeeping struct {
	Nodes []struct {
		Name               string `json:"name"`
		BootID             string `json:"boot_id"`
		RebootPendingSince string `json:"reboot_pending_since"`
	} `json:"nodes"`
}

func pnOperatorNamespace(t *testing.T, ctx context.Context, r k3s.Runner) string {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, `get daemonset -A -l app.kubernetes.io/component=node-reporter -o jsonpath='{.items[0].metadata.namespace}'`)
	if err != nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(out), "'")
}

func pnBookkeepingRead(t *testing.T, ctx context.Context, r k3s.Runner, namespace string) pnBookkeeping {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, fmt.Sprintf(
		`get configmap kubenest-os-patching-bookkeeping -n %s -o jsonpath='{.data.bookkeeping\.json}'`, namespace))
	if err != nil {
		t.Fatalf("reading the operator's patch bookkeeping: %v", err)
	}
	var book pnBookkeeping
	if err := json.Unmarshal([]byte(strings.Trim(strings.TrimSpace(out), "'")), &book); err != nil {
		t.Fatalf("the patch bookkeeping ConfigMap is not the documented JSON: %v", err)
	}
	return book
}

// pnReporterSummary is the node reporter's bounded answer: the one host-side
// evidence the operator cannot see without it — the updates timer, apt's last
// attempt and last success, and Ubuntu's raw marker.
type pnReporterSummary struct {
	SchemaVersion int    `json:"schema_version"`
	CollectedAt   string `json:"collected_at"`
	UpdateTimer   struct {
		State string `json:"state"`
	} `json:"update_timer"`
	AptPeriodic struct {
		LastAttempt struct {
			State string `json:"state"`
			At    string `json:"at"`
		} `json:"last_attempt"`
		LastSuccess struct {
			State string `json:"state"`
			At    string `json:"at"`
		} `json:"last_success"`
	} `json:"apt_periodic"`
	Reboot struct {
		State string `json:"state"`
		Since string `json:"since"`
	} `json:"reboot_required"`
}

// pnReporter is one node's reporter pod: where it runs and where to reach it.
type pnReporter struct {
	node string
	ip   string
	port int
}

func pnReporters(t *testing.T, ctx context.Context, r k3s.Runner, namespace string) []pnReporter {
	t.Helper()
	out, err := k3s.Kubectl(ctx, r, fmt.Sprintf(`get pods -n %s -l app.kubernetes.io/component=node-reporter -o json`, namespace))
	if err != nil {
		return nil
	}
	var list struct {
		Items []struct {
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				PodIP string `json:"podIP"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("the node reporter pod list is not JSON: %v", err)
	}
	port := 9111
	if portOut, err := k3s.Kubectl(ctx, r, fmt.Sprintf(
		`get daemonset -n %s -l app.kubernetes.io/component=node-reporter -o jsonpath='{.items[0].spec.template.spec.containers[0].ports[0].containerPort}'`, namespace)); err == nil {
		if parsed, convErr := strconv.Atoi(strings.Trim(strings.TrimSpace(portOut), "'")); convErr == nil {
			port = parsed
		}
	}
	var reporters []pnReporter
	for _, item := range list.Items {
		if item.Status.PodIP == "" {
			continue
		}
		reporters = append(reporters, pnReporter{node: item.Spec.NodeName, ip: item.Status.PodIP, port: port})
	}
	return reporters
}

// pnReporterSummaryFor reads one node's reporter over the pod network FROM that
// node, which is how the operator reaches it too.
func pnReporterSummaryFor(t *testing.T, ctx context.Context, r k3s.Runner, reporter pnReporter) pnReporterSummary {
	t.Helper()
	out := pnHostRun(t, ctx, r, fmt.Sprintf("curl -s --max-time 5 http://%s:%d/", reporter.ip, reporter.port))
	var summary pnReporterSummary
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("the node reporter on %s answered something that is not its documented summary: %v (%s)", reporter.node, err, out)
	}
	return summary
}

// --- the outside watcher ----------------------------------------------------

// pnWatch samples the API server every two seconds and records, with the
// timestamp it observed, which nodes were cordoned or NotReady. The
// serialisation claim is about an interval, so it needs an observer that runs
// during the interval rather than two readings taken afterwards.
type pnWatch struct {
	mu        sync.Mutex
	samples   []string
	worst     int
	transits  []string
	seenBoots map[string]string
	stop      chan struct{}
	done      chan struct{}
}

func pnStartWatch(ctx context.Context, t *testing.T, r k3s.Runner) (*pnWatch, func()) {
	t.Helper()
	w := &pnWatch{seenBoots: map[string]string{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			states := pnReadNodes(t, ctx, r)
			if states == nil {
				continue
			}
			disruptive := pnDisruptive(states)
			w.mu.Lock()
			if len(disruptive) > w.worst {
				w.worst = len(disruptive)
			}
			w.samples = append(w.samples, fmt.Sprintf("%s: cordoned/NotReady=%v", time.Now().Format(time.RFC3339), disruptive))
			for name, state := range states {
				if previous, ok := w.seenBoots[name]; ok && previous != state.BootID {
					w.transits = append(w.transits, fmt.Sprintf("%s bootID %s → %s", name, previous, state.BootID))
				}
				w.seenBoots[name] = state.BootID
			}
			w.mu.Unlock()
		}
	}()
	return w, func() {
		close(w.stop)
		<-w.done
	}
}

func (w *pnWatch) report() (samples int, worst int, transitions []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.samples), w.worst, append([]string(nil), w.transits...)
}

func (w *pnWatch) transcript() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(append([]string(nil), w.samples...), "\n  ")
}

// --- teardown ---------------------------------------------------------------

func pnUninstallAll(t *testing.T, ctx context.Context, nodes []pnNode) {
	t.Helper()
	var installed []uninstall.Node
	for _, node := range nodes {
		role := uninstall.RoleServer
		if node.agent {
			role = uninstall.RoleAgent
		}
		installed = append(installed, uninstall.Node{Address: node.address, Role: role, Runner: node.runner})
	}
	if err := uninstallAll(ctx, t, installed); err != nil {
		t.Fatal(err)
	}
}

// pnNodeName is the Kubernetes node name for a lab address, which is the name
// every surface reports.
func pnNodeName(t *testing.T, ctx context.Context, r k3s.Runner) string {
	t.Helper()
	return pnHostRun(t, ctx, r, "hostname")
}

// pnLiveRecord reads the cluster's live operation record, or nil when it holds
// none.
func pnLiveRecord(t *testing.T, ctx context.Context, r k3s.Runner) *operation.Stored {
	t.Helper()
	store := &operation.Store{Runner: r, Operator: "gate@e2e"}
	live, err := store.Current(ctx)
	if err != nil {
		t.Fatalf("reading the operation record: %v", err)
	}
	return live
}

// --- the gate ---------------------------------------------------------------

func TestS3PatchNightGate(t *testing.T) {
	env := pnEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Hour)
	defer cancel()
	pnHome(t, env)

	client, err := api.New(env.controlPlane, api.WithToken(env.token))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("the three-node cluster is installed at the candidate bundle", func(t *testing.T) {
		bundle := fetchBundle(t, client, env.bundle)
		opts := install.Options{
			Bundle: env.bundle, Name: env.patchCluster, HATier: "single-server",
			Servers: []string{env.server}, Agents: []string{env.agent1, env.agent2},
			SSHUser: env.sshUser, SSHKey: env.sshKey,
		}
		s, _ := session(t, env.gateEnv, t.TempDir()+"/install.json", bundle, opts)
		defer s.Close()
		if _, err := install.Execute(ctx, s, install.Plan(s)); err != nil {
			t.Fatalf("installing the patch-night cluster at %s: %v", env.bundle, err)
		}
		t.Logf("[%s] installed %s at bundle %s: server %s, agents %s and %s",
			pnNow(), env.patchCluster, env.bundle, env.server, env.agent1, env.agent2)
	})
	if t.Failed() {
		return
	}

	nodes := pnConnect(t, env)
	server := nodes[0].runner
	t.Cleanup(func() {
		if os.Getenv("KUBENEST_GATE_KEEP") != "" {
			t.Logf("[%s] KUBENEST_GATE_KEEP is set: leaving the patch-night cluster installed", pnNow())
			return
		}
		pnUninstallAll(t, context.Background(), pnConnect(t, env))
	})

	orgID, clusterID := pnOrgIDAndCluster(ctx, client, env.patchCluster)
	if clusterID == "" {
		t.Fatalf("the cluster %s this test just installed is not registered to the control plane, so nothing it reports can be read", env.patchCluster)
	}

	// The workload S3 measures: two replicas, spread over the nodes, reachable
	// through a NodePort. It runs on the patch-night cluster because that is the
	// fleet whose nodes are going down.
	if err := kubectlApplyDoc(ctx, server, availabilityWorkload); err != nil {
		t.Fatalf("deploying the availability workload: %v", err)
	}
	readyDeadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(readyDeadline) {
		out, err := k3s.Kubectl(ctx, server,
			"get deployment always-up -n gate-availability -o jsonpath='{.status.readyReplicas}'")
		if err == nil && strings.Trim(strings.TrimSpace(out), "'") == "2" {
			t.Logf("[%s] the two-replica availability workload is ready and reachable", pnNow())
			break
		}
		time.Sleep(5 * time.Second)
	}

	probe := pollWorkload(t, server, []string{env.server + ":30080", env.agent1 + ":30080", env.agent2 + ":30080"})
	probeClosed := false
	probeAttempts, probeFailures := 0, []string(nil)
	stopProbe := func() {
		if probeClosed {
			return
		}
		probeClosed = true
		probeAttempts, probeFailures = probe.result()
	}
	defer stopProbe()

	// Step 1: the window opens in fifteen minutes, written with the REAL
	// command so T3.4's agreement rules run. All seven days are listed because
	// the six-hour window may cross local midnight in the fixture's zone, and a
	// crossing window is accepted only when it names all seven days.
	var opening time.Time
	// serverNodeName and serverRebootAt are shared with the manual-reboot
	// subtests, which assert the same fact from the health surface and from the
	// alert record.
	var serverNodeName string
	var serverRebootAt time.Time
	t.Run("the window is set to open in fifteen minutes", func(t *testing.T) {
		start := time.Now().In(env.zone).Add(15 * time.Minute).Truncate(time.Minute)
		end := start.Add(6 * time.Hour)
		out := pnSetWindow(t, env, env.patchCluster, weekdays(), start.Format("15:04"), end.Format("15:04"), env.zone.String())
		t.Logf("[%s] kubenest cluster set-window:\n%s", pnNow(), out)
		record := pnWindowRecord(t, ctx, client, clusterID)
		opening = pnWindowOpening(t, record, time.Now())
		if left := time.Until(opening); left < 8*time.Minute || left > 25*time.Minute {
			t.Fatalf("the stored window opens in %s, want about fifteen minutes: the shut-window observation depends on it", left.Round(time.Second))
		}
		if record.State == "" || record.State == api.WindowStateNone {
			t.Errorf("the control plane reports the stored window's state as %q; a stored window has a state and it is never collapsed with none", record.State)
		}
		t.Logf("[%s] window opens %s (%s), state %q at revision %v",
			pnNow(), opening.In(env.zone).Format("Mon 2 Jan 15:04 MST"), opening.UTC().Format(time.RFC3339), record.State, record.Revision)
	})

	// Step 2: every node needs a reboot, in the two ways the scenario names.
	// The node with a real pending update takes the distro's own path; every
	// other node gets the marker, which is the deterministic probe of the
	// reboot path.
	realPath := map[string]bool{}
	// markedBootIDs is the boot id every later subtest compares against: one
	// reading taken when the nodes were marked, so a reboot anywhere between
	// then and the assertion is visible.
	markedBootIDs := map[string]string{}
	t.Run("every node is made to need a reboot, one through unattended-upgrades and the rest by the marker", func(t *testing.T) {
		for _, node := range nodes {
			if len(realPath) == 0 {
				if exists, detail := pnUnattendedDryRun(t, ctx, node.runner); exists {
					t.Logf("[%s] %s has real updates: %s", pnNow(), node.address, detail)
					pnApplyUnattended(t, ctx, node.runner)
					if pnRebootPending(t, ctx, node.runner) {
						realPath[node.address] = true
						t.Logf("[%s] %s needs a reboot, written by the distro's own path after unattended-upgrades", pnNow(), node.address)
						continue
					}
					t.Logf("[%s] %s applied its updates but the distro wrote no reboot marker", pnNow(), node.address)
				}
			}
			pnTouchMarker(t, ctx, node.runner)
			t.Logf("[%s] %s needs a reboot through /var/run/reboot-required", pnNow(), node.address)
		}
		if len(realPath) == 0 {
			t.Errorf("no node took the real unattended-upgrades path: the fixture image has no pending update, so the path the scenario names was not exercised. Provision a host with a pending security update (a kernel or libc update is the usual one)")
		}
		if len(realPath) == len(nodes) {
			t.Errorf("every node took the real path, so the deterministic marker path was never the reason a node needed a reboot")
		}
		for _, node := range nodes {
			if !pnRebootPending(t, ctx, node.runner) {
				t.Errorf("%s does not carry /var/run/reboot-required after it was applied or created", node.address)
			}
			markedBootIDs[node.address] = pnBootID(t, ctx, node.runner)
		}
	})

	// Planted negative (b): while a NEW window is applying, every node is held
	// and says so, and nothing reboots. The revision genuinely changes because
	// the opening moves, which is why this runs BEFORE the shut-window
	// observation below.
	//
	// The scenario lists the window-plants in the other order. It is reversed
	// here for one reason: the observation below needs ten minutes with the
	// window SHUT, and the step-1 window opens fifteen minutes after it was
	// written, which the marking step may have spent. Rewriting the window here
	// — twenty minutes out, so the observation and the health check fit — makes
	// the shut-window claim true by construction rather than by luck.
	t.Run("a new window holds every node while it applies", func(t *testing.T) {
		before := map[string]string{}
		for _, node := range nodes {
			before[node.address] = pnBootID(t, ctx, node.runner)
		}
		start := time.Now().In(env.zone).Add(20 * time.Minute).Truncate(time.Minute)
		end := start.Add(6 * time.Hour)
		out := pnSetWindow(t, env, env.patchCluster, weekdays(), start.Format("15:04"), end.Format("15:04"), env.zone.String())
		t.Logf("[%s] changed the window:\n%s", pnNow(), out)

		// Read the window's state and the hold labels TOGETHER, and pass as soon
		// as ONE sample has BOTH a non-active revision and every node held. The
		// rollout is asynchronous — the write returns before the operator has
		// labelled anything — so a sample where the labels are not there yet is
		// only kept as evidence of what was seen, never failed on.
		var observations []string
		sawHeld := false
		sawNotActive := false
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) && !sawHeld {
			record := pnWindowRecord(t, ctx, client, clusterID)
			states := pnReadNodes(t, ctx, server)
			if len(states) == 0 {
				time.Sleep(time.Second)
				continue
			}
			held := 0
			var unheld []string
			for name, state := range states {
				if state.HasAutoRebootKey && state.AutoRebootLabel == "false" {
					held++
				} else {
					unheld = append(unheld, name)
				}
			}
			observations = append(observations, fmt.Sprintf("%s state=%s held=%d/%d unheld=%v",
				time.Now().Format(time.RFC3339), record.State, held, len(states), unheld))
			if record.State != api.WindowStateActive {
				sawNotActive = true
				if held == len(states) {
					sawHeld = true
					// "and health says so", read at the same instant the labels
					// are: T3.6's per-node evidence carries auto_reboot, and a
					// held node must not be reported as one that reboots by
					// itself. When the surface does not carry the field this is
					// logged rather than passed, because a silent skip reads as
					// a pass.
					view := pnClusterHealth(t, ctx, env, clusterID)
					if check, ok := view.check("os_patching"); ok {
						entries, _ := check.Detail["nodes"].([]any)
						carried := false
						for _, item := range entries {
							entry, ok := item.(map[string]any)
							if !ok {
								continue
							}
							value, present := entry["auto_reboot"]
							if !present {
								continue
							}
							carried = true
							if auto, ok := value.(bool); !ok || auto {
								t.Errorf("health reports %v auto_reboot=%v while the window is applying: a held node must not be reported as one kured may reboot",
									entry["name"], value)
							}
						}
						if carried {
							t.Logf("[%s] health reports auto_reboot=false for every node while the window is applying", pnNow())
						} else {
							t.Logf("[%s] the os_patching detail does not carry auto_reboot on this surface, so the node label is what proves the hold", pnNow())
						}
					}
				}
			}
			time.Sleep(time.Second)
		}
		if !sawHeld {
			explain := ""
			if !sawNotActive {
				explain = " (the window was also never seen as stored/applying)"
			}
			t.Errorf("no sample showed every node held while the new window was not yet active%s: kured would be free to reboot a node before the configuration it must obey is in place\n  %s",
				explain, strings.Join(observations, "\n  "))
		}
		for _, node := range nodes {
			if got := pnBootID(t, ctx, node.runner); got != before[node.address] {
				t.Errorf("%s's boot id changed (%s → %s) while the new window was being applied", node.address, before[node.address], got)
			}
		}
		// The new configuration must actually reach the cluster before the
		// shut-window observation below: until it does, the PREVIOUS window is
		// still the one kured obeys, and that one may open sooner.
		active := false
		for activeDeadline := time.Now().Add(3 * time.Minute); time.Now().Before(activeDeadline); {
			if pnWindowRecord(t, ctx, client, clusterID).State == api.WindowStateActive {
				active = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !active {
			t.Fatalf("the control plane never reported the new window active, so the cluster is still obeying the previous one: %s", strings.Join(observations, "\n  "))
		}
		opening = pnWindowOpening(t, pnWindowRecord(t, ctx, client, clusterID), time.Now())
		t.Logf("[%s] the new window is active and opens at %s (%s)",
			pnNow(), opening.In(env.zone).Format("Mon 2 Jan 15:04 MST"), opening.UTC().Format(time.RFC3339))
	})

	// Step 3: while the window has NOT opened, every node's reboot state is
	// visible in fleet health with the time it was FIRST seen and with the
	// updates timer's last attempt and last success, and no boot ID changes.
	t.Run("before the window opens, fleet health carries each node's pending state with the time it was first seen", func(t *testing.T) {
		// Two report intervals plus slack: one report can be in flight while
		// another is written, and a missing one is not evidence of a stopped
		// producer.
		time.Sleep(150 * time.Second)

		view := pnClusterHealth(t, ctx, env, clusterID)
		detail, check := pnPatchDetail(t, view)
		if check.Status == "unknown" {
			t.Errorf("the patch evidence reads unknown (%s): %s — an unread producer is never a green fleet", check.ReasonCode, check.Message)
		}
		byName := map[string]pnPatchNode{}
		for _, node := range detail {
			byName[node.Name] = node
		}
		for _, node := range nodes {
			name := pnNodeName(t, ctx, node.runner)
			entry, ok := byName[name]
			if !ok {
				t.Errorf("health's os_patching evidence names no entry for %s, so that host's patch state is invisible", name)
				continue
			}
			if entry.Status == "unknown" {
				t.Errorf("health reports %s unknown: %s", name, entry.Reason)
			}
		}
		t.Logf("[%s] health carries %d node(s) of patch evidence, check %q status %q, received %v",
			pnNow(), len(detail), check.ReasonCode, check.Status, view.ReceivedAt)

		// The time the reboot was FIRST seen lives in the operator's own
		// bookkeeping, which is what survives an operator restart.
		namespace := pnOperatorNamespace(t, ctx, server)
		if namespace == "" {
			t.Fatalf("no node reporter DaemonSet is running, so the T3.6 producer is not installed: the fixture must run a bundle that ships it")
		}
		book := pnBookkeepingRead(t, ctx, server, namespace)
		seen := map[string]string{}
		for _, entry := range book.Nodes {
			seen[entry.Name] = entry.RebootPendingSince
		}
		for _, node := range nodes {
			name := pnNodeName(t, ctx, node.runner)
			since := seen[name]
			if since == "" {
				t.Errorf("the operator's bookkeeping has no first-seen instant for %s, so how long it has been waiting cannot be answered", name)
				continue
			}
			at, err := time.Parse(time.RFC3339, since)
			if err != nil {
				t.Errorf("the first-seen instant for %s is %q, which is not RFC3339", name, since)
				continue
			}
			if age := time.Since(at); age < 0 || age > 2*time.Hour {
				t.Errorf("the first-seen instant for %s is %s (age %s): the marker was created minutes ago", name, since, age.Round(time.Second))
			}
			t.Logf("[%s] %s reboot first seen pending at %s", pnNow(), name, at.Format(time.RFC3339))
		}

		// The distro's own last attempt and last success, which only the host
		// reporter can see.
		reporters := pnReporters(t, ctx, server, namespace)
		if len(reporters) == 0 {
			t.Fatalf("no node reporter pod is running in %s, so no host's updates-timer evidence can be read", namespace)
		}
		byHost := map[string]pnReporter{}
		for _, reporter := range reporters {
			byHost[reporter.node] = reporter
		}
		for _, node := range nodes {
			name := pnNodeName(t, ctx, node.runner)
			reporter, ok := byHost[name]
			if !ok {
				t.Errorf("no node reporter pod runs on %s, so its updates timer cannot be judged", name)
				continue
			}
			summary := pnReporterSummaryFor(t, ctx, node.runner, reporter)
			if summary.AptPeriodic.LastAttempt.State != "known" || summary.AptPeriodic.LastAttempt.At == "" {
				t.Errorf("%s reports no readable last-attempt stamp (%q)", name, summary.AptPeriodic.LastAttempt.State)
			}
			if summary.AptPeriodic.LastSuccess.State != "known" || summary.AptPeriodic.LastSuccess.At == "" {
				t.Errorf("%s reports no readable last-success stamp (%q): a host that never patched looks like this and must not read healthy", name, summary.AptPeriodic.LastSuccess.State)
			}
			if summary.UpdateTimer.State != "enabled" {
				t.Errorf("%s reports its updates timer %q: nothing schedules updates there, which is the state this component exists to surface", name, summary.UpdateTimer.State)
			}
			if summary.Reboot.State != "pending" {
				t.Errorf("%s reports reboot_required %q while the marker is present", name, summary.Reboot.State)
			}
			t.Logf("[%s] %s apt last attempt %s, last success %s, timer %s, reboot %s since %s",
				pnNow(), name, summary.AptPeriodic.LastAttempt.At, summary.AptPeriodic.LastSuccess.At,
				summary.UpdateTimer.State, summary.Reboot.State, summary.Reboot.Since)
		}

		for _, node := range nodes {
			if got := pnBootID(t, ctx, node.runner); got != markedBootIDs[node.address] {
				t.Errorf("%s's boot id changed (%s → %s) while the window was shut", node.address, markedBootIDs[node.address], got)
			}
		}
	})

	// Planted negative (a): with every node marked and the window shut, the
	// scenario's ten minutes pass and NOTHING reboots.
	t.Run("outside the window nothing reboots even with the marker present", func(t *testing.T) {
		observe := env.outside
		if left := time.Until(opening) - 3*time.Minute; left < observe {
			observe = left
		}
		if observe < 3*time.Minute {
			t.Fatalf("only %s is left before the window opens, which is less than the report intervals plus slack this observation needs: the fixture's own steps took too long", observe.Round(time.Second))
		}
		if observe < env.outside {
			t.Logf("[%s] the shut-window observation is clamped to %s (the scenario asks for %s) so it finishes before the opening",
				pnNow(), observe.Round(time.Second), env.outside)
		}
		startedAt := time.Now()
		for time.Now().Before(startedAt.Add(observe)) {
			if !pnRebootPending(t, ctx, nodes[0].runner) {
				t.Errorf("the marker disappeared on %s while the window was shut", nodes[0].address)
			}
			time.Sleep(20 * time.Second)
		}
		for _, node := range nodes {
			if got := pnBootID(t, ctx, node.runner); got != markedBootIDs[node.address] {
				t.Errorf("%s rebooted (%s → %s) with the window shut and the marker present: that is the exact case the window exists to prevent",
					node.address, markedBootIDs[node.address], got)
			}
		}
		view := pnClusterHealth(t, ctx, env, clusterID)
		detail, _ := pnPatchDetail(t, view)
		t.Logf("[%s] after %s with the window shut, %d node(s) still show patch evidence and no boot id changed",
			pnNow(), observe.Round(time.Second), len(detail))
	})

	// Steps 5 and 6: the window opens, the two agents reboot ONE AT A TIME, and
	// through all of it the workload stays inside its limits.
	watch, stopWatch := pnStartWatch(ctx, t, server)
	t.Run("when the window opens the agents reboot one at a time", func(t *testing.T) {
		if wait := time.Until(opening); wait > 0 {
			t.Logf("[%s] waiting %s for the window to open", pnNow(), wait.Round(time.Second))
			select {
			case <-time.After(wait + 30*time.Second):
			case <-ctx.Done():
				t.Fatal("the test's deadline passed while waiting for the window")
			}
		}
		agentBootBefore := map[string]string{}
		for _, node := range nodes[1:] {
			agentBootBefore[node.address] = pnBootID(t, ctx, node.runner)
		}
		// kured's period is the chart's an-hour default, so the reboot can take
		// up to a period after the opening; the six-hour window keeps the wait
		// inside it. While an agent is DOWN its boot id cannot be read at all,
		// and an unreadable host is "keep waiting", never a value — which is
		// why the polling uses the tolerant read.
		deadline := opening.Add(75 * time.Minute)
		seen := map[string]string{}
		for time.Now().Before(deadline) {
			done := true
			for _, node := range nodes[1:] {
				got := pnBootIDOrEmpty(ctx, node.runner)
				if got != "" && got != agentBootBefore[node.address] {
					seen[node.address] = got
					continue
				}
				done = false
			}
			if done {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("the test's deadline passed while waiting for the agents to reboot")
			}
			time.Sleep(15 * time.Second)
		}
		for _, node := range nodes[1:] {
			after := seen[node.address]
			if after == "" {
				t.Errorf("agent %s's boot id has not changed %s after the window opened: kured either never got the window or never saw the marker",
					node.address, time.Since(opening).Round(time.Minute))
				continue
			}
			t.Logf("[%s] agent %s rebooted (boot id %s → %s)", pnNow(), node.address, agentBootBefore[node.address], after)
			if pnRebootPending(t, ctx, node.runner) {
				t.Errorf("agent %s still carries the reboot marker after rebooting: its pending state did not clear", node.address)
			}
		}
		// The serialisation, from the outside: the watcher sampled the API
		// server every two seconds throughout.
		samples, worst, transitions := watch.report()
		t.Logf("[%s] the watcher took %d samples; the most cordoned/NotReady at once was %d", pnNow(), samples, worst)
		if samples < 30 {
			t.Errorf("only %d samples over the agent reboots: too few to claim anything about serialisation", samples)
		}
		if worst > 1 {
			t.Errorf("TWO OR MORE NODES WERE CORDONED OR NOTREADY AT ONCE (%d): the reboots were not serialised\n  %s", worst, watch.transcript())
		}
		if len(transitions) < 2 {
			t.Errorf("the watcher saw %d boot-id change(s) through the API server, want one per rebooted agent: %v", len(transitions), transitions)
		}
		for _, transition := range transitions {
			t.Logf("[%s] %s", pnNow(), transition)
		}
	})
	stopWatch()
	stopProbe()

	t.Run("the workload stayed within its limits, reported separately from the Kubernetes API's reading", func(t *testing.T) {
		t.Logf("[%s] the outside probe polled every node's NodePort once a second with retries off: %d attempts over the patch night",
			pnNow(), probeAttempts)
		if probeAttempts < 60 {
			t.Errorf("only %d probes: too few to claim anything about availability", probeAttempts)
		}
		if len(probeFailures) > 0 {
			t.Errorf("THE WORKLOAD WAS UNAVAILABLE during the patch night: %d of %d probes failed:\n  %s",
				len(probeFailures), probeAttempts, strings.Join(probeFailures, "\n  "))
		}
		// Reported SEPARATELY, never merged with the outside measurement: a node
		// answering from its own API is not the same claim as a customer
		// reaching the workload.
		out, err := k3s.Kubectl(ctx, server, "get deployment always-up -n gate-availability -o jsonpath='{.status.readyReplicas}'")
		if err != nil {
			t.Fatalf("reading the workload from the Kubernetes API: %v", err)
		}
		t.Logf("[%s] the Kubernetes API separately reports %q ready replicas after the patch night",
			pnNow(), strings.Trim(strings.TrimSpace(out), "'"))
	})

	// Step 6's second half: no NODE_NOT_READY alert fired for a planned reboot.
	// T2.7's grace split is what makes that true, and the alert record is where
	// it is read.
	t.Run("no NODE_NOT_READY alert fired for a planned reboot", func(t *testing.T) {
		alerts := pnAlerts(t, ctx, env, orgID)
		var offending []pnAlert
		for _, alert := range alerts {
			if alert.ClusterID != clusterID || alert.Check != "nodes" || alert.ReasonCode != "NODE_NOT_READY" {
				continue
			}
			if alert.FirstSeenAt.Before(opening.Add(-2 * time.Minute)) {
				continue
			}
			offending = append(offending, alert)
		}
		for _, alert := range offending {
			t.Errorf("a NODE_NOT_READY alert fired at %s: %s", alert.FirstSeenAt.Format(time.RFC3339), alert.Message)
		}
		t.Logf("[%s] %d alert(s) in the org, none a NODE_NOT_READY for %s since the window opened", pnNow(), len(alerts), env.patchCluster)
	})

	// Step 7: servers are NOT rebooted automatically. The server is still held,
	// still needs a reboot, and it waits for an explicit --confirm.
	t.Run("the server is not rebooted automatically and waits for an explicit decision", func(t *testing.T) {
		states := pnReadNodes(t, ctx, server)
		serverNode := pnNodeName(t, ctx, server)
		serverNodeName = serverNode
		state, ok := states[serverNode]
		if !ok {
			t.Fatalf("the server node %s is not in the cluster's node list", serverNode)
		}
		if !state.HasAutoRebootKey || state.AutoRebootLabel != "false" {
			t.Errorf("the server %s does not carry kubenest.io/auto-reboot=false: a server must never reboot by itself in 1.2", serverNode)
		}
		if !pnRebootPending(t, ctx, server) {
			t.Errorf("the server %s no longer carries the reboot marker, so there is nothing left to reboot manually", env.server)
		}
		if got := pnBootID(t, ctx, server); got != markedBootIDs[env.server] {
			t.Errorf("the server rebooted by itself (%s → %s): servers are not rebooted automatically", markedBootIDs[env.server], got)
		}
		for _, name := range pnKuredPodNodes(t, ctx, server) {
			if name == serverNode {
				t.Errorf("a kured pod is running on the held server %s, so the hold is not in force", serverNode)
			}
		}
		// Without --confirm the verb prints the plan and changes nothing.
		before := pnBootID(t, ctx, server)
		var out bytes.Buffer
		err := pnRunCLI(&out, "node", "reboot", "--cluster", env.patchCluster, "--node", env.server,
			"--ssh-user", env.sshUser, "--ssh-key", env.sshKey)
		if err == nil {
			t.Errorf("the server reboot proceeded with no --confirm:\n%s", out.String())
		} else if !strings.Contains(out.String(), "--confirm") && !strings.Contains(err.Error(), "--confirm") {
			t.Errorf("the refusal does not name --confirm, which is the explicit decision this verb waits for: %v\n%s", err, out.String())
		}
		if after := pnBootID(t, ctx, server); after != before {
			t.Errorf("a run with no --confirm changed the boot id (%s → %s)", before, after)
		}
		t.Logf("[%s] server %s: reboot-required present, kubenest.io/auto-reboot=false, boot id unchanged — reboot pending, manual",
			pnNow(), serverNode)
	})

	t.Run("the server reboot waits from outside and reports Ready", func(t *testing.T) {
		before := pnBootID(t, ctx, server)
		serverRebootAt = time.Now()
		// Watch the health surface for the planned-reboot marker WHILE the
		// platform reboots the node: the marker is what tells the backend to
		// apply the planned-reboot grace instead of the short not-ready one,
		// and it exists only for the duration of the reboot.
		stopMarker := make(chan struct{})
		markerDone := make(chan struct{})
		var markerMu sync.Mutex
		var markerSights []string
		var markerSurface bool
		go func() {
			defer close(markerDone)
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopMarker:
					return
				case <-ticker.C:
				}
				var view pnHealthView
				if err := pnGetJSONQuiet(ctx, env, "/api/v1/clusters/"+url.PathEscape(clusterID)+"/health", &view); err != nil {
					continue
				}
				check, ok := view.check("planned_reboot")
				if !ok {
					continue
				}
				markerMu.Lock()
				markerSurface = true
				if names, ok := check.Detail["in_planned_reboot"].([]any); ok {
					for _, name := range names {
						markerSights = append(markerSights, fmt.Sprintf("%s %s", time.Now().Format(time.RFC3339), fmt.Sprint(name)))
					}
				}
				markerMu.Unlock()
			}
		}()

		var out bytes.Buffer
		started := time.Now()
		err := pnRunCLI(&out, "node", "reboot", "--cluster", env.patchCluster, "--node", env.server,
			"--ssh-user", env.sshUser, "--ssh-key", env.sshKey, "--confirm", "--wait")
		elapsed := time.Since(started)
		close(stopMarker)
		<-markerDone
		output := out.String()
		if err != nil {
			t.Fatalf("the manual server reboot failed after %s: %v\n%s", elapsed.Round(time.Second), err, output)
		}
		if after := pnBootID(t, ctx, server); after == before {
			t.Errorf("the boot id did not change (%s): the server was not rebooted", after)
		}
		for _, want := range []string{"SSH reachable", "the k3s service active", "the cluster API answering with the node Ready"} {
			if !strings.Contains(output, want) {
				t.Errorf("the manual reboot never reported %q, so the wait from outside is not visible:\n%s", want, output)
			}
		}
		live := pnLiveRecord(t, ctx, server)
		if live == nil {
			t.Fatal("no operation record is on the cluster after the manual server reboot")
		}
		if live.Record.Request.Kind != operation.KindNodeReboot {
			t.Errorf("the record is about %q, want %q", live.Record.Request.Kind, operation.KindNodeReboot)
		}
		if !live.Record.Terminal {
			t.Error("the manual reboot left its operation record live")
		}

		// T2.7's marker is what makes this reboot "planned" to the backend. It
		// is asserted when the surface carries the check, and its absence is a
		// failure that names the missing writer rather than a quiet pass.
		markerMu.Lock()
		sights, surface := append([]string(nil), markerSights...), markerSurface
		markerMu.Unlock()
		if surface {
			named := false
			for _, sight := range sights {
				if strings.Contains(sight, serverNodeName) {
					named = true
				}
			}
			if !named {
				t.Errorf("the health surface never named %s as being in a planned reboot while the platform rebooted it: without that marker the backend applies the SHORT not-ready grace and alerts on a reboot the platform ordered. `kubenest node reboot` must write the planned-reboot marker (T3.5, pkg/cmd/node_reboot.go)", serverNodeName)
			} else {
				t.Logf("[%s] health named %s in a planned reboot during the manual reboot: %v", pnNow(), serverNodeName, sights)
			}
		} else {
			t.Logf("[%s] the health surface carries no planned_reboot check (the bundle predates T2.7's grace key), so only the alert record can show this reboot was planned", pnNow())
		}
		t.Logf("[%s] the server rebooted in %s, waited from outside over SSH and reported Ready", pnNow(), elapsed.Round(time.Second))
	})

	// The other half of step 6's assertion, for the MANUAL path: a planned
	// reboot the platform ordered must not be reported as a dead node.
	t.Run("no NODE_NOT_READY alert fired for the server's planned manual reboot", func(t *testing.T) {
		if serverRebootAt.IsZero() {
			t.Skip("the manual server reboot did not run")
		}
		alerts := pnAlerts(t, ctx, env, orgID)
		var offending []pnAlert
		for _, alert := range alerts {
			if alert.ClusterID != clusterID || alert.Check != "nodes" || alert.ReasonCode != "NODE_NOT_READY" {
				continue
			}
			if alert.FirstSeenAt.Before(serverRebootAt.Add(-time.Minute)) {
				continue
			}
			if !strings.Contains(alert.Message, serverNodeName) {
				continue
			}
			offending = append(offending, alert)
		}
		for _, alert := range offending {
			t.Errorf("a NODE_NOT_READY alert fired at %s for the server %s the platform was rebooting on purpose: %s — a planned reboot must not read as a dead node (T2.7's grace split, and the marker `kubenest node reboot` has to write, T3.5)",
				alert.FirstSeenAt.Format(time.RFC3339), serverNodeName, alert.Message)
		}
		t.Logf("[%s] %d alert(s) in the org, none a NODE_NOT_READY for %s after its manual reboot started at %s",
			pnNow(), len(alerts), serverNodeName, serverRebootAt.Format(time.RFC3339))
	})

	// Planted negative (c): a window whose meaning the CLI, the backend and
	// kured would disagree on is refused at set-window, and nothing on the
	// cluster moves.
	t.Run("a midnight-crossing window that does not list all seven days is refused and changes nothing", func(t *testing.T) {
		before := pnHelmChartConfigValues(t, ctx, server)
		if before == "" {
			t.Skip("the kured HelmChartConfig is not readable from the cluster, so 'the refused window changed nothing' cannot be shown rather than assumed")
		}
		var out bytes.Buffer
		err := pnRunCLI(&out, "cluster", "set-window", "--cluster", env.patchCluster,
			"--days", "mon,tue,wed,thu,fri,sat", "--start", "22:00", "--end", "04:00", "--timezone", env.zone.String())
		if err == nil {
			t.Fatalf("a midnight-crossing window that does not list all seven days was accepted:\n%s", out.String())
		}
		refusal := err.Error() + out.String()
		for _, want := range []string{"seven days", "do not cross midnight"} {
			if !strings.Contains(refusal, want) {
				t.Errorf("the refusal does not name %q, one of the two ways to write a window that works:\n%s", want, refusal)
			}
		}
		if after := pnHelmChartConfigValues(t, ctx, server); after != before {
			t.Errorf("a refused window changed the kured HelmChartConfig: %q → %q", before, after)
		}
		t.Logf("[%s] the crossing window was refused before anything was written", pnNow())
	})

	// Lane A: the single-server cluster the harness already names. Its absence
	// is a skip, not an install — a second single-server cluster here would be a
	// different fixture.
	t.Run("lane A's single-server server is held and reboots through the same manual verb", func(t *testing.T) {
		_, laneClusterID := pnOrgIDAndCluster(ctx, client, env.gateEnv.cluster)
		if laneClusterID == "" {
			t.Skipf("the lane-A cluster %q is not registered to this control plane: bring lane A up and register it rather than installing a second single-server cluster here", env.gateEnv.cluster)
		}
		record, err := client.BundleRecord(ctx, laneClusterID)
		if err != nil {
			t.Fatalf("reading lane A's record: %v", err)
		}
		var laneHost *api.HostRecord
		for i := range record.Hosts {
			if record.Hosts[i].Role == "server" {
				laneHost = &record.Hosts[i]
				break
			}
		}
		if laneHost == nil || laneHost.SSHAddress == "" {
			t.Skipf("lane A's record carries no server host with an SSH address, so its server cannot be reached from here")
		}
		lane := pnDial(t, env, laneHost.SSHAddress, envOr("KUBENEST_LAB_SSH_USER", laneHost.SSHUser))
		// A window that is open at any instant this gate runs: lane A's window
		// is not this gate's to define, and T3.1 is where the window's own
		// refusals are accepted.
		storeWindow(t, ctx, client, laneClusterID, api.MaintenanceWindow{Days: weekdays(), Start: "00:00", End: "23:59", Timezone: "UTC"})
		pnTouchMarker(t, ctx, lane)
		states := pnReadNodes(t, ctx, lane)
		laneNode := pnNodeName(t, ctx, lane)
		if state, ok := states[laneNode]; ok {
			if !state.HasAutoRebootKey || state.AutoRebootLabel != "false" {
				t.Errorf("lane A's single server %s does not carry kubenest.io/auto-reboot=false, so a server would be free to reboot by itself", laneNode)
			}
		} else {
			t.Errorf("lane A's server %s is not in its own node list", laneNode)
		}
		before := pnBootID(t, ctx, lane)
		var out bytes.Buffer
		started := time.Now()
		err = pnRunCLI(&out, "node", "reboot", "--cluster", env.gateEnv.cluster, "--node", laneHost.SSHAddress,
			"--ssh-user", env.sshUser, "--ssh-key", env.sshKey, "--confirm", "--wait")
		if err != nil {
			t.Fatalf("lane A's manual server reboot failed after %s: %v\n%s", time.Since(started).Round(time.Second), err, out.String())
		}
		if after := pnBootID(t, ctx, lane); after == before {
			t.Errorf("lane A's boot id did not change (%s): the host was not rebooted", after)
		}
		t.Logf("[%s] lane A's server %s rebooted in %s, waited from outside over SSH and reported Ready",
			pnNow(), laneNode, time.Since(started).Round(time.Second))
	})
}
