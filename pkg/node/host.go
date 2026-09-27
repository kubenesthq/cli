// Package node owns the cluster's host inventory (kn-t50) and the verbs that
// act on one machine of a cluster: add (T5.2), remove (T5.3), replace (T5.4).
//
// WHAT LIVES HERE. The vocabulary shared by every writer of the inventory —
// the host ID, the roles and lifecycle states, the entry helper (hostid.go) —
// and, since T5.2/T5.3, the parts of a node operation that are the same
// whichever verb it is: dialling a host from the inventory and re-checking the
// host key, reading the cluster's Node objects, picking a Ready server to work
// through, reading the local volumes a node holds, and the session that the
// staging engine drives (engine.go is T5.1's interlock, pkg/stages is the
// engine itself).
//
// WHY IT IS NOT IN pkg/cmd. `kubenest node reboot` (T3.5) predates this
// package and carries its own copies of the dialler and the Node reader; a
// verb written now does not get to add a third copy, and a verb written later
// (T5.4) has somewhere to put them. What does NOT belong here is anything the
// command line decides: flags, output wording, and which API client to build.
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/sshx"
)

// Transport is one open SSH connection to a host: the runner every command
// goes through, the host key the handshake negotiated (so the inventory's
// fingerprint can be re-checked), and Close. *sshx.Client satisfies it.
type Transport interface {
	k3s.Runner
	HostKeyFingerprint() string
	Close() error
}

// DialHost opens one SSH connection to an inventory host.
//
// The ADDRESS, PORT and USER come from the record: a node verb that took its
// target from a local journal would act on whatever machine the laptop that
// ran the install happened to know about, which is the failure the inventory
// exists to remove. --ssh-user and --ssh-key only override how this laptop
// AUTHENTICATES to that address; they never decide WHICH machine it is.
func DialHost(ctx context.Context, host api.HostRecord, user, key string) (Transport, error) {
	if host.SSHAddress == "" {
		return nil, fmt.Errorf("the cluster's inventory records no SSH address for host %s", host.HostID)
	}
	if user == "" {
		user = host.SSHUser
	}
	opts := sshx.Options{User: user, KeyPath: key, Port: host.SSHPort}
	ep, err := sshx.Resolve(host.SSHAddress, opts)
	if err != nil {
		return nil, err
	}
	if host.SSHPort != 0 {
		ep.Port = host.SSHPort
	}
	return sshx.Dial(ctx, ep, opts)
}

// CheckFingerprint re-checks the host key a live connection negotiated against
// the one the inventory recorded when the host joined.
//
// A machine that answers on a recorded address with a different host key is
// not the machine the record describes — a rebuilt host, a reused address, or
// something in the middle — and acting on it is how a node verb takes down a
// machine nobody asked about. An empty fingerprint on EITHER side is "not
// recorded"/"not observed", which is not a match this function can claim; the
// caller says so rather than pretending it verified.
func CheckFingerprint(host api.HostRecord, conn Transport) error {
	recorded := host.HostKeyFingerprint
	observed := conn.HostKeyFingerprint()
	if recorded == "" || observed == "" {
		return nil
	}
	if recorded != observed {
		return fmt.Errorf("the host at %s presented host key %s, and the cluster's inventory recorded %s for host %s when it joined: this is not the machine the record describes, so nothing was changed. If the host was rebuilt, re-run the install's record stage",
			host.SSHAddress, observed, recorded, host.HostID)
	}
	return nil
}

// FindHost looks a machine up by anything the operator could have typed: the
// host ID the record minted, the SSH address it reaches the host at, the
// address the host joined through, or the Node UID the cluster's own object
// carries.
//
// It does NOT filter on lifecycle state: whether a removed host is a refusal
// or a machine to look at is the verb's question, and a lookup that answered
// it would be answering with the caller's policy.
func FindHost(hosts []api.HostRecord, want string) (api.HostRecord, bool) {
	for _, h := range hosts {
		switch {
		case h.HostID == want, h.SSHAddress == want, h.JoinAddress == want:
			return h, true
		case h.NodeUID != "" && h.NodeUID == want:
			return h, true
		}
	}
	return api.HostRecord{}, false
}

// DescribeHosts renders an inventory for an operator, one host per entry.
func DescribeHosts(hosts []api.HostRecord) string {
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		parts = append(parts, fmt.Sprintf("%s (%s, role %s, %s)", h.HostID, h.SSHAddress, h.Role, h.LifecycleState))
	}
	return strings.Join(parts, "; ")
}

// ClusterNode is one Node object, as much of it as a node verb reads.
type ClusterNode struct {
	Name          string
	UID           string
	Addresses     []string
	Labels        map[string]string
	Annotations   map[string]string
	Ready         bool
	Unschedulable bool
}

// Matches reports whether a Node object is the one an inventory entry
// describes: by the recorded UID first, then by any address the host joined
// through. Without a recorded UID (an install that could not read it) the
// address is the only identity there is, and the caller says so.
func (c ClusterNode) Matches(host api.HostRecord) bool {
	if host.NodeUID != "" && c.UID == host.NodeUID {
		return true
	}
	for _, a := range c.Addresses {
		if a == host.SSHAddress || (host.JoinAddress != "" && a == host.JoinAddress) {
			return true
		}
	}
	return false
}

// ReadClusterNodes reads every Node object once.
func ReadClusterNodes(ctx context.Context, r k3s.Runner) ([]ClusterNode, error) {
	out, err := k3s.Kubectl(ctx, r, "get nodes -o json")
	if err != nil {
		return nil, fmt.Errorf("reading the cluster's nodes: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				UID         string            `json:"uid"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing `kubectl get nodes -o json`: %w", err)
	}
	nodes := make([]ClusterNode, 0, len(list.Items))
	for _, item := range list.Items {
		c := ClusterNode{
			Name:          item.Metadata.Name,
			UID:           item.Metadata.UID,
			Labels:        item.Metadata.Labels,
			Annotations:   item.Metadata.Annotations,
			Unschedulable: item.Spec.Unschedulable,
		}
		for _, a := range item.Status.Addresses {
			if a.Address != "" {
				c.Addresses = append(c.Addresses, a.Address)
			}
		}
		for _, cond := range item.Status.Conditions {
			if cond.Type == "Ready" {
				c.Ready = cond.Status == "True"
			}
		}
		nodes = append(nodes, c)
	}
	return nodes, nil
}

// NodeFor finds the Node object an inventory entry describes.
//
// The recorded Node UID is checked FIRST and a mismatch is a REFUSAL, not a
// repair: the UID is how a rebuilt, renamed or re-joined host is told apart
// from the machine the record describes, and acting on the new object with the
// old record's assumptions is how a node verb destroys a machine nobody asked
// about. An entry with no recorded UID falls back to the addresses, and the
// caller reports that the UID was not verified rather than claiming it was.
func NodeFor(nodes []ClusterNode, host api.HostRecord) (ClusterNode, error) {
	if host.NodeUID != "" {
		for _, c := range nodes {
			if c.UID == host.NodeUID {
				return c, nil
			}
		}
		return ClusterNode{}, fmt.Errorf(
			"the cluster's inventory records Node UID %s for host %s (%s), and no node of this cluster has it: the host was rebuilt, re-joined or renamed, so this command refuses rather than acting on a machine the record does not describe. If the host really is this cluster's, re-run the install's record stage so the inventory describes it",
			host.NodeUID, host.HostID, host.SSHAddress)
	}
	for _, c := range nodes {
		if c.Matches(host) {
			return c, nil
		}
	}
	var known []string
	for _, c := range nodes {
		known = append(known, c.Name+" ("+strings.Join(c.Addresses, ", ")+")")
	}
	return ClusterNode{}, fmt.Errorf("host %s (%s) is in the cluster's inventory but is not one of its nodes, so there is no Node object to act on. The cluster's nodes are: %s",
		host.HostID, host.SSHAddress, strings.Join(known, "; "))
}

// ReadyServer picks the server every cluster read goes through: a server of
// this cluster that ANSWERS and is Ready, read through a connection whose host
// key matches the inventory.
//
// It is deliberately not "the first server installed": the address order in
// the inventory is the order the install happened to use, and a join through a
// server that is down or NotReady fails in a way that looks like a problem
// with the NEW host. Every candidate is therefore tried in turn, and the
// refusal names what each one answered.
func ReadyServer(ctx context.Context, hosts []api.HostRecord, dial func(context.Context, api.HostRecord) (Transport, error)) (api.HostRecord, Transport, error) {
	var servers []api.HostRecord
	for _, h := range hosts {
		if Role(h.Role) == RoleServer && LifecycleState(h.LifecycleState) != StateRemoved {
			servers = append(servers, h)
		}
	}
	if len(servers) == 0 {
		return api.HostRecord{}, nil, fmt.Errorf("no server of this cluster is in its inventory, so there is no machine whose API can be read: the inventory holds %s",
			DescribeHosts(hosts))
	}
	var tried []string
	for _, s := range servers {
		conn, err := dial(ctx, s)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s (%s): could not connect: %v", s.HostID, s.SSHAddress, err))
			continue
		}
		if err := CheckFingerprint(s, conn); err != nil {
			conn.Close()
			tried = append(tried, fmt.Sprintf("%s (%s): %v", s.HostID, s.SSHAddress, err))
			continue
		}
		nodes, err := ReadClusterNodes(ctx, conn)
		if err != nil {
			conn.Close()
			tried = append(tried, fmt.Sprintf("%s (%s): the cluster's API did not answer: %v", s.HostID, s.SSHAddress, err))
			continue
		}
		self, err := NodeFor(nodes, s)
		if err != nil || !self.Ready {
			conn.Close()
			if err != nil {
				tried = append(tried, fmt.Sprintf("%s (%s): %v", s.HostID, s.SSHAddress, err))
			} else {
				tried = append(tried, fmt.Sprintf("%s (%s): the node %s is NOT Ready", s.HostID, s.SSHAddress, self.Name))
			}
			continue
		}
		return s, conn, nil
	}
	return api.HostRecord{}, nil, fmt.Errorf("no server of this cluster is Ready, so there is nothing to join a new machine to:\n  %s",
		strings.Join(tried, "\n  "))
}

// JoinURL is the address an agent joins through: the server's own address and
// k3s's API port. It is built here rather than read from a flag, because the
// flag would be what an operator THINKS the cluster's address is.
func JoinURL(server api.HostRecord) string {
	return "https://" + server.SSHAddress + ":6443"
}

// HostPort is the TCP port a live connection reached its host on, so the
// inventory can record how to reach the machine again. A default of 22 is
// reported rather than an empty port: a recorded port of 0 is a record the
// contract refuses, and "the port ssh itself used" is the honest answer when
// the connection does not say.
func HostPort(conn Transport) int {
	if c, ok := conn.(*sshx.Client); ok && c.Endpoint != nil && c.Endpoint.Port != 0 {
		return c.Endpoint.Port
	}
	return 22
}

// HostUser is the user a live connection authenticated as, for the same
// reason: the inventory records how the CLI reaches this machine.
func HostUser(conn Transport) string {
	if c, ok := conn.(*sshx.Client); ok && c.Endpoint != nil {
		return c.Endpoint.User
	}
	return ""
}
