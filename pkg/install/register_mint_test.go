package install_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/install"
)

// mintAPI is a fake control plane for the register stage, and it keeps the
// bytes of the mint request it was sent.
//
// THE REQUEST BODY IS THE MECHANISM. The control plane cannot read the bundle
// out of the cluster's record at mint time — the mint is stage 2 and the record
// is written at the last stage — so the bundle has to travel on this request or
// a cluster being installed at 1.2 is handed the operator of the newest
// RELEASED bundle instead of the candidate 1.2 pins. An assertion on the
// function argument would pass while the request carried nothing, so what is
// kept here is what actually crossed the wire.
type mintAPI struct {
	mu sync.Mutex
	// minted is the request body, and saw records that a mint happened at all:
	// an install whose mint carried no body must fail as "it named no bundle",
	// never as "it never minted", or the failure would name the wrong defect.
	minted []byte
	saw    bool
}

func (m *mintAPI) serve(t *testing.T) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/orgs":
			_, _ = io.WriteString(w, `[{"id":"org-1","name":"Acme","slug":"acme"}]`)
		case req.Method == http.MethodGet && req.URL.Path == "/api/v1/orgs/org-1/clusters":
			_, _ = io.WriteString(w, `{"data":[],"total_count":0,"page":1,"items_per_page":100,"has_more":false}`)
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/orgs/org-1/clusters":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"cluster-1","name":"prod-1","status":"pending","org_id":"org-1"}`)
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/clusters/cluster-1/agent-credentials":
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("reading the mint request body: %v", err)
			}
			m.mu.Lock()
			m.minted = body
			m.saw = true
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"cluster_id":"cluster-1",`+
				`"agent_jwt":{"token":"t","hub_url":"wss://hub/ws/operator","token_version":1},`+
				`"operator":{"namespace":"kubenest-system",`+
				`"chart_ref":"oci://ghcr.io/kubenesthq/candidate/kubenest-operator-2:2.7.0-rc.1"}}`)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return client
}

func (m *mintAPI) mintRequest(t *testing.T) []byte {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.saw {
		t.Fatal("the register stage never minted: the install would carry no agent credentials")
	}
	return m.minted
}

// The register stage sends the bundle it is installing on the mint request
// (kn-t72…0xx7.1).
//
// The bundle cannot be left to the control plane's default. It resolves the
// operator chart reference from the bundle it is told about — sources
// .kubenest-agent plus the core pin — and its fallback is the newest RELEASED
// bundle, which for a 1.2 install is the stable repository at 2.6.17, not the
// candidate chart 1.2 declares. The argument this stage passes to the mint is
// therefore load-bearing on a real cluster, and this is the test that holds it.
func TestTheRegisterStageSendsTheBundleItIsInstalling(t *testing.T) {
	fake := &mintAPI{}
	s := sessionWithOpts(t, install.Options{
		Bundle: "1.2", Name: "prod-1", Servers: []string{"10.0.1.10"}, HATier: "single-server",
	}, &recorder{})
	s.API = fake.serve(t)

	if err := runStage(t, s, install.StageRegister); err != nil {
		t.Fatalf("the register stage failed: %v", err)
	}

	raw := fake.mintRequest(t)
	var body struct {
		BundleVersion string `json:"bundle_version"`
	}
	if len(raw) == 0 {
		t.Fatal("the install minted with no body, so it named no bundle: the control plane then " +
			"answers from the newest released bundle, which is not the operator 1.2 declares")
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("the mint body is not JSON (%q): %v", raw, err)
	}
	if body.BundleVersion != "1.2" {
		t.Errorf("the install minted at bundle %q, want 1.2: the control plane resolves the "+
			"operator chart reference from the bundle named here, and a request that names none "+
			"is answered from the newest released bundle — not the operator 1.2 declares",
			body.BundleVersion)
	}
}

// runStage runs one stage of the install plan. It is the only way an external
// test package can drive a stage the package keeps unexported.
func runStage(t *testing.T, s *install.Session, name string) error {
	t.Helper()
	for _, stage := range install.Plan(s) {
		if stage.Name == name {
			return stage.Run(context.Background())
		}
	}
	t.Fatalf("the install plan has no %s stage", name)
	return nil
}
