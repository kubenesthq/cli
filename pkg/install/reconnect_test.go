package install

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"kubenest.io/cli/pkg/sshx"
)

// The classification that matters: a dead socket is redialled, a failed
// command is not. pkg/sshx returns a non-zero exit as a Result, never an
// error, so nothing here can mistake "the command failed" for "the
// connection died".
func TestConnectionIsDead(t *testing.T) {
	dead := []error{
		io.EOF,
		net.ErrClosed,
		syscall.ECONNRESET,
		errors.New("read tcp 192.168.1.3:38652->188.245.95.58:22: read: connection reset by peer"),
		errors.New("write: broken pipe"),
		errors.New("ssh: client is closed"),
		errors.New("dial tcp 10.0.1.10:22: i/o timeout"),
	}
	for _, err := range dead {
		if !connectionIsDead(err) {
			t.Errorf("must be treated as a dead connection: %v", err)
		}
	}

	alive := []error{
		nil,
		errors.New("kubectl get pods -n openebs: exit 1: Error from server (NotFound)"),
		errors.New("sudo: a password is required"),
		errors.New("HelmChart kubenest-velero has no version"),
	}
	for _, err := range alive {
		if connectionIsDead(err) {
			t.Errorf("must NOT be treated as a dead connection: %v", err)
		}
	}
}

// A reset mid-install must cost one redial, not the whole install: without
// this, every convergence probe after the reset returns the same dead-socket
// error and the stage waits out its full deadline observing nothing.
func TestRunRedialsOnceAfterAReset(t *testing.T) {
	r := &reconnectingRunner{address: "10.0.1.10"}
	var dials int
	r.dial = func(ctx context.Context) (runConn, error) {
		dials++
		if dials == 1 {
			return &fakeConn{err: errors.New("read: connection reset by peer")}, nil
		}
		return &fakeConn{stdout: "ok"}, nil
	}

	res, err := r.Run(context.Background(), "kubectl get pods")
	if err != nil {
		t.Fatalf("a reset must be recovered by a redial: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "ok" {
		t.Errorf("stdout = %q, want the retried command's output", res.Stdout)
	}
	if dials != 2 {
		t.Errorf("dialled %d times, want 2 (the original and one redial)", dials)
	}
}

// A command that simply fails is not retried — retrying a failing command
// would double every side effect the install performs.
func TestAFailedCommandIsNotRetried(t *testing.T) {
	r := &reconnectingRunner{address: "10.0.1.10"}
	var dials int
	r.dial = func(ctx context.Context) (runConn, error) {
		dials++
		return &fakeConn{err: errors.New("kubectl: exit 1: NotFound")}, nil
	}
	if _, err := r.Run(context.Background(), "kubectl get pods"); err == nil {
		t.Fatal("want the command's error")
	}
	if dials != 1 {
		t.Errorf("dialled %d times, want 1 — a failed command is not a dead connection", dials)
	}
}

// If the redial itself fails, the error names both halves: what dropped and
// why it could not be re-established.
func TestARedialFailureNamesBoth(t *testing.T) {
	r := &reconnectingRunner{address: "10.0.1.10"}
	var dials int
	r.dial = func(ctx context.Context) (runConn, error) {
		dials++
		if dials == 1 {
			return &fakeConn{err: errors.New("connection reset by peer")}, nil
		}
		return nil, errors.New("dial tcp 10.0.1.10:22: no route to host")
	}
	_, err := r.Run(context.Background(), "uptime")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"10.0.1.10", "reset by peer", "no route to host"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is missing %q: %v", want, err)
		}
	}
}

// The streamed path retries too, and re-sends the same bytes: the component
// installers detect a streaming runner by interface, and losing that would
// inline a few hundred KB of CRDs into one exec command.
func TestRunInputRetriesWithTheSamePayload(t *testing.T) {
	r := &reconnectingRunner{address: "10.0.1.10"}
	var seen []string
	var dials int
	r.dial = func(ctx context.Context) (runConn, error) {
		dials++
		first := dials == 1
		return &fakeConn{
			onInput: func(body string) { seen = append(seen, body) },
			errFn: func() error {
				if first {
					return errors.New("broken pipe")
				}
				return nil
			},
		}, nil
	}
	if _, err := r.RunInput(context.Background(), "tee /tmp/x", strings.NewReader("apiVersion: v1")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != seen[1] || seen[0] != "apiVersion: v1" {
		t.Errorf("the retry must send the same bytes, saw %q", seen)
	}
}

// fakeConn is a scripted runConn.
type fakeConn struct {
	stdout  string
	err     error
	errFn   func() error
	onInput func(string)
}

func (f *fakeConn) Run(context.Context, string) (sshx.Result, error) {
	return sshx.Result{Stdout: f.stdout}, f.fail()
}

func (f *fakeConn) RunInput(_ context.Context, _ string, stdin io.Reader) (sshx.Result, error) {
	body, _ := io.ReadAll(stdin)
	if f.onInput != nil {
		f.onInput(string(body))
	}
	return sshx.Result{Stdout: f.stdout}, f.fail()
}

func (f *fakeConn) fail() error {
	if f.errFn != nil {
		return f.errFn()
	}
	return f.err
}

func (f *fakeConn) Close() error { return nil }
