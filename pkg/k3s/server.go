package k3s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/manifest"
)

// The platform's canonical k3s server flags. Every path that installs a
// server — the installer's stage 3, the host-test scaffolds — uses these and
// nothing else, because each one is load-bearing:
//
//	--cluster-init          single-node embedded etcd on EVERY tier
//	                        (decision A, 2026-08-20). It makes single-server
//	                        -> ha "join two more servers" instead of a
//	                        cluster rebuild, and leaves ONE snapshot
//	                        mechanism instead of two.
//	--disable traefik       the platform pins its own Traefik and the Gateway
//	                        API CRDs (D9); k3s's bundled one is a different
//	                        version nobody chose.
//	--disable local-storage k3s ships local-path as a DEFAULT StorageClass.
//	                        The platform's default is kubenest-local (OpenEBS
//	                        Local PV LVM). Two defaults make PVC binding
//	                        order-dependent, and storage.Verify refuses that
//	                        state either way — observed for real on the
//	                        2026-08-20 lab host, where k3s went in without
//	                        the flag.
var serverFlags = []string{"--cluster-init", "--disable", "traefik", "--disable", "local-storage"}

// ServerFlags returns the canonical flag set. Exported so a test can assert on
// it rather than on a string literal it copied.
func ServerFlags() []string { return append([]string(nil), serverFlags...) }

// tokenFile is where a joining node's cluster token lives. Root-only, and
// PERMANENT — see below.
//
// The token never travels on a command line, because a command line is
// visible in the target host's process list. It goes to a 0600 file owned by
// root and is passed as --token-file.
//
// AND IT MUST STAY THERE. k3s bakes the --token-file flag into the systemd
// unit it writes, so the file is read again on every start of the service,
// not only at join. Deleting it after a successful join — which this code did
// — leaves a node that works perfectly until the first time it restarts, and
// then hangs forever on:
//
//	Waiting for file "/etc/rancher/kubenest-join-token" to be created
//
// Found on a real two-node cluster during an upgrade: the agent drained,
// restarted onto the new version, and never came back. The same would happen
// on a kured reboot, or any reboot at all. A node that installs cleanly and
// dies at its first restart is precisely the day-2 failure this product
// exists to prevent, and no unit test would have shown it.
//
// k3s's own installer stores the token on disk too (K3S_TOKEN lands in the
// service's environment file), so this is the upstream shape rather than a
// concession: a joining node needs its credential at every start, and the
// protection that matters is the file's ownership and mode.
const tokenFile = "/etc/rancher/kubenest-join-token"

// ServerOptions configures one control-plane node's install.
type ServerOptions struct {
	// JoinURL is empty for the first server, which initialises the etcd
	// cluster, and https://<first-server>:6443 for the second and third.
	JoinURL string
	// Token is the cluster token, required when joining. It is written to a
	// root-only file on the target and passed as --token-file.
	Token string
}

// InstallServer installs k3s at the bundle's pinned version on a control-plane
// node and waits for it to be Ready.
//
// Idempotent: a node already running the pinned version is left alone, which
// is what makes a resumed install converge rather than reinstall. A node
// running a DIFFERENT version is an error, not an upgrade — moving between
// k3s versions is bundle upgrade orchestration (kn-fuo), and doing it
// silently inside an install would be the least safe possible way to do it.
func InstallServer(ctx context.Context, r Runner, bundle *manifest.Manifest, opts ServerOptions, rep converge.Reporter) error {
	version, err := bundle.Core.Version("k3s")
	if err != nil {
		return err
	}
	installed, current, err := installedVersion(ctx, r, "k3s")
	if err != nil {
		return err
	}
	if installed {
		if current != version {
			return fmt.Errorf(
				"this node already runs k3s %s but bundle %s pins %s: changing the Kubernetes version is a bundle upgrade, not an install (`kubenest platform upgrade`)",
				current, bundle.Bundle, version)
		}
		return waitNodeReady(ctx, r, bundle, rep)
	}

	args := append([]string{"server"}, serverFlags...)
	if opts.JoinURL != "" {
		if opts.Token == "" {
			return fmt.Errorf("joining %s needs the cluster token", opts.JoinURL)
		}
		if err := writeTokenFile(ctx, r, opts.Token); err != nil {
			return err
		}
		args = append(args, "--server", opts.JoinURL, "--token-file", tokenFile)
	}

	if err := runInstaller(ctx, r, version, args); err != nil {
		return err
	}
	return waitNodeReady(ctx, r, bundle, rep)
}

// InstallAgent joins a worker node to the cluster.
func InstallAgent(ctx context.Context, r Runner, bundle *manifest.Manifest, serverURL, token string, rep converge.Reporter) error {
	version, err := bundle.Core.Version("k3s")
	if err != nil {
		return err
	}
	if serverURL == "" || token == "" {
		return fmt.Errorf("an agent needs the server URL and the cluster token to join")
	}
	installed, current, err := installedVersion(ctx, r, "k3s")
	if err != nil {
		return err
	}
	if installed {
		if current != version {
			return fmt.Errorf(
				"this node already runs k3s %s but bundle %s pins %s: changing the Kubernetes version is a bundle upgrade, not an install",
				current, bundle.Bundle, version)
		}
		return nil
	}

	if err := writeTokenFile(ctx, r, token); err != nil {
		return err
	}
	return runInstaller(ctx, r, version, []string{"agent", "--server", serverURL, "--token-file", tokenFile})
}

// runInstaller runs get.k3s.io with the pinned version. INSTALL_K3S_VERSION
// is what pins it; there is no "latest" in a platform bundle.
func runInstaller(ctx context.Context, r Runner, version string, args []string) error {
	cmd := fmt.Sprintf("curl -sfL https://get.k3s.io | sudo -n INSTALL_K3S_VERSION=%s sh -s - %s",
		shellQuote(version), strings.Join(args, " "))
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("installing k3s %s: %w", version, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("installing k3s %s: exit %d: %s", version, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// installedVersion reports whether k3s is present and at what version.
func installedVersion(ctx context.Context, r Runner, binary string) (bool, string, error) {
	res, err := r.Run(ctx, fmt.Sprintf("command -v %s >/dev/null 2>&1 && %s --version 2>/dev/null | head -1 || true", binary, binary))
	if err != nil {
		return false, "", err
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		return false, "", nil
	}
	// "k3s version v1.35.7+k3s1 (abcdef)" -> "v1.35.7+k3s1"
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			return true, fields[i+1], nil
		}
	}
	return true, out, nil
}

// NodeToken reads the cluster token from an installed server. It is a
// credential: it is returned for immediate use joining other nodes and must
// not be journalled, logged or printed.
func NodeToken(ctx context.Context, r Runner) (string, error) {
	res, err := r.Run(ctx, "sudo -n cat /var/lib/rancher/k3s/server/node-token")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("reading the cluster token: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	token := strings.TrimSpace(res.Stdout)
	if token == "" {
		return "", fmt.Errorf("the server has no cluster token yet: k3s did not finish starting")
	}
	return token, nil
}

// writeTokenFile stages the cluster token root-only, base64-encoded in
// transit so nothing needs shell quoting and the value never appears as a
// command argument.
func writeTokenFile(ctx context.Context, r Runner, token string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(token))
	cmd := fmt.Sprintf(
		"sudo -n install -d -m 0700 /etc/rancher && printf '%%s' %s | base64 -d | sudo -n install -m 0600 /dev/stdin %s",
		encoded, tokenFile)
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("staging the cluster token: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("staging the cluster token: exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// waitNodeReady waits for THIS node to report Ready, within the bundle's
// node-ready deadline. Never a single sample.
func waitNodeReady(ctx context.Context, r Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("node-ready")
	if err != nil {
		return err
	}
	res, err := converge.Wait(ctx, nodesReadyProbe(r, 1), converge.Options{
		Name: "node-ready", Deadline: deadline, Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// WaitNodesReady waits until `count` nodes are Ready — stage 4's condition
// once every agent has been joined.
func WaitNodesReady(ctx context.Context, r Runner, bundle *manifest.Manifest, count int, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("node-ready")
	if err != nil {
		return err
	}
	res, err := converge.Wait(ctx, nodesReadyProbe(r, count), converge.Options{
		Name: "nodes-ready", Deadline: deadline, Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

type nodeList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// nodesReadyProbe observes how many nodes are Ready, naming the first that is
// not and why. An API server that is still starting is an observation, not a
// verdict — the deadline decides.
func nodesReadyProbe(r Runner, want int) converge.Probe {
	return func(ctx context.Context) (bool, converge.State, error) {
		out, err := Kubectl(ctx, r, "get nodes -o json")
		if err != nil {
			return false, converge.State{Object: "nodes", Status: "the API server is not answering yet"}, err
		}
		var nodes nodeList
		if err := json.Unmarshal([]byte(out), &nodes); err != nil {
			return false, converge.State{Object: "nodes", Status: "unparsable"}, err
		}
		ready := 0
		var stuck converge.State
		for _, n := range nodes.Items {
			isReady := false
			for _, c := range n.Status.Conditions {
				if c.Type != "Ready" {
					continue
				}
				if c.Status == "True" {
					isReady = true
					break
				}
				stuck = converge.State{
					Object: "node " + n.Metadata.Name,
					Status: "Ready=" + c.Status + " (" + c.Reason + ")",
					Detail: c.Message,
				}
			}
			if isReady {
				ready++
			}
		}
		if ready >= want {
			return true, converge.State{Object: "nodes", Status: fmt.Sprintf("%d/%d Ready", ready, want)}, nil
		}
		if stuck.Object != "" {
			return false, stuck, nil
		}
		return false, converge.State{
			Object: "nodes",
			Status: fmt.Sprintf("%d/%d Ready", ready, want),
			Detail: "waiting for the remaining node(s) to register",
		}, nil
	}
}

// shellQuote single-quotes a value for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
