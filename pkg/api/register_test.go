package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/api"
)

func newClient(t *testing.T, handler http.Handler) (*api.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := api.New(srv.URL, api.WithToken("knp_test"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return c, srv
}

func TestListOrgClustersWalksEveryPage(t *testing.T) {
	// A cluster stranded on page 2 is a cluster the register stage creates
	// twice, so the walk is load-bearing, not a nicety.
	const perPage = 100
	var seenPages []string

	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		seenPages = append(seenPages, page)

		var data []map[string]string
		switch page {
		case "1":
			for i := 0; i < perPage; i++ {
				data = append(data, map[string]string{"id": fmt.Sprintf("c%d", i), "name": fmt.Sprintf("n%d", i)})
			}
		case "2":
			data = append(data, map[string]string{"id": "late", "name": "stranded"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": page == "1"})
	}))

	clusters, err := c.ListOrgClusters(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListOrgClusters: %v", err)
	}
	if len(clusters) != perPage+1 {
		t.Fatalf("got %d clusters, want %d", len(clusters), perPage+1)
	}
	if clusters[len(clusters)-1].Name != "stranded" {
		t.Errorf("page 2 was not walked; last cluster is %q", clusters[len(clusters)-1].Name)
	}
	if len(seenPages) != 2 {
		t.Errorf("requested pages %v, want exactly 1 and 2", seenPages)
	}
}

func TestListOrgClustersStopsOnAShortPage(t *testing.T) {
	// A control plane that always says has_more must not spin us forever.
	calls := 0
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":     []map[string]string{{"id": "c1", "name": "only"}},
			"has_more": true,
		})
	}))

	clusters, err := c.ListOrgClusters(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("ListOrgClusters: %v", err)
	}
	if len(clusters) != 1 || calls != 1 {
		t.Errorf("got %d clusters in %d calls; a short page ends the walk", len(clusters), calls)
	}
}

func TestCreateClusterSurfacesTheDuplicateNameCases(t *testing.T) {
	cases := []struct {
		name          string
		detail        string
		wantTaken     bool
		wantElsewhere bool
	}{
		{
			name:      "same org",
			detail:    "Cluster with name 'prod-1' already exists in this organization",
			wantTaken: true,
		},
		{
			name: "another org",
			detail: "Cluster name 'prod-1' is already taken in another organization. " +
				"Cluster names are unique across KubeNest.",
			wantTaken:     true,
			wantElsewhere: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]string{"detail": tc.detail})
			}))

			_, err := c.CreateCluster(context.Background(), "org-1", "prod-1", "")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := api.IsNameTaken(err); got != tc.wantTaken {
				t.Errorf("IsNameTaken = %v, want %v (err: %v)", got, tc.wantTaken, err)
			}
			if got := api.IsNameTakenInAnotherOrg(err); got != tc.wantElsewhere {
				t.Errorf("IsNameTakenInAnotherOrg = %v, want %v", got, tc.wantElsewhere)
			}
		})
	}
}

func TestIsNameTakenIgnoresUnrelatedFailures(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail": "database is down"}`))
	}))
	_, err := c.CreateCluster(context.Background(), "org-1", "prod-1", "")
	if api.IsNameTaken(err) {
		t.Errorf("a 500 was read as a name collision: %v", err)
	}
}

func TestErrorCarriesTheContractCode(t *testing.T) {
	// 403 insufficient_scope must be distinguishable from every other 403, so
	// the CLI can say "your token lacks clusters:register" rather than "denied".
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail": {"error": "insufficient_scope",
			"message": "This action requires the clusters:register scope.",
			"details": {"required_scope": "clusters:register"}}}`))
	}))

	_, err := c.MintAgentCredentials(context.Background(), "cluster-1")
	var apiErr *api.Error
	if !asAPIError(err, &apiErr) {
		t.Fatalf("expected *api.Error, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", apiErr.Status)
	}
	if apiErr.Code != "insufficient_scope" {
		t.Errorf("code = %q, want insufficient_scope", apiErr.Code)
	}
}

func TestUnauthorizedKeepsItsLoginMessageAndGainsACode(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail": {"error": "token_expired", "message": "expired"}}`))
	}))

	_, err := c.ListOrgs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kubenest login") {
		t.Fatalf("401 must still tell the user to log in, got: %v", err)
	}
	var apiErr *api.Error
	if !asAPIError(err, &apiErr) || apiErr.Code != "token_expired" {
		t.Errorf("expected token_expired code, got %+v", apiErr)
	}
}

func TestMintDecodesCredentialsButKeepsThemUnprintable(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("mint used %s; it must be a POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"cluster_id": "cluster-1",
			"agent_jwt": {"token": "super-secret-jwt", "expires_at": "2027-08-21T00:00:00Z",
			              "hub_url": "wss://hub/ws/operator", "token_version": 3},
			"repo_credential": {"private_key": "-----BEGIN OPENSSH PRIVATE KEY-----",
			                    "repo_url": "ssh://git@gitea/kubenest/gitops-cluster-1.git",
			                    "branch": "main"},
			"operator": {"namespace": "kubenest-system", "chart_ref": "oci://reg/kubenest-agent:2.2.0"}}`))
	}))

	creds, err := c.MintAgentCredentials(context.Background(), "cluster-1")
	if err != nil {
		t.Fatalf("MintAgentCredentials: %v", err)
	}
	if creds.AgentJWT.TokenVersion != 3 {
		t.Errorf("token_version = %d, want 3", creds.AgentJWT.TokenVersion)
	}
	if creds.AgentJWT.Token.Reveal() != "super-secret-jwt" {
		t.Error("the token did not decode")
	}
	if creds.Operator.ChartRef == "" || creds.RepoCredential.Branch != "main" {
		t.Errorf("non-secret fields did not decode: %+v", creds.Operator)
	}

	// The whole response must refuse to be written down.
	if _, err := json.Marshal(creds); err == nil {
		t.Error("AgentCredentials marshalled; it must refuse")
	}
	if rendered := fmt.Sprintf("%+v", creds); strings.Contains(rendered, "super-secret-jwt") ||
		strings.Contains(rendered, "PRIVATE KEY") {
		t.Errorf("credentials leaked into %%+v: %s", rendered)
	}
}

func TestMintRejectsAResponseWithNoToken(t *testing.T) {
	// A 201 with an empty token is worse than an error: the install would
	// proceed and put an unusable credential on the host.
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"cluster_id": "cluster-1", "agent_jwt": {"token": ""}}`))
	}))
	if _, err := c.MintAgentCredentials(context.Background(), "cluster-1"); err == nil {
		t.Fatal("expected an error for an empty agent token")
	}
}

func TestMintWithoutGitOpsHasNoRepoCredential(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"cluster_id": "c1",
			"agent_jwt": {"token": "t", "token_version": 1},
			"repo_credential": null,
			"operator": {"namespace": "kubenest-system", "chart_ref": "oci://x:1"}}`))
	}))
	creds, err := c.MintAgentCredentials(context.Background(), "c1")
	if err != nil {
		t.Fatalf("a control plane without Gitea must still mint: %v", err)
	}
	if creds.RepoCredential != nil {
		t.Error("expected no repo credential")
	}
}

func TestRegisterCallsSendTheBearerToken(t *testing.T) {
	var got string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	if _, err := c.ListOrgs(context.Background()); err != nil {
		t.Fatalf("ListOrgs: %v", err)
	}
	if got != "Bearer knp_test" {
		t.Errorf("Authorization = %q", got)
	}
}

// asAPIError is errors.As specialised, kept here so the test file reads.
func asAPIError(err error, target **api.Error) bool {
	for err != nil {
		if e, ok := err.(*api.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
