package preflight

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The node-to-node ports from install.mdx's table. Every one of them is a
// cluster that half-works if it is blocked: a closed 8472 gives pods that
// cannot reach pods on another node, which surfaces hours later as an
// application bug rather than as an install failure.
type portSpec struct {
	Port    int
	Proto   string // "tcp" or "udp"
	Purpose string
	// HAOnly marks etcd peer replication, which exists only on the 3-node tier.
	HAOnly bool
	// AgentsToServer marks traffic that only ever goes one way.
	AgentsToServer bool
	// Overlay marks the port Flannel's VXLAN device carries. On a node already
	// running k3s the kernel holds it in a device, not a process, so no probe
	// is ever answered there and it has to be proven another way.
	Overlay bool
}

const flannelPurpose = "Flannel VXLAN overlay"

var nodePorts = []portSpec{
	{Port: 6443, Proto: "tcp", Purpose: "Kubernetes API", AgentsToServer: true},
	{Port: 8472, Proto: "udp", Purpose: flannelPurpose, Overlay: true},
	{Port: 10250, Proto: "tcp", Purpose: "Kubelet metrics"},
	{Port: 2379, Proto: "tcp", Purpose: "etcd client", HAOnly: true},
	{Port: 2380, Proto: "tcp", Purpose: "etcd peer replication", HAOnly: true},
}

// listenerWindow is how long a target node holds its probe listeners open.
// Long enough for every peer to connect, short enough that an abandoned
// preflight leaves nothing running.
const listenerWindow = 25 * time.Second

// listenerMarker identifies the probe's processes so they can be stopped by
// name when it is done. It has to be distinctive: pkill against a loose
// pattern on a customer's node is not something to be casual about.
const listenerMarker = "kubenest-preflight-port-probe"

// markerMatch is listenerMarker written so that pkill -f — whose own command
// line carries the pattern — does not match, and kill, itself.
const markerMatch = "[k]ubenest-preflight-port-probe"

// noncePrefix tags every datagram the arrival proof sends, so a capture on a
// live port picks the probe out of whatever else is talking on it.
const noncePrefix = "kubenest-portprobe:"

// reportPaths are where a target's listener and capture report what they saw.
// They are per run, so a file left by an interrupted probe can never be read
// as this run's evidence.
func reportPaths(nonce string) (ports, capture string) {
	return "/tmp/" + listenerMarker + "." + nonce + ".ports",
		"/tmp/" + listenerMarker + "." + nonce + ".capture"
}

// checkPorts proves the node-to-node paths are open, by actually opening a
// listener on the target and connecting to it from every peer.
//
// A plain connect test before k3s exists cannot distinguish "blocked" from
// "nothing listening yet", so it would prove nothing. This binds the real
// port numbers on the real hosts and speaks to them.
//
// A single-node install has no node-to-node traffic, and says so rather than
// silently passing an unrun check.
func checkPorts(ctx context.Context, opts Options, rep *Report) {
	reachable := make([]Node, 0, len(opts.Nodes))
	for _, n := range opts.Nodes {
		if n.Runner != nil && n.DialErr == nil {
			reachable = append(reachable, n)
		}
	}
	if len(reachable) < 2 {
		rep.add(Result{
			Check: CheckPorts, Outcome: Pass,
			Detail: "single node: no node-to-node traffic to check",
		})
		return
	}

	for _, target := range reachable {
		specs := portsFor(opts.HATier, target)
		if len(specs) == 0 {
			continue
		}
		peers := peersOf(reachable, target, specs)
		if len(peers) == 0 {
			continue
		}
		probeOneTarget(ctx, opts, target, peers, specs, rep)
	}
}

// portsFor is which ports must be open ON this node, given its role and the
// tier.
func portsFor(tier string, target Node) []portSpec {
	var out []portSpec
	for _, spec := range nodePorts {
		if spec.HAOnly && (tier != "ha" || target.Role != "server") {
			continue
		}
		if spec.AgentsToServer && target.Role != "server" {
			continue
		}
		out = append(out, spec)
	}
	return out
}

// peersOf is which nodes must be able to reach the target. etcd peers are
// servers only; the API server is reached by everyone else.
func peersOf(all []Node, target Node, specs []portSpec) []Node {
	needsServerOnly := true
	for _, s := range specs {
		if !s.HAOnly {
			needsServerOnly = false
		}
	}
	var out []Node
	for _, n := range all {
		if n.Address == target.Address {
			continue
		}
		if needsServerOnly && n.Role != "server" {
			continue
		}
		out = append(out, n)
	}
	return out
}

func probeOneTarget(ctx context.Context, opts Options, target Node, peers []Node, specs []portSpec, rep *Report) {
	var tcp, udp []string
	for _, s := range specs {
		if s.Proto == "tcp" {
			tcp = append(tcp, strconv.Itoa(s.Port))
			continue
		}
		udp = append(udp, strconv.Itoa(s.Port))
	}

	nonce := probeNonce()
	portsReport, captureReport := reportPaths(nonce)

	// Start the listeners. Ports already in use are NOT an error: on a
	// resumed install k3s itself owns 6443 and 10250 and Flannel's VXLAN
	// device owns UDP 8472. Which ones those are decides how they can still
	// be proven, so the listener reports them back.
	start := fmt.Sprintf("nohup python3 -c %s %s %s %s >%s 2>/dev/null & echo started",
		shellQuote(listenerScript), shellQuote(strings.Join(tcp, ",")),
		shellQuote(strings.Join(udp, ",")), shellQuote(listenerMarker), shellQuote(portsReport))
	if _, err := run(ctx, target.Runner, start); err != nil {
		rep.add(Result{
			Check: CheckPorts, Node: target.Address, Outcome: Fail,
			Detail: "could not start the port probe on this node: " + err.Error(),
			Fix:    "python3 must be present (it ships on the Ubuntu 24.04 cloud image); without it the node-to-node ports cannot be proven open",
		})
		return
	}
	// Give the listeners a moment to bind before anyone connects.
	select {
	case <-ctx.Done():
		return
	case <-time.After(750 * time.Millisecond):
	}

	// The listeners MUST be gone before anything installs, because the ports
	// they hold are the ports k3s binds. Observed on a real host: the probe
	// still owned 6443 when the k3s stage started, and k3s died with
	// "address already in use" — an install failed by its own preflight.
	defer stopListeners(context.WithoutCancel(ctx), target, specs, portsReport, captureReport)

	held := readPortReport(ctx, target, portsReport)

	// A UDP port the listener could not bind is not a pass on trust. A peer
	// that is already a cluster member proves it end to end through the
	// overlay; a peer that is not has to be seen to reach the target, so the
	// target captures what arrives. The capture is only started when some
	// unbound port is UDP.
	if heldUDP := heldUDPPorts(held, specs); len(heldUDP) > 0 {
		startCapture(ctx, target, heldUDP, nonce, captureReport)
	}

	flannelIP := ""
	for _, s := range specs {
		if held[portKey(s)] && s.Overlay {
			flannelIP = flannelAddress(ctx, target)
			break
		}
	}

	var failures []portFailure
	var pending []pendingProof
	for i, peer := range peers {
		args := make([]string, 0, len(specs))
		for _, s := range specs {
			spec := s.Proto + ":" + strconv.Itoa(s.Port)
			if held[portKey(s)] {
				if s.Overlay {
					spec += ":flannel"
				} else {
					spec += ":held"
				}
			}
			args = append(args, shellQuote(spec))
		}
		connect := fmt.Sprintf("python3 -c %s %s %s %s %s %s",
			shellQuote(clientScript), shellQuote(target.Address), shellQuote(orDash(flannelIP)),
			shellQuote(noncePrefix+nonce), shellQuote(strconv.Itoa(i)), strings.Join(args, " "))
		out, err := run(ctx, peer.Runner, connect)
		if err != nil {
			failures = append(failures, portFailure{text: fmt.Sprintf("%s -> %s: probe failed (%v)", peer.Address, target.Address, err)})
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			spec, verdict := fields[0], fields[1]
			switch verdict {
			case "open":
				continue
			case "sent":
				if len(fields) < 3 {
					failures = append(failures, blockedPort(peer, target, spec, held, "the arrival proof produced no nonce"))
					continue
				}
				pending = append(pending, pendingProof{peer: peer, spec: spec, token: fields[2]})
			default:
				failures = append(failures, blockedPort(peer, target, spec, held, ""))
			}
		}
	}

	// The arrival proof is a proof only once the target has seen the datagram.
	if len(pending) > 0 {
		settle(ctx)
		seen := capturedTokens(ctx, target, captureReport)
		for _, p := range pending {
			if seen[p.token] {
				continue
			}
			failures = append(failures, blockedPort(p.peer, target, p.spec, held, "the target never saw the probe datagram arrive"))
		}
	}

	if len(failures) > 0 {
		rep.add(Result{
			Check: CheckPorts, Node: target.Address, Outcome: Fail,
			Detail: blockedSummary(failures),
			Fix:    "open these between the cluster nodes — a blocked overlay or kubelet port produces a cluster that installs and then misbehaves, which is far more expensive than a refused install",
		})
		return
	}
	detail := fmt.Sprintf("reachable from %d peer(s) on %s", len(peers), describePorts(specs))
	if len(held) > 0 {
		detail += fmt.Sprintf("; %d port(s) already held on this host were proven from every peer", len(held))
	}
	rep.add(Result{Check: CheckPorts, Node: target.Address, Outcome: Pass, Detail: detail})
}

// pendingProof is a peer that sent its probe datagram and whose proof now
// rests on the target's capture having seen it.
type pendingProof struct {
	peer  Node
	spec  string
	token string
}

// portFailure is one peer-to-target port that could not be proven open.
type portFailure struct {
	text string
	// flannel marks a failure on the port the target's own Flannel VXLAN
	// device holds. Only that kind may be blamed on Flannel.
	flannel bool
}

// blockedPort names one failure. Only a port the target's own Flannel VXLAN
// device holds is blamed on Flannel. 6443 and 10250 are held by k3s on a
// running node, and a held TCP port is proven by connecting to the real
// service, so a blocked TCP port is plain "blocked" whether or not it is held.
func blockedPort(peer, target Node, spec string, held map[string]bool, why string) portFailure {
	msg := fmt.Sprintf("%s -> %s %s (%s)", peer.Address, target.Address, spec, purposeOf(spec))
	flannel := held[spec] && purposeOf(spec) == flannelPurpose
	if flannel {
		msg += ": the target already runs Flannel, whose VXLAN device holds this port, so no process there can answer a probe"
	}
	if why != "" {
		msg += " — " + why
	}
	return portFailure{text: msg, flannel: flannel}
}

// blockedSummary leads with the cause only when EVERY failure is a port the
// target's Flannel holds: "blocked" alone sent an operator to look at a
// firewall that was never the problem, and blaming Flannel for a service k3s
// owns sends them somewhere else that is also wrong.
func blockedSummary(failures []portFailure) string {
	allFlannel := len(failures) > 0
	texts := make([]string, 0, len(failures))
	for _, f := range failures {
		if !f.flannel {
			allFlannel = false
		}
		texts = append(texts, f.text)
	}
	if allFlannel {
		return "the target already runs Flannel, which holds these ports, and the overlay path to them is not open: " + strings.Join(texts, "; ")
	}
	return "blocked: " + strings.Join(texts, "; ")
}

// portKey is how a spec is named between the listener's report, the peer's
// verdicts and the held set: "udp:8472".
func portKey(s portSpec) string { return s.Proto + ":" + strconv.Itoa(s.Port) }

// heldUDPPorts is the numbers of the UDP ports the target's listener could not
// bind.
func heldUDPPorts(held map[string]bool, specs []portSpec) []string {
	var out []string
	for _, s := range specs {
		if s.Proto == "udp" && held[portKey(s)] {
			out = append(out, strconv.Itoa(s.Port))
		}
	}
	return out
}

// readPortReport reads which ports the target's listener could not bind. It
// waits for the listener's completion marker, because a report read half
// written would present an unbound port as free.
func readPortReport(ctx context.Context, target Node, path string) map[string]bool {
	held := map[string]bool{}
	wait := fmt.Sprintf("for i in $(seq 1 30); do grep -q '^done$' %s 2>/dev/null && break; sleep 0.1; done; cat %s 2>/dev/null || true",
		shellQuote(path), shellQuote(path))
	out, err := run(ctx, target.Runner, wait)
	if err != nil {
		return held
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[2] == "held" {
			held[fields[0]+":"+fields[1]] = true
		}
	}
	return held
}

// capturedTokens is the nonces a target's packet capture saw arrive, from the
// source addresses it saw them from.
func capturedTokens(ctx context.Context, target Node, path string) map[string]bool {
	tokens := map[string]bool{}
	out, err := run(ctx, target.Runner,
		"sudo -n cat "+shellQuote(path)+" 2>/dev/null || cat "+shellQuote(path)+" 2>/dev/null || true")
	if err != nil {
		return tokens
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		tokens[fields[len(fields)-1]] = true
	}
	return tokens
}

// flannelAddress is the node's overlay address, which a cluster peer can reach
// through the same VXLAN path UDP 8472 carries. Empty on a host with no
// Flannel.
func flannelAddress(ctx context.Context, target Node) string {
	out, err := run(ctx, target.Runner,
		"ip -4 -o addr show flannel.1 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -n1")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// startCapture watches for the peers' probe datagrams arriving at the target.
// It runs as root (AF_PACKET needs CAP_NET_RAW) and in a session of its own,
// so the SSH channel that started it closing cannot hang it up before the
// peers have sent anything.
func startCapture(ctx context.Context, target Node, ports []string, nonce, report string) {
	seconds := int(listenerWindow / time.Second)
	cmd := fmt.Sprintf("sudo -n setsid -f python3 -c %s %s %s %d %s %s >/dev/null 2>&1",
		shellQuote(captureScript), shellQuote(strings.Join(ports, ",")),
		shellQuote(noncePrefix+nonce), seconds, shellQuote(report), shellQuote(listenerMarker))
	_, _ = run(ctx, target.Runner, cmd)
}

// settle gives the last probe datagram time to reach the target's capture
// before its report is read. It bounds a race; it never substitutes for the
// evidence, which has to be there or the port fails.
func settle(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(300 * time.Millisecond):
	}
}

// probeNonce makes one probe run's evidence distinguishable from any other's.
func probeNonce() string {
	var b [8]byte
	// crypto/rand.Read never returns an error (Go 1.24+): it stops the process
	// instead. The nonce names files a root capture writes under /tmp, so it
	// must stay unpredictable.
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stopListeners ends the probe on a target and waits for its ports to be free
// again. A listener that has been signalled is not the same as a port that is
// free, and the next thing to want these ports is k3s itself.
func stopListeners(ctx context.Context, target Node, specs []portSpec, portsReport, captureReport string) {
	// The capture runs as root (AF_PACKET needs CAP_NET_RAW), so stopping it
	// takes sudo too; the marker is bracketed so pkill does not match the
	// process running this very command.
	_, _ = run(ctx, target.Runner,
		"sudo -n pkill -f "+shellQuote(markerMatch)+" 2>/dev/null || pkill -f "+shellQuote(markerMatch)+" 2>/dev/null || true")

	var tcpPorts []string
	for _, s := range specs {
		if s.Proto == "tcp" {
			tcpPorts = append(tcpPorts, strconv.Itoa(s.Port))
		}
	}
	if len(tcpPorts) > 0 {
		pattern := ":(" + strings.Join(tcpPorts, "|") + ")$"
		for i := 0; i < 10; i++ {
			out, err := run(ctx, target.Runner,
				"ss -ltnH 2>/dev/null | awk '{print $4}' | grep -Eq "+shellQuote(pattern)+" && echo held || echo free")
			if err != nil || strings.TrimSpace(out) == "free" {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	_, _ = run(ctx, target.Runner,
		"sudo -n rm -f "+shellQuote(portsReport)+" "+shellQuote(captureReport)+" 2>/dev/null || true")
}

func purposeOf(spec string) string {
	proto, port, _ := strings.Cut(spec, ":")
	for _, s := range nodePorts {
		if s.Proto == proto && strconv.Itoa(s.Port) == port {
			return s.Purpose
		}
	}
	return "unknown"
}

func describePorts(specs []portSpec) string {
	var out []string
	for _, s := range specs {
		out = append(out, fmt.Sprintf("%d/%s", s.Port, s.Proto))
	}
	return strings.Join(out, ", ")
}

// shellQuote single-quotes an argument for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// listenerScript binds every port the target must accept, and holds them for
// listenerWindow. A port it cannot bind is not fatal — on a resumed install
// k3s owns 6443 and 10250 and Flannel's VXLAN device owns UDP 8472 — but it
// must be REPORTED, because a port nothing here can answer still has to be
// proven from every peer.
//
// It prints one line per port ("<proto> <port> bound|held"), then the word
// done so the caller can tell a complete report from a half-written one.
const listenerScript = `
# kubenest-port-listener
import socket, sys, select, time
tcp = [int(p) for p in (sys.argv[1].split(",") if len(sys.argv) > 1 and sys.argv[1] else [])]
udp = [int(p) for p in (sys.argv[2].split(",") if len(sys.argv) > 2 and sys.argv[2] else [])]
def report(kind, port, state):
    sys.stdout.write("%s %d %s\n" % (kind, port, state))
    sys.stdout.flush()
socks = []
for p in tcp:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(("0.0.0.0", p)); s.listen(16); socks.append(("tcp", p, s)); report("tcp", p, "bound")
    except OSError:
        s.close(); report("tcp", p, "held")
for p in udp:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(("0.0.0.0", p)); socks.append(("udp", p, s)); report("udp", p, "bound")
    except OSError:
        s.close(); report("udp", p, "held")
sys.stdout.write("done\n"); sys.stdout.flush()
deadline = time.time() + 25
while time.time() < deadline and socks:
    ready, _, _ = select.select([x[2] for x in socks], [], [], 1.0)
    for s in ready:
        kind = [k for k, p, x in socks if x is s][0]
        if kind == "tcp":
            try:
                c, _ = s.accept(); c.close()
            except OSError:
                pass
        else:
            try:
                data, addr = s.recvfrom(64); s.sendto(b"kubenest-ok", addr)
            except OSError:
                pass
for kind, p, s in socks:
    s.close()
`

// clientScript proves every port the target must accept FROM this peer. For a
// port the target's own listener bound, the echo is the proof, because an
// unanswered datagram is indistinguishable from a dropped one.
//
// A port the target could not bind is different, and how it is proven depends
// on whether this peer can already reach the target's overlay:
//
//   - The peer is a cluster member, so it pings the target's flannel.1
//     address. That rides the very VXLAN path UDP 8472 carries, end to end.
//   - The peer has no overlay (it is a new host being added), so it sends a
//     nonce datagram and the target's capture has to witness it arrive. The
//     peer prints "sent <token>"; the caller decides, from the capture.
//
// Arguments: the target, the target's flannel address or "-", the run nonce,
// this peer's index, then one "<proto>:<port>[:flannel|:held]" per port.
const clientScript = `
# kubenest-port-proof
import os, socket, subprocess, sys
host = sys.argv[1]
flannel_ip = sys.argv[2]
nonce = sys.argv[3]
peer = sys.argv[4]
have_flannel = os.path.isdir("/sys/class/net/flannel.1")
def send_nonce(port, token):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        for _ in range(3):
            s.sendto(token.encode(), (host, port))
    finally:
        s.close()
for spec in sys.argv[5:]:
    parts = spec.split(":")
    proto, port = parts[0], int(parts[1])
    held = parts[2] if len(parts) > 2 else ""
    if proto == "tcp":
        try:
            s = socket.create_connection((host, port), timeout=5); s.close()
            print("tcp:%d open" % port)
        except Exception:
            print("tcp:%d blocked" % port)
        continue
    token = "%s-%s-%s" % (nonce, peer, port)
    if held == "flannel" and have_flannel and flannel_ip != "-":
        try:
            rc = subprocess.call(["ping", "-c", "1", "-W", "2", flannel_ip],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        except OSError:
            rc = -1
        if rc == 0:
            print("udp:%d open" % port)
            continue
        if rc != -1:
            print("udp:%d blocked" % port)
            continue
        send_nonce(port, token)
        print("udp:%d sent %s" % (port, token))
        continue
    if held:
        send_nonce(port, token)
        print("udp:%d sent %s" % (port, token))
        continue
    ok = False
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(5)
        s.sendto(b"kubenest-probe", (host, port))
        s.recvfrom(64); s.close(); ok = True
    except Exception:
        ok = False
    print("udp:%d %s" % (port, "open" if ok else "blocked"))
`

// captureScript sees the peers' probe datagrams ARRIVE at the target: an
// AF_PACKET socket is copied a packet as the kernel receives it, before any
// netfilter rule can drop it. It writes "<source address> <payload>" for every
// datagram to one of the watched ports whose payload carries the run's nonce,
// so the caller can tell which peer's datagram it was.
//
// Arguments: the ports, the nonce prefix, how long to watch, and where to
// write, in seconds.
const captureScript = `
# kubenest-port-capture
import socket, struct, sys, time
ports = set(int(p) for p in sys.argv[1].split(",") if p)
prefix = sys.argv[2].encode()
seconds = float(sys.argv[3])
out = open(sys.argv[4], "w")
out.write("# watching udp %s for %s\n" % (sorted(ports), sys.argv[2])); out.flush()
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.ntohs(0x0003))
s.settimeout(1.0)
deadline = time.time() + seconds
while time.time() < deadline:
    try:
        pkt = s.recv(65535)
    except (socket.timeout, OSError):
        continue
    if len(pkt) < 14:
        continue
    etype = struct.unpack("!H", pkt[12:14])[0]
    off = 14
    while etype in (0x8100, 0x88A8) and len(pkt) >= off + 4:
        etype = struct.unpack("!H", pkt[off + 2:off + 4])[0]
        off += 4
    if etype != 0x0800 or len(pkt) < off + 20:
        continue
    ip = pkt[off:]
    ihl = (ip[0] & 0x0F) * 4
    if ip[9] != 17 or len(ip) < ihl + 8:
        continue
    dport = struct.unpack("!H", ip[ihl + 2:ihl + 4])[0]
    if dport not in ports:
        continue
    payload = ip[ihl + 8:]
    if payload.startswith(prefix):
        src = socket.inet_ntoa(ip[12:16])
        out.write("%s %s\n" % (src, payload.decode("ascii", "replace").strip()))
        out.flush()
out.close()
`
