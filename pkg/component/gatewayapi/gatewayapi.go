// Package gatewayapi installs the Gateway API CRDs — the standard-channel
// release bundle, at the version pinned as core.gateway-api in the bundle
// manifest. Stage 5 (platform-networking) applies this before Traefik so the
// Gateway provider has its types, and before cert-manager so its gateway
// integration does.
package gatewayapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
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
	if err := k3s.WriteManifest(ctx, r, "kubenest-gateway-api", data); err != nil {
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
