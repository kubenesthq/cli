package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoginPostsPasswordForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/login" {
			t.Errorf("path = %q, want /api/v1/login", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content-type = %q, want form encoding", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("username") != "op@example.com" || r.PostForm.Get("password") != "pw" {
			t.Errorf("form = %v, want username/password fields", r.PostForm)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "tok-abc", "token_type": "bearer"}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Login(context.Background(), "op@example.com", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tok.AccessToken != "tok-abc" {
		t.Errorf("token = %q, want tok-abc", tok.AccessToken)
	}
}

func TestUnauthorizedSaysHowToRecover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail": "expired"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("stale"))
	_, err := c.CurrentUser(context.Background())
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "kubenest login") {
		t.Errorf("401 error %q does not tell the user to log in again", err)
	}
}

func TestErrorsSurfaceBackendDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail": "Wrong email or password."}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	_, err := c.Login(context.Background(), "x@example.com", "bad")
	if err == nil || !strings.Contains(err.Error(), "Wrong email or password.") {
		t.Errorf("error %v does not carry the backend detail", err)
	}
}

// The debug trace is the one place a request is ever written out, so it must
// never carry the credential.
func TestDebugTraceRedactsCredentials(t *testing.T) {
	t.Setenv("KUBENEST_DEBUG", "1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"email": "op@example.com"}`))
	}))
	defer srv.Close()

	var trace bytes.Buffer
	const secret = "super-secret-bearer-token"
	c, _ := New(srv.URL, WithToken(secret), WithDebugWriter(&trace))
	if _, err := c.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}

	out := trace.String()
	if out == "" {
		t.Fatal("expected a debug trace with KUBENEST_DEBUG=1")
	}
	if strings.Contains(out, secret) {
		t.Errorf("debug trace leaks the bearer token:\n%s", out)
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
