// Package gatewayapi installs the Gateway API CRDs — the standard-channel
// release bundle, at the version pinned as core.gateway-api in the bundle
// manifest. Stage 5 (platform-networking) applies this before Traefik so the
// Gateway provider has its types, and before cert-manager so its gateway
// integration does.
package gatewayapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
	"kubenest.io/cli/pkg/sshx"
)

// ReleaseBaseURL is where the pinned release manifest is fetched from. A
// variable so tests can point it at a local server.
var ReleaseBaseURL = "https://github.com/kubernetes-sigs/gateway-api/releases/download"

// StandardCRDs are the standard-channel CustomResourceDefinitions the bundle
// installs; the verify step requires every one Established.
var StandardCRDs = []string{
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
	"grpcroutes.gateway.networking.k8s.io",
	"referencegrants.gateway.networking.k8s.io",
}

// URL returns the release-manifest URL for a pinned version.
func URL(version string) string {
	return fmt.Sprintf("%s/%s/standard-install.yaml", ReleaseBaseURL, version)
}

// Install fetches the pinned standard-channel manifest, places it in the k3s
// auto-deploy directory, and converges until every CRD is Established. The
// download happens on the installer machine — target nodes need no GitHub
// access for this.
func Install(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	version, err := bundle.Core.Version("gateway-api")
	if err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}

	data, err := fetch(ctx, URL(version))
	if err != nil {
		return fmt.Errorf("download Gateway API %s release manifest: %w", version, err)
	}
	if err := writeStreamed(ctx, r, "kubenest-gateway-api", data); err != nil {
		return err
	}

	res, err := converge.Wait(ctx, component.CRDsEstablishedProbe(r, StandardCRDs), converge.Options{
		Name:     "gateway-api-crds",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	return res.Err()
}

// InputRunner is a Runner that can stream stdin — *sshx.Client implements
// it. The CRD bundle is ~700KB: inlined into a single exec command it blows
// the SSH packet cap (observed as an EOF on a real host), so it must stream.
type InputRunner interface {
	k3s.Runner
	RunInput(ctx context.Context, command string, stdin io.Reader) (sshx.Result, error)
}

// writeStreamed places content in the k3s auto-deploy directory via stdin.
func writeStreamed(ctx context.Context, r k3s.Runner, name string, content []byte) error {
	ir, ok := r.(InputRunner)
	if !ok {
		// Small-content fallback keeps scripted test runners working.
		return k3s.WriteManifest(ctx, r, name, content)
	}
	path := k3s.ManifestDir + "/" + name + ".yaml"
	res, err := ir.RunInput(ctx, "sudo -n tee "+path+" >/dev/null", bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s: exit %d: %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
