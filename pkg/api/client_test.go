package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUnauthorizedSaysHowToRecover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail": "expired"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("stale"))
	_, err := c.ListBundles(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "kubenest login") {
		t.Errorf("401 error %q does not tell the user to log in again", err)
	}
}

func TestErrorsSurfaceBackendDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail": "bundle catalog unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	_, err := c.ListBundles(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bundle catalog unavailable") {
		t.Errorf("error %v does not carry the backend detail", err)
	}
}

func TestListBundles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/bundles" {
			t.Errorf("path = %q, want /api/v1/bundles", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer knp_tok" {
			t.Errorf("Authorization = %q, want the stored token", auth)
		}
		w.Write([]byte(`{"data": [{"version": "1.0", "ha_tiers": ["single-server", "ha"], "profiles": ["observability"]}]}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	bundles, err := c.ListBundles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 || bundles[0].Version != "1.0" {
		t.Errorf("bundles = %+v, want the 1.0 entry", bundles)
	}
}

// The debug trace is the one place a request is ever written out, so it must
// never carry the credential.
func TestDebugTraceRedactsCredentials(t *testing.T) {
	t.Setenv("KUBENEST_DEBUG", "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()

	var trace bytes.Buffer
	const secret = "knp_super-secret-cli-token"
	c, _ := New(srv.URL, WithToken(secret), WithDebugWriter(&trace))
	if _, err := c.ListBundles(context.Background()); err != nil {
		t.Fatal(err)
	}

	out := trace.String()
	if out == "" {
		t.Fatal("expected a debug trace with KUBENEST_DEBUG=1")
	}
	if strings.Contains(out, secret) {
		t.Errorf("debug trace leaks the CLI token:\n%s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("debug trace should mark the Authorization header redacted:\n%s", out)
	}
}

func TestNewRejectsGarbageURL(t *testing.T) {
	for _, bad := range []string{"", "not-a-url", "//half"} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) accepted an invalid control plane URL", bad)
		}
	}
}

// --- Device flow (contract v1.12.0: /auth/cli/device*) ---

// deviceServer implements the contract's device endpoints: N pending polls,
// then one slow_down, then the token.
func deviceServer(t *testing.T, pendingPolls int) (*httptest.Server, *[]string) {
	t.Helper()
	polls := 0
	var scopesSeen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/cli/device":
			var req struct {
				RequestedScopes []string `json:"requested_scopes"`
				ClientName      string   `json:"client_name"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			scopesSeen = req.RequestedScopes
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "dev-123",
				"user_code":                 "WDJB-MJHT",
				"verification_uri":          "https://console.example.com/cli-authorize",
				"verification_uri_complete": "https://console.example.com/cli-authorize?code=WDJB-MJHT",
				"expires_in":                900,
				"interval":                  5,
			})
		case "/api/v1/auth/cli/device/token":
			var req struct {
				DeviceCode string `json:"device_code"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.DeviceCode != "dev-123" {
				t.Errorf("device_code = %q, want dev-123", req.DeviceCode)
			}
			polls++
			w.Header().Set("Content-Type", "application/json")
			switch {
			case polls <= pendingPolls:
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending", "message": "not yet"})
			case polls == pendingPolls+1:
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "slow_down", "message": "easy"})
			default:
				json.NewEncoder(w).Encode(map[string]string{"token": "knp_abc123"})
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &scopesSeen
}

func TestDeviceFlowObtainsTokenHonoringIntervals(t *testing.T) {
	srv, scopes := deviceServer(t, 2)

	c, _ := New(srv.URL)
	var slept []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	auth, err := c.StartDeviceAuth(context.Background(), "kubenest-cli test")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if auth.UserCode != "WDJB-MJHT" || auth.Interval != 5 {
		t.Errorf("auth = %+v", auth)
	}
	for _, want := range []string{"clusters:read", "clusters:register", "bundles:read", "install:report"} {
		if !strings.Contains(strings.Join(*scopes, " "), want) {
			t.Errorf("requested scopes %v missing %s", *scopes, want)
		}
	}

	token, err := c.WaitForDeviceToken(context.Background(), auth)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if token != "knp_abc123" {
		t.Errorf("token = %q", token)
	}

	// Two pending polls at the server's 5s interval, then slow_down raises
	// it to 10s (RFC 8628).
	if len(slept) < 3 {
		t.Fatalf("slept %v times, want at least 3", len(slept))
	}
	if slept[0] != 5*time.Second {
		t.Errorf("first interval = %s, want the server's 5s", slept[0])
	}
	if last := slept[len(slept)-1]; last != 10*time.Second {
		t.Errorf("interval after slow_down = %s, want 10s", last)
	}
}

func TestDeviceFlowDenialAndExpiryAreTerminal(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{
		{"access_denied", ErrAccessDenied},
		{"expired_token", ErrExpiredToken},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": tc.code})
		}))
		c, _ := New(srv.URL)
		c.sleep = func(ctx context.Context, d time.Duration) error { return nil }
		_, err := c.WaitForDeviceToken(context.Background(),
			DeviceAuth{DeviceCode: "d", Interval: 1, ExpiresIn: 900})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.code, err, tc.want)
		}
		srv.Close()
	}
}

// A control plane restored WITHOUT the kit's certificate authority is refused
// by a CLI that already holds that authority — and that check is LIVE, not
// assumed.
//
// T4.8's planted negative. The whole reason the control-plane kit carries
// CONTROL_PLANE_CA is that every CLI and agent in the fleet pinned it at install
// time: a recovery that minted a fresh authority would produce a control plane
// nobody can reach, and the first symptom would be an unexplained TLS failure
// halfway through an operation. This asserts the failure rather than describing
// it, over REAL TLS against two REAL authorities — a mocked transport would
// prove nothing about a certificate chain.
func TestAControlPlaneRestoredWithoutTheKitsCAIsRefused(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": []}`))
	})
	// The authority the recovery set's kit carries, and a control plane that
	// was restored under it.
	kitsAuthorityPEM, kitsServer := selfSignedServer(t, handler)

	// A DIFFERENT authority: what got installed when the kit's CA was not used.
	deadAuthorityPEM, deadServer := selfSignedServer(t, handler)

	// The CLI holds the authority from the DEAD control plane's kit, and dials
	// the restored one, which presents a different chain. The handshake fails.
	wrong, err := New(deadServer.URL, WithCACert(deadAuthorityPEM))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.ListBundles(context.Background()); err != nil {
		t.Fatalf("a CLI holding its own control plane's authority could not reach it: %v", err)
	}
	mismatched, err := New(kitsServer.URL, WithCACert(deadAuthorityPEM))
	if err != nil {
		t.Fatal(err)
	}
	_, err = mismatched.ListBundles(context.Background())
	if err == nil {
		t.Fatal("a control plane presenting a certificate from an authority the CLI does not hold was accepted: the kit's CA is then not what identifies the control plane")
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) && !strings.Contains(strings.ToLower(err.Error()), "certificate") {
		t.Fatalf("the refusal is not a certificate-verification failure, so it would read as an outage rather than as the wrong authority: %v", err)
	}

	// The positive control: the same CLI, holding the RESTORED control plane's
	// authority (the kit's), reaches it. Without this the test would pass for a
	// client that can reach nothing at all.
	trusted, err := New(kitsServer.URL, WithCACert(kitsAuthorityPEM))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trusted.ListBundles(context.Background()); err != nil {
		t.Fatalf("a CLI holding the recovered control plane's own authority could not reach it: %v", err)
	}
}

// selfSignedServer runs a real HTTPS server under its own freshly minted
// authority and returns the authority's PEM and the server. httptest's own
// helper reuses ONE certificate for every server it starts, which cannot express
// "two authorities", so the certificate is minted here.
func selfSignedServer(t *testing.T, handler http.Handler) ([]byte, *httptest.Server) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kubenest-test-authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return certPEM, srv
}
