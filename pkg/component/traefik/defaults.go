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
	// CAIssuerName is the platform's default ClusterIssuer. It signs from a
	// cluster-local CA generated at install — honest TLS-on-day-one for a
	// cluster that has no DNS or public issuer configured yet. The customer
	// replaces it with Let's Encrypt or an enterprise issuer on day 2; the
	// Gateway annotation is the single switch point.
	CAIssuerName = "kubenest-ca"

	// DefaultCertSecret backs the HTTPS listener. The listener carries no
	// hostname filter (routes bring their own hostnames, per the app-layer
	// contract), so cert-manager's per-listener shim cannot mint this one —
	// it is an explicit Certificate with a documented internal name, served
	// when no better certificate matches the SNI.
	DefaultCertSecret = "kubenest-gateway-default-cert"

	// certManagerNamespace is where the CA material lives: a CA ClusterIssuer
	// reads its secret from cert-manager's own cluster-resource namespace.
	certManagerNamespace = "cert-manager"
)

// DefaultsManifest renders the platform's ingress defaults: the namespace,
// the CA issuer chain, the default listener certificate, and the Gateway the
// entire app layer attaches to.
//
// The Gateway's name and namespace are the app-layer contract (kn-e7qy):
// HTTPRoutes render parentRefs → kubenest-gateway in kubenest-system, no
// sectionName, no TLS. Listeners therefore allow routes from every namespace
// and carry no hostname filter; TLS terminates here and HTTP redirects at
// the entrypoint (see Values).
const DefaultsManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: ` + Namespace + `
---
# Bootstrap issuer: exists only to sign the CA below.
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: kubenest-selfsigned-bootstrap
spec:
  selfSigned: {}
---
# The cluster-local platform CA. Trust this one certificate to trust every
# default cert the platform issues.
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: kubenest-ca
  namespace: ` + certManagerNamespace + `
spec:
  isCA: true
  commonName: KubeNest Platform CA
  secretName: kubenest-ca
  duration: 87600h # 10y — rotation is a day-2 operation with its own bead
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: kubenest-selfsigned-bootstrap
    kind: ClusterIssuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ` + CAIssuerName + `
spec:
  ca:
    secretName: kubenest-ca
---
# Default certificate for the hostname-less HTTPS listener. Served when no
# per-host certificate matches the SNI; the name is deliberately an internal
# placeholder, replaced per-host on day 2.
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: kubenest-gateway-default
  namespace: ` + Namespace + `
spec:
  commonName: default.gateway.kubenest.internal
  dnsNames:
    - default.gateway.kubenest.internal
  secretName: ` + DefaultCertSecret + `
  issuerRef:
    name: ` + CAIssuerName + `
    kind: ClusterIssuer
    group: cert-manager.io
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ` + GatewayName + `
  namespace: ` + Namespace + `
  annotations:
    # Day-2 switch point: repoint this at a real issuer and cert-manager's
    # gateway integration takes over per-listener certificates.
    cert-manager.io/cluster-issuer: ` + CAIssuerName + `
spec:
  gatewayClassName: ` + GatewayClassName + `
  # Listener ports are Traefik's ENTRYPOINT ports (chart: web=8000,
  # websecure=8443), not the externally exposed 80/443 — Traefik's Gateway
  # provider matches listeners to entrypoints by port and marks the listener
  # PortUnavailable otherwise (observed on a real host). The Service still
  # exposes 80/443; klipper-lb maps them onto the node.
  listeners:
    - name: web
      port: 8000
      protocol: HTTP
      # Requests on 80 never route: the entrypoint 308s them to websecure.
      allowedRoutes:
        namespaces:
          from: All
    - name: websecure
      port: 8443
      protocol: HTTPS
      # No hostname filter: app HTTPRoutes carry real customer hostnames and
      # attach without a sectionName, so a filter here would orphan them.
      allowedRoutes:
        namespaces:
          from: All
      tls:
        mode: Terminate
        certificateRefs:
          - name: ` + DefaultCertSecret + `
`

// InstallGatewayDefaults applies the defaults and converges: CA Ready →
// default certificate Ready → Gateway Programmed. Requires Traefik (Install)
// and cert-manager (pkg/component/certmanager) — the k3s deploy controller
// retries until their CRDs and webhooks answer, and the converge deadline
// bounds the wait either way.
func InstallGatewayDefaults(ctx context.Context, r k3s.Runner, bundle *manifest.Manifest, rep converge.Reporter) error {
	deadline, err := bundle.Limits.Timeouts.For("component-ready")
	if err != nil {
		return err
	}
	if err := k3s.WriteManifest(ctx, r, "kubenest-gateway-defaults", []byte(DefaultsManifest)); err != nil {
		return err
	}

	waits := []struct {
		name      string
		resource  string
		namespace string
		cond      string
	}{
		{"platform-ca-ready", "certificate/kubenest-ca", certManagerNamespace, "Ready"},
		{"default-cert-ready", "certificate/kubenest-gateway-default", Namespace, "Ready"},
		{"gateway-programmed", "gateway/" + GatewayName, Namespace, "Programmed"},
	}
	for _, w := range waits {
		res, err := converge.Wait(ctx,
			component.ConditionProbe(r, w.resource, w.namespace, w.cond),
			converge.Options{Name: w.name, Deadline: deadline, Reporter: rep})
		if err != nil {
			return err
		}
		if err := res.Err(); err != nil {
			return fmt.Errorf("%w (gateway defaults need cert-manager and Traefik installed first)", err)
		}
	}
	return nil
}
