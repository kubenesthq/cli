package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"

	"kubenest.io/cli/pkg/sshx"
)

// reconnectingRunner keeps one node reachable for the whole install.
//
// An install runs for minutes over a single SSH connection, from an
// operator's laptop or a bastion, across whatever is between them. A TCP
// reset in that window is ordinary — a NAT table expiring, a wifi handover, a
// router rebooting — and without this, one reset ends the install: every
// convergence probe from that moment returns the same dead-socket error, the
// stage waits out its full deadline observing nothing, and the failure says
// "pods in openebs is unobservable" instead of naming a fix.
//
// Observed for real on the kn-7k8 host gate (2026-08-21): a reset during
// platform-storage produced ten minutes of identical unobservable states.
//
// So a dead connection is redialled and the command retried once. This is a
// transport concern and it belongs here rather than in pkg/converge, whose
// rule is unchanged: an error is an observation, and only the deadline
// decides. Reconnecting simply makes the next observation truthful.
type reconnectingRunner struct {
	address string
	opts    sshx.Options
	// dial opens a fresh connection. A field so tests can exercise the
	// reconnect logic without an SSH server; production sets it once.
	dial func(ctx context.Context) (runConn, error)

	mu   sync.Mutex
	conn runConn
}

// runConn is the part of *sshx.Client this needs: run a command, stream one,
// hang up.
type runConn interface {
	Run(ctx context.Context, command string) (sshx.Result, error)
	RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error)
	Close() error
}

// newReconnectingRunner takes ownership of an already-dialled client and
// knows how to replace it.
func newReconnectingRunner(address string, opts sshx.Options, client *sshx.Client) *reconnectingRunner {
	r := &reconnectingRunner{address: address, opts: opts, conn: client}
	r.dial = func(ctx context.Context) (runConn, error) {
		endpoint, err := sshx.Resolve(r.address, r.opts)
		if err != nil {
			return nil, err
		}
		return sshx.Dial(ctx, endpoint, r.opts)
	}
	return r
}

// Run executes a command, redialling once if the connection has died.
func (r *reconnectingRunner) Run(ctx context.Context, command string) (sshx.Result, error) {
	return r.attempt(ctx, func(c runConn) (sshx.Result, error) {
		return c.Run(ctx, command)
	})
}

// RunInput streams stdin, redialling once on a dead connection. It exists
// because the component installers detect a streaming runner by interface:
// without RunInput here, the Gateway API CRD bundle would fall back to being
// inlined into one exec command, which blows the SSH packet cap on a real
// host.
func (r *reconnectingRunner) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	// A retried stream must send the same bytes again, so the payload is
	// buffered. Everything streamed during an install is a manifest — the
	// largest is the Gateway API CRD bundle at a few hundred KB.
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return sshx.Result{}, err
	}
	return r.attempt(ctx, func(c runConn) (sshx.Result, error) {
		return c.RunInput(ctx, command, bytes.NewReader(payload))
	})
}

func (r *reconnectingRunner) attempt(ctx context.Context, do func(runConn) (sshx.Result, error)) (sshx.Result, error) {
	conn, err := r.connection(ctx)
	if err != nil {
		return sshx.Result{}, err
	}
	res, err := do(conn)
	if err == nil || !connectionIsDead(err) {
		return res, err
	}
	// The operator aborting is not a dead connection.
	if ctx.Err() != nil {
		return res, err
	}

	r.discard(conn)
	fresh, dialErr := r.connection(ctx)
	if dialErr != nil {
		return sshx.Result{}, fmt.Errorf("the connection to %s dropped (%v) and could not be re-established: %w", r.address, err, dialErr)
	}
	return do(fresh)
}

// connection returns a live connection, dialling if there is none.
func (r *reconnectingRunner) connection(ctx context.Context) (runConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		return r.conn, nil
	}
	conn, err := r.dial(ctx)
	if err != nil {
		return nil, err
	}
	r.conn = conn
	return conn, nil
}

// discard drops a connection observed dead, unless someone else already
// replaced it.
func (r *reconnectingRunner) discard(dead runConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == dead {
		_ = r.conn.Close()
		r.conn = nil
	}
}

// Close releases the connection.
func (r *reconnectingRunner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return nil
	}
	err := r.conn.Close()
	r.conn = nil
	return err
}

// connectionIsDead distinguishes "this socket is gone" from "the command
// failed". A command that exits non-zero is not an error here at all —
// pkg/sshx returns that as a Result — so everything this matches really is
// the transport.
func connectionIsDead(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset by peer",
		"broken pipe",
		"use of closed network connection",
		"connection closed by remote host",
		"client is closed",
		"eof",
		"no route to host",
		"connection refused",
		"i/o timeout",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
