// Package sshx is the CLI's SSH transport for reaching target hosts.
//
// Credentials come from, in order of precedence: an explicit key file
// (--ssh-key), a running ssh-agent, or the IdentityFile from ~/.ssh/config.
// Key material NEVER leaves this machine: it is not sent to the control plane,
// not written to logs, and not serializable — those properties are enforced by
// tests in this package, not promised in prose.
package sshx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Options controls how a target host is resolved and authenticated.
type Options struct {
	// User is the SSH user (--ssh-user). Empty falls back to ssh_config,
	// then the current OS user.
	User string
	// KeyPath is an explicit private key file (--ssh-key). Empty falls back
	// to ssh-agent, then the ssh_config IdentityFile.
	KeyPath string
	// Port overrides the SSH port. Zero falls back to ssh_config, then 22.
	Port int

	// ConfigPath overrides ~/.ssh/config (tests). Empty uses the default.
	ConfigPath string
	// KnownHostsPath overrides ~/.ssh/known_hosts (tests).
	KnownHostsPath string
	// AgentSocket overrides $SSH_AUTH_SOCK (tests).
	AgentSocket string

	// Passphrase is called if the key file is encrypted. Nil means an
	// encrypted key is an error telling the user to use ssh-agent.
	Passphrase func() ([]byte, error)

	// DialTimeout bounds the TCP connect. Zero means 10s.
	DialTimeout time.Duration
}

// Endpoint is a resolved target: where to connect and as whom. The
// authentication methods live in unexported fields so an Endpoint can be
// logged or serialized without ever carrying credentials.
type Endpoint struct {
	Host     string // as the user named it (alias or address)
	HostName string // what we actually connect to
	Port     int
	User     string
	// AuthSources names where credentials came from, for error messages —
	// e.g. ["key file /home/x/.ssh/id_ed25519", "ssh-agent"]. Names only,
	// never material.
	AuthSources []string

	auth []ssh.AuthMethod
}

// Resolve determines the address, user, port and authentication for a host,
// consulting ssh_config the way OpenSSH would.
func Resolve(host string, opts Options) (*Endpoint, error) {
	cfg, err := loadSSHConfig(opts.ConfigPath)
	if err != nil {
		return nil, err
	}

	ep := &Endpoint{Host: host, HostName: host}
	if hn := cfgGet(cfg, host, "HostName"); hn != "" {
		ep.HostName = hn
	}

	switch {
	case opts.Port != 0:
		ep.Port = opts.Port
	default:
		ep.Port = 22
		if p := cfgGet(cfg, host, "Port"); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil, fmt.Errorf("ssh config: Port %q for host %s is not a number", p, host)
			}
			ep.Port = n
		}
	}

	switch {
	case opts.User != "":
		ep.User = opts.User
	case cfgGet(cfg, host, "User") != "":
		ep.User = cfgGet(cfg, host, "User")
	default:
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("no --ssh-user given, none in ssh config, and the current user is unknown: %w", err)
		}
		ep.User = u.Username
	}

	// Authentication, in precedence order. All available sources are offered;
	// the SSH handshake tries them in order.
	if opts.KeyPath != "" {
		m, err := keyFileAuth(opts.KeyPath, opts.Passphrase)
		if err != nil {
			return nil, err
		}
		ep.auth = append(ep.auth, m)
		ep.AuthSources = append(ep.AuthSources, "key file "+opts.KeyPath)
	}

	sock := opts.AgentSocket
	if sock == "" {
		sock = os.Getenv("SSH_AUTH_SOCK")
	}
	if sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			ep.auth = append(ep.auth, ssh.PublicKeysCallback(ag.Signers))
			ep.AuthSources = append(ep.AuthSources, "ssh-agent")
		}
	}

	if idf := cfgGet(cfg, host, "IdentityFile"); idf != "" && idf != "~/.ssh/identity" {
		path := expandHome(idf)
		if path != opts.KeyPath {
			if _, err := os.Stat(path); err == nil {
				if m, err := keyFileAuth(path, opts.Passphrase); err == nil {
					ep.auth = append(ep.auth, m)
					ep.AuthSources = append(ep.AuthSources, "IdentityFile "+path+" from ssh config")
				}
			}
		}
	}

	if len(ep.auth) == 0 {
		return nil, fmt.Errorf(
			"no SSH credential for %s: pass --ssh-key, add the key to ssh-agent, or set an IdentityFile for it in ~/.ssh/config", host)
	}
	return ep, nil
}

func loadSSHConfig(path string) (*ssh_config.Config, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".ssh", "config")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no config is fine
		}
		return nil, err
	}
	defer f.Close()
	cfg, err := ssh_config.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

func cfgGet(cfg *ssh_config.Config, host, key string) string {
	if cfg == nil {
		return ""
	}
	v, err := cfg.Get(host, key)
	if err != nil {
		return ""
	}
	// The library returns its built-in default for some keys; treat the
	// defaults we handle ourselves as absent.
	if key == "Port" && v == "22" {
		return ""
	}
	return v
}

func expandHome(p string) string {
	if len(p) >= 2 && p[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func keyFileAuth(path string, passphrase func() ([]byte, error)) (ssh.AuthMethod, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SSH key %s: %w", path, err)
	}
	km := newKeyMaterial(raw)

	signer, err := km.signer()
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if passphrase == nil {
			return nil, fmt.Errorf("SSH key %s is passphrase-protected: add it to ssh-agent, or run interactively so the passphrase can be prompted", path)
		}
		pw, perr := passphrase()
		if perr != nil {
			return nil, perr
		}
		signer, err = km.signerWithPassphrase(pw)
	}
	if err != nil {
		return nil, fmt.Errorf("parse SSH key %s: %w", path, err)
	}
	return ssh.PublicKeys(signer), nil
}

// Client is an established SSH connection to one host.
type Client struct {
	Endpoint *Endpoint
	conn     *ssh.Client
}

// Dial connects to the resolved endpoint, verifying the host key with
// accept-new semantics against known_hosts (see hostkey.go).
func Dial(ctx context.Context, ep *Endpoint, opts Options) (*Client, error) {
	hostKey, err := hostKeyCallback(opts.KnownHostsPath)
	if err != nil {
		return nil, err
	}

	timeout := opts.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	addr := net.JoinHostPort(ep.HostName, strconv.Itoa(ep.Port))

	d := net.Dialer{Timeout: timeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}

	conf := &ssh.ClientConfig{
		User:            ep.User,
		Auth:            ep.auth,
		HostKeyCallback: hostKey,
		Timeout:         timeout,
	}
	c, chans, reqs, err := ssh.NewClientConn(raw, addr, conf)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("SSH to %s@%s (credentials tried: %v): %w", ep.User, addr, ep.AuthSources, err)
	}
	return &Client{Endpoint: ep, conn: ssh.NewClient(c, chans, reqs)}, nil
}

// Result is the outcome of one remote command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes one command on the host and waits for it.
func (c *Client) Run(ctx context.Context, command string) (Result, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()

	var stdout, stderr limitedBuffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()

	select {
	case <-ctx.Done():
		sess.Close()
		<-done
		return Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1}, ctx.Err()
	case err := <-done:
		res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitStatus()
			return res, nil
		}
		if err != nil {
			return res, err
		}
		return res, nil
	}
}

func (c *Client) Close() error { return c.conn.Close() }

// limitedBuffer caps captured output at 8 MiB so a runaway remote command
// cannot exhaust memory on the installer machine.
type limitedBuffer struct {
	b []byte
}

const outputCap = 8 << 20

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if len(l.b) < outputCap {
		room := outputCap - len(l.b)
		if room > len(p) {
			room = len(p)
		}
		l.b = append(l.b, p[:room]...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return string(l.b) }
