package api

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// serverCertPEM is the CA a httptest TLS server's certificate chains to —
// what the CLI stores as Config.ControlPlaneCA.
func serverCertPEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

// The login is FastAPI's OAuth2PasswordRequestForm: form-encoded with
// `username`, not JSON with `email`. Getting that wrong is a 422 on a live
// control plane, which is exactly the first call the management cluster's
// install makes.
func TestPasswordLoginSendsFormCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/login" {
			t.Errorf("path = %q, want /api/v1/login", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want form-encoded", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		if r.PostForm.Get("username") != "admin@example.com" || r.PostForm.Get("password") != "hunter2" {
			t.Errorf("form = %v, want username/password", r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "session-abc", "token_type": "bearer"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.PasswordLogin(context.Background(), "admin@example.com", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "session-abc" {
		t.Errorf("access token = %q, want session-abc", tok)
	}
}

func TestCreateCLITokenSendsBearerScopesAndTTL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user/me/cli-tokens" {
			t.Errorf("path = %q, want /api/v1/user/me/cli-tokens", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer session-abc" {
			t.Errorf("Authorization = %q, want the session bearer", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var body struct {
			Name    string   `json:"name"`
			Scopes  []string `json:"scopes"`
			TTLDays int      `json:"ttl_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		if body.Name != "install-cluster-1" || body.TTLDays != 365 {
			t.Errorf("body = %+v, want name and ttl", body)
		}
		if len(body.Scopes) != 2 || body.Scopes[0] != "clusters:read" {
			t.Errorf("scopes = %v", body.Scopes)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token": "knp_minted"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithToken("session-abc"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.CreateCLIToken(context.Background(), "install-cluster-1",
		[]string{"clusters:read", "clusters:register"}, 365)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "knp_minted" {
		t.Errorf("token = %q, want knp_minted", tok)
	}
}

// A control plane the CLI installed presents a certificate from the platform
// CA, which the installer machine has never seen. WithCACert must add it
// without discarding the system roots.
func TestWithCACertTrustsTheControlPlanesOwnCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()

	untrusted, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.ListBundles(context.Background()); err == nil {
		t.Fatal("a certificate from an unknown CA was accepted without WithCACert")
	}

	c, err := New(srv.URL, WithCACert(serverCertPEM(t, srv)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListBundles(context.Background()); err != nil {
		t.Fatalf("WithCACert did not make the control-plane CA trusted: %v", err)
	}
}

func TestWithCACertRejectsInvalidPEM(t *testing.T) {
	if _, err := New("https://api.example.com", WithCACert([]byte("not a certificate"))); err == nil {
		t.Fatal("invalid CA PEM accepted")
	}
}

// WithDialContext is how the management cluster's CLI reaches the control
// plane through an SSH tunnel, alongside a CA it does not otherwise trust.
// Both options land on the one transport.
func TestWithDialContextAndCACertCompose(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()

	var dials atomic.Int64
	std := &net.Dialer{}
	c, err := New(srv.URL,
		WithCACert(serverCertPEM(t, srv)),
		WithDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return std.DialContext(ctx, network, addr)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListBundles(context.Background()); err != nil {
		t.Fatalf("request through the custom dialer and CA: %v", err)
	}
	if dials.Load() == 0 {
		t.Error("the custom dialer was never used")
	}
}

// Assembling the transport must not drop WithTimeout: the timeout is the
// only thing bounding a control plane that accepts a TCP connection and then
// never answers, which a crash-looping backend does.
func TestWithTimeoutSurvivesTheTransportOptions(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithCACert(serverCertPEM(t, srv)), WithTimeout(30*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListBundles(context.Background())
	if err == nil {
		t.Fatal("a request slower than the configured timeout succeeded")
	}
	// A deadline, not a rejected certificate: the handshake got through.
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("error = %v, want the request timeout to have fired", err)
	}
}
