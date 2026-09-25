package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/version"
)

// A RESUME MUST NOT NEED THE BACKEND THE UPGRADE IT RESUMES HAS FENCED OFF
// (kn-t70-control-plane-version-identity-4xso.1).
//
// The fence is the CLI's own state: it raised the fence, stopped the backend for
// the migration, and left both in place when the migration failed. The command's
// first step is the version check, which read the PUBLIC route — fenced — and
// the run died with `GET /api/v1/version: HTTP 503`. The advertised way on
// ("fix what the error names, then run the identical command again") did not
// work, and the only way out was by hand.
//
// The three answers a fenced command must give:
//
//	the fence's own 503   reach the backend through the node's tunnel, which is
//	                      where the backend is while the fence is up;
//	no backend either     use the version and window the operation RECORDED when
//	                      it began — the record is on the cluster and needs no
//	                      backend to read;
//	any other 503         a failure. A load balancer's 503 is not permission.

// fenceStub is the fence: every answer is the fence's 503, carrying its header.
func fenceStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.FenceHeader, api.FenceHeaderUp)
		http.Error(w, "The KubeNest control plane is being upgraded and is briefly unavailable.", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// backendStub stands in for the backend reachable only through the node.
func backendStub(t *testing.T, contract int, build string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.Error(w, `{"detail": "no route"}`, http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"contract": %d, "build": %q}`, contract, build)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func clientFor(t *testing.T, url string) *api.Client {
	t.Helper()
	c, err := api.New(url, api.WithToken("knp_tok"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// THE FENCE'S 503 IS NOT A FAILURE TO READ. The resume asks the node.
func TestAResumeBehindTheFenceReadsTheVersionThroughTheTunnel(t *testing.T) {
	ctx := context.Background()
	fenced := fenceStub(t)
	floor, _ := version.RequiredEra()
	backend := backendStub(t, floor.Era+1, "9e9698dcafe4")

	source := fencedVersionSource{
		Public: clientFor(t, fenced.URL),
		Node: func(context.Context) (*api.Client, error) {
			return clientFor(t, backend.URL), nil
		},
	}
	got, known, err := source.version(ctx)
	if err != nil || !known {
		t.Fatalf("the fenced version could not be established (known=%v): %v", known, err)
	}
	if err := requireControlPlaneForUpgrade(got, known); err != nil {
		t.Fatalf("a fenced resume was refused although the backend answers through the node: %v", err)
	}
	if got.Build != "9e9698dcafe4" {
		t.Errorf("the version came from %q, want the backend behind the node: a fenced resume must not read the public route", got.Build)
	}
}

// AND WHEN THE BACKEND IS NOT THERE EITHER, the operation's own record answers.
//
// This is the failed-migration case: the chart applied `backend.replicas: 0`
// with the new image, so neither the fenced route nor the tunnel can answer.
func TestAResumeWithTheBackendStoppedUsesTheRecordedVersion(t *testing.T) {
	ctx := context.Background()
	fenced := fenceStub(t)
	floor, _ := version.RequiredEra()
	recorded := api.ControlPlaneVersion{Contract: floor.Era + 2, Build: "c121ed887750b1d3"}

	source := fencedVersionSource{
		Public: clientFor(t, fenced.URL),
		Node: func(context.Context) (*api.Client, error) {
			return nil, fmt.Errorf("the backend Service has no ClusterIP yet")
		},
		Recorded: recorded,
	}
	got, known, err := source.version(ctx)
	if err != nil || !known {
		t.Fatalf("a fenced resume with no backend and a recorded version was refused (known=%v): %v", known, err)
	}
	if got != recorded {
		t.Errorf("the version is %+v, want the recorded %+v", got, recorded)
	}
	if err := requireControlPlaneForUpgrade(got, known); err != nil {
		t.Errorf("the recorded version was not used for the floor check: %v", err)
	}

	// With NOTHING recorded it still refuses, and says what it needed: an
	// operation that recorded nothing is one this build cannot continue, and
	// guessing an era is how a resume walks into a control plane it does not
	// understand.
	empty := fencedVersionSource{Public: clientFor(t, fenced.URL), Node: source.Node}
	if _, known, err := empty.version(ctx); err == nil || known {
		t.Fatal("a fenced resume with no backend and no recorded version was accepted")
	} else if !strings.Contains(err.Error(), "record") {
		t.Errorf("the refusal does not say that nothing was recorded:\n%v", err)
	}
}

// A PLAIN 503 IS STILL A FAILURE. The fence being up is a fact the CLI's own
// record knows; a load balancer, an ingress timeout and a dead backend all
// answer 503 without ever being the fence.
func TestAPlain503IsNotReadAsTheFence(t *testing.T) {
	ctx := context.Background()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
	}))
	defer plain.Close()
	backend := backendStub(t, 9, "should-not-be-read")

	asked := false
	source := fencedVersionSource{
		Public: clientFor(t, plain.URL),
		Node: func(context.Context) (*api.Client, error) {
			asked = true
			return clientFor(t, backend.URL), nil
		},
		Recorded: api.ControlPlaneVersion{Contract: 9, Build: "should-not-be-read-either"},
	}
	_, known, err := source.version(ctx)
	if err == nil || known {
		t.Fatal("a 503 without the fence's header was accepted as the fence")
	}
	if api.IsControlPlaneFenced(err) {
		t.Errorf("a plain 503 was read as the fence: %v", err)
	}
	if asked {
		t.Error("the node was consulted for a 503 that did not identify itself as the fence")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("the failure does not carry the status the caller needs:\n%v", err)
	}
}
