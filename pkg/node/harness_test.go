package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/sshx"
	"kubenest.io/cli/pkg/stages"
	"kubenest.io/cli/pkg/storage"
	"kubenest.io/cli/pkg/upgrade"
	"kubenest.io/cli/pkg/window"
)

// The fixtures for the node verbs. They are the style pkg/cmd's reboot tests
// use: a scripted SSH+kubectl machine, a REAL compare-and-swap on the two
// objects these verbs write through a resourceVersion (kured's DaemonSet and
// the operation record's ConfigMap), and a control-plane record kept in
// memory. A fake that accepted every write would let a lost update pass as a
// success, which is the failure the interlock and the record exist to prevent.

const (
	testServerAddr = "10.0.3.7"
	testAgentAddr  = "10.0.3.9"
	testServerNode = "prod-1-srv-1"
	testAgentNode  = "prod-1-agt-1"
	testPID        = "10.0.3.9"
	testDevice     = "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive3"
	// The machine a later `node add` gives a removed host's address to: its own
	// host ID and Node object, at the address the removed record still holds.
	testReusedHostID  = "h-new"
	testReusedNode    = "prod-1-agt-2"
	testReusedNodeUID = "uid-new"
)

// entry is one command a verb issued, in the order it was issued, with the
// payload it streamed and the clock the run was reading at the time.
type entry struct {
	host    string
	command string
	input   string
	at      time.Time
}

// logbook is the ordered record of everything that ran, across every host: an
// assertion about ORDER ("the label goes on before the readiness read") is an
// assertion about this, and one about WHEN ("the locks are taken only once the
// window opens") is an assertion about the clock it carries.
type logbook struct {
	mu      sync.Mutex
	entries []entry
	clock   func() time.Time
}

func (l *logbook) record(host, command string, input []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	at := time.Time{}
	if l.clock != nil {
		at = l.clock()
	}
	l.entries = append(l.entries, entry{host: host, command: command, input: string(input), at: at})
}

// indexOf is the position of the first command containing substr, or -1.
func (l *logbook) indexOf(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, e := range l.entries {
		if strings.Contains(e.command, substr) {
			return i
		}
	}
	return -1
}

// at is the clock reading of the first command containing substr.
func (l *logbook) at(substr string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if strings.Contains(e.command, substr) {
			return e.at, true
		}
	}
	return time.Time{}, false
}

// commands is every command issued to one host.
func (l *logbook) commands(host string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.entries {
		if e.host == host {
			out = append(out, e.command)
		}
	}
	return out
}

// hasCommand reports whether anything containing substr ran on any host.
func (l *logbook) hasCommand(substr string) bool { return l.indexOf(substr) >= 0 }

// inputOf is the stdin payload of the first command containing substr. A
// streamed write carries what the command cannot: the CoreDNS patch sets the
// replica count in the document, never in the command string (pkg/k3s), so the
// count is only observable here.
func (l *logbook) inputOf(substr string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if strings.Contains(e.command, substr) {
			return e.input
		}
	}
	return ""
}

// inputsOf is the stdin payload of every command containing substr, in order,
// so a test can read the SEQUENCE of counts a cluster-DNS re-assert passes
// through across a swap.
func (l *logbook) inputsOf(substr string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.entries {
		if strings.Contains(e.command, substr) {
			out = append(out, e.input)
		}
	}
	return out
}

// count counts the commands containing substr.
func (l *logbook) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		if strings.Contains(e.command, substr) {
			n++
		}
	}
	return n
}

// rule is one scripted answer: the first rule whose substring matches wins.
type rule struct {
	match string
	resp  func(f *fakeHost, command string) (sshx.Result, error)
}

func ok(stdout string) func(*fakeHost, string) (sshx.Result, error) {
	return func(*fakeHost, string) (sshx.Result, error) { return sshx.Result{Stdout: stdout}, nil }
}

func fail(exit int, stderr string) func(*fakeHost, string) (sshx.Result, error) {
	return func(*fakeHost, string) (sshx.Result, error) {
		return sshx.Result{ExitCode: exit, Stderr: stderr}, nil
	}
}

// healthyCoreDNSPods is what the fake cluster answers for the CoreDNS pods:
// two Ready pods on two distinct nodes, which satisfies the layout check for
// any replica count the rule can ask for (pkg/k3s.CoreDNSReplicas caps it at
// two). Tests that assert on the layout script their own answer with on().
const healthyCoreDNSPods = `{"items":[
  {"metadata":{"name":"coredns-aaaa"},"spec":{"nodeName":"node-a"},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}},
  {"metadata":{"name":"coredns-bbbb"},"spec":{"nodeName":"node-b"},
   "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}
]}`

// fakeHost is one scripted machine: the SSH commands a verb makes on it, the
// k3s objects it answers, and the two compare-and-swap writes it accepts.
type fakeHost struct {
	mu      sync.Mutex
	log     *logbook
	address string
	fp      string
	// nodes is the answer `kubectl get nodes -o json` gives on this host.
	nodes string
	// ds is kured's DaemonSet, which the interlock reads and writes.
	ds   map[string]any
	dsRV string
	// objects holds the ConfigMaps the operation record lives in.
	objects map[string]map[string]any
	rules   []rule
	rv      int
	// released is what the uninstall script command answers.
	unreachable bool
}

func newFakeHost(log *logbook, address, fingerprint, nodesJSON string) *fakeHost {
	h := &fakeHost{log: log, address: address, fp: fingerprint, nodes: nodesJSON, objects: map[string]map[string]any{}}
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

func (h *fakeHost) on(match string, resp func(f *fakeHost, command string) (sshx.Result, error)) *fakeHost {
	h.rules = append(h.rules, rule{match: match, resp: resp})
	return h
}

func (h *fakeHost) HostKeyFingerprint() string { return h.fp }
func (h *fakeHost) Close() error               { return nil }

// setNodes replaces the node list this host's API reports.
func (h *fakeHost) setNodes(nodesJSON string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes = nodesJSON
}

func (h *fakeHost) Run(ctx context.Context, command string) (sshx.Result, error) {
	h.log.record(h.address, command, nil)
	h.mu.Lock()
	unreachable := h.unreachable
	h.mu.Unlock()
	if unreachable {
		return sshx.Result{}, fmt.Errorf("dial tcp %s: connect: host is unreachable", h.address)
	}
	for _, r := range h.rules {
		if strings.Contains(command, r.match) {
			return r.resp(h, command)
		}
	}
	return h.scripted(command)
}

func (h *fakeHost) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	h.log.record(h.address, command, raw)
	switch {
	case strings.HasSuffix(command, "replace -f - -o json"), strings.HasSuffix(command, "create -f - -o json"):
		return h.writeConfigMap(raw, strings.Contains(command, "create"))
	case strings.HasSuffix(command, "replace -f -"):
		return h.replaceDaemonSet(raw)
	}
	for _, r := range h.rules {
		if strings.Contains(command, r.match) {
			return r.resp(h, command)
		}
	}
	return h.scripted(command)
}

// scripted answers everything a node verb reads by default: a healthy Ubuntu
// host, a Ready cluster, and the objects the verbs write.
func (h *fakeHost) scripted(command string) (sshx.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.Contains(command, "get daemonset -n kube-system kured -o json"):
		return h.jsonResult(h.ds)
	case strings.Contains(command, "get configmap kubenest-operation") && strings.Contains(command, " -o json") && !strings.Contains(command, " -o jsonpath"):
		name := configMapNameIn(command)
		if obj, ok := h.objects[name]; ok {
			return h.jsonResult(obj)
		}
		return sshx.Result{ExitCode: 1, Stderr: `Error from server (NotFound): configmaps "` + name + `" not found`}, nil
	case strings.Contains(command, "get nodes -o json"):
		return sshx.Result{Stdout: h.nodes}, nil
	case strings.Contains(command, "get pods -n kube-system -l k8s-app=kube-dns -o json"):
		// The fake cluster's cluster DNS is as healthy as it can be asked to
		// be: TWO Ready pods on two distinct nodes, which is the most the
		// layout ever requires (pkg/k3s.CoreDNSReplicas caps at two). A test
		// that cares about the layout scripts its own answer; without this
		// default every verb whose record stage runs would sit in the
		// convergence wait until the bundle's component-ready deadline.
		return sshx.Result{Stdout: healthyCoreDNSPods}, nil
	case strings.HasPrefix(command, "sudo -n k3s kubectl get node ") && strings.HasSuffix(command, " -o json"):
		return sshx.Result{Stdout: `{"spec":{"unschedulable":false}}`}, nil
	case strings.Contains(command, "get pv -o json"):
		return sshx.Result{Stdout: `{"items":[]}`}, nil
	case strings.Contains(command, "get pods --all-namespaces --field-selector spec.nodeName=") && strings.Contains(command, " -o json"):
		// A node with no pods bound to it: the replace that takes a machine out
		// first reads the pods of the dead node, and a cluster that was drained
		// by the node's own death may report none at all.
		return sshx.Result{Stdout: `{"items":[]}`}, nil
	case strings.Contains(command, "get pods --all-namespaces -o json"):
		return sshx.Result{Stdout: `{"items":[]}`}, nil
	case strings.Contains(command, "get poddisruptionbudgets -A -o json"):
		return sshx.Result{Stdout: `{"items":[]}`}, nil
	case strings.Contains(command, "/etc/os-release"):
		return sshx.Result{Stdout: "ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"}, nil
	case strings.Contains(command, "sudo -n true"):
		return sshx.Result{}, nil
	case strings.Contains(command, "command -v"):
		return sshx.Result{Stdout: "\n"}, nil
	case strings.Contains(command, "MemTotal"):
		return sshx.Result{Stdout: "cpu=4\nmemkb=7936000\ndiskbytes=80284000000\n"}, nil
	case strings.Contains(command, "cat /var/lib/rancher/k3s/server/node-token"):
		// A credential in the test, never in a record: what matters is that
		// the verb reads it on the SERVER rather than asking the operator.
		return sshx.Result{Stdout: "K10testtoken::server:test\n"}, nil
	case strings.Contains(command, "vgs"):
		return sshx.Result{Stdout: "  53687091200\n"}, nil
	case strings.Contains(command, "test -b "):
		return sshx.Result{}, nil
	case strings.Contains(command, "blkid -p"):
		return sshx.Result{ExitCode: 2, Stderr: "blkid: no signature detected"}, nil
	case strings.Contains(command, "curl"):
		return sshx.Result{Stdout: "https://get.k3s.io 200\nhttps://ghcr.io/v2/ 401\nhttps://charts.jetstack.io/index.yaml 200\n"}, nil
	case strings.Contains(command, "systemctl is-active"):
		return sshx.Result{Stdout: "active\n"}, nil
	case strings.Contains(command, "ss -ltnH"):
		// The port probe's cleanup asks whether the probe's own listeners are
		// gone; they are, because the fake never started any.
		return sshx.Result{Stdout: "free\n"}, nil
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

// replaceDaemonSet is kured's lock write: a compare-and-swap on the
// resourceVersion that was read, exactly as the API server is one.
func (h *fakeHost) replaceDaemonSet(raw []byte) (sshx.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return sshx.Result{ExitCode: 1, Stderr: "invalid document"}, nil
	}
	meta, _ := incoming["metadata"].(map[string]any)
	rv, _ := meta["resourceVersion"].(string)
	if rv != h.dsRV {
		return sshx.Result{ExitCode: 1, Stderr: `Error from server (Conflict): Operation cannot be fulfilled on daemonsets.apps "kured": the object has been modified`}, nil
	}
	h.rv++
	h.dsRV = strconv.Itoa(h.rv)
	meta["resourceVersion"] = h.dsRV
	h.ds = incoming
	return h.jsonResult(h.ds)
}

// writeConfigMap is the operation record's create/replace, with the same
// precondition.
func (h *fakeHost) writeConfigMap(raw []byte, create bool) (sshx.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
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

// kuredLock returns the lock annotation kured's DaemonSet currently carries.
func (h *fakeHost) kuredLock() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	meta, _ := h.ds["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	s, _ := ann["weave.works/kured-node-lock"].(string)
	return s
}

// liveRecord reads the operation record the fake cluster holds.
func (h *fakeHost) liveRecord(t *testing.T) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	obj, ok := h.objects["kubenest-operation"]
	if !ok {
		t.Fatal("no operation record was created on the cluster")
	}
	return obj
}

// fakeDialer hands out the scripted machines by address.
type fakeDialer struct {
	mu    sync.Mutex
	hosts map[string]*fakeHost
	// dials is every address a verb asked to dial, in order, INCLUDING one the
	// dialer cannot reach. Dialling is how a host key is read, and it leaves no
	// command behind, so an assertion about it needs this: "this machine was
	// never dialled", or "the machine WAS dialled, so its key was compared".
	dials []string
}

func (d *fakeDialer) dial(_ context.Context, host api.HostRecord) (Transport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials = append(d.dials, host.SSHAddress)
	if h, ok := d.hosts[host.SSHAddress]; ok {
		return h, nil
	}
	return nil, fmt.Errorf("dial tcp %s: no route to host", host.SSHAddress)
}

// dialed reports whether a verb asked for this address.
func (d *fakeDialer) dialed(address string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.dials {
		if a == address {
			return true
		}
	}
	return false
}

// fakeRecords is the cluster's record in the control plane: the bundle, the
// hosts and the revision a write must carry back.
type fakeRecords struct {
	mu      sync.Mutex
	log     *logbook
	records []upgrade.Recorded
	saved   []api.BundleRecord
	// failOn makes the Nth write (1-based) fail, so a test can watch what a
	// verb does when the inventory write does not land.
	failOn int
	// failSave makes the next write fail with this error.
	failSave error
}

func newFakeRecords(log *logbook, bundle api.ClusterBundle) *fakeRecords {
	return &fakeRecords{log: log, records: []upgrade.Recorded{{ClusterBundle: bundle}}}
}

func (f *fakeRecords) Load(context.Context) (api.ClusterBundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[len(f.records)-1].ClusterBundle, nil
}

// apiRefusal is a fake control-plane refusal that errors.As can match as an
// *api.Error while still carrying a readable message: the real client builds
// one from the response body, the fake builds one from the check it imitates.
type apiRefusal struct {
	detail string
	err    *api.Error
}

func (e apiRefusal) Error() string { return e.detail }
func (e apiRefusal) Unwrap() error { return e.err }

// Save applies the record with the checks the control plane applies, in the
// order it applies them: the BODY is validated first (FastAPI refuses a bad
// enum with 422 before the handler runs), and only then the revision
// compare-and-swap (409). A write based on a revision the record has moved
// past is REFUSED, not applied.
//
// Every host entry's `volume_group_ownership` must be one of the two enum
// values — app/schemas/cluster.py:235 requires it on EVERY entry, including a
// joining one, which is the write this fake exists to keep honest.
func (f *fakeRecords) Save(_ context.Context, record api.BundleRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A planted failure stands in for whatever the control plane answered.
	if f.failSave != nil {
		err := f.failSave
		f.failSave = nil
		return err
	}
	for i, h := range record.Hosts {
		if !validOwnership(h.VolumeGroupOwnership) {
			detail := fmt.Sprintf("hosts[%d].volume_group_ownership: Input should be 'customer-created' or 'installer-created', input: %q",
				i, h.VolumeGroupOwnership)
			return apiRefusal{
				detail: "PUT /api/v1/clusters/prod-1/bundle: [422] " + detail,
				err:    &api.Error{Status: http.StatusUnprocessableEntity, Detail: detail},
			}
		}
	}
	current := f.records[len(f.records)-1]
	if record.Revision != current.Revision {
		detail := fmt.Sprintf("the inventory has moved on: it is at revision %d, your write carried %d", current.Revision, record.Revision)
		return apiRefusal{
			detail: "PUT /api/v1/clusters/prod-1/bundle: [409] " + detail,
			err:    &api.Error{Status: http.StatusConflict, Detail: detail},
		}
	}
	f.saved = append(f.saved, record)
	// The write is journalled where an assertion about ORDER can see it: an
	// inventory write happens between the commands around it, and "the host was
	// written down before it was touched" is a claim about that order.
	if f.log != nil {
		for _, h := range record.Hosts {
			f.log.record("control-plane", "inventory-write "+h.HostID+" "+h.LifecycleState, nil)
		}
	}
	if f.failOn > 0 && len(f.saved) == f.failOn {
		return fmt.Errorf("the control plane did not accept the write (planted for this test)")
	}
	next := current.ClusterBundle
	next.BundleVersion, next.Profiles, next.HATier, next.VolumeGroupOwnership = record.BundleVersion, record.Profiles, record.HATier, record.VolumeGroupOwnership
	next.Hosts = record.Hosts
	next.Revision = current.Revision + 1
	f.records = append(f.records, upgrade.Recorded{ClusterBundle: next})
	return nil
}

// validOwnership is the backend's VolumeGroupOwnership enum.
func validOwnership(v string) bool {
	return v == string(storage.CustomerCreated) || v == string(storage.InstallerCreated)
}

// inventory is what the record holds now.
func (f *fakeRecords) inventory() []api.HostRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[len(f.records)-1].Hosts
}

// savedRecords is every inventory write, in order.
func (f *fakeRecords) savedRecords() []api.BundleRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.BundleRecord(nil), f.saved...)
}

// fakeCatalog is the control plane's offer of bundles, as preflight reads it.
type fakeCatalog struct{ entries []preflight.BundleEntry }

func (c fakeCatalog) ListBundles(context.Context) ([]preflight.BundleEntry, error) {
	return c.entries, nil
}

// fakeMetadata stands in for the S3 recovery-set refresh.
type fakeMetadata struct {
	mu      sync.Mutex
	calls   int
	err     error
	cluster string
}

func (m *fakeMetadata) Refresh(_ context.Context, cluster string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.cluster = cluster
	if m.err != nil {
		return "", m.err
	}
	return "the recovery set was refreshed", nil
}

// testManifest is the bundle the node tests run against. It carries the pins
// and the timeouts the verbs read, and no kubenest-agent pin: that gate has
// its own tests, and a missing pin must keep passing.
func testManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`
bundle: "1.1"
core:
  k3s: v1.35.7+k3s1
  openebs-lvm-localpv: 4.2.0
os:
  supported: [ubuntu-24.04]
ha-tiers: [single-server, ha]
limits:
  resources:
    floor: { cpu: 2, memory: 3.7Gi, disk: 36Gi }
    recommended: { cpu: 4, memory: 7.4Gi, disk: 92Gi }
    upgrade-headroom: { disk: 10Gi }
  timeouts:
    node-ready: 5m
    node-drain: 15m
    node-reboot: 20m
    install-total: 30m
    component-ready: 10m
health:
  backup:
    max-backup-age: 24h
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// clusterRecord is the record of a single-server cluster with one agent.
func clusterRecord(hosts ...api.HostRecord) api.ClusterBundle {
	return api.ClusterBundle{
		BundleVersion:        "1.1",
		Profiles:             []string{"observability"},
		HATier:               "single-server",
		VolumeGroupOwnership: "installer-created",
		Hosts:                hosts,
		Revision:             7,
	}
}

// serverHost and agentHost are inventory entries for the two machines the
// tests start with. They carry the volume-group ownership the control plane
// requires on EVERY host entry — a real inventory always does, and the fake
// record now refuses an entry that does not.
func serverHost() api.HostRecord {
	return api.HostRecord{
		HostID: "h-srv", Role: "server", SSHAddress: testServerAddr, SSHPort: 22, SSHUser: "ubuntu",
		NodeUID: "uid-srv", HostKeyFingerprint: "SHA256:server", JoinAddress: testServerAddr,
		StorageDevice: "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive1", LifecycleState: "active",
		VolumeGroupOwnership: string(storage.InstallerCreated),
	}
}

func agentHost() api.HostRecord {
	return api.HostRecord{
		HostID: "h-agt", Role: "agent", SSHAddress: "10.0.3.8", SSHPort: 22, SSHUser: "ubuntu",
		NodeUID: "uid-agt", HostKeyFingerprint: "SHA256:agent", JoinAddress: "https://" + testServerAddr + ":6443",
		StorageDevice: "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive2", LifecycleState: "active",
		VolumeGroupOwnership: string(storage.InstallerCreated),
	}
}

// removedAgent is the entry `node remove` keeps of a machine it took out of the
// cluster: its host ID, its address and its host key are kept as the record that
// the machine was there, and its Node object is gone. The ADDRESS is the part
// that outlives the machine — a cloud hands a freed address to the next machine
// that asks for it — which is why the fixture puts this entry at the address the
// tests add a new machine at.
func removedAgent() api.HostRecord {
	return api.HostRecord{
		HostID: "h-gone", Role: "agent", SSHAddress: testAgentAddr, SSHPort: 22, SSHUser: "ubuntu",
		NodeUID: "uid-gone", HostKeyFingerprint: "SHA256:gone", JoinAddress: "https://" + testServerAddr + ":6443",
		StorageDevice: testDevice, LifecycleState: string(StateRemoved),
		VolumeGroupOwnership: string(storage.InstallerCreated),
	}
}

// reusedAddressHost is the entry a later `node add` writes for a new machine at
// an address a removed record still holds: a host ID of its own, active, its own
// host key, and the Node object it joined as. It is what makes ONE ADDRESS CARRY
// TWO ENTRIES — the case every lookup has to answer for, and the one the lab hit
// when w5 was given w4's address.
func reusedAddressHost() api.HostRecord {
	return api.HostRecord{
		HostID: testReusedHostID, Role: "agent", SSHAddress: testAgentAddr, SSHPort: 22, SSHUser: "ubuntu",
		NodeUID: testReusedNodeUID, HostKeyFingerprint: "SHA256:newagent", JoinAddress: "https://" + testServerAddr + ":6443",
		StorageDevice: testDevice, LifecycleState: string(StateActive),
		VolumeGroupOwnership: string(storage.InstallerCreated),
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

// nodeFixture is the world one node verb runs in: the scripted machines, the
// record, and the session built on them.
type nodeFixture struct {
	t       *testing.T
	log     *logbook
	dialer  *fakeDialer
	server  *fakeHost
	agent   *fakeHost
	records *fakeRecords
	meta    *fakeMetadata
	session *Session
	journal *stages.Journal
	// journalPath is the same journal, kept so a second session (a resume from
	// another process) opens the one the first run wrote.
	journalPath string
	out         *bytes.Buffer
	// agentNodes is the machine's own answer to `kubectl get nodes`, which is
	// only used when a test asks the agent itself.
	agentNodes string
	now        time.Time
	slept      *[]time.Duration
}

// newFixture builds the session the staging engine will drive.
func newFixture(t *testing.T, hosts ...api.HostRecord) *nodeFixture {
	t.Helper()
	f := &nodeFixture{t: t, log: &logbook{}, meta: &fakeMetadata{}, out: &bytes.Buffer{}}
	f.now = time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	// The logbook carries the run's clock, so an assertion about WHEN
	// something ran is made against the same clock the verb read.
	f.log.clock = func() time.Time { return f.now }
	f.records = newFakeRecords(f.log, clusterRecord(hosts...))
	f.dialer = &fakeDialer{hosts: map[string]*fakeHost{}}

	// The server answers the cluster's API and is Ready; the machine being
	// added answers SSH but is not yet a node.
	f.server = f.addHost(testServerAddr, "SHA256:server", nodesJSON(t,
		testNode{Name: testServerNode, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
		testNode{Name: testAgentNode, UID: "uid-agt", Addresses: []string{"10.0.3.8"}, Ready: true},
	))
	f.agent = f.addHost(testAgentAddr, "SHA256:newagent", nodesJSON(t))
	f.agentNodes = nodesJSON(t)

	// A journal in a temporary directory, so a test never reads or writes the
	// operator's own state. It is opened the way the command layer opens it,
	// including the one line a replaced FINISHED journal is worth.
	f.journal = f.openJournal(t, t.TempDir()+"/journal.json")
	f.journalPath = f.journal.Path()

	slept := []time.Duration{}
	f.slept = &slept
	f.session = &Session{
		ID:      "run-1",
		Cluster: "prod-1",
		Records: f.records,
		Bundle:  testManifest(t),
		Catalog: fakeCatalog{entries: []preflight.BundleEntry{
			{Version: "1.1", HATiers: []string{"single-server", "ha"}, Profiles: []string{"observability"}},
		}},
		Jnl:  f.journal,
		Emit: stages.NopEmitter{},
		Out:  f.out,
		Now:  func() time.Time { return f.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			// The fake clock MOVES with the sleep: a verb that waits for a
			// window or an observation is exercised rather than spun on.
			f.now = f.now.Add(d)
			return nil
		},
		Poll:     time.Second,
		Dial:     f.dialer.dial,
		Metadata: f.meta,
		Store: func(runner k3s.Runner) *operation.Store {
			return &operation.Store{Runner: runner, Operator: "test@laptop"}
		},
	}
	f.session.Window = openWindow()
	return f
}

// addHost registers one more scripted machine the dialer can reach.
func (f *nodeFixture) addHost(address, fingerprint, nodesJSON string) *fakeHost {
	h := newFakeHost(f.log, address, fingerprint, nodesJSON)
	f.dialer.hosts[address] = h
	return h
}

// openWindow is a window that is open whenever it is asked: the tests that
// care about the window build a closed one themselves.
func openWindow() *window.Window {
	days := make([]time.Weekday, 0, 7)
	for d := time.Sunday; d <= time.Saturday; d++ {
		days = append(days, d)
	}
	return &window.Window{Days: days, Start: 0, End: 24 * 60, Location: time.UTC}
}

// fixtureJournalIdentity is what the fixture opens its journal with. A node
// verb's journal is per (kind, cluster); the real identity also carries the
// machine, the bundle and the storage flag, which is what a leftover journal
// from another run differs on.
func fixtureJournalIdentity() stages.Identity {
	return stages.Identity{Kind: "node-test", Cluster: "prod-1"}
}

// openJournal opens the journal the way the command layer does, printing the
// one line a replaced FINISHED journal is worth, and failing the test when the
// journal is refused.
func (f *nodeFixture) openJournal(t *testing.T, path string) *stages.Journal {
	t.Helper()
	journal, note, err := stages.OpenJournalReplacingFinished(path, fixtureJournalIdentity())
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	if note != "" {
		fmt.Fprintln(f.out, note)
	}
	return journal
}

// reopen is a second CLI invocation over the same world and the same HOME: the
// same machines and the same record, a new run id, and the journal read from
// the same path the way the command reads it. It RETURNS the open error instead
// of failing, so a test can assert on the refusal.
func (f *nodeFixture) reopen(t *testing.T, id string) (*Session, string, error) {
	t.Helper()
	journal, note, err := stages.OpenJournalReplacingFinished(f.journalPath, fixtureJournalIdentity())
	if err != nil {
		return nil, "", err
	}
	if note != "" {
		fmt.Fprintln(f.out, note)
	}
	next := *f.session
	next.ID = id
	next.Jnl = journal
	next.Window = openWindow()
	return &next, note, nil
}

// seedJournal lays down a journal file the way a previous process left it. It
// deliberately does not go through OpenJournal: the point is to hand the next
// run an identity and a set of entries that are already on disk.
func (f *nodeFixture) seedJournal(t *testing.T, identity stages.Identity, entries ...stages.Entry) {
	t.Helper()
	raw, err := json.Marshal(stages.Journal{Identity: identity, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.journalPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sessionFor returns a second session over the SAME world, which is what a
// resume from another process looks like: the record, the machines and the
// journal are the ones the first run left.
func (f *nodeFixture) sessionFor(t *testing.T, id string) *Session {
	t.Helper()
	next := *f.session
	next.ID = id
	next.Jnl = f.openJournal(t, f.journalPath)
	next.Window = openWindow()
	return &next
}

// lastState is the lifecycle state the record holds for one host.
func (f *nodeFixture) lastState(hostID string) string {
	for _, h := range f.records.inventory() {
		if h.HostID == hostID {
			return h.LifecycleState
		}
	}
	return ""
}

// hostIn returns the inventory entry for one host.
func (f *nodeFixture) hostIn(hostID string) (api.HostRecord, bool) {
	for _, h := range f.records.inventory() {
		if h.HostID == hostID {
			return h, true
		}
	}
	return api.HostRecord{}, false
}
