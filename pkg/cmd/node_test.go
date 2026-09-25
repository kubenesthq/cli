package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/bundles"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
)

// The fixtures below are the style pkg/cmd's other command tests use: a
// scripted SSH transport (pkg/component/componenttest/runner.go's pattern) and
// a fake control plane served by httptest (platform_test.go's windowServer).
//
// The one thing this file adds is a REAL compare-and-swap on the two objects
// node reboot writes through a resourceVersion — kured's DaemonSet and the
// operation record's ConfigMap. A fake that accepted every write would let a
// lost update pass as a success, which is the failure the interlock and the
// record exist to prevent.

const (
	testServerAddr = "10.0.3.7"
	testAgentAddr  = "10.0.3.8"
	testNodeName   = "prod-1-srv-1"
	testAgentNode  = "prod-1-agt-1"
	testCluster    = "prod-1"
)

func serverHost() api.HostRecord {
	return api.HostRecord{
		HostID: "h-srv", Role: "server", SSHAddress: testServerAddr, SSHPort: 22, SSHUser: "ubuntu",
		NodeUID: "uid-srv", HostKeyFingerprint: "SHA256:server", JoinAddress: testServerAddr,
		StorageDevice: "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive1", LifecycleState: "active",
	}
}

func agentHost() api.HostRecord {
	return api.HostRecord{
		HostID: "h-agt", Role: "agent", SSHAddress: testAgentAddr, SSHPort: 22, SSHUser: "ubuntu",
		NodeUID: "uid-agt", HostKeyFingerprint: "SHA256:agent", JoinAddress: "10.0.3.7",
		StorageDevice: "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive2", LifecycleState: "active",
	}
}

// testNode is one Node object the fake cluster reports.
type testNode struct {
	Name          string
	UID           string
	Addresses     []string
	Ready         bool
	Labels        map[string]string
	Unschedulable bool
}

func nodesJSON(t *testing.T, nodes ...testNode) string {
	t.Helper()
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		addresses := make([]map[string]any, 0, len(n.Addresses))
		for _, a := range n.Addresses {
			addresses = append(addresses, map[string]any{"type": "InternalIP", "address": a})
		}
		ready := "False"
		if n.Ready {
			ready = "True"
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": n.Name, "uid": n.UID, "labels": n.Labels},
			"spec":     map[string]any{"unschedulable": n.Unschedulable},
			"status": map[string]any{
				"addresses":  addresses,
				"conditions": []map[string]any{{"type": "Ready", "status": ready}},
			},
		})
	}
	raw, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// fakeRule is one scripted answer: the first rule whose substring matches wins.
type fakeRule struct {
	match string
	resp  func(command string) (sshx.Result, error)
}

func ok(stdout string) func(string) (sshx.Result, error) {
	return func(string) (sshx.Result, error) { return sshx.Result{Stdout: stdout}, nil }
}

func fail(exit int, stderr string) func(string) (sshx.Result, error) {
	return func(string) (sshx.Result, error) { return sshx.Result{ExitCode: exit, Stderr: stderr}, nil }
}

func transportErr(message string) func(string) (sshx.Result, error) {
	return func(string) (sshx.Result, error) { return sshx.Result{}, fmt.Errorf("%s", message) }
}

// fakeHost is one scripted machine.
type fakeHost struct {
	mu            sync.Mutex
	address       string
	fp            string
	nodeJSON      string
	ds            map[string]any
	dsRV          string
	unschedulable bool
	objects       map[string]map[string]any
	rules         []fakeRule
	commands      []string
	inputs        []string
	closes        int
	rv            int
}

func newFakeHost(address, fingerprint, nodes string) *fakeHost {
	h := &fakeHost{
		address:  address,
		fp:       fingerprint,
		nodeJSON: nodes,
		objects:  map[string]map[string]any{},
	}
	h.rv = 10
	h.dsRV = "10"
	h.ds = map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":            "kured",
			"namespace":       "kube-system",
			"resourceVersion": h.dsRV,
		},
	}
	return h
}

func (h *fakeHost) on(match string, resp func(string) (sshx.Result, error)) *fakeHost {
	h.rules = append(h.rules, fakeRule{match: match, resp: resp})
	return h
}

// prepend puts a rule in front of the scripted defaults, for a test that needs
// the OPPOSITE answer from one of them.
func (h *fakeHost) prepend(match string, resp func(string) (sshx.Result, error)) *fakeHost {
	h.rules = append([]fakeRule{{match: match, resp: resp}}, h.rules...)
	return h
}

func (h *fakeHost) HostKeyFingerprint() string { return h.fp }

func (h *fakeHost) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closes++
	return nil
}

func (h *fakeHost) Run(ctx context.Context, command string) (sshx.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, command)
	switch {
	case strings.Contains(command, "get daemonset -n kube-system kured -o json"):
		return h.jsonResult(h.ds)
	case strings.Contains(command, "get configmap kubenest-operation") &&
		strings.Contains(command, " -o json") && !strings.Contains(command, " -o jsonpath"):
		name := configMapNameIn(command)
		if obj, ok := h.objects[name]; ok {
			return h.jsonResult(obj)
		}
		return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "` + name + `" not found`}, nil
	case strings.Contains(command, "get nodes -o json"):
		return sshx.Result{Stdout: h.nodeJSON}, nil
	case strings.HasPrefix(command, "sudo -n k3s kubectl get node ") && strings.HasSuffix(command, " -o json"):
		// kured's lock records whether the node was already unschedulable when
		// it was taken (interlock.nodeUnschedulable), so this read has to
		// answer even though the ready probe below answers the readiness
		// question.
		out, _ := json.Marshal(map[string]any{"spec": map[string]any{"unschedulable": h.unschedulable}})
		return sshx.Result{Stdout: string(out)}, nil
	}
	for _, r := range h.rules {
		if strings.Contains(command, r.match) {
			return r.resp(command)
		}
	}
	return sshx.Result{}, nil
}

func (h *fakeHost) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commands = append(h.commands, command)
	h.inputs = append(h.inputs, string(raw))
	switch {
	case command == "sudo -n k3s kubectl replace -f -":
		return h.replaceDaemonSet(raw)
	case strings.HasSuffix(command, "replace -f - -o json"):
		return h.writeConfigMap(raw, false)
	case strings.HasSuffix(command, "create -f - -o json"):
		return h.writeConfigMap(raw, true)
	}
	for _, r := range h.rules {
		if strings.Contains(command, r.match) {
			return r.resp(command)
		}
	}
	return sshx.Result{}, nil
}

func (h *fakeHost) jsonResult(v any) (sshx.Result, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return sshx.Result{}, err
	}
	return sshx.Result{Stdout: string(raw)}, nil
}

// replaceDaemonSet is a compare-and-swap, exactly as the API server is one: a
// write carrying a resourceVersion the object has moved past is refused.
func (h *fakeHost) replaceDaemonSet(raw []byte) (sshx.Result, error) {
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return sshx.Result{ExitCode: 1, Stderr: "invalid document"}, nil
	}
	meta, _ := incoming["metadata"].(map[string]any)
	rv, _ := meta["resourceVersion"].(string)
	if rv != h.dsRV {
		return sshx.Result{ExitCode: 1, Stderr: `Error from server (Conflict): Operation cannot be fulfilled on daemonsets.apps "kured": the object has been modified; please apply your changes to the latest version and try again`}, nil
	}
	h.rv++
	h.dsRV = strconv.Itoa(h.rv)
	meta["resourceVersion"] = h.dsRV
	h.ds = incoming
	return h.jsonResult(h.ds)
}

// writeConfigMap is the record's create/replace, with the same precondition.
func (h *fakeHost) writeConfigMap(raw []byte, create bool) (sshx.Result, error) {
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return sshx.Result{ExitCode: 1, Stderr: "invalid document"}, nil
	}
	meta, _ := incoming["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if name == "" {
		return sshx.Result{ExitCode: 1, Stderr: "no name"}, nil
	}
	existing, found := h.objects[name]
	if create && found {
		return sshx.Result{ExitCode: 1, Stderr: `Error from server (AlreadyExists): configmaps "` + name + `" already exists`}, nil
	}
	if !create {
		if !found {
			return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "` + name + `" not found`}, nil
		}
		want, _ := meta["resourceVersion"].(string)
		have, _ := existing["metadata"].(map[string]any)["resourceVersion"].(string)
		if want != have {
			return sshx.Result{ExitCode: 1, Stderr: "Error from server (Conflict): the object has been modified"}, nil
		}
	}
	h.rv++
	meta["resourceVersion"] = strconv.Itoa(h.rv)
	incoming["metadata"] = meta
	h.objects[name] = incoming
	return h.jsonResult(incoming)
}

func configMapNameIn(command string) string {
	for _, field := range strings.Fields(command) {
		if strings.HasPrefix(field, "kubenest-operation") {
			return field
		}
	}
	return ""
}

// kuredLock returns the lock annotation kured's DaemonSet carries, if any.
func (h *fakeHost) kuredLock() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, _ := h.ds["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	s, _ := ann["weave.works/kured-node-lock"].(string)
	return s
}

func (h *fakeHost) hasCommand(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.commands {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

// mutations is every command that CHANGES something on the host or the cluster.
// A test that asserts "nothing was touched" asserts on this, not on the whole
// log: reads are how a plan is built.
func (h *fakeHost) mutations() []string {
	markers := []string{
		"systemctl reboot", "systemctl restart", "kubectl cordon", "kubectl drain",
		"kubectl uncordon", "kubectl label", "kubectl create", "kubectl replace", "kubectl delete",
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.commands {
		for _, m := range markers {
			if strings.Contains(c, m) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func (h *fakeHost) indexOf(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range h.commands {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

// liveRecordDoc reads the live operation record off the fake cluster, so a test
// asserts what the lock actually named rather than what it was asked to name.
func (h *fakeHost) liveRecordDoc(t *testing.T) map[string]any {
	t.Helper()
	h.mu.Lock()
	obj, ok := h.objects["kubenest-operation"]
	h.mu.Unlock()
	if !ok {
		t.Fatal("no operation record was created on the host")
	}
	data, _ := obj["data"].(map[string]any)
	raw, _ := data["record.json"].(string)
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("the record on the host is not a record: %v", err)
	}
	return doc
}

func (h *fakeHost) liveRecord(t *testing.T) operation.Record {
	t.Helper()
	raw, err := json.Marshal(h.liveRecordDoc(t))
	if err != nil {
		t.Fatal(err)
	}
	var rec operation.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (h *fakeHost) recordRequest(t *testing.T) operation.Request {
	t.Helper()
	return h.liveRecord(t).Request
}

func (h *fakeHost) recordField(t *testing.T, field string) any {
	t.Helper()
	return h.liveRecordDoc(t)[field]
}

// fakeDialer hands out the fake hosts and counts how many times the verb opened
// a connection — the observable behind "holds nothing while it waits" and "the
// re-checks happen before anything destructive".
type fakeDialer struct {
	mu    sync.Mutex
	hosts map[string]*fakeHost
	fail  func(call int) error
	calls int
	at    []time.Time
	clock func() time.Time
}

func (d *fakeDialer) dial(ctx context.Context, host api.HostRecord) (nodeTransport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.clock != nil {
		d.at = append(d.at, d.clock())
	}
	if d.fail != nil {
		if err := d.fail(d.calls); err != nil {
			return nil, err
		}
	}
	h, ok := d.hosts[host.SSHAddress]
	if !ok {
		return nil, fmt.Errorf("no fake host at %s", host.SSHAddress)
	}
	return h, nil
}

func (d *fakeDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *fakeDialer) first() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.at) == 0 {
		return time.Time{}
	}
	return d.at[0]
}

// fakeClock is the run's clock: it advances only when the run sleeps, so a
// twenty-minute timeout is measured rather than endured.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept time.Duration
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.t = c.t.Add(d)
		c.slept += d
	}
	return nil
}

// rebootControlPlane serves the four routes node reboot reads: the cluster it
// resolves the name through, the record (the host inventory), the bundle
// manifest, and the maintenance window.
func rebootControlPlane(t *testing.T, hosts []api.HostRecord, window string) *api.Client {
	t.Helper()
	raw, err := bundles.Raw("1.1")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return rebootControlPlaneWithManifest(t, hosts, window, doc)
}

func rebootControlPlaneWithManifest(t *testing.T, hosts []api.HostRecord, window string, doc map[string]any) *api.Client {
	t.Helper()
	manifestJSON, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	bundle := api.ClusterBundle{BundleVersion: "1.1", Profiles: []string{}, HATier: "single-server", Hosts: hosts, Revision: 3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/orgs":
			io.WriteString(w, `[{"id":"o1","name":"acme","slug":"acme"}]`)
		case "/api/v1/orgs/o1/clusters":
			io.WriteString(w, `{"data":[{"id":"c1","name":"`+testCluster+`","status":"connected","org_id":"o1"}],"has_more":false}`)
		case "/api/v1/clusters/c1/bundle":
			json.NewEncoder(w).Encode(bundle)
		case "/api/v1/bundles/1.1":
			w.Write(manifestJSON)
		case "/api/v1/clusters/c1/maintenance-window":
			io.WriteString(w, window)
		default:
			// The record's mirror posts to a route this fixture does not
			// serve; the tests build their store without a mirror, so a request
			// here is a wiring mistake worth failing on.
			t.Errorf("unexpected control-plane request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// manifestWithTimeouts is the real bundle 1.1 manifest with the two timeouts a
// test varies. Reading the shipped manifest rather than a hand-written fixture
// is what makes "the timeout comes from the manifest" an assertion about the
// manifest the CLI actually installs from.
func manifestWithTimeouts(t *testing.T, drain, reboot string) map[string]any {
	t.Helper()
	raw, err := bundles.Raw("1.1")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	limits, _ := doc["limits"].(map[string]any)
	timeouts, _ := limits["timeouts"].(map[string]any)
	if timeouts == nil {
		t.Fatal("the bundle manifest has no limits.timeouts")
	}
	if drain != "" {
		timeouts["node-drain"] = drain
	}
	if reboot != "" {
		timeouts["node-reboot"] = reboot
	}
	return doc
}

func windowRecord(days []string, start, end, zone string) string {
	payload := map[string]any{
		"window":           map[string]any{"days": days, "start": start, "end": end, "timezone": zone},
		"revision":         4,
		"state":            "active",
		"applied_revision": 4,
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

// rebootFixture is one assembled run: the control plane, the fake hosts, the
// clock and the dialer.
type rebootFixture struct {
	verb   *nodeReboot
	out    *bytes.Buffer
	client *api.Client
	clock  *fakeClock
	dialer *fakeDialer
	server *fakeHost
	target *fakeHost
}

type rebootFixtureOpts struct {
	flags    NodeRebootFlags
	hosts    []api.HostRecord
	window   string
	server   *fakeHost
	target   *fakeHost
	manifest map[string]any
	clock    *fakeClock
	gates    []rebootGate
	drain    string
	reboot   string
}

// newRebootFixture wires a run the way the command does, with the host it acts
// on reachable only through the fake transport.
func newRebootFixture(t *testing.T, opts rebootFixtureOpts) *rebootFixture {
	t.Helper()
	if opts.flags.Cluster == "" {
		opts.flags.Cluster = testCluster
	}
	if opts.window == "" {
		// A window that is open at the fake clock's instant.
		opts.window = windowRecord([]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, "00:00", "23:59", "UTC")
	}
	if opts.clock == nil {
		opts.clock = newFakeClock(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	}
	client := rebootControlPlaneWithManifest(t, opts.hosts, opts.window,
		firstNonNil(opts.manifest, manifestWithTimeouts(t, opts.drain, opts.reboot)))

	dialer := &fakeDialer{hosts: map[string]*fakeHost{}, clock: opts.clock.now}
	if opts.server != nil {
		dialer.hosts[opts.server.address] = opts.server
	}
	if opts.target != nil && opts.target != opts.server {
		dialer.hosts[opts.target.address] = opts.target
	}

	out := &bytes.Buffer{}
	sleep := opts.clock.sleep
	n := &nodeReboot{
		out:    out,
		f:      opts.flags,
		client: client,
		now:    opts.clock.now,
		sleep:  sleep,
		poll:   time.Second,
		dial:   dialer.dial,
		gates:  opts.gates,
		store: func(runner k3s.Runner) *operation.Store {
			return &operation.Store{Runner: runner, Operator: "test@laptop"}
		},
	}
	return &rebootFixture{verb: n, out: out, client: client, clock: opts.clock, dialer: dialer, server: opts.server, target: opts.target}
}

func firstNonNil(v map[string]any, other map[string]any) map[string]any {
	if v != nil {
		return v
	}
	return other
}

// runFixture runs the verb and returns the output and the error.
func (f *rebootFixture) run(t *testing.T) (string, error) {
	t.Helper()
	err := f.verb.run(context.Background())
	return f.out.String(), err
}

// permissiveGates returns three gates that pass and count their runs, so a test
// can show that every check ran even when one refused.
func permissiveGates(ran *[]string) []rebootGate {
	return fakeGates(func(name string) (bool, string, string) { return true, name + " observed", "" }, ran)
}

func fakeGates(verdict func(name string) (bool, string, string), ran *[]string) []rebootGate {
	names := []string{gateQuorum, gateStorage, gateRecoveryPoint}
	out := make([]rebootGate, 0, len(names))
	for _, name := range names {
		name := name
		out = append(out, rebootGate{Name: name, Check: func(context.Context) (bool, string, string) {
			*ran = append(*ran, name)
			return verdict(name)
		}})
	}
	return out
}

// scriptedServer is the fake host the CLI reads and writes the cluster through:
// it answers the record, the interlock and the node reads, and every mutating
// command node reboot issues.
func scriptedServer(t *testing.T, nodes ...testNode) *fakeHost {
	t.Helper()
	h := newFakeHost(testServerAddr, "SHA256:server", nodesJSON(t, nodes...))
	h.on("kubectl get node "+testNodeName+` -o jsonpath`, ok("True\n"))
	h.on("jsonpath", ok("True\n"))
	h.on("sudo -n systemctl reboot", transportErr("ssh: session closed by remote host"))
	h.on("systemctl is-active k3s", ok("active\n"))
	h.on("findmnt", ok("rw,relatime\n"))
	h.on("df -B1 -P /var/lib", ok("8000000000\n"))
	h.on("sudo -n test -b", ok(""))
	h.on("backupstoragelocations", ok(""))
	return h
}

func scriptedAgent(t *testing.T, nodes ...testNode) *fakeHost {
	t.Helper()
	h := newFakeHost(testAgentAddr, "SHA256:agent", nodesJSON(t, nodes...))
	h.on("systemctl is-active k3s-agent", ok("active\n"))
	h.on("sudo -n systemctl reboot", transportErr("ssh: session closed by remote host"))
	h.on("findmnt", ok("rw,relatime\n"))
	h.on("df -B1 -P /var/lib", ok("8000000000\n"))
	h.on("sudo -n test -b", ok(""))
	return h
}

// ---------------------------------------------------------------------------
// The named tests.
// ---------------------------------------------------------------------------

// OUTSIDE THE WINDOW NOTHING HAPPENS, AND THE REFUSAL NAMES WHEN TO COME BACK
// IN BOTH CLOCKS (PLAN 7.4 item 3).
func TestRebootRefusesOutsideTheWindowNamingLocalAndUTC(t *testing.T) {
	// Monday 02:00-06:00 in Kolkata; the fake clock is Wednesday noon UTC, so
	// the window is closed and its next opening needs both clocks to read.
	clock := newFakeClock(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true},
		hosts:  []api.HostRecord{serverHost()},
		window: windowRecord([]string{"mon"}, "02:00", "06:00", "Asia/Kolkata"),
		server: server,
		clock:  clock,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("a reboot outside the maintenance window was allowed:\n%s", out)
	}
	// Local time first and UTC second, both in one instant.
	for _, want := range []string{"outside the maintenance window", "IST", "UTC", "it next opens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%s", want, err)
		}
	}
	if len(ran) != 0 {
		t.Errorf("the gates ran (%v) before the window rule refused", ran)
	}
	// The window rule is decided from the control plane's record, before a
	// single host is contacted: nothing may be touched to find out it is early.
	if got := f.dialer.count(); got != 0 {
		t.Errorf("the window rule opened %d SSH connection(s); it must refuse before reaching a host", got)
	}
	for _, m := range server.mutations() {
		t.Errorf("a refused reboot changed something: %s", m)
	}
}

// --NOW BYPASSES THE WINDOW AND NOTHING ELSE.
//
// Two halves, both load-bearing: with a CLOSED window the run still proceeds
// (the window is what --now buys), and each of the three other checks still
// refuses when it fails (what --now must not buy).
func TestNowSkipsOnlyTheWindow(t *testing.T) {
	closed := windowRecord([]string{"mon"}, "02:00", "06:00", "Asia/Kolkata")

	t.Run("the window does not refuse", func(t *testing.T) {
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost()},
			window: closed,
			server: server,
			gates:  permissiveGates(&ran),
		})
		out, err := f.run(t)
		if err != nil {
			t.Fatalf("--now was refused by the window: %v\n%s", err, out)
		}
		if !server.hasCommand("sudo -n systemctl reboot") {
			t.Errorf("--now with a closed window did not reboot:\n%s", out)
		}
		if len(ran) != 3 {
			t.Errorf("only %d of 3 checks ran under --now: %v", len(ran), ran)
		}
		if !strings.Contains(out, "window is bypassed") {
			t.Errorf("the run does not say it bypassed the window:\n%s", out)
		}
	})

	for _, refusing := range []string{gateQuorum, gateStorage, gateRecoveryPoint} {
		t.Run(refusing+" still refuses", func(t *testing.T) {
			server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
			ran := []string{}
			gates := fakeGates(func(name string) (bool, string, string) {
				if name == refusing {
					return false, name + " would not survive this reboot", "fix " + name
				}
				return true, name + " observed", ""
			}, &ran)
			f := newRebootFixture(t, rebootFixtureOpts{
				flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
				hosts:  []api.HostRecord{serverHost()},
				window: closed,
				server: server,
				gates:  gates,
			})
			out, err := f.run(t)
			if err == nil {
				t.Fatalf("--now bypassed %s as well:\n%s", refusing, out)
			}
			if !strings.Contains(err.Error(), refusing) || !strings.Contains(err.Error(), "fix "+refusing) {
				t.Errorf("the refusal does not name %s and its fix:\n%s", refusing, err)
			}
			if len(ran) != 3 {
				t.Errorf("a failing check stopped the others: %v", ran)
			}
			if _, ok := f.server.objects["kubenest-operation"]; ok {
				t.Error("a refused reboot created the operation record")
			}
			for _, m := range server.mutations() {
				t.Errorf("a refused reboot changed something: %s", m)
			}
		})
	}
}

// --WAIT HOLDS NOTHING, THEN TAKES THE RECORD AND RE-CHECKS EVERY GATE AGAINST
// THE STATE AT THAT MOMENT (PLAN 7.2).
func TestWaitTakesTheLockAndRechecksWhenTheWindowOpens(t *testing.T) {
	// The window opens at 02:00; the run starts two minutes early.
	clock := newFakeClock(time.Date(2026, 9, 28, 1, 58, 0, 0, time.UTC))
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	ranAt := []time.Time{}
	gates := []rebootGate{{
		Name: gateQuorum,
		Check: func(context.Context) (bool, string, string) {
			ranAt = append(ranAt, clock.now())
			return true, "observed after the wait", ""
		},
	}}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Wait: true},
		hosts:  []api.HostRecord{serverHost()},
		window: windowRecord([]string{"mon"}, "02:00", "06:00", "UTC"),
		server: server,
		clock:  clock,
		gates:  gates,
	})

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("--wait run failed: %v\n%s", err, out)
	}
	opening := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
	if !strings.Contains(out, "Waiting") {
		t.Errorf("the run does not say it waited:\n%s", out)
	}
	if len(ranAt) != 1 || ranAt[0].Before(opening) {
		t.Errorf("the gates ran at %v, before the window opened at %v: --wait must re-check against the state at that moment", ranAt, opening)
	}
	// Nothing was held or touched while it waited: the first SSH connection
	// happens after the opening.
	if first := f.dialer.first(); first.Before(opening) {
		t.Errorf("a host was contacted at %v, before the window opened at %v: a wait holds nothing", first, opening)
	}
	if first := f.dialer.first(); first.Equal(time.Time{}) {
		t.Error("the run never connected to the host at all")
	}
	// The lock is taken AFTER the window opens, and it names this node.
	req := f.server.recordRequest(t)
	if req.Kind != operation.KindNodeReboot {
		t.Errorf("the record is about %q, want %q", req.Kind, operation.KindNodeReboot)
	}
	if len(req.Targets) != 1 || req.Targets[0].HostID != "h-srv" || req.Targets[0].NodeUID != "uid-srv" {
		t.Errorf("the record names targets %+v, want the host and Node UID this run acted on", req.Targets)
	}
	if req.Versions["mode"] != "host-reboot" {
		t.Errorf("the record's mode is %q, want host-reboot", req.Versions["mode"])
	}
}

// --K3S-ONLY RESTARTS THE SERVICE AND NEVER THE HOST, AND SAYS WHAT IT RENEWS.
func TestK3sOnlyRestartsTheServiceAndNeverRebootsTheHost(t *testing.T) {
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	server.on("sudo -n systemctl restart k3s", ok(""))
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true, K3sOnly: true},
		hosts:  []api.HostRecord{serverHost()},
		server: server,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("--k3s-only failed: %v\n%s", err, out)
	}
	if server.hasCommand("systemctl reboot") {
		t.Errorf("--k3s-only issued a reboot:\n%s", commandLog(server))
	}
	if agentHasReboot := strings.Contains(commandLog(server), " systemctl reboot\n"); agentHasReboot {
		t.Errorf("--k3s-only issued a host reboot:\n%s", commandLog(server))
	}
	if !server.hasCommand("sudo -n systemctl restart k3s") {
		t.Errorf("--k3s-only did not restart k3s:\n%s", commandLog(server))
	}
	// The certificate case is why the flag exists, and the output has to name
	// it: the remedy text that points at this command depends on it.
	if !strings.Contains(out, "certificate") {
		t.Errorf("the output does not name the certificate renewal --k3s-only performs:\n%s", out)
	}
	req := server.recordRequest(t)
	if req.Versions["mode"] != "k3s-only" {
		t.Errorf("the record's mode is %q, want k3s-only", req.Versions["mode"])
	}
}

// A SINGLE-SERVER CLUSTER IS NOT DRAINED, AND THE COMMAND SAYS SO.
func TestSingleServerRebootSkipsTheDrainAndSaysSo(t *testing.T) {
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost()},
		server: server,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("single-server reboot failed: %v\n%s", err, out)
	}
	if !server.hasCommand("sudo -n systemctl reboot") {
		t.Errorf("the host was not rebooted:\n%s", commandLog(server))
	}
	for _, forbidden := range []string{"kubectl drain", "kubectl cordon", "kubectl uncordon"} {
		if server.hasCommand(forbidden) {
			t.Errorf("a single-server cluster must not drain: %s", forbidden)
		}
	}
	if !strings.Contains(out, "NOT draining") || !strings.Contains(out, "nowhere for the pods to go") {
		t.Errorf("the command did not say it skipped the drain:\n%s", out)
	}
}

// A MULTI-NODE CLUSTER IS CORDONED, DRAINED WITHIN THE MANIFEST'S TIMEOUT, AND
// UNCORDONED ONLY AFTER THE NODE IS READY.
func TestMultiNodeRebootCordonsDrainsAndUncordons(t *testing.T) {
	server := scriptedServer(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
	)
	agent := scriptedAgent(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
	)
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost(), agentHost()},
		server: server,
		target: agent,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("multi-node reboot failed: %v\n%s", err, out)
	}
	cordon := server.indexOf("kubectl cordon " + testAgentNode)
	drain := server.indexOf("kubectl drain " + testAgentNode)
	uncordon := server.indexOf("kubectl uncordon " + testAgentNode)
	if cordon < 0 || drain < 0 || uncordon < 0 {
		t.Fatalf("the sequence is incomplete:\n%s", commandLog(server))
	}
	if !(cordon < drain && drain < uncordon) {
		t.Errorf("cordon (%d), drain (%d) and uncordon (%d) are out of order", cordon, drain, uncordon)
	}
	if !agent.hasCommand("sudo -n systemctl reboot") {
		t.Errorf("the agent was not rebooted:\n%s", commandLog(agent))
	}
	drainCmd := server.commands[drain]
	if strings.Contains(drainCmd, "--force") {
		t.Errorf("the drain force-deletes pods: %s", drainCmd)
	}
	if !strings.Contains(drainCmd, "--ignore-daemonsets") || !strings.Contains(drainCmd, "--delete-emptydir-data") {
		t.Errorf("the drain is missing the flags a node with DaemonSets and emptyDir pods needs: %s", drainCmd)
	}
	// The timeout is asserted against the MANIFEST, not a literal: the bundle
	// decides it (and may retune it), so a test that pinned the number would go
	// red when the manifest moved rather than when the wiring broke.
	bundleManifest, err := bundles.Manifest("1.1")
	if err != nil {
		t.Fatal(err)
	}
	drainFor, err := bundleManifest.Limits.Timeouts.For("node-drain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drainCmd, "--timeout="+drainFor.String()) {
		t.Errorf("the drain timeout is not the manifest's limits.timeouts.node-drain (%s): %s", drainFor, drainCmd)
	}
}

// THE DRAIN TIMEOUT IS THE MANIFEST'S, NOT A CONSTANT.
func TestDrainTimeoutComesFromTheManifest(t *testing.T) {
	for _, want := range []string{"7m", "41m"} {
		t.Run(want, func(t *testing.T) {
			server := scriptedServer(t,
				testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
				testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
			)
			agent := scriptedAgent(t,
				testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
				testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
			)
			ran := []string{}
			f := newRebootFixture(t, rebootFixtureOpts{
				flags:    NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
				hosts:    []api.HostRecord{serverHost(), agentHost()},
				server:   server,
				target:   agent,
				gates:    permissiveGates(&ran),
				manifest: manifestWithTimeouts(t, want, ""),
			})
			out, err := f.run(t)
			if err != nil {
				t.Fatalf("reboot failed: %v\n%s", err, out)
			}
			if got := server.commands[server.indexOf("kubectl drain")]; !strings.Contains(got, "--timeout="+want+"0s") {
				t.Errorf("the drain timeout is not the manifest's %s: %s", want, got)
			}
		})
	}
}

// AN UNCONFIRMED CALL PRINTS THE PLAN AND TOUCHES NOTHING.
func TestUnconfirmedCallPrintsThePlanAndStops(t *testing.T) {
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr},
		hosts:  []api.HostRecord{serverHost()},
		server: server,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("an unconfirmed reboot ran:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Errorf("the refusal does not say how to proceed:\n%s", err)
	}
	for _, want := range []string{"Rebooting node", testNodeName, "systemctl reboot", "limits.timeouts.node-reboot"} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan is missing %q:\n%s", want, out)
		}
	}
	if len(server.mutations()) != 0 {
		t.Errorf("an unconfirmed call changed something: %v", server.mutations())
	}
	if len(server.objects) != 0 {
		t.Errorf("an unconfirmed call created %d cluster object(s)", len(server.objects))
	}
}

// --WAIT AND --NOW ARE MUTUALLY EXCLUSIVE, AND THE REFUSAL COMES FIRST.
func TestWaitAndNowAreMutuallyExclusive(t *testing.T) {
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Wait: true, Now: true},
		hosts:  []api.HostRecord{serverHost()},
		server: server,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("--wait --now was accepted:\n%s", out)
	}
	for _, want := range []string{"--wait", "--now", "mutually exclusive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q: %s", want, err)
		}
	}
	if got := f.dialer.count(); got != 0 {
		t.Errorf("the flags were refused after %d SSH connection(s)", got)
	}
	if out != "" {
		t.Errorf("a flag refusal printed a plan:\n%s", out)
	}
}

// EACH OF THE THREE OUTSIDE STEPS IS REPORTED AS ITS OWN, AND A TIMEOUT NAMES
// THE STEP THAT DID NOT HAPPEN (PLAN 7.3).
func TestWaitOrderingSSHThenK3sThenAPI(t *testing.T) {
	cases := []struct {
		name  string
		host  func(t *testing.T) *fakeHost
		dial  func(call int) error
		wants []string
	}{
		{
			name: "the host never answers SSH",
			host: func(t *testing.T) *fakeHost {
				return scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
			},
			// The first connection is the cluster read; every connection AFTER
			// the reboot fails, which is the host that never comes back.
			dial:  func(call int) error { return nil },
			wants: []string{waitStepSSH},
		},
		{
			name: "k3s never comes up",
			host: func(t *testing.T) *fakeHost {
				h := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
				return h.prepend("systemctl is-active k3s", fail(3, "inactive\n"))
			},
			wants: []string{waitStepService},
		},
		{
			name: "the API never answers",
			host: func(t *testing.T) *fakeHost {
				// The first match wins, so the kubectl read is refused before
				// the generic Ready rule can answer it: the API itself is what
				// is missing.
				h := newFakeHost(testServerAddr, "SHA256:server",
					nodesJSON(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true}))
				h.on("kubectl get node", fail(1, "The connection to the server 127.0.0.1:6443 was refused"))
				h.on("systemctl is-active k3s", ok("active\n"))
				h.on("sudo -n systemctl reboot", transportErr("ssh: session closed by remote host"))
				return h
			},
			wants: []string{waitStepAPI},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := tc.host(t)
			ran := []string{}
			f := newRebootFixture(t, rebootFixtureOpts{
				flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
				hosts:  []api.HostRecord{serverHost()},
				server: server,
				gates:  permissiveGates(&ran),
			})
			if tc.dial != nil {
				// Dial once for the cluster read and the target, then fail
				// every attempt the wait makes.
				f.dialer.fail = func(call int) error {
					if call <= 1 {
						return nil
					}
					return fmt.Errorf("connect to %s: connection refused", testServerAddr)
				}
			}

			out, err := f.run(t)
			if err == nil {
				t.Fatalf("a step that never happened was reported as a success:\n%s", out)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the timeout does not name %q: %s", want, err)
				}
			}
			if !strings.Contains(err.Error(), "limits.timeouts.node-reboot") {
				t.Errorf("the timeout does not name the manifest's deadline: %s", err)
			}
			// The steps before the failing one were REPORTED as observed, in
			// order: a wait that never said what it saw is a wait nobody can
			// debug.
			if tc.name == "k3s never comes up" && !strings.Contains(out, "- "+waitStepSSH+": ") {
				t.Errorf("the SSH step was not reported before the k3s step:\n%s", out)
			}
			if tc.name == "the API never answers" {
				for _, want := range []string{"- " + waitStepSSH + ": ", "- " + waitStepService + ": "} {
					if !strings.Contains(out, want) {
						t.Errorf("the output does not report %q before the API step:\n%s", want, out)
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The checks --now must not skip, each on its own.
// ---------------------------------------------------------------------------

// A SERVER REBOOT THAT WOULD LEAVE NO QUORUM IS REFUSED. The count comes from
// the inventory, because that is the record of which machines vote.
func TestQuorumGateRefusesAServerRebootThatLosesTheQuorum(t *testing.T) {
	cases := []struct {
		name    string
		servers int
		host    api.HostRecord
		refuse  bool
	}{
		{name: "one server has no quorum to lose", servers: 1, host: serverHost()},
		{name: "three servers survive one reboot", servers: 3, host: serverHost()},
		{name: "four servers survive one reboot", servers: 4, host: serverHost()},
		{name: "two servers do not", servers: 2, host: serverHost(), refuse: true},
		{name: "an agent changes no vote", servers: 2, host: agentHost()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := &nodeReboot{host: tc.host, servers: tc.servers}
			passed, detail, fix := n.checkQuorum(context.Background())
			if passed == tc.refuse {
				t.Fatalf("checkQuorum(servers=%d, role=%s) passed=%v, want refuse=%v: %s", tc.servers, tc.host.Role, passed, tc.refuse, detail)
			}
			if !passed && fix == "" {
				t.Error("a refused check must name the fix")
			}
			if detail == "" {
				t.Error("a check must say what it observed")
			}
		})
	}
}

// A NODE WHOSE DATABASE FILESYSTEM IS READ-ONLY, OR WHOSE RECORDED DEVICE IS
// GONE, IS NOT A NODE TO REBOOT.
func TestStorageGateRefusesAReadOnlyOrMissingDatastore(t *testing.T) {
	cases := []struct {
		name   string
		rules  []fakeRule
		host   api.HostRecord
		refuse bool
		want   string
	}{
		{
			name:  "writable with the recorded device present",
			rules: []fakeRule{{match: "findmnt", resp: ok("rw,relatime\n")}, {match: "sudo -n test -b", resp: ok("")}, {match: "df -B1", resp: ok("9000000\n")}},
			host:  serverHost(),
		},
		{
			name:   "mounted read-only",
			rules:  []fakeRule{{match: "findmnt", resp: ok("ro,relatime\n")}, {match: "sudo -n test -b", resp: ok("")}, {match: "df -B1", resp: ok("9000000\n")}},
			host:   serverHost(),
			refuse: true,
			want:   "read-only",
		},
		{
			name:   "the recorded device is gone",
			rules:  []fakeRule{{match: "findmnt", resp: ok("rw\n")}, {match: "sudo -n test -b", resp: fail(1, "")}, {match: "df -B1", resp: ok("9000000\n")}},
			host:   serverHost(),
			refuse: true,
			want:   "not a block device",
		},
		{
			name:   "the free space cannot be measured",
			rules:  []fakeRule{{match: "findmnt", resp: ok("rw\n")}, {match: "sudo -n test -b", resp: ok("")}, {match: "df -B1", resp: fail(1, "df: error")}},
			host:   serverHost(),
			refuse: true,
			want:   "could not be measured",
		},
		{
			name:   "the device is not a device path at all",
			rules:  []fakeRule{{match: "findmnt", resp: ok("rw\n")}},
			host:   api.HostRecord{HostID: "h-srv", Role: "server", SSHAddress: testServerAddr, StorageDevice: "/dev/sda; reboot"},
			refuse: true,
			want:   "will not put it in a shell command",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(tc.host.SSHAddress, "SHA256:server", "")
			for _, r := range tc.rules {
				h.on(r.match, r.resp)
			}
			n := &nodeReboot{host: tc.host, target: h, out: io.Discard}
			passed, detail, fix := n.checkStorage(context.Background())
			if passed == tc.refuse {
				t.Fatalf("checkStorage passed=%v, want refuse=%v: %s", passed, tc.refuse, detail)
			}
			if tc.refuse && !strings.Contains(detail, tc.want) {
				t.Errorf("the refusal does not say why (%q): %s", tc.want, detail)
			}
			if tc.refuse && fix == "" {
				t.Error("a refused check must name the fix")
			}
			if !tc.refuse && !strings.Contains(detail, "writable") {
				t.Errorf("a passing check must say what it measured: %s", detail)
			}
		})
	}
}

// THE RECOVERY POINT IS THE NEWEST COMPLETED BACKUP, AND ITS ABSENCE IS NOT A
// PASS: an unread cluster is refused, a stale one is refused, and only a
// cluster that has NEVER had a backup target — and so has no recovery point a
// reboot could make stale — passes.
func TestRecoveryPointGateSpeaksForTheNewestCompletedBackup(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	backupDoc := func(phase, completed string, age time.Duration) func(string) (sshx.Result, error) {
		payload := map[string]any{"items": []map[string]any{{
			"metadata": map[string]any{"name": "nightly-1", "creationTimestamp": now.Add(-age).Format(time.RFC3339)},
			"status":   map[string]any{"phase": phase, "completionTimestamp": completed},
		}}}
		raw, _ := json.Marshal(payload)
		return ok(string(raw))
	}

	cases := []struct {
		name   string
		bsl    func(string) (sshx.Result, error)
		backup func(string) (sshx.Result, error)
		refuse bool
		want   string
	}{
		{
			name: "no backup target configured",
			bsl:  ok(""),
			// The cluster's cluster-wide state: nothing to invalidate.
			want: "",
		},
		{
			name:   "a target with no completed backup",
			bsl:    ok("default"),
			backup: backupDoc("InProgress", "", time.Minute),
			refuse: true,
			want:   "NO completed workload backup",
		},
		{
			name:   "a recovery point older than the bundle allows",
			bsl:    ok("default"),
			backup: backupDoc("Completed", now.Add(-72*time.Hour).Format(time.RFC3339), 72*time.Hour),
			refuse: true,
			want:   "older than the 48h0m0s",
		},
		{
			name:   "a fresh recovery point",
			bsl:    ok("default"),
			backup: backupDoc("Completed", now.Add(-3*time.Hour).Format(time.RFC3339), 3*time.Hour),
			want:   "inside the 48h0m0s",
		},
		{
			name:   "the backup target state cannot be read",
			bsl:    fail(1, "Error from server: the server could not find the requested resource"),
			refuse: true,
			want:   "could not be read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(testServerAddr, "SHA256:server", "")
			h.on("backupstoragelocations", tc.bsl)
			if tc.backup != nil {
				h.on("get backups.velero.io", tc.backup)
			}
			parsed, err := bundles.Manifest("1.1")
			if err != nil {
				t.Fatal(err)
			}
			n := &nodeReboot{host: serverHost(), serverConn: h, bundle: parsed, out: io.Discard, now: func() time.Time { return now }}
			passed, detail, fix := n.checkRecoveryPoint(context.Background())
			if passed == tc.refuse {
				t.Fatalf("checkRecoveryPoint passed=%v, want refuse=%v: %s", passed, tc.refuse, detail)
			}
			if tc.refuse && !strings.Contains(detail, tc.want) {
				t.Errorf("the refusal does not say why (%q): %s", tc.want, detail)
			}
			if tc.refuse && fix == "" {
				t.Error("a refused check must name the fix")
			}
			if !tc.refuse && !strings.Contains(detail, tc.want) {
				t.Errorf("the passing check does not say what it read (%q): %s", tc.want, detail)
			}
		})
	}
}

// The inventory's Node UID is what tells a rebuilt host from the recorded one:
// a mismatch must be refused BEFORE anything is changed.
func TestRebootRefusesARebuiltNodeWithADifferentUID(t *testing.T) {
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-different", Addresses: []string{testServerAddr}, Ready: true})
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost()},
		server: server,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("a node whose UID does not match the inventory was rebooted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "uid-different") || !strings.Contains(err.Error(), "uid-srv") {
		t.Errorf("the refusal does not name both UIDs: %s", err)
	}
	if len(server.mutations()) != 0 {
		t.Errorf("the mismatch was found after something was changed: %v", server.mutations())
	}
}

// A host key the inventory does not recognise is refused the same way: the
// machine answering on a recorded address is not the machine the record holds.
func TestRebootRefusesAHostKeyTheInventoryDoesNotKnow(t *testing.T) {
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	server.fp = "SHA256:something-else"
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost()},
		server: server,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("a host with a different key was rebooted:\n%s", out)
	}
	for _, want := range []string{"SHA256:something-else", "SHA256:server"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %s", want, err)
		}
	}
	if len(server.mutations()) != 0 {
		t.Errorf("the mismatch was found after something was changed: %v", server.mutations())
	}
}

// THE INTERLOCK IS HELD FOR THE DURATION, AND A LOCK ANOTHER NODE HOLDS IS A
// REFUSAL RATHER THAN AN OVERWRITE.
func TestRebootTakesKuredsLockAndRefusesAnotherNodes(t *testing.T) {
	t.Run("the lock is taken, held for the whole run, and released", func(t *testing.T) {
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err != nil {
			t.Fatalf("reboot failed: %v\n%s", err, out)
		}
		// Held before the disruptive step, and released after it: the two
		// writes are the TAKE and the RELEASE, and their order is the property.
		rebootIdx := server.indexOf("sudo -n systemctl reboot")
		var lockWrites []int
		for i, c := range server.commands {
			if c == "sudo -n k3s kubectl replace -f -" {
				lockWrites = append(lockWrites, i)
			}
		}
		if len(lockWrites) < 2 {
			t.Fatalf("kured's lock was written %d time(s): it must be taken and then released: %v", len(lockWrites), lockWrites)
		}
		if lockWrites[0] > rebootIdx {
			t.Errorf("the lock was taken after the reboot (%d > %d): kured could have acted first", lockWrites[0], rebootIdx)
		}
		if lockWrites[len(lockWrites)-1] < rebootIdx {
			t.Errorf("the lock was released before the reboot (%d < %d)", lockWrites[len(lockWrites)-1], rebootIdx)
		}
		// ...and gone afterwards.
		if lock := server.kuredLock(); lock != "" {
			t.Errorf("kured's lock is still held after a successful reboot: %s", lock)
		}
		if !strings.Contains(out, "interlock") {
			t.Errorf("the run does not report the interlock:\n%s", out)
		}
	})

	t.Run("another node's lock is refused", func(t *testing.T) {
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		held := fmt.Sprintf(`{"nodeID":"%s","metadata":{"unschedulable":false},"created":%q,"TTL":2100000000000}`,
			"some-other-node", time.Now().UTC().Format(time.RFC3339Nano))
		meta := server.ds["metadata"].(map[string]any)
		meta["annotations"] = map[string]any{"weave.works/kured-node-lock": held}
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			gates:  permissiveGates(&ran),
		})

		out, err := f.run(t)
		if err == nil {
			t.Fatalf("a reboot ran while another node held kured's lock:\n%s", out)
		}
		if !strings.Contains(err.Error(), "some-other-node") {
			t.Errorf("the refusal does not name the node holding the lock: %s", err)
		}
		for _, forbidden := range []string{"systemctl reboot", "kubectl cordon", "kubectl drain"} {
			if server.hasCommand(forbidden) {
				t.Errorf("a refused reboot issued %s", forbidden)
			}
		}
	})
}

// The reboot hold is placed through the landed helper and lifted only when this
// run placed it — a server carries it permanently and must keep it.
func TestRebootHoldsAKuredEligibleNodeAndLiftsOnlyItsOwnHold(t *testing.T) {
	t.Run("an unheld node is held for the run and put back", func(t *testing.T) {
		server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			gates:  permissiveGates(&ran),
		})
		out, err := f.run(t)
		if err != nil {
			t.Fatalf("reboot failed: %v\n%s", err, out)
		}
		held := server.indexOf("kubectl label node " + testNodeName + " kubenest.io/auto-reboot=false")
		lifted := server.indexOf("kubectl label node " + testNodeName + " kubenest.io/auto-reboot-")
		if held < 0 {
			t.Fatalf("the node was not held:\n%s", commandLog(server))
		}
		if lifted < 0 {
			t.Fatalf("the hold this run placed was not lifted:\n%s", commandLog(server))
		}
		if held > server.indexOf("sudo -n systemctl reboot") {
			t.Error("the hold was placed after the reboot started")
		}
	})

	t.Run("a node that already carries the hold is not relabelled", func(t *testing.T) {
		server := scriptedServer(t, testNode{
			Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true,
			Labels: map[string]string{"kubenest.io/auto-reboot": "false"},
		})
		ran := []string{}
		f := newRebootFixture(t, rebootFixtureOpts{
			flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
			hosts:  []api.HostRecord{serverHost()},
			server: server,
			gates:  permissiveGates(&ran),
		})
		out, err := f.run(t)
		if err != nil {
			t.Fatalf("reboot failed: %v\n%s", err, out)
		}
		if server.hasCommand("kubectl label node") {
			t.Errorf("a node that already carried the hold was relabelled:\n%s", commandLog(server))
		}
	})
}

// A NODE THAT NEVER COMES BACK LEAVES THE RECORD FAILED, THE NODE CORDONED AND
// KURED'S LOCK HELD — IT IS NOT RETRIED, AND NO SECOND NODE IS TOUCHED.
func TestUnreturningNodeLeavesAFailedRecordAndAHeldLock(t *testing.T) {
	server := scriptedServer(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
	)
	agent := scriptedAgent(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
	)
	agent.prepend("systemctl is-active k3s-agent", fail(3, "inactive\n"))
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost(), agentHost()},
		server: server,
		target: agent,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("a node that never came back was reported as rebooted:\n%s", out)
	}
	if !strings.Contains(err.Error(), waitStepService) {
		t.Errorf("the failure does not name the step that never happened: %s", err)
	}
	if result := f.server.recordField(t, "result"); result != "failed" {
		t.Errorf("the record's result is %q, want failed", result)
	}
	if terminal := f.server.recordField(t, "terminal"); terminal != true {
		t.Errorf("the record is not terminal: %v", terminal)
	}
	// The node stays cordoned and the lock stays held: that halts the sequence
	// instead of letting the next node go down while somebody investigates.
	if !server.hasCommand("kubectl cordon " + testAgentNode) {
		t.Error("the node was not cordoned")
	}
	if server.hasCommand("kubectl uncordon") {
		t.Error("a node that never came back was uncordoned")
	}
	if lock := server.kuredLock(); lock == "" {
		t.Error("kured's lock was released while the node was still missing")
	}
	if !strings.Contains(out, "annotate") {
		t.Errorf("the output does not name kured's documented manual recovery:\n%s", out)
	}
	// No second node: the agent is the only machine that was rebooted.
	if server.hasCommand("systemctl reboot") {
		t.Error("the others server(s) were touched as well")
	}
}

// A DRAIN THAT CANNOT FINISH IS A REFUSAL, AND A REFUSAL PUTS THE NODE BACK
// INTO SERVICE INSTEAD OF LEAVING IT CORDONED.
func TestADrainThatFailsUncordonsAndChangesNothingElse(t *testing.T) {
	server := scriptedServer(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
	)
	server.prepend("kubectl drain", fail(1, "error when evicting pods: Cannot evict pod as it would violate the pod's disruption budget"))
	agent := scriptedAgent(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
	)
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost(), agentHost()},
		server: server,
		target: agent,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("a drain that could not finish was accepted:\n%s", out)
	}
	if !strings.Contains(err.Error(), "PodDisruptionBudget") {
		t.Errorf("the refusal does not carry the drain's own reason: %s", err)
	}
	if !server.hasCommand("kubectl uncordon " + testAgentNode) {
		t.Errorf("the node was left cordoned by a refusal:\n%s", commandLog(server))
	}
	if agent.hasCommand("systemctl reboot") {
		t.Error("the host was rebooted after its drain failed")
	}
	if result := f.server.recordField(t, "result"); result != "failed" {
		t.Errorf("the record's result is %q, want failed", result)
	}
	if lock := server.kuredLock(); lock != "" {
		t.Error("kured's lock was left held although nothing was taken down")
	}
}

// THE RECORD IS THE LOCK: a second operation is refused while one is live.
func TestRebootRefusesWhileAnotherOperationHoldsTheRecord(t *testing.T) {
	server := scriptedServer(t, testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true})
	live := operation.Record{
		OperationID: "abcdef0123456789",
		Request:     operation.Request{Kind: operation.KindNodeReboot, Cluster: testCluster},
		Executor:    operation.Executor{Token: "someone-elses", Operator: "ana@laptop", State: operation.ExecutorRunning},
	}
	seedLiveRecord(t, server, live)
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testServerAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost()},
		server: server,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("a second operation took a cluster with a live record:\n%s", out)
	}
	if !strings.Contains(err.Error(), "ana@laptop") {
		t.Errorf("the refusal does not name the other operator: %s", err)
	}
	if server.hasCommand("systemctl reboot") {
		t.Error("a refused operation rebooted the host")
	}
}

// A remote action is recorded BEFORE it is submitted, with a postcondition a
// successor can observe — the property that makes an interrupted reboot
// resumable instead of a guess.
func TestEveryDisruptiveActionIsRecordedBeforeItIsSubmitted(t *testing.T) {
	server := scriptedServer(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
	)
	agent := scriptedAgent(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{testAgentAddr}, Ready: true},
	)
	ran := []string{}
	f := newRebootFixture(t, rebootFixtureOpts{
		flags:  NodeRebootFlags{Node: testAgentAddr, Confirm: true, Now: true},
		hosts:  []api.HostRecord{serverHost(), agentHost()},
		server: server,
		target: agent,
		gates:  permissiveGates(&ran),
	})

	out, err := f.run(t)
	if err != nil {
		t.Fatalf("reboot failed: %v\n%s", err, out)
	}
	actions := recordedActions(t, server)
	if len(actions) < 4 {
		t.Fatalf("%d action(s) were recorded, want the hold, the cordon, the drain, the reboot and the uncordon:\n%s", len(actions), out)
	}
	for _, a := range actions {
		if a.Postcondition == "" {
			t.Errorf("action %s has no postcondition, so a successor could not tell whether it happened", a.ID)
		}
		if a.Status == operation.ActionRecorded {
			t.Errorf("action %s is still recorded-but-not-submitted after a successful run", a.ID)
		}
	}
	// The reboot itself is the uncertain one: its outcome is established by the
	// wait, and the record says so rather than claiming the SSH call returned.
	var reboot operation.Action
	for _, a := range actions {
		if a.Stage == "disrupt" {
			reboot = a
		}
	}
	if reboot.ID == "" {
		t.Fatalf("the reboot was not recorded as an action: %+v", actions)
	}
	if reboot.Status != operation.ActionSucceeded {
		t.Errorf("the reboot's recorded status is %q, want succeeded once the node came back", reboot.Status)
	}
}

// seedLiveRecord puts a live record on the fake cluster, the way another
// laptop's running operation would have left it.
func seedLiveRecord(t *testing.T, h *fakeHost, rec operation.Record) {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.objects["kubenest-operation"] = map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "kubenest-operation", "namespace": "kube-system", "resourceVersion": "55"},
		"data":       map[string]string{"record.json": string(raw)},
	}
}

func recordedActions(t *testing.T, h *fakeHost) []operation.Action {
	t.Helper()
	return h.liveRecord(t).Actions
}

func commandLog(h *fakeHost) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.commands, "\n")
}
