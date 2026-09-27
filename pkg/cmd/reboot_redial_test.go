package cmd

// The connection the reboot kills (kn-t35-…-m8l8.1).
//
// Found on hardware: a server reboot answered SSH again, so the wait from
// outside succeeded, and then every step AFTER the wait still ran through the
// session the reboot had killed — the operation record's own reads and writes,
// the uncordon, kured's lock release and the marker removal. The server was
// left cordoned, the record open and the lock held until its TTL.
//
// The fix is a connection the run SHARES and can replace (liveConn): the
// record's store and every operation.Guarded wrapper hold it, so swapping the
// connection underneath reaches all of them. This file asserts that with a
// session that dies the way a reboot makes one die — EOF on every call after
// the disruptive command — and with a redial that presents the WRONG host key,
// which is the only thing standing between this verb and rebooting a machine
// the inventory does not describe.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/operation"
	"kubenest.io/cli/pkg/sshx"
)

// dyingConn is one SSH session to a host that dies with the host: from the
// moment the disruptive command is issued, every call fails with EOF, exactly
// as the CLI saw on hardware. It records what was routed through it AFTER it
// died, which is how "every later step used the new session" is asserted
// rather than assumed.
type dyingConn struct {
	inner *fakeHost
	mu    sync.Mutex
	dead  bool
	post  []string
}

func (c *dyingConn) diesOn(command string) bool {
	return strings.Contains(command, "systemctl reboot") || strings.Contains(command, "systemctl restart")
}

func (c *dyingConn) observe(command string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dead {
		return false
	}
	c.post = append(c.post, command)
	return true
}

func (c *dyingConn) Run(ctx context.Context, command string) (sshx.Result, error) {
	if c.observe(command) {
		return sshx.Result{}, io.EOF
	}
	res, err := c.inner.Run(ctx, command)
	if c.diesOn(command) {
		c.mu.Lock()
		c.dead = true
		c.mu.Unlock()
	}
	return res, err
}

func (c *dyingConn) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	// Reading stdin here would consume the document the inner host needs, so a
	// call after death is recorded without touching it.
	if c.observe(command) {
		return sshx.Result{}, io.EOF
	}
	res, err := c.inner.RunInput(ctx, command, stdin)
	if c.diesOn(command) {
		c.mu.Lock()
		c.dead = true
		c.mu.Unlock()
	}
	return res, err
}

func (c *dyingConn) HostKeyFingerprint() string { return c.inner.HostKeyFingerprint() }
func (c *dyingConn) Close() error               { return c.inner.Close() }

func (c *dyingConn) afterDeath() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.post...)
}

// typedConn is a healthy connection that presents a chosen host key, for the
// redial that must be refused.
type typedConn struct {
	inner *fakeHost
	fp    string
}

func (c *typedConn) Run(ctx context.Context, command string) (sshx.Result, error) {
	return c.inner.Run(ctx, command)
}

func (c *typedConn) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	return c.inner.RunInput(ctx, command, stdin)
}

func (c *typedConn) HostKeyFingerprint() string { return c.fp }
func (c *typedConn) Close() error               { return c.inner.Close() }

// rebootDials hands out the scripted connections in order and counts the dials,
// so a test can see that the command redialled at all.
type rebootDials struct {
	mu    sync.Mutex
	conns []nodeTransport
	calls int
}

func (d *rebootDials) dial(_ context.Context, _ api.HostRecord) (nodeTransport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if len(d.conns) == 0 {
		return nil, fmt.Errorf("no connection was scripted")
	}
	if d.calls <= len(d.conns) {
		return d.conns[d.calls-1], nil
	}
	return d.conns[len(d.conns)-1], nil
}

func (d *rebootDials) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// newRedialRun wires a single-server reboot whose first connection dies with
// the host and whose redial is `redial`.
func newRedialRun(t *testing.T, redial nodeTransport) (*nodeReboot, *bytes.Buffer, *rebootDials, *fakeHost) {
	t.Helper()
	server := scriptedServer(t,
		testNode{Name: testNodeName, UID: "uid-srv", Addresses: []string{testServerAddr}, Ready: true},
	)
	dead := &dyingConn{inner: server}
	dials := &rebootDials{conns: []nodeTransport{dead, redial}}
	clock := newFakeClock(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	client := rebootControlPlaneWithManifest(t, []api.HostRecord{serverHost()},
		windowRecord([]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, "00:00", "23:59", "UTC"),
		manifestWithTimeouts(t, "", ""))
	out := &bytes.Buffer{}
	ran := []string{}
	n := &nodeReboot{
		out:    out,
		f:      NodeRebootFlags{Cluster: testCluster, Node: testServerAddr, Confirm: true, Now: true},
		client: client,
		now:    clock.now,
		sleep:  clock.sleep,
		poll:   time.Second,
		dial:   dials.dial,
		gates:  permissiveGates(&ran),
		store: func(runner k3s.Runner) *operation.Store {
			return &operation.Store{Runner: runner, Operator: "test@laptop"}
		},
	}
	return n, out, dials, server
}

// THE COMMAND FINISHES THROUGH A NEW SESSION ONCE THE HOST IS BACK.
func TestNodeRebootRedialsAfterTheHostReturns(t *testing.T) {
	t.Run("the record, the uncordon, the lock and the marker all use the new session", func(t *testing.T) {
		n, out, dials, server := newRedialRun(t, nil)
		// The redial is the host itself: a healthy session, same host key.
		dials.mu.Lock()
		dials.conns[1] = server
		dials.mu.Unlock()
		// A multi-node fixture would need the drain; the single-server shape is
		// what the bead's hardware run used and what makes the SERVER the host
		// that rebooted.
		server.prepend("sudo -n systemctl reboot", transportErr("ssh: session closed by remote host"))

		runErr := n.run(context.Background())
		outStr := out.String()
		if runErr != nil {
			t.Fatalf("the reboot failed: %v\n%s", runErr, outStr)
		}
		if dials.count() < 2 {
			t.Fatalf("the command never opened a second connection (%d dial(s)), so everything after the reboot ran through the session the reboot killed:\n%s", dials.count(), outStr)
		}
		// The record was closed, the lock released and the marker cleared —
		// all of which happen AFTER the reboot, so all of them went through the
		// new session.
		rec := server.liveRecord(t)
		if !rec.Terminal || rec.Result != string(operation.ResultSucceeded) {
			t.Errorf("the record is terminal=%t result=%q, want a succeeded terminal record read and written after the reboot", rec.Terminal, rec.Result)
		}
		if lock := server.kuredLock(); lock != "" {
			t.Errorf("kured's lock is still held (%s): the release did not reach the cluster through the new session", lock)
		}
		if !server.hasCommand(markerClearCommand(testNodeName)) {
			t.Errorf("the planned-reboot marker was not cleared, so that step did not reach the cluster either:\n%s", outStr)
		}
		// Nothing that changes the cluster may have been attempted on the dead
		// session: a health probe is the one call that has to try it, because
		// that is how the command learns the session is gone.
		dead, ok := dials.conns[0].(*dyingConn)
		if !ok {
			t.Fatal("the first scripted connection is not the dying one")
		}
		for _, command := range dead.afterDeath() {
			if strings.TrimSpace(command) == "true" {
				continue
			}
			t.Errorf("a command was routed through the session the reboot killed: %q", command)
		}
		if !strings.Contains(outStr, "planned:") {
			t.Errorf("the run does not report the marker it cleared:\n%s", outStr)
		}
	})

	t.Run("the redial verifies the host key again", func(t *testing.T) {
		wrong := &typedConn{}
		n, out, dials, server := newRedialRun(t, wrong)
		wrong.inner, wrong.fp = server, "SHA256:someone-elses"
		server.prepend("sudo -n systemctl reboot", transportErr("ssh: session closed by remote host"))

		runErr := n.run(context.Background())
		outStr := out.String()
		if runErr == nil {
			t.Fatalf("a redial that presented a different host key was accepted:\n%s", outStr)
		}
		if !strings.Contains(runErr.Error(), "SHA256:someone-elses") || !strings.Contains(runErr.Error(), "SHA256:server") {
			t.Errorf("the refusal does not name both host keys, so an operator cannot see which machine answered: %v", runErr)
		}
		if !strings.Contains(runErr.Error(), testServerAddr) {
			t.Errorf("the refusal does not name the host: %v", runErr)
		}
		if dials.count() < 2 {
			t.Errorf("the command did not redial, so the refusal came from somewhere else: %d dial(s)", dials.count())
		}
		// Nothing after the wait may have been changed: the refusal happens
		// before the uncordon and before the marker is cleared.
		if server.hasCommand("kubectl uncordon") {
			t.Error("the node was uncordoned even though the redial presented another host's key")
		}
		if server.hasCommand(markerClearCommand(testNodeName)) {
			t.Error("the planned-reboot marker was cleared even though the redial presented another host's key")
		}
		if lock := server.kuredLock(); lock == "" {
			t.Error("kured's lock was released through an unverified connection")
		}
	})
}
