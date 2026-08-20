// Package componenttest provides a scripted k3s.Runner for component
// installer unit tests. The real transport is exercised by pkg/sshx's tests
// against a real SSH server, and real cluster behavior by the e2e suite;
// these fakes exist to test command construction and response parsing.
package componenttest

import (
	"context"
	"sync"

	"kubenest.io/cli/pkg/sshx"
)

// FakeRunner records every command and answers via Respond.
type FakeRunner struct {
	mu       sync.Mutex
	commands []string

	// Respond maps a command to its result. Nil means every command
	// succeeds with empty output.
	Respond func(command string) (sshx.Result, error)
}

func (f *FakeRunner) Run(ctx context.Context, command string) (sshx.Result, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	f.mu.Unlock()
	if f.Respond == nil {
		return sshx.Result{}, nil
	}
	return f.Respond(command)
}

// Commands returns a copy of every command run so far.
func (f *FakeRunner) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}
