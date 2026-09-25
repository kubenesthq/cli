package operation

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/sshx"
)

// tunnelTransport is an SSH transport that can open a tunnel, as sshx.Client can.
type tunnelTransport struct {
	dialed []string
}

func (f *tunnelTransport) Run(context.Context, string) (sshx.Result, error) {
	return sshx.Result{}, nil
}

func (f *tunnelTransport) RunInput(context.Context, string, io.Reader) (sshx.Result, error) {
	return sshx.Result{}, nil
}

func (f *tunnelTransport) DialTCP(_ context.Context, addr string) (net.Conn, error) {
	f.dialed = append(f.dialed, addr)
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

// commandOnlyTransport runs commands and cannot open a tunnel.
type commandOnlyTransport struct{}

func (commandOnlyTransport) Run(context.Context, string) (sshx.Result, error) {
	return sshx.Result{}, nil
}

func (commandOnlyTransport) RunInput(context.Context, string, io.Reader) (sshx.Result, error) {
	return sshx.Result{}, nil
}

// The control-plane upgrade validates the new backend through the node while
// the public route is fenced, and every stage's transport is this decorator.
// Measured on hardware (2026-09-25): a decorator that hid the tunnel stopped
// the upgrade at validation with "the SSH connection to the server cannot open
// a tunnel", after the migration and the new chart had already been applied.
// A tunnel changes nothing on the cluster, so it is not an action and is
// neither recorded nor skipped.
func TestAGuardedRunnerOpensTunnelsThroughItsTransport(t *testing.T) {
	inner := &tunnelTransport{}
	var runner any = &Guarded{Inner: inner, Stage: "control-plane-validation"}
	dialer, ok := runner.(interface {
		DialTCP(ctx context.Context, addr string) (net.Conn, error)
	})
	if !ok {
		t.Fatal("the guarded runner cannot open a tunnel, so a stage that validates through the node fails behind it")
	}
	conn, err := dialer.DialTCP(context.Background(), "10.43.9.164:8000")
	if err != nil {
		t.Fatalf("DialTCP: %v", err)
	}
	_ = conn.Close()
	if len(inner.dialed) != 1 || inner.dialed[0] != "10.43.9.164:8000" {
		t.Errorf("the tunnel did not go through the transport: dialed %v", inner.dialed)
	}
}

func TestAGuardedRunnerOverATransportWithoutTunnelsRefusesToDial(t *testing.T) {
	g := &Guarded{Inner: commandOnlyTransport{}, Stage: "control-plane-validation"}
	if _, err := g.DialTCP(context.Background(), "10.43.9.164:8000"); err == nil || !strings.Contains(err.Error(), "10.43.9.164:8000") {
		t.Errorf("DialTCP over a transport with no tunnels returned %v, want a refusal naming the address", err)
	}
}
