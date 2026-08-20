package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeyCallback verifies host keys against known_hosts with OpenSSH's
// StrictHostKeyChecking=accept-new semantics: a host never seen before is
// recorded and accepted; a host whose key CHANGED is refused hard. That is the
// right posture for an installer that targets freshly provisioned machines —
// trust on first use, never trust a key swap.
func hostKeyCallback(path string) (ssh.HostKeyCallback, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return nil, err
		}
	}

	check, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return func(hostport string, remote net.Addr, key ssh.PublicKey) error {
		err := check(hostport, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
			// Unknown host: record and accept.
			return appendKnownHost(path, hostport, key)
		}
		if errors.As(err, &keyErr) {
			return fmt.Errorf(
				"HOST KEY CHANGED for %s: the key on the host does not match %s. If the machine was legitimately rebuilt, remove its old entry and retry; otherwise do not proceed", hostport, path)
		}
		return err
	}, nil
}

func appendKnownHost(path, hostport string, key ssh.PublicKey) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := knownhosts.Line([]string{hostport}, key)
	_, err = fmt.Fprintln(f, line)
	return err
}
