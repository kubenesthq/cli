package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// genKey returns a fresh ed25519 keypair: the PEM private key bytes and the
// SSH public key.
func genKey(t *testing.T) ([]byte, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), sshPub
}

func writeKey(t *testing.T, pemBytes []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_test")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// noAgent makes agent resolution deterministic regardless of the developer's
// environment.
const noAgent = "/nonexistent/agent.sock"

func TestResolveReadsSSHConfig(t *testing.T) {
	pemBytes, _ := genKey(t)
	keyPath := writeKey(t, pemBytes)

	cfgPath := filepath.Join(t.TempDir(), "config")
	cfg := fmt.Sprintf("Host web\n  HostName 10.9.8.7\n  User ubuntu\n  Port 2222\n  IdentityFile %s\n", keyPath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	ep, err := Resolve("web", Options{ConfigPath: cfgPath, AgentSocket: noAgent})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ep.HostName != "10.9.8.7" || ep.Port != 2222 || ep.User != "ubuntu" {
		t.Errorf("resolved %s@%s:%d, want ubuntu@10.9.8.7:2222", ep.User, ep.HostName, ep.Port)
	}
	if len(ep.AuthSources) != 1 || !strings.Contains(ep.AuthSources[0], "IdentityFile") {
		t.Errorf("auth sources = %v, want the config IdentityFile", ep.AuthSources)
	}
}

func TestResolveFlagsBeatSSHConfig(t *testing.T) {
	pemBytes, _ := genKey(t)
	explicit := writeKey(t, pemBytes)

	cfgPath := filepath.Join(t.TempDir(), "config")
	cfg := "Host web\n  HostName 10.9.8.7\n  User ubuntu\n  Port 2222\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	ep, err := Resolve("web", Options{
		User: "root", Port: 22022, KeyPath: explicit,
		ConfigPath: cfgPath, AgentSocket: noAgent,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ep.User != "root" || ep.Port != 22022 {
		t.Errorf("resolved %s:%d, flags should beat ssh config (root:22022)", ep.User, ep.Port)
	}
	if !strings.Contains(ep.AuthSources[0], "key file") {
		t.Errorf("auth sources = %v, want the explicit key first", ep.AuthSources)
	}
}

func TestResolveWithNoCredentialNamesTheThreeWays(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config") // does not exist
	_, err := Resolve("bare-host", Options{ConfigPath: cfgPath, AgentSocket: noAgent})
	if err == nil {
		t.Fatal("expected an error with no credential available")
	}
	for _, way := range []string{"--ssh-key", "ssh-agent", "ssh/config"} {
		if !strings.Contains(err.Error(), way) {
			t.Errorf("error %q does not mention %s", err, way)
		}
	}
}

func TestEncryptedKeyWithoutPromptIsActionable(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	keyPath := writeKey(t, pem.EncodeToMemory(block))

	cfgPath := filepath.Join(t.TempDir(), "config")
	_, err = Resolve("h", Options{KeyPath: keyPath, ConfigPath: cfgPath, AgentSocket: noAgent})
	if err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Errorf("error %v should say the key needs a passphrase", err)
	}
}

// --- The property the docs sell: key material never leaves the machine. ---

// keyBody extracts the base64 payload of the PEM, the recognizable secret.
func keyBody(t *testing.T, pemBytes []byte) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(pemBytes)), "\n")
	if len(lines) < 3 {
		t.Fatal("unexpected PEM shape")
	}
	return strings.Join(lines[1:len(lines)-1], "")
}

func TestKeyMaterialNeverPrints(t *testing.T) {
	pemBytes, _ := genKey(t)
	km := newKeyMaterial(pemBytes)
	secret := keyBody(t, pemBytes)

	for _, rendered := range []string{
		fmt.Sprint(km),
		fmt.Sprintf("%v", km),
		fmt.Sprintf("%+v", km),
		fmt.Sprintf("%#v", km),
		fmt.Sprintf("%s", km),
		fmt.Sprintf("%x", km),
		fmt.Sprintf("%q", km),
	} {
		if strings.Contains(rendered, secret) || strings.Contains(rendered, "PRIVATE KEY") {
			t.Fatalf("key material leaked through fmt: %q", rendered)
		}
		if !strings.Contains(rendered, "redacted") {
			t.Errorf("rendering %q should carry the redaction marker", rendered)
		}
	}
}

func TestKeyMaterialRefusesSerialization(t *testing.T) {
	pemBytes, _ := genKey(t)
	km := newKeyMaterial(pemBytes)

	if _, err := json.Marshal(km); err == nil {
		t.Error("json.Marshal of key material must fail")
	}
	if _, err := km.MarshalText(); err == nil {
		t.Error("MarshalText of key material must fail")
	}
	if _, err := km.MarshalBinary(); err == nil {
		t.Error("MarshalBinary of key material must fail")
	}
}

// A resolved Endpoint is the object that gets logged and could plausibly be
// reported to the control plane. Serializing it must yield no key bytes.
func TestEndpointSerializesWithoutKeyMaterial(t *testing.T) {
	pemBytes, _ := genKey(t)
	keyPath := writeKey(t, pemBytes)
	secret := keyBody(t, pemBytes)

	cfgPath := filepath.Join(t.TempDir(), "config")
	ep, err := Resolve("10.0.0.5", Options{
		User: "ubuntu", KeyPath: keyPath, ConfigPath: cfgPath, AgentSocket: noAgent,
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := json.Marshal(ep)
	if err != nil {
		t.Fatalf("an Endpoint must serialize cleanly (without credentials): %v", err)
	}
	if strings.Contains(string(out), secret) {
		t.Fatal("endpoint JSON contains private key material")
	}
	for _, rendered := range []string{fmt.Sprintf("%v", ep), fmt.Sprintf("%+v", ep)} {
		if strings.Contains(rendered, secret) {
			t.Fatal("endpoint fmt output contains private key material")
		}
	}
}

// --- Transport against a real in-process SSH server (no mocks). ---

// startSSHServer runs a minimal but genuine SSH server: real handshake, real
// public-key auth, real session channel with exec support.
func startSSHServer(t *testing.T, authorized ssh.PublicKey) (addr string, hostKey ssh.PublicKey) {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	conf := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("unknown key")
		},
	}
	conf.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func(raw net.Conn) {
				conn, chans, reqs, err := ssh.NewServerConn(raw, conf)
				if err != nil {
					return
				}
				defer conn.Close()
				go ssh.DiscardRequests(reqs)
				for newCh := range chans {
					if newCh.ChannelType() != "session" {
						newCh.Reject(ssh.UnknownChannelType, "only session")
						continue
					}
					ch, chReqs, err := newCh.Accept()
					if err != nil {
						continue
					}
					go func(ch ssh.Channel, chReqs <-chan *ssh.Request) {
						defer ch.Close()
						for req := range chReqs {
							if req.Type != "exec" {
								req.Reply(false, nil)
								continue
							}
							var payload struct{ Command string }
							ssh.Unmarshal(req.Payload, &payload)
							req.Reply(true, nil)

							exit := uint32(0)
							switch payload.Command {
							case "echo hello":
								ch.Write([]byte("hello\n"))
							case "false":
								exit = 1
							case "wc -c":
								// Consume streamed stdin fully; report bytes.
								n, _ := io.Copy(io.Discard, ch)
								fmt.Fprintf(ch, "%d\n", n)
							default:
								ch.Stderr().Write([]byte("unknown command\n"))
								exit = 127
							}
							status := struct{ Status uint32 }{exit}
							ch.SendRequest("exit-status", false, ssh.Marshal(&status))
							return
						}
					}(ch, chReqs)
				}
			}(raw)
		}
	}()

	return ln.Addr().String(), hostSigner.PublicKey()
}

func TestDialAndRunAgainstRealServer(t *testing.T) {
	pemBytes, pub := genKey(t)
	keyPath := writeKey(t, pemBytes)
	addr, _ := startSSHServer(t, pub)

	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	opts := Options{
		User: "test", KeyPath: keyPath, Port: port,
		ConfigPath:     filepath.Join(t.TempDir(), "config"),
		KnownHostsPath: knownHosts,
		AgentSocket:    noAgent,
	}
	ep, err := Resolve(host, opts)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	client, err := Dial(ctx, ep, opts)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	res, err := client.Run(ctx, "echo hello")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stdout != "hello\n" || res.ExitCode != 0 {
		t.Errorf("run = %+v, want stdout 'hello' exit 0", res)
	}

	res, err = client.Run(ctx, "false")
	if err != nil {
		t.Fatalf("run false: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit = %d, want 1 (non-zero exit is a result, not an error)", res.ExitCode)
	}

	// First connect recorded the host key (accept-new).
	kh, err := os.ReadFile(knownHosts)
	if err != nil || len(kh) == 0 {
		t.Fatalf("known_hosts not written on first connect: %v", err)
	}

	// Second connect verifies against the recorded key.
	client2, err := Dial(ctx, ep, opts)
	if err != nil {
		t.Fatalf("second dial should verify against recorded key: %v", err)
	}
	client2.Close()
}

// A payload far past the SSH exec-request packet cap must stream cleanly
// over stdin — inlining ~700KB of manifest into the command string is what
// failed on a real host (EOF), so this is the regression test at 1MB.
func TestRunInputStreamsLargePayload(t *testing.T) {
	pemBytes, pub := genKey(t)
	keyPath := writeKey(t, pemBytes)
	addr, _ := startSSHServer(t, pub)

	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	opts := Options{
		User: "test", KeyPath: keyPath, Port: port,
		ConfigPath:     filepath.Join(t.TempDir(), "config"),
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
		AgentSocket:    noAgent,
	}
	ep, err := Resolve(host, opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(context.Background(), ep, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	payload := bytes.Repeat([]byte("y"), 1<<20)
	res, err := client.RunInput(context.Background(), "wc -c", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("run with stdin: %v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "1048576" {
		t.Errorf("server received %s bytes, want 1048576 — stdin did not stream intact", got)
	}
}

func TestChangedHostKeyIsRefused(t *testing.T) {
	pemBytes, pub := genKey(t)
	keyPath := writeKey(t, pemBytes)
	addr, _ := startSSHServer(t, pub)

	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// Pre-record a DIFFERENT key for this host, as if the machine changed.
	_, otherPub := genKey(t)
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	entry := fmt.Sprintf("[%s]:%d %s", host, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(otherPub))))
	if err := os.WriteFile(knownHosts, []byte(entry+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		User: "test", KeyPath: keyPath, Port: port,
		ConfigPath:     filepath.Join(t.TempDir(), "config"),
		KnownHostsPath: knownHosts,
		AgentSocket:    noAgent,
	}
	ep, err := Resolve(host, opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Dial(context.Background(), ep, opts)
	if err == nil || !strings.Contains(err.Error(), "HOST KEY CHANGED") {
		t.Errorf("dial with a changed host key must refuse loudly, got: %v", err)
	}
}
