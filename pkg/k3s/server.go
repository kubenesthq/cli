package k3s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
//	--secrets-encryption    encrypts Secrets at rest in the embedded etcd
//	                        datastore, with a key k3s generates and keeps on
//	                        first start. A datastore snapshot is a copy of
//	                        that datastore, and every snapshot this platform
//	                        ships lands in the customer's own bucket —
//	                        without the flag it carries every Kubernetes
//	                        Secret in plaintext, the bucket credentials it was
//	                        uploaded with among them. That is exactly the
//	                        state bundle 1.2 removes, and unlike the other
//	                        three the flag is cluster-wide: it is set by the
//	                        etcd members, so an agent must never carry it and
//	                        the upgrade path must never backfill it (F19 —
//	                        see ServerFlags).
var serverFlags = []string{"--cluster-init", "--disable", "traefik", "--disable", "local-storage", "--secrets-encryption"}

// ServerFlags returns the canonical flag set. Exported so a test can assert on
// it rather than on a string literal it copied.
//
// --secrets-encryption is deliberately NOT part of InstallAgent: an agent is
// not a server, and the flag is cluster-wide from the etcd members, so a
// worker has nothing to encrypt and nothing to configure. It must also never
// be introduced into the upgrade path: 1.1 clusters are not backfilled (F19),
// and enabling it on an existing datastore without a recovery kit produces
// exactly the unprotected-snapshot state this bundle removes — a flag that
// appears to protect data while the old snapshots in the bucket stay
// readable.
func ServerFlags() []string { return append([]string(nil), serverFlags...) }

// tokenFile is where a node's cluster token lives — staged by every server
// install, the first included, not only by a joiner. Root-only, and
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
	// Token is the cluster token. It is written to a root-only file on the
	// target and passed as --token-file.
	//
	// Required when joining. On the FIRST server it is what the install plan
	// generates once and derives every join from; left empty, InstallServer
	// generates one with GenerateToken, because a server that is the cluster's
	// token source must have a token before k3s starts — reading it back
	// afterwards (NodeToken) is a read path, not a way to create one.
	Token string
}

// InstallServer installs k3s at the bundle's pinned version on a control-plane
// node and waits for it to be Ready.
//
// The cluster token is staged on EVERY server, the first included. The first
// server is where the token is born — k3s writes one to
// /var/lib/rancher/k3s/server/node-token only once it has started — so
// without a token up front the credential exists nowhere until the first
// server is up, and the node that owns the datastore has nothing to
// authenticate the rest of the cluster with. Minting it here (GenerateToken
// when the caller has none; the install plan's one token otherwise) and
// passing --token-file gives the first server the same shape as a joining
// one, and the file persists because k3s bakes the flag into the systemd
// unit.
//
// Idempotent: a node already running the pinned version is left alone, which
// is what makes a resumed install converge rather than reinstall. That early
// return is also why no token is generated or written for it — a resumed
// install must not rotate a cluster's credential, and a node already at the
// pinned version is already a member. A node running a DIFFERENT version is
// an error, not an upgrade — moving between k3s versions is bundle upgrade
// orchestration (kn-fuo), and doing it silently inside an install would be
// the least safe possible way to do it.
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
	token := opts.Token
	if opts.JoinURL != "" {
		if token == "" {
			return fmt.Errorf("joining %s needs the cluster token", opts.JoinURL)
		}
		args = append(args, "--server", opts.JoinURL)
	} else if token == "" {
		generated, err := GenerateToken()
		if err != nil {
			return err
		}
		token = generated
	}
	// Same staging and same flag for the first server and for a joiner: the
	// token travels over STDIN, never in the command string, and the file is
	// PERMANENT (k3s re-reads it on every start of the service).
	if err := writeTokenFile(ctx, r, token); err != nil {
		return err
	}
	args = append(args, "--token-file", tokenFile)

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

// GenerateToken mints a cluster token in k3s's own shape:
//
//	K10<40 lowercase hex>::server:<40 lowercase hex>
//
// The two halves are independent random draws, so two servers installed from
// one plan cannot collide and a token read off one node is not a prefix of
// another's.
//
// This is why the install path mints a token instead of reading one back
// (NodeToken): the first server's token is created by k3s only while it
// starts, so a cluster whose first node has no token staged has no
// credential to join with until that node is up — and the joining servers
// need it before their own k3s starts. Generating it up front, from
// crypto/rand, is what makes the very first server's --token-file possible
// and the whole cluster's token a single value the install plan derives
// every join from.
//
// crypto/rand, not math/rand: this value is a credential. It must never be
// predictable from a sequence of other tokens, and it must never be zero —
// on a rand failure the caller gets an error, because a server started with
// an empty or guessed token is a cluster anyone can join.
func GenerateToken() (string, error) {
	lower, err := tokenHex(20)
	if err != nil {
		return "", err
	}
	upper, err := tokenHex(20)
	if err != nil {
		return "", err
	}
	return "K10" + lower + "::server:" + upper, nil
}

// tokenHex returns n random bytes as 2n lowercase hex characters.
func tokenHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("minting the cluster token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// NodeToken reads the cluster token from an installed server. It is a
// credential: it is returned for immediate use joining other nodes and must
// not be journalled, logged or printed. It is now the READ path only — for
// clusters installed before this change and for `node add` against them; the
// install path generates the token with GenerateToken.
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

// writeTokenFile stages the cluster token root-only. The token travels over
// STDIN and never in the command string.
//
// This comment previously claimed "the value never appears as a command
// argument" while base64-encoding it into exactly that argument — the same
// false-guarantee-plus-encoding shape kn-40rd was filed to remove from
// WriteManifest. The cluster join token is the credential that lets a machine
// become a server node; it belongs on stdin like every other secret here.
func writeTokenFile(ctx context.Context, r Runner, token string) error {
	cmd := fmt.Sprintf(
		"sudo -n install -d -m 0700 /etc/rancher && sudo -n install -m 0600 /dev/stdin %s",
		tokenFile)
	res, err := r.RunInput(ctx, cmd, strings.NewReader(token))
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

// nodeInventory is the slice of `kubectl get nodes -o json` the inventory
// read needs: the Node's UID and every address the cluster knows it by.
type nodeInventory struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"metadata"`
		Status struct {
			Addresses []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"status"`
	} `json:"items"`
}

// NodeUIDsByAddress maps every address the cluster knows a node by to that
// node's metadata.uid (kn-t50).
//
// By ADDRESS, and by all of them, because the two sides name a machine
// differently: the installer has the address the operator typed (or the one
// its ssh connection resolved to), and the Node object has its hostname plus
// whatever InternalIP/ExternalIP the kubelet registered. Matching on one name
// would leave the other hosts' UIDs empty, which reads as "the node does not
// exist yet" about a node that does.
//
// The UID, not the name, is what an inventory entry records: a rebuilt host
// can come back with the same hostname and is a different Node object, and a
// node operation has to be able to tell that apart.
func NodeUIDsByAddress(ctx context.Context, r Runner) (map[string]string, error) {
	out, err := Kubectl(ctx, r, "get nodes -o json")
	if err != nil {
		return nil, err
	}
	var nodes nodeInventory
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return nil, fmt.Errorf("parsing `kubectl get nodes -o json`: %w", err)
	}
	byAddress := make(map[string]string, len(nodes.Items))
	for _, n := range nodes.Items {
		if n.Metadata.UID == "" {
			continue
		}
		if n.Metadata.Name != "" {
			byAddress[n.Metadata.Name] = n.Metadata.UID
		}
		for _, a := range n.Status.Addresses {
			if a.Address != "" {
				byAddress[a.Address] = n.Metadata.UID
			}
		}
	}
	return byAddress, nil
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
