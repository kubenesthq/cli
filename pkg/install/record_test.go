package install_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/install"
	"kubenest.io/cli/pkg/sshx"
)

// recordAPI is a fake control plane that keeps what the record stage wrote.
//
// It is a real HTTP server rather than a mocked Client because the thing under
// test includes the wire shape: a host entry the backend cannot decode is a
// host entry that does not exist, and an in-process fake would hide exactly
// that.
type recordAPI struct {
	mu      sync.Mutex
	reads   int
	written []api.BundleRecord
	current api.ClusterBundle
}

func (r *recordAPI) serve(t *testing.T) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/bundle") {
			http.NotFound(w, req)
			return
		}
		switch req.Method {
		case http.MethodGet:
			r.mu.Lock()
			r.reads++
			current := r.current
			r.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(current)
		case http.MethodPut:
			var body api.BundleRecord
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.mu.Lock()
			r.written = append(r.written, body)
			r.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"bundle_version": body.BundleVersion, "revision": body.Revision + 1})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return client
}

func (r *recordAPI) lastWrite(t *testing.T) api.BundleRecord {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.written) == 0 {
		t.Fatal("the record stage wrote nothing")
	}
	return r.written[len(r.written)-1]
}

// recordSession is an install that has reached the record stage: two servers
// and one agent, each with a scripted connection, and a control plane whose
// record the stage reads first.
func recordSession(t *testing.T, rec *recordAPI, opts install.Options) *install.Session {
	t.Helper()
	s := sessionWithOpts(t, opts, &recorder{})
	s.Jnl.ClusterID = "0199a8a0-0000-7000-8000-000000000001"
	s.API = rec.serve(t)
	s.Nodes = []install.Node{
		{Address: opts.Servers[0], Role: install.RoleServer, Runner: nodeRunner(t)},
		{Address: opts.Agents[0], Role: install.RoleAgent, Runner: nodeRunner(t)},
	}
	return s
}

// nodeRunner answers the one read the record stage makes of the cluster:
// `kubectl get nodes -o json`. The addresses differ from the node names on
// purpose — the mapping from "the address the operator typed" to "the Node
// object's UID" is the part that can silently come back empty.
func nodeRunner(t *testing.T) *componenttest.FakeRunner {
	t.Helper()
	payload := `{"items":[
	  {"metadata":{"name":"cp-1","uid":"uid-cp-1"},
	   "status":{"addresses":[{"type":"InternalIP","address":"10.0.1.10"}]}},
	  {"metadata":{"name":"worker-1","uid":"uid-worker-1"},
	   "status":{"addresses":[{"type":"InternalIP","address":"10.0.1.20"}]}}
	]}`
	return &componenttest.FakeRunner{Respond: func(cmd string) (sshx.Result, error) {
		if strings.Contains(cmd, "get nodes -o json") {
			return sshx.Result{Stdout: payload}, nil
		}
		return sshx.Result{ExitCode: 127, Stderr: "unexpected command: " + cmd}, nil
	}}
}

func runRecordStage(t *testing.T, s *install.Session) error {
	t.Helper()
	for _, stage := range install.Plan(s) {
		if stage.Name == install.StageRecord {
			return stage.Run(context.Background())
		}
	}
	t.Fatal("the plan has no record stage")
	return nil
}

func recordOptions() install.Options {
	return install.Options{
		Bundle:        "1.0",
		Name:          "prod-1",
		Servers:       []string{"10.0.1.10"},
		Agents:        []string{"10.0.1.20"},
		HATier:        "single-server",
		Profiles:      []string{"observability"},
		SSHUser:       "kubenest",
		StorageDevice: "/dev/disk/by-id/nvme-eui.0000000000000001",
	}
}

// The record stage is the one moment a cluster's hosts are all known at once,
// so it is where the inventory is written. A host the record does not name is
// a host no node verb can find later.
func TestStageRecordWritesEveryHostWithItsRoleAndStoragePath(t *testing.T) {
	rec := &recordAPI{}
	s := recordSession(t, rec, recordOptions())

	if err := runRecordStage(t, s); err != nil {
		t.Fatalf("record stage: %v", err)
	}
	written := rec.lastWrite(t)

	if len(written.Hosts) != 2 {
		t.Fatalf("the record carries %d hosts, want one per --server and per --agent (2): %+v",
			len(written.Hosts), written.Hosts)
	}
	byAddress := map[string]api.HostRecord{}
	ids := map[string]bool{}
	for _, host := range written.Hosts {
		byAddress[host.SSHAddress] = host
		if host.HostID == "" {
			t.Errorf("host %s has no host ID: the inventory's identity is what every node operation names its target by", host.SSHAddress)
		}
		if ids[host.HostID] {
			t.Errorf("two hosts share the host ID %q; IDs are never reused", host.HostID)
		}
		ids[host.HostID] = true
	}

	server, ok := byAddress["10.0.1.10"]
	if !ok {
		t.Fatalf("the record has no entry for the server 10.0.1.10: %+v", written.Hosts)
	}
	agent, ok := byAddress["10.0.1.20"]
	if !ok {
		t.Fatalf("the record has no entry for the agent 10.0.1.20: %+v", written.Hosts)
	}
	if server.Role != "server" || agent.Role != "agent" {
		t.Errorf("roles = %q/%q, want server/agent", server.Role, agent.Role)
	}
	for _, host := range []api.HostRecord{server, agent} {
		if host.StorageDevice != "/dev/disk/by-id/nvme-eui.0000000000000001" {
			t.Errorf("host %s records storage device %q, want the --storage-device by-id path",
				host.SSHAddress, host.StorageDevice)
		}
		if host.LifecycleState != "active" {
			t.Errorf("host %s records lifecycle %q, want active: the install has finished with it",
				host.SSHAddress, host.LifecycleState)
		}
		if host.SSHUser != "kubenest" {
			t.Errorf("host %s records SSH user %q, want the --ssh-user", host.SSHAddress, host.SSHUser)
		}
		if host.JoinAddress == "" {
			t.Errorf("host %s records no join address", host.SSHAddress)
		}
	}
	// The Node UID is read from the cluster, by ADDRESS: the node's name is
	// not the address the operator typed.
	if server.NodeUID != "uid-cp-1" {
		t.Errorf("server Node UID = %q, want uid-cp-1 (read from `kubectl get nodes -o json`)", server.NodeUID)
	}
	if agent.NodeUID != "uid-worker-1" {
		t.Errorf("agent Node UID = %q, want uid-worker-1", agent.NodeUID)
	}
}

// A write that carried a revision it did not read is refused by the control
// plane, so the stage reads the record first and sends what it found. Assuming
// 0 would break every resume and every cluster whose record has moved on.
func TestStageRecordSendsTheRevisionItRead(t *testing.T) {
	rec := &recordAPI{current: api.ClusterBundle{BundleVersion: "1.0", Revision: 7}}
	s := recordSession(t, rec, recordOptions())

	if err := runRecordStage(t, s); err != nil {
		t.Fatalf("record stage: %v", err)
	}
	if got := rec.lastWrite(t).Revision; got != 7 {
		t.Errorf("the record write carries revision %d, want 7 — the revision it read", got)
	}
	rec.mu.Lock()
	reads := rec.reads
	rec.mu.Unlock()
	if reads == 0 {
		t.Error("the record stage never read the record, so it cannot know which revision its write is based on")
	}
}
