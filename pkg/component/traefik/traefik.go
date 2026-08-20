// Package traefik installs the platform's core ingress: Traefik with the
// Gateway API provider (D9 — ingress-nginx is EOL and never enters the
// bundle), plus the platform's Gateway defaults.
//
// Two installers live here, matching two moments in the 13-stage sequence:
//
//   - Install (stage 5, platform-networking): the pinned Traefik chart with
//     the Gateway API provider on and the Ingress provider off — the platform
//     is Gateway-native from day one. HTTP→HTTPS redirect and responding
//     timeouts are declared here at the entrypoint level.
//   - InstallGatewayDefaults (after stage 6 has cert-manager up): the
//     kubenest CA issuer chain, the default listener certificate, and the
//     Gateway itself — named and namespaced exactly as the app layer's
//     HTTPRoutes expect (kn-e7qy: parentRefs → kubenest-gateway in
//     kubenest-system; routes carry no TLS and no sectionName, so TLS and
//     redirect live here and listeners carry no hostname filter).
package traefik

import (
	"context"
	"fmt"

	"kubenest.io/cli/pkg/component"
	"kubenest.io/cli/pkg/converge"
	"kubenest.io/cli/pkg/k3s"
	"kubenest.io/cli/pkg/manifest"
)

const (
	// Namespace is where Traefik and the Gateway live. The app layer's
	// HTTPRoute parentRefs point here (backend KUBENEST_GATEWAY_NAMESPACE).
	Namespace = "kubenest-system"
	// GatewayName matches the backend's KUBENEST_GATEWAY_NAME default.
	GatewayName = "kubenest-gateway"
	// GatewayClassName is the class the Traefik chart creates and the
	// Gateway references.
	GatewayClassName = "traefik"

	chartRepo = "https://traefik.github.io/charts"
)

// Values is the chart values document the platform installs Traefik with.
// Declared as one literal so a reviewer reads the entire ingress posture in
// one place. Key structure follows chart 41.x (verified against
// `helm show values traefik/traefik --version 41.2.0`).
const Values = `# KubeNest platform ingress posture (kn-pgu).
providers:
  kubernetesGateway:
    # The platform is Gateway API native.
    enabled: true
  kubernetesIngress:
    # No Ingress controller: an Ingress object on a platform cluster is a
    # bug, and the e2e gate asserts there are none.
    enabled: false
gateway:
  # The chart's default Gateway is not used; the platform owns
  # kubenest-gateway (see InstallGatewayDefaults) with cert-manager TLS.
  enabled: false
gatewayClass:
  enabled: true
  name: traefik
tlsStore:
  # The platform's default serving certificate, issued by cert-manager from
  # the platform CA (see defaults.go). Traefik falls back to the default
  # store certificate when no certificate matches the SNI — without this it
  # serves its own self-signed TRAEFIK DEFAULT CERT (observed on a real
  # host), and nothing a client saw would chain to the platform CA.
  default:
    defaultCertificate:
      secretName: kubenest-gateway-default-cert
ports:
  web:
    http:
      redirections:
        # Sane default: everything on 80 is permanently redirected to TLS
        # before any routing happens. ACME HTTP-01 still works — Let's
        # Encrypt follows redirects to HTTPS.
        entryPoint:
          to: websecure
          scheme: https
          permanent: true
  websecure:
    transport:
      # Sensible timeouts, declared rather than inherited: 60s to read a
      # request, no write cap (long downloads and SSE), 3m idle keep-alive.
      respondingTimeouts:
        readTimeout: 60s
        writeTimeout: 0s
        idleTimeout: 180s
`

// Chart renders the pinned HelmChart custom resource for Traefik.
func Chart(bundle *manifest.Manifest) (k3s.HelmChart, error) {
	version, err := bundle.Core.Version("traefik")
	if err != nil {
		return k3s.HelmChart{}, err
	}
	return k3s.HelmChart{
		Name:            "kubenest-traefik",
		Repo:            chartRepo,
		Chart:           "traefik",
		Version:         version,
		TargetNamespace: Namespace,
		ValuesYAML:      Values,
	}, nil
}

// Install applies the Traefik chart and converges until its pods are Ready
// and the GatewayClass is Accepted. Requires the Gateway API CRDs
// (pkg/component/gatewayapi) — stage order guarantees it; on a mis-ordered
// run the converge deadline fails with the accepting condition named.
func Install(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	chart, err := Chart(bundle)
	if err != nil {
		return err
	}
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}

	doc, err := chart.Manifest()
	if err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, chart.Name, doc); err != nil {
		return err
	}

	res, err := converge.Wait(ctx, k3s.PodsReadyProbe(r, Namespace), converge.Options{
		Name:     "traefik-ready",
		Deadline: deadline,
		Reporter: rep,
	})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return err
	}

	res, err = converge.Wait(ctx,
		component.ConditionProbe(r, "gatewayclass/"+GatewayClassName, "", "Accepted"),
		converge.Options{Name: "gatewayclass-accepted", Deadline: deadline, Reporter: rep})
	if err != nil {
		return err
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("%w (is the Gateway API CRD stage installed before Traefik?)", err)
	}
	return nil
}
