package preflight

import (
	"context"
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
}

var nodePorts = []portSpec{
	{Port: 6443, Proto: "tcp", Purpose: "Kubernetes API", AgentsToServer: true},
	{Port: 8472, Proto: "udp", Purpose: "Flannel VXLAN overlay"},
	{Port: 10250, Proto: "tcp", Purpose: "Kubelet metrics"},
	{Port: 2379, Proto: "tcp", Purpose: "etcd client", HAOnly: true},
	{Port: 2380, Proto: "tcp", Purpose: "etcd peer replication", HAOnly: true},
}

// listenerWindow is how long a target node holds its probe listeners open.
// Long enough for every peer to connect, short enough that an abandoned
// preflight leaves nothing running.
const listenerWindow = 25 * time.Second

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

	// Start the listeners. Ports already in use are NOT an error: on a
	// resumed install k3s itself is listening on 6443 and 10250, and
	// connecting to the real service proves the same thing the probe would.
	start := fmt.Sprintf("nohup python3 -c %s %s %s >/dev/null 2>&1 & echo started",
		shellQuote(listenerScript), shellQuote(strings.Join(tcp, ",")), shellQuote(strings.Join(udp, ",")))
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

	var blocked []string
	for _, peer := range peers {
		args := make([]string, 0, len(specs))
		for _, s := range specs {
			args = append(args, shellQuote(s.Proto+":"+strconv.Itoa(s.Port)))
		}
		connect := fmt.Sprintf("python3 -c %s %s %s",
			shellQuote(clientScript), shellQuote(target.Address), strings.Join(args, " "))
		out, err := run(ctx, peer.Runner, connect)
		if err != nil {
			blocked = append(blocked, fmt.Sprintf("%s -> %s: probe failed (%v)", peer.Address, target.Address, err))
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[1] == "open" {
				continue
			}
			blocked = append(blocked, fmt.Sprintf("%s -> %s %s (%s)",
				peer.Address, target.Address, fields[0], purposeOf(fields[0])))
		}
	}

	if len(blocked) > 0 {
		rep.add(Result{
			Check: CheckPorts, Node: target.Address, Outcome: Fail,
			Detail: "blocked: " + strings.Join(blocked, "; "),
			Fix:    "open these between the cluster nodes — a blocked overlay or kubelet port produces a cluster that installs and then misbehaves, which is far more expensive than a refused install",
		})
		return
	}
	rep.add(Result{
		Check: CheckPorts, Node: target.Address, Outcome: Pass,
		Detail: fmt.Sprintf("reachable from %d peer(s) on %s", len(peers), describePorts(specs)),
	})
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
// listenerWindow. A port already in use is skipped, not fatal: on a resumed
// install k3s owns 6443 and 10250, and a peer connecting to the real service
// proves reachability just as well.
const listenerScript = `
import socket, sys, select, time
tcp = [int(p) for p in (sys.argv[1].split(',') if len(sys.argv) > 1 and sys.argv[1] else [])]
udp = [int(p) for p in (sys.argv[2].split(',') if len(sys.argv) > 2 and sys.argv[2] else [])]
socks = []
for p in tcp:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(('0.0.0.0', p)); s.listen(16); socks.append(('tcp', s))
    except OSError:
        s.close()
for p in udp:
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(('0.0.0.0', p)); socks.append(('udp', s))
    except OSError:
        s.close()
deadline = time.time() + 25
while time.time() < deadline and socks:
    ready, _, _ = select.select([s for _, s in socks], [], [], 1.0)
    for s in ready:
        kind = [k for k, x in socks if x is s][0]
        if kind == 'tcp':
            try:
                c, _ = s.accept(); c.close()
            except OSError:
                pass
        else:
            try:
                data, addr = s.recvfrom(64); s.sendto(b'kubenest-ok', addr)
            except OSError:
                pass
for _, s in socks:
    s.close()
`

// clientScript connects to each port and prints "<proto>:<port> open|blocked".
// UDP is proven by the echo, because an unanswered datagram is
// indistinguishable from a dropped one.
const clientScript = `
import socket, sys
host = sys.argv[1]
for spec in sys.argv[2:]:
    proto, port = spec.split(':')
    port = int(port)
    ok = False
    try:
        if proto == 'tcp':
            s = socket.create_connection((host, port), timeout=5); s.close(); ok = True
        else:
            s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(5)
            s.sendto(b'kubenest-probe', (host, port))
            s.recvfrom(64); s.close(); ok = True
    except Exception:
        ok = False
    print('%s:%d %s' % (proto, port, 'open' if ok else 'blocked'))
`
