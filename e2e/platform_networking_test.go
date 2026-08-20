//go:build e2e

// Package e2e runs the platform-networking acceptance test against a REAL
// host (no mocks — workspace rule §1): a Hetzner node from
// `./scripts/ephemeral-env.sh up --profile host`.
//
// Run from the umbrella workspace:
//
//	source lab/hetzner/.lab-env.sh
//	cd kubenest-cli && go test -tags e2e -v -timeout 30m ./e2e/
//
// The suite installs pinned k3s (the stage-3 harness), then the real
// stage-5/6 components — Gateway API CRDs, Traefik, cert-manager, the
// platform Gateway defaults — and then proves the kn-pgu × kn-e7qy contract
// end to end: an app exposed by an HTTPRoute shaped exactly like the
// backend's render is reachable over TLS through Traefik, its certificate
// chains to the cert-manager-issued platform CA, plain HTTP redirects, and
// the cluster contains zero Ingress objects.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/component/certmanager"
	"kubenest.io/cli/pkg/component/gatewayapi"
	"kubenest.io/cli/pkg/component/traefik"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

const appHost = "whoami.e2e.test"

// whoamiManifest deploys a test app and exposes it with an HTTPRoute shaped
// exactly like kubenest-backend's gateway_route.py render: parentRefs to the
// platform Gateway, a real hostname, PathPrefix, no sectionName, no TLS.
const whoamiManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: e2e-whoami
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: whoami
  namespace: e2e-whoami
spec:
  replicas: 1
  selector:
    matchLabels: {app: whoami}
  template:
    metadata:
      labels: {app: whoami}
    spec:
      containers:
        - name: whoami
          image: traefik/whoami:v1.10.2
          ports: [{containerPort: 80}]
---
apiVersion: v1
kind: Service
metadata:
  name: whoami
  namespace: e2e-whoami
spec:
  selector: {app: whoami}
  ports: [{port: 80, targetPort: 80}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: whoami
  namespace: e2e-whoami
spec:
  parentRefs:
    - name: ` + traefik.GatewayName + `
      namespace: ` + traefik.Namespace + `
  hostnames: ["` + appHost + `"]
  rules:
    - matches:
        - path: {type: PathPrefix, value: /}
      backendRefs:
        - name: whoami
          port: 80
`

type testEnv struct {
	client *sshx.Client
	bundle *manifest.Manifest
	rep    converge.Reporter
	ip     string
}

func setup(t *testing.T) *testEnv {
	t.Helper()

	ip := os.Getenv("KUBENEST_LAB_NODE1_IP")
	if ip == "" {
		t.Skip("KUBENEST_LAB_NODE1_IP unset — bring up a host with ./scripts/ephemeral-env.sh up --profile host and `source lab/hetzner/.lab-env.sh`")
	}
	bundlePath := os.Getenv("KUBENEST_BUNDLE_MANIFEST")
	if bundlePath == "" {
		bundlePath = filepath.Join("..", "..", "kubenest-contracts", "bundles", "platform-1.0.yaml")
	}
	m, err := manifest.Load(bundlePath)
	if err != nil {
		t.Skipf("bundle manifest not readable (set KUBENEST_BUNDLE_MANIFEST): %v", err)
	}

	user := os.Getenv("KUBENEST_LAB_SSH_USER")
	if user == "" {
		user = "ubuntu"
	}
	keyPath := os.Getenv("KUBENEST_LAB_SSH_KEY")
	if keyPath == "" {
		home, _ := os.UserHomeDir()
		keyPath = filepath.Join(home, ".ssh", "id_ed25519")
	}

	opts := sshx.Options{
		User:    user,
		KeyPath: keyPath,
		// Lab IPs are recycled cloud IPs; pinning them in the user's
		// known_hosts would only produce clashes (same call the lab script
		// makes with UserKnownHostsFile=/dev/null).
		KnownHostsPath: filepath.Join(t.TempDir(), "known_hosts"),
		DialTimeout:    15 * time.Second,
	}
	ep, err := sshx.Resolve(ip, opts)
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshx.Dial(context.Background(), ep, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	return &testEnv{
		client: client,
		bundle: m,
		rep:    converge.NewTextReporter(testWriter{t}),
		ip:     ip,
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// installK3s is the stage-3 HARNESS (kn-7k8 owns the real stage): pinned k3s
// as a single server with embedded etcd (decision A) and the bundled Traefik
// disabled — the platform pins its own (D9).
func installK3s(t *testing.T, env *testEnv) {
	ctx := context.Background()
	if res, err := env.client.Run(ctx, "systemctl is-active k3s"); err == nil && strings.TrimSpace(res.Stdout) == "active" {
		t.Log("k3s already active — skipping install (idempotent re-run)")
		return
	}

	version, err := env.bundle.Core.Version("k3s")
	if err != nil {
		t.Fatal(err)
	}
	nodeReady, err := env.bundle.Limits.Timeouts.For("node-ready")
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("installing k3s %s (server, embedded etcd, bundled traefik and local-path disabled)", version)
	// --disable local-storage matches the bundle's stage-3 shape: OpenEBS is
	// the platform's storage, and stock local-path would be a second default
	// StorageClass (kn-7k8 / kn-1nn).
	cmd := fmt.Sprintf(
		"curl -sfL https://get.k3s.io | sudo -n INSTALL_K3S_VERSION='%s' sh -s - server --cluster-init --disable traefik --disable local-storage",
		version)
	res, err := env.client.Run(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("k3s install exit %d:\n%s", res.ExitCode, res.Stderr)
	}

	nodesReady := func(ctx context.Context) (bool, converge.State, error) {
		out, err := k3s.Kubectl(ctx, env.client, "get nodes -o json")
		if err != nil {
			return false, converge.State{Object: "nodes", Status: "unobservable"}, err
		}
		var nodes struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					Conditions []struct {
						Type    string `json:"type"`
						Status  string `json:"status"`
						Message string `json:"message"`
					} `json:"conditions"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &nodes); err != nil {
			return false, converge.State{Object: "nodes", Status: "unparsable"}, err
		}
		if len(nodes.Items) == 0 {
			return false, converge.State{Object: "nodes", Status: "none registered yet"}, nil
		}
		for _, n := range nodes.Items {
			for _, c := range n.Status.Conditions {
				if c.Type == "Ready" && c.Status != "True" {
					return false, converge.State{Object: "node " + n.Metadata.Name, Status: "NotReady", Detail: c.Message}, nil
				}
			}
		}
		return true, converge.State{Object: "nodes", Status: "Ready"}, nil
	}

	result, err := converge.Wait(ctx, nodesReady, converge.Options{
		Name: "node-ready", Deadline: nodeReady, Reporter: env.rep,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestPlatformNetworking(t *testing.T) {
	env := setup(t)
	ctx := context.Background()

	installK3s(t, env)

	// Stage 5: networking.
	if err := gatewayapi.Install(ctx, env.client, env.bundle, env.rep); err != nil {
		t.Fatalf("gateway-api: %v", err)
	}
	if err := traefik.Install(ctx, env.client, env.bundle, env.rep); err != nil {
		t.Fatalf("traefik: %v", err)
	}
	// Stage 6: certs, then the platform Gateway defaults on top.
	if err := certmanager.Install(ctx, env.client, env.bundle, env.rep); err != nil {
		t.Fatalf("cert-manager: %v", err)
	}
	if err := traefik.InstallGatewayDefaults(ctx, env.client, env.bundle, env.rep); err != nil {
		t.Fatalf("gateway defaults: %v", err)
	}

	// The app, exposed exactly the way the backend exposes workloads.
	if err := k3s.WriteManifest(ctx, env.client, "e2e-whoami", []byte(whoamiManifest)); err != nil {
		t.Fatal(err)
	}

	deadline, err := env.bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		t.Fatal(err)
	}

	// EVERY acceptance below is a convergence check, never a sample — routes
	// attach, certs issue, klipper-lb binds and helm upgrades roll on their
	// own schedules (a sampled cert check here caught Traefik's built-in
	// cert seconds before the upgraded config served the platform one).
	acceptance := []struct {
		name    string
		command string
		accept  func(stdout string) (ok bool, status string)
	}{
		{
			// The app answers over TLS, through the proxy, with its body.
			name: "app-served-via-tls",
			command: fmt.Sprintf(
				"curl -ksS --resolve %s:443:%s https://%s/", appHost, env.ip, appHost),
			accept: func(out string) (bool, string) {
				switch {
				case !strings.Contains(out, "Hostname: whoami-"):
					return false, "response is not the whoami app yet"
				case !strings.Contains(out, "X-Forwarded-Host: "+appHost):
					return false, "response not passing through the proxy"
				}
				return true, "served via Traefik"
			},
		},
		{
			// The serving certificate is cert-manager's, from the platform CA.
			name: "cert-issued-by-platform-ca",
			command: fmt.Sprintf(
				"echo | openssl s_client -connect %s:443 -servername %s 2>/dev/null | openssl x509 -noout -issuer", env.ip, appHost),
			accept: func(out string) (bool, string) {
				if !strings.Contains(out, "KubeNest Platform CA") {
					return false, strings.TrimSpace(out)
				}
				return true, "issuer is the platform CA"
			},
		},
		{
			// Plain HTTP permanently redirects to TLS before any routing
			// (Traefik's `permanent: true` answers 301).
			name: "http-redirects-to-https",
			command: fmt.Sprintf(
				"curl -s -o /dev/null -w '%%{http_code} %%{redirect_url}' --resolve %s:80:%s http://%s/", appHost, env.ip, appHost),
			accept: func(out string) (bool, string) {
				if !strings.HasPrefix(out, "301 https://"+appHost+"/") {
					return false, strings.TrimSpace(out)
				}
				return true, "301 to https"
			},
		},
	}
	for _, a := range acceptance {
		a := a
		probe := func(ctx context.Context) (bool, converge.State, error) {
			res, err := env.client.Run(ctx, a.command)
			if err != nil {
				return false, converge.State{Object: a.name, Status: "unreachable"}, err
			}
			ok, status := a.accept(res.Stdout)
			state := converge.State{Object: a.name, Status: status}
			if !ok && res.ExitCode != 0 {
				state.Detail = strings.TrimSpace(res.Stderr)
			}
			return ok, state, nil
		}
		result, err := converge.Wait(ctx, probe, converge.Options{
			Name: a.name, Deadline: deadline, Reporter: env.rep,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := result.Err(); err != nil {
			t.Error(err)
		}
	}

	// Zero Ingress objects: the platform is Gateway-native, and nothing on a
	// platform cluster may quietly depend on the retired stack.
	out, err := k3s.Kubectl(ctx, env.client, "get ingress -A -o json")
	if err != nil {
		t.Fatal(err)
	}
	var ingresses struct {
		Items []any `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &ingresses); err != nil {
		t.Fatal(err)
	}
	if len(ingresses.Items) != 0 {
		t.Errorf("found %d Ingress objects on a platform cluster, want zero", len(ingresses.Items))
	}
}
