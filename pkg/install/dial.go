package install

import (
	"context"
	"io"
	"time"

	"kubenest.io/cli/pkg/preflight"
	"kubenest.io/cli/pkg/sshx"
)

// dialTimeout bounds one node's TCP connect. A node that is simply down
// should fail preflight in seconds, not hold the whole install at a default
// kernel timeout.
const dialTimeout = 15 * time.Second

// dialAll opens a connection to every target node.
//
// A failed dial is NOT an error here: it is the SSH-reachability check's
// observation, and preflight reports it alongside every other failure so one
// re-run can fix everything the operator can see. Returning early on the
// first unreachable node would make a three-node install a three-run install.
//
// Key material comes from --ssh-key, ssh-agent or ~/.ssh/config and never
// leaves this machine.
func (s *Session) dialAll(ctx context.Context) []preflight.Node {
	opts := sshx.Options{
		User:        s.Opts.SSHUser,
		KeyPath:     s.Opts.SSHKey,
		DialTimeout: dialTimeout,
	}
	var nodes []preflight.Node
	add := func(address string, role NodeRole) {
		node := preflight.Node{Address: address, Role: string(role)}
		endpoint, err := sshx.Resolve(address, opts)
		if err != nil {
			node.DialErr = err
			nodes = append(nodes, node)
			return
		}
		client, err := sshx.Dial(ctx, endpoint, opts)
		if err != nil {
			node.DialErr = err
			nodes = append(nodes, node)
			return
		}
		s.closers = append(s.closers, io.Closer(client))
		node.Runner = client
		nodes = append(nodes, node)
	}
	for _, address := range s.Opts.Servers {
		add(address, RoleServer)
	}
	for _, address := range s.Opts.Agents {
		add(address, RoleAgent)
	}
	return nodes
}
