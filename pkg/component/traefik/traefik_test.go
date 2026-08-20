package traefik

import (
	"errors"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"kubenest.io/cli/pkg/manifest"
)

func bundle(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(`
bundle: "1.0"
core:
  traefik: 41.2.0
  cert-manager: v1.21.1
  gateway-api: v1.6.1
limits:
  timeouts:
    component-ready: 10m
`))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// dig walks a parsed YAML map by key path.
func dig(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %T is not a map", path, cur)
		}
		cur, ok = m[key]
		if !ok {
			t.Fatalf("path %v: key %q missing", path, key)
		}
	}
	return cur
}

func TestChartThreadsTheManifestPin(t *testing.T) {
	c, err := Chart(bundle(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "41.2.0" {
		t.Errorf("version = %q, want the manifest pin 41.2.0", c.Version)
	}
	if c.Repo != "https://traefik.github.io/charts" || c.Chart != "traefik" {
		t.Errorf("chart source = %s/%s", c.Repo, c.Chart)
	}
	if c.TargetNamespace != "kubenest-system" {
		t.Errorf("target namespace = %q", c.TargetNamespace)
	}
}

func TestChartWithoutPinIsAnError(t *testing.T) {
	m, err := manifest.Parse([]byte("bundle: \"1.0\"\ncore:\n  k3s: v1\nlimits:\n  timeouts:\n    component-ready: 10m\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Chart(m)
	if err == nil || !strings.Contains(err.Error(), "core.traefik") {
		t.Errorf("missing pin must error naming core.traefik, got %v", err)
	}
}

// The ingress posture, asserted structurally: Gateway provider on, Ingress
// provider off, chart Gateway off, redirect and timeouts declared.
func TestValuesDeclareTheIngressPosture(t *testing.T) {
	var v map[string]any
	if err := yaml.Unmarshal([]byte(Values), &v); err != nil {
		t.Fatalf("Values is not valid YAML: %v", err)
	}

	if got := dig(t, v, "providers", "kubernetesGateway", "enabled"); got != true {
		t.Error("Gateway API provider must be enabled")
	}
	if got := dig(t, v, "providers", "kubernetesIngress", "enabled"); got != false {
		t.Error("Ingress provider must be disabled — the platform is Gateway-native (D9)")
	}
	if got := dig(t, v, "gateway", "enabled"); got != false {
		t.Error("the chart's default Gateway must be off; the platform owns kubenest-gateway")
	}
	if got := dig(t, v, "gatewayClass", "name"); got != GatewayClassName {
		t.Errorf("gatewayClass name = %v", got)
	}

	redirect := dig(t, v, "ports", "web", "http", "redirections", "entryPoint").(map[string]any)
	if redirect["to"] != "websecure" || redirect["scheme"] != "https" || redirect["permanent"] != true {
		t.Errorf("HTTP->HTTPS redirect not declared: %v", redirect)
	}

	timeouts := dig(t, v, "ports", "websecure", "transport", "respondingTimeouts").(map[string]any)
	for _, key := range []string{"readTimeout", "writeTimeout", "idleTimeout"} {
		if _, ok := timeouts[key]; !ok {
			t.Errorf("responding timeout %s not declared", key)
		}
	}

	// Any SNI without a per-host certificate is served the platform default
	// cert, not Traefik's built-in self-signed one — the secret here must be
	// the one the defaults' Certificate writes.
	if got := dig(t, v, "tlsStore", "default", "defaultCertificate", "secretName"); got != DefaultCertSecret {
		t.Errorf("default TLS store certificate = %v, want %s", got, DefaultCertSecret)
	}
}

// The defaults manifest is the other half of kn-e7qy's landed app-layer
// change: the backend renders HTTPRoutes with parentRefs to EXACTLY this
// Gateway. These constants are a cross-repo contract — if they change, the
// backend's KUBENEST_GATEWAY_NAME / KUBENEST_GATEWAY_NAMESPACE must change
// with them.
func TestGatewayMatchesTheAppLayerContract(t *testing.T) {
	if GatewayName != "kubenest-gateway" || Namespace != "kubenest-system" {
		t.Fatalf("Gateway %s/%s no longer matches the backend's parentRefs default — coordinate with kubenest-backend KUBENEST_GATEWAY_* before changing this", Namespace, GatewayName)
	}

	docs := parseDocs(t, DefaultsManifest)
	gw := findDoc(t, docs, "Gateway", "kubenest-gateway")

	if got := dig(t, gw, "metadata", "namespace"); got != "kubenest-system" {
		t.Errorf("Gateway namespace = %v", got)
	}
	if got := dig(t, gw, "spec", "gatewayClassName"); got != GatewayClassName {
		t.Errorf("gatewayClassName = %v", got)
	}

	listeners := dig(t, gw, "spec", "listeners").([]any)
	if len(listeners) != 2 {
		t.Fatalf("want 2 listeners (web, websecure), got %d", len(listeners))
	}
	// Traefik matches Gateway listeners to entrypoints BY PORT, and the
	// chart's entrypoints are 8000/8443 (the Service exposes 80/443). A
	// listener on 80/443 is PortUnavailable — a real-host observation, and
	// the values in Values and these must move together.
	if p := dig(t, listeners[0].(map[string]any), "port"); p != 8000 {
		t.Errorf("web listener port = %v, want the web entrypoint port 8000", p)
	}
	if p := dig(t, listeners[1].(map[string]any), "port"); p != 8443 {
		t.Errorf("websecure listener port = %v, want the websecure entrypoint port 8443", p)
	}
	for _, l := range listeners {
		lm := l.(map[string]any)
		// Routes attach from app namespaces with no sectionName; a listener
		// that restricts namespaces or filters hostnames orphans them.
		if from := dig(t, lm, "allowedRoutes", "namespaces", "from"); from != "All" {
			t.Errorf("listener %v: allowedRoutes.namespaces.from = %v, want All", lm["name"], from)
		}
		if _, has := lm["hostname"]; has {
			t.Errorf("listener %v carries a hostname filter — app routes bring their own hostnames", lm["name"])
		}
	}

	https := listeners[1].(map[string]any)
	refs := dig(t, https, "tls", "certificateRefs").([]any)
	if name := refs[0].(map[string]any)["name"]; name != DefaultCertSecret {
		t.Errorf("listener certificateRef = %v, want %s", name, DefaultCertSecret)
	}
}

// The issuer chain must be internally consistent, or the Gateway waits
// forever on a secret nothing produces.
func TestIssuerChainIsConsistent(t *testing.T) {
	docs := parseDocs(t, DefaultsManifest)

	ca := findDoc(t, docs, "Certificate", "kubenest-ca")
	if got := dig(t, ca, "spec", "issuerRef", "name"); got != "kubenest-selfsigned-bootstrap" {
		t.Errorf("CA issuerRef = %v", got)
	}
	if got := dig(t, ca, "spec", "isCA"); got != true {
		t.Error("kubenest-ca must be a CA certificate")
	}
	caSecret := dig(t, ca, "spec", "secretName").(string)

	issuer := findDoc(t, docs, "ClusterIssuer", CAIssuerName)
	if got := dig(t, issuer, "spec", "ca", "secretName"); got != caSecret {
		t.Errorf("ClusterIssuer %s reads secret %v but the CA writes %s", CAIssuerName, got, caSecret)
	}

	defCert := findDoc(t, docs, "Certificate", "kubenest-gateway-default")
	if got := dig(t, defCert, "spec", "issuerRef", "name"); got != CAIssuerName {
		t.Errorf("default cert issuerRef = %v, want %s", got, CAIssuerName)
	}
	if got := dig(t, defCert, "spec", "secretName"); got != DefaultCertSecret {
		t.Errorf("default cert writes secret %v but the listener reads %s", got, DefaultCertSecret)
	}

	gw := findDoc(t, docs, "Gateway", GatewayName)
	if got := dig(t, gw, "metadata", "annotations", "cert-manager.io/cluster-issuer"); got != CAIssuerName {
		t.Errorf("Gateway cluster-issuer annotation = %v", got)
	}
}

func parseDocs(t *testing.T, doc string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(doc))
	for {
		var d map[string]any
		err := dec.Decode(&d)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("defaults manifest is not valid YAML: %v", err)
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	if len(docs) < 5 {
		t.Fatalf("expected ns + issuer chain + cert + gateway, got %d docs", len(docs))
	}
	return docs
}

func findDoc(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == kind {
			if md, ok := d["metadata"].(map[string]any); ok && md["name"] == name {
				return d
			}
		}
	}
	t.Fatalf("no %s named %s in the defaults manifest", kind, name)
	return nil
}
