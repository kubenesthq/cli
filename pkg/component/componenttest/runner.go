// Package componenttest provides a scripted k3s.Runner for component
// installer unit tests. The real transport is exercised by pkg/sshx's tests
// against a real SSH server, and real cluster behavior by the e2e suite;
// these fakes exist to test command construction and response parsing.
package componenttest

import (
	"context"
	"io"
	"sync"

	"kubenest.io/cli/pkg/sshx"
)

// Execution is one call the code under test made: the command string, and
// the stdin payload if it streamed one.
//
// The two are kept apart deliberately. A test asserting that a secret never
// reaches the target host's process list must scan Command and not Stdin,
// and a test asserting the secret DID reach the cluster must scan Stdin and
// not Command. Flattening them into one haystack makes both assertions
// unfalsifiable — which is how kn-40rd survived two tests named for exactly
// the property it violated.
type Execution struct {
	Command string
	Stdin   []byte
}

// FakeRunner records every execution and answers via Respond.
type FakeRunner struct {
	mu   sync.Mutex
	runs []Execution

	// Respond maps a command to its result. Nil means every command
	// succeeds with empty output.
	Respond func(command string) (sshx.Result, error)
}

func (f *FakeRunner) Run(ctx context.Context, command string) (sshx.Result, error) {
	return f.record(command, nil)
}

// RunInput records the streamed payload alongside its command. The fake
// drains stdin exactly as a real transport would, so a test that forgets to
// provide a readable payload fails here rather than passing vacuously.
func (f *FakeRunner) RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error) {
	var payload []byte
	if stdin != nil {
		var err error
		payload, err = io.ReadAll(stdin)
		if err != nil {
			return sshx.Result{}, err
		}
	}
	return f.record(command, payload)
}

func (f *FakeRunner) record(command string, stdin []byte) (sshx.Result, error) {
	f.mu.Lock()
	f.runs = append(f.runs, Execution{Command: command, Stdin: stdin})
	f.mu.Unlock()
	if f.Respond == nil {
		return sshx.Result{}, nil
	}
	return f.Respond(command)
}

// Commands returns a copy of every command run so far, without the streamed
// payloads. This is the process-list view of the install: what a local user
// on the target host would see in `ps auxww`.
func (f *FakeRunner) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, r.Command)
	}
	return out
}

// Inputs returns a copy of every stdin payload streamed so far.
func (f *FakeRunner) Inputs() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]byte
	for _, r := range f.runs {
		if r.Stdin != nil {
			out = append(out, append([]byte(nil), r.Stdin...))
		}
	}
	return out
}

// Executions returns a copy of the full record, commands paired with their
// payloads.
func (f *FakeRunner) Executions() []Execution {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Execution(nil), f.runs...)
}
