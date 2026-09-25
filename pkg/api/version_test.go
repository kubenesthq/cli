package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The version route is the one call whose 404 means something: a control plane
// built before the contract counter does not serve it, so "no answer" and "the
// behaviour is missing" are different facts and only the first is knowable.
func TestA404AtTheVersionRouteIsCannotTellNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail": "Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	_, err := c.ControlPlaneVersion(context.Background())
	if err == nil {
		t.Fatal("expected an error for a control plane that does not serve the route")
	}
	if !IsVersionEndpointAbsent(err) {
		t.Errorf("err = %v, want it to be recognized as %v", err, ErrVersionEndpointAbsent)
	}
}

// A 500 is NOT the absent case: the route exists and answered wrongly, which is
// a failure to read rather than an answer that the capability is unknown.
func TestAServerErrorAtTheVersionRouteIsNotReadAsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail": "database is down"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	_, err := c.ControlPlaneVersion(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if IsVersionEndpointAbsent(err) {
		t.Errorf("a 500 was read as %v; the route answered, it did not decline", ErrVersionEndpointAbsent)
	}
}

func TestControlPlaneVersionReadsTheContractAndTheBuild(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			t.Errorf("path = %q, want /api/v1/version", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer knp_tok" {
			t.Errorf("Authorization = %q, want the stored token", auth)
		}
		w.Write([]byte(`{"contract": 3, "build": "c121ed8"}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL, WithToken("knp_tok"))
	v, err := c.ControlPlaneVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Contract != 3 || v.Build != "c121ed8" {
		t.Errorf("version = %+v, want contract 3 build c121ed8", v)
	}
}

// An unstamped image answers build: null, which is not a build identity: the
// field must stay empty rather than becoming the string "null".
func TestAnUnstampedBuildStaysEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"contract": 3, "build": null}`))
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	v, err := c.ControlPlaneVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Build != "" {
		t.Errorf("build = %q, want empty for an unstamped image", v.Build)
	}
	if !strings.Contains(ErrVersionEndpointAbsent.Error(), "/api/v1/version") {
		t.Errorf("the sentinel does not name the route it is about: %v", ErrVersionEndpointAbsent)
	}
}

// The absent error must still be unwrappable to the HTTP error, so a caller
// that wants the status is not forced to match on a string.
func TestTheAbsentErrorKeepsItsCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail": "Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	_, err := c.ControlPlaneVersion(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want it to carry the underlying 404", err)
	}
}
