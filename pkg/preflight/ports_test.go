package preflight_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	"kubenest.io/cli/pkg/component/componenttest"
	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/sshx"
)

// probeWorld is the wire between the fake hosts' probe scripts. A target's
// packet capture can only see a datagram a peer actually sent, so the peer
// records what it sent and the target reads it back. Nothing in these tests
// needs to know the token's shape — only that the same string leaves the peer
// and reaches the target.
type probeWorld struct {
	mu     sync.Mutex
	sent   map[string][]string
	hosts  map[string]*probeHost
	tokens int
}

func newProbeWorld() *probeWorld {
	return &probeWorld{sent: map[string][]string{}, hosts: map[string]*probeHost{}}
}

func (w *probeWorld) register(h *probeHost) { w.hosts[h.addr] = h }

// holds is the kernel's behavior, not the controller's: a peer that sends a
// plain datagram to a port Flannel owns on the target gets nothing back,
// whether or not the controller remembered to say so.
func (w *probeWorld) holds(target, spec string) bool {
	h := w.hosts[target]
	return h != nil && h.held[spec]
}

func (w *probeWorld) remember(target, token string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent[target] = append(w.sent[target], token)
}

// next makes every token a peer sends distinct, the way a nonce does on a real
// host.
func (w *probeWorld) next() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tokens++
	return w.tokens
}

func (w *probeWorld) received(target string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.sent[target]...)
}

// probeHost emulates, at the SSH boundary, what the probe scripts do on a real
// host. It models exactly the three things the controller cannot fake away:
// which ports the listener can bind, whether this host runs Flannel's VXLAN
// device, and whether a datagram sent to a port Flannel owns is swallowed
// rather than echoed. A host is both a target and a peer in these tests, so one
// responder answers both roles.
type probeHost struct {
	addr       string
	role       string
	world      *probeWorld
	specs      []string        // what preflight asks of this node ("udp:8472", ...)
	held       map[string]bool // the ports something else already owns on this node
	flannel    string          // flannel.1's address; empty: this host has no overlay
	pingOK     bool            // as a peer: its overlay ping to the other node's flannel.1 answers
	echoOK     bool            // as a peer: the echo from a port the listener bound comes back
	tcpBlocked bool            // as a peer: connecting to the target's held TCP services times out
	capture    bool            // as a target: its packet capture sees the peer's datagram
	from       string          // the source address the capture records
	overrides  map[string]sshx.Result

	runner *componenttest.FakeRunner
}

// runner is this host's connection, created once: the commands a test asserts
// on are the ones preflight itself ran, not a second empty fake.
func (h *probeHost) connection() *componenttest.FakeRunner {
	if h.runner == nil {
		h.runner = &componenttest.FakeRunner{Respond: h.respond}
	}
	return h.runner
}

func (h *probeHost) respond(cmd string) (sshx.Result, error) {
	switch {
	case strings.Contains(cmd, "# kubenest-port-listener"):
		// Started in the background; the report is read by its own command.
		return sshx.Result{}, nil
	case strings.Contains(cmd, "kubenest-probe"):
		return sshx.Result{Stdout: h.peerVerdicts(cmd)}, nil
	case strings.Contains(cmd, "# kubenest-port-capture"):
		return sshx.Result{}, nil
	case strings.Contains(cmd, ".capture"):
		return sshx.Result{Stdout: h.captureReport()}, nil
	case strings.Contains(cmd, "^done$"):
		return sshx.Result{Stdout: h.listenerReport()}, nil
	case strings.Contains(cmd, "addr show flannel.1"):
		return sshx.Result{Stdout: h.flannel + "\n"}, nil
	case strings.Contains(cmd, "ss -ltnH"):
		return sshx.Result{Stdout: "free\n"}, nil
	}
	return healthyHost(h.overrides)(cmd)
}

func (h *probeHost) listenerReport() string {
	var b strings.Builder
	for _, spec := range h.specs {
		proto, port, _ := strings.Cut(spec, ":")
		state := "bound"
		if h.held[spec] {
			state = "held"
		}
		fmt.Fprintf(&b, "%s %s %s\n", proto, port, state)
	}
	b.WriteString("done\n")
	return b.String()
}

func (h *probeHost) captureReport() string {
	if !h.capture {
		return ""
	}
	var b strings.Builder
	b.WriteString("# what arrived\n")
	for _, token := range h.world.received(h.addr) {
		fmt.Fprintf(&b, "%s %s\n", h.from, token)
	}
	return b.String()
}

// targetIn is the host address the probe command is aimed at: the only
// registered address quoted in it.
func (w *probeWorld) targetIn(cmd string) string {
	for addr := range w.hosts {
		if strings.Contains(cmd, "'"+addr+"'") {
			return addr
		}
	}
	return ""
}

// specPattern finds the ports the controller asked this peer to prove. It is
// matched against the whole command, so it works whatever shape the probe
// command has — the script itself carries no literal "tcp:<port>".
var specPattern = regexp.MustCompile(`'(tcp|udp):([0-9]+)(:([a-z]+))?'`)

// peerVerdicts is the peer-side script: "<proto>:<port> open|blocked", or
// "<proto>:<port> sent <token>" when the proof is a datagram the target has to
// witness.
func (h *probeHost) peerVerdicts(cmd string) string {
	target := h.world.targetIn(cmd)
	if target == "" {
		return ""
	}
	var out []string
	for _, m := range specPattern.FindAllStringSubmatch(cmd, -1) {
		proto, port, marker := m[1], m[2], m[4]
		switch {
		case proto == "tcp":
			if h.tcpBlocked {
				out = append(out, fmt.Sprintf("tcp:%s blocked", port))
			} else {
				out = append(out, fmt.Sprintf("tcp:%s open", port))
			}
		case marker == "flannel" && h.flannel == "":
			// A peer with no overlay cannot ping: it sends and the target's
			// capture has to see it.
			out = append(out, h.send(target, port))
		case marker == "flannel" && h.pingOK:
			out = append(out, fmt.Sprintf("udp:%s open", port))
		case marker == "flannel":
			out = append(out, fmt.Sprintf("udp:%s blocked", port))
		case marker != "":
			out = append(out, h.send(target, port))
		case h.world.holds(target, proto+":"+port):
			out = append(out, fmt.Sprintf("udp:%s blocked", port))
		case h.echoOK:
			out = append(out, fmt.Sprintf("udp:%s open", port))
		default:
			out = append(out, fmt.Sprintf("udp:%s blocked", port))
		}
	}
	return strings.Join(out, "\n")
}

func (h *probeHost) send(target, port string) string {
	token := fmt.Sprintf("token-%s-%s-%d", target, port, h.world.next())
	h.world.remember(target, token)
	return fmt.Sprintf("udp:%s sent %s", port, token)
}

func (h *probeHost) node() preflight.Node {
	return preflight.Node{
		Address: h.addr, Role: h.role,
		Runner:            h.connection(),
		ExistingK3sIsOurs: h.overrides != nil,
		StorageIsOurs:     h.overrides != nil,
	}
}

func probeOptions(t *testing.T, hosts ...*probeHost) preflight.Options {
	t.Helper()
	opts := preflight.Options{
		Bundle: testManifest(t), BundleVersion: "1.0", HATier: "single-server",
		Egress:  []preflight.EgressTarget{{Name: "container registry", URL: "https://ghcr.io/v2/"}},
		Catalog: fakeCatalog{entries: []preflight.BundleEntry{{Version: "1.0", HATiers: []string{"single-server", "ha"}}}},
	}
	for _, h := range hosts {
		h.world.register(h)
		opts.Nodes = append(opts.Nodes, h.node())
	}
	return opts
}

// portResults is every node-to-node port verdict in the report, in order.
func portResults(rep preflight.Report) []preflight.Result {
	var out []preflight.Result
	for _, r := range rep.Results {
		if r.Check == preflight.CheckPorts {
			out = append(out, r)
		}
	}
	return out
}

func requirePortsPass(t *testing.T, rep preflight.Report, err error) {
	t.Helper()
	results := portResults(rep)
	if len(results) == 0 {
		t.Fatal("the node-to-node ports check did not run")
	}
	for _, r := range results {
		if r.Outcome != preflight.Pass {
			t.Errorf("node-to-node ports on %s: %s %s", r.Node, r.Outcome, r.Detail)
		}
	}
	if err != nil {
		t.Fatalf("the host must pass:\n%v", err)
	}
}

// A re-run of the install, or a node add against an existing server: the
// target already runs Flannel, whose VXLAN device owns UDP 8472, so the
// listener cannot bind it and the daemon behind it answers no probe. The check
// must prove the port a different way rather than refuse a cluster whose
// overlay works.
//
// Case (a): the peer is itself a cluster member, so the proof is end to end
// through the overlay — the peer pings the target's flannel.1 address, which
// rides the very VXLAN path UDP 8472 carries.
func TestHeldUDPPortIsProvenFromAClusterPeer(t *testing.T) {
	world := newProbeWorld()
	server := &probeHost{
		addr: "10.0.1.10", role: "server", world: world,
		specs:     []string{"tcp:6443", "udp:8472", "tcp:10250"},
		held:      map[string]bool{"tcp:6443": true, "udp:8472": true, "tcp:10250": true},
		flannel:   "10.42.0.1",
		pingOK:    true,
		overrides: map[string]sshx.Result{"command -v": {Stdout: "k3s\ncontainerd\n"}},
	}
	agent := &probeHost{
		addr: "10.0.1.11", role: "agent", world: world,
		specs:     []string{"udp:8472", "tcp:10250"},
		held:      map[string]bool{"udp:8472": true, "tcp:10250": true},
		flannel:   "10.42.0.2",
		pingOK:    true,
		overrides: map[string]sshx.Result{"command -v": {Stdout: "k3s\ncontainerd\n"}},
	}

	rep, err := preflight.Run(context.Background(), probeOptions(t, server, agent))
	requirePortsPass(t, rep, err)

	// The proof the peers used is the overlay one: they never had to send a
	// datagram for the target to witness.
	if got := world.received(server.addr); len(got) != 0 {
		t.Errorf("a cluster peer should prove 8472 through the overlay, not by arrival: %v", got)
	}
}

// Case (b): the peer is a new host with no Flannel — node add sending to the
// existing server. There is no overlay to ping, so the proof is that the
// peer's datagram is seen to ARRIVE at the target: the target captures it,
// keyed on the peer and a nonce payload, and the port passes only if it did.
func TestNodeAddPreflightPassesWhenTheServerHoldsTheVXLANPort(t *testing.T) {
	world := newProbeWorld()
	server := &probeHost{
		addr: "5.75.252.113", role: "server", world: world,
		specs:     []string{"tcp:6443", "udp:8472", "tcp:10250"},
		held:      map[string]bool{"udp:8472": true},
		flannel:   "10.42.0.1",
		capture:   true,
		from:      "178.105.10.26",
		echoOK:    true, // the new host's own 8472 listener answers the server
		overrides: map[string]sshx.Result{"command -v": {Stdout: "k3s\ncontainerd\n"}},
	}
	agent := &probeHost{
		addr: "178.105.10.26", role: "agent", world: world,
		specs:   []string{"udp:8472", "tcp:10250"},
		held:    map[string]bool{},
		flannel: "",
		echoOK:  true, // the new host's own 8472 listener echoes
	}

	opts := probeOptions(t, server, agent)
	rep, err := preflight.Run(context.Background(), opts)
	requirePortsPass(t, rep, err)

	// The peer really did take the arrival path, and the target really did
	// witness it: a pass that never exercised the proof would be trust.
	var proved bool
	for _, cmd := range agent.connection().Commands() {
		if strings.Contains(cmd, "kubenest-probe") && strings.Contains(cmd, ":flannel") {
			proved = true
		}
	}
	if !proved {
		t.Error("the new host was never asked to prove UDP 8472")
	}
	if got := world.received(server.addr); len(got) == 0 {
		t.Error("the new host sent nothing for the server's capture to see")
	}
}

// The other half of the same rule: a held UDP port is not a pass on trust. If
// the proof does not come back, the node is refused — and the refusal names the
// real cause (Flannel holds the port on a running node) rather than the vague
// "blocked" the re-run install was refused with.
func TestHeldUDPPortWithNoArrivalProofIsRefused(t *testing.T) {
	t.Run("the target's capture never sees the datagram", func(t *testing.T) {
		world := newProbeWorld()
		server := &probeHost{
			addr: "5.75.252.113", role: "server", world: world,
			specs:     []string{"tcp:6443", "udp:8472", "tcp:10250"},
			held:      map[string]bool{"udp:8472": true},
			flannel:   "10.42.0.1",
			capture:   false, // the datagram is dropped before the target's capture
			echoOK:    true,
			overrides: map[string]sshx.Result{"command -v": {Stdout: "k3s\ncontainerd\n"}},
		}
		agent := &probeHost{
			addr: "178.105.10.26", role: "agent", world: world,
			specs:   []string{"udp:8472", "tcp:10250"},
			held:    map[string]bool{},
			flannel: "",
			echoOK:  true,
		}

		rep, err := preflight.Run(context.Background(), probeOptions(t, server, agent))
		if err == nil {
			t.Fatal("a held UDP port nobody could prove open must be refused")
		}
		res := portResults(rep)
		if len(res) == 0 || res[0].Outcome != preflight.Fail {
			t.Fatalf("want the port check to fail, got %+v", res)
		}
		for _, want := range []string{"Flannel", "holds this port"} {
			if !strings.Contains(res[0].Detail, want) {
				t.Errorf("the refusal must name why the port could not be proven (%q), got %q", want, res[0].Detail)
			}
		}
	})

	t.Run("a cluster peer's overlay ping goes unanswered", func(t *testing.T) {
		world := newProbeWorld()
		server := &probeHost{
			addr: "10.0.1.10", role: "server", world: world,
			specs:     []string{"tcp:6443", "udp:8472", "tcp:10250"},
			held:      map[string]bool{"udp:8472": true},
			flannel:   "10.42.0.1",
			overrides: map[string]sshx.Result{"command -v": {Stdout: "k3s\ncontainerd\n"}},
		}
		agent := &probeHost{
			addr: "10.0.1.11", role: "agent", world: world,
			specs:     []string{"udp:8472", "tcp:10250"},
			held:      map[string]bool{"udp:8472": true},
			flannel:   "10.42.0.2",
			pingOK:    false, // the overlay between the two nodes is closed
			overrides: map[string]sshx.Result{"command -v": {Stdout: "k3s\ncontainerd\n"}},
		}

		rep, err := preflight.Run(context.Background(), probeOptions(t, server, agent))
		if err == nil {
			t.Fatal("a held UDP port the overlay cannot carry must be refused")
		}
		res := portResults(rep)
		if len(res) == 0 || res[0].Outcome != preflight.Fail {
			t.Fatalf("want the port check to fail, got %+v", res)
		}
		for _, want := range []string{"Flannel", "holds this port"} {
			if !strings.Contains(res[0].Detail, want) {
				t.Errorf("the refusal must name why the port could not be proven (%q), got %q", want, res[0].Detail)
			}
		}
	})
}

// A port nobody holds is still proven the plain way: the listener binds it and
// the peer's datagram must come back. An unanswered echo is a refusal, and this
// guards against a change that would let the new held-port path swallow it.
func TestUnansweredDatagramToAFreeUDPPortIsRefused(t *testing.T) {
	world := newProbeWorld()
	server := &probeHost{
		addr: "10.0.1.10", role: "server", world: world,
		specs:  []string{"tcp:6443", "udp:8472", "tcp:10250"},
		held:   map[string]bool{},
		echoOK: true,
	}
	agent := &probeHost{
		addr: "10.0.1.11", role: "agent", world: world,
		specs:  []string{"udp:8472", "tcp:10250"},
		held:   map[string]bool{},
		echoOK: false, // the peer's datagram to the target is dropped
	}

	rep, err := preflight.Run(context.Background(), probeOptions(t, server, agent))
	if err == nil {
		t.Fatal("an unanswered datagram to a free UDP port must be refused")
	}
	res := portResults(rep)
	if len(res) == 0 || res[0].Outcome != preflight.Fail {
		t.Fatalf("want the port check to fail, got %+v", res)
	}
}

// entryFor returns the part of a failure summary that describes one port. The
// summary is a "; "-joined list, one entry per peer-to-target port.
func entryFor(summary, spec string) string {
	for _, entry := range strings.Split(summary, "; ") {
		if strings.Contains(entry, spec) {
			return entry
		}
	}
	return ""
}

// A held TCP port is not a Flannel fact. 6443 and 10250 are held by k3s on a
// running node, and a held TCP port is proven by connecting to the real
// service — so when that connect is blocked it is plain "blocked", whether or
// not the port is held. Only a held Flannel UDP port may be blamed on Flannel.
//
// Seen on hardware: a node add from a host the cluster firewall blocks was
// told "the target already runs Flannel, which holds these ports ... tcp:6443
// (Kubernetes API): the target already holds this port", which sends the
// operator to look for Flannel on ports k3s owns.
func TestBlockedHeldTCPPortsAreNotBlamedOnFlannel(t *testing.T) {
	world := newProbeWorld()
	server := &probeHost{
		addr: "5.75.252.113", role: "server", world: world,
		specs:     []string{"tcp:6443", "udp:8472", "tcp:10250"},
		held:      map[string]bool{"tcp:6443": true, "udp:8472": true, "tcp:10250": true},
		flannel:   "10.42.0.1",
		echoOK:    true,
		capture:   false, // the datagram is dropped before the target's capture
		overrides: map[string]sshx.Result{"command -v": {Stdout: "k3s\ncontainerd\n"}},
	}
	agent := &probeHost{
		addr: "167.233.20.250", role: "agent", world: world,
		specs:      []string{"udp:8472", "tcp:10250"},
		held:       map[string]bool{},
		flannel:    "",
		tcpBlocked: true, // the cluster firewall drops the peer's TCP connects
		echoOK:     true,
	}

	rep, err := preflight.Run(context.Background(), probeOptions(t, server, agent))
	if err == nil {
		t.Fatal("a host whose ports are all blocked must be refused")
	}
	res := portResults(rep)
	if len(res) == 0 || res[0].Outcome != preflight.Fail {
		t.Fatalf("want the port check to fail, got %+v", res)
	}
	detail := res[0].Detail
	if !strings.HasPrefix(detail, "blocked:") {
		t.Errorf("a failure that is not all Flannel-held UDP must be summarised as blocked, got %q", detail)
	}
	for _, spec := range []string{"tcp:6443", "tcp:10250"} {
		entry := entryFor(detail, spec)
		if entry == "" {
			t.Fatalf("no entry for %s in %q", spec, detail)
		}
		for _, wrong := range []string{"Flannel", "holds"} {
			if strings.Contains(entry, wrong) {
				t.Errorf("a blocked TCP port must not be blamed on %q: %q", wrong, entry)
			}
		}
	}
	if entry := entryFor(detail, "udp:8472"); !strings.Contains(entry, "Flannel") {
		t.Errorf("the unproven held UDP port must still name Flannel, got %q", entry)
	}
}
