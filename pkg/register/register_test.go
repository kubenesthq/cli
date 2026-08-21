package register_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/register"
)

// fakeAPI is a control plane with the one behaviour that matters here: cluster
// names are unique across ALL orgs, as the real backend enforces.
type fakeAPI struct {
	orgs     []api.Org
	clusters []api.Cluster

	createCalls int
	mintCalls   int
	nextVersion int

	listOrgsErr     error
	listClustersErr error
	// hideFromListing suppresses a name from ListOrgClusters while still
	// colliding on create — the "taken but invisible" case.
	hideFromListing string
}

func (f *fakeAPI) ListOrgs(context.Context) ([]api.Org, error) {
	return f.orgs, f.listOrgsErr
}

func (f *fakeAPI) ListOrgClusters(_ context.Context, orgID string) ([]api.Cluster, error) {
	if f.listClustersErr != nil {
		return nil, f.listClustersErr
	}
	var out []api.Cluster
	for _, c := range f.clusters {
		if c.OrgID == orgID && c.Name != f.hideFromListing {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeAPI) CreateCluster(_ context.Context, orgID, name, _ string) (*api.Cluster, error) {
	f.createCalls++
	for _, c := range f.clusters {
		if c.Name != name {
			continue
		}
		detail := "Cluster with name '" + name + "' already exists in this organization"
		if c.OrgID != orgID {
			detail = "Cluster name '" + name + "' is already taken in another organization. " +
				"Cluster names are unique across KubeNest."
		}
		return nil, &api.Error{
			Method: http.MethodPost, Path: "/api/v1/orgs/" + orgID + "/clusters",
			Status: http.StatusUnprocessableEntity, Detail: detail,
		}
	}
	created := api.Cluster{
		ID:     fmt.Sprintf("cluster-%d", len(f.clusters)+1),
		Name:   name,
		Status: "pending",
		OrgID:  orgID,
	}
	f.clusters = append(f.clusters, created)
	return &created, nil
}

func (f *fakeAPI) MintAgentCredentials(_ context.Context, clusterID string) (*api.AgentCredentials, error) {
	f.mintCalls++
	f.nextVersion++
	return &api.AgentCredentials{
		ClusterID: clusterID,
		AgentJWT: api.AgentJWT{
			Token:        api.NewSecret("jwt-" + clusterID),
			HubURL:       "wss://hub.example/ws/operator",
			TokenVersion: f.nextVersion,
		},
		RepoCredential: &api.RepoCredential{
			PrivateKey: api.NewSecret("-----BEGIN OPENSSH PRIVATE KEY-----"),
			RepoURL:    "ssh://git@gitea/kubenest/gitops-" + clusterID + ".git",
			Branch:     "main",
		},
		Operator: api.OperatorInstallInfo{Namespace: "kubenest-system", ChartRef: "oci://reg/kubenest-agent:2.2.0"},
	}, nil
}

func oneOrg() []api.Org {
	return []api.Org{{ID: "org-1", Name: "Crest", Slug: "crest"}}
}

func TestEnsureClusterCreatesThenAdopts(t *testing.T) {
	f := &fakeAPI{orgs: oneOrg()}
	ctx := context.Background()

	first, adopted, err := register.EnsureCluster(ctx, f, "org-1", "prod-1", "")
	if err != nil {
		t.Fatalf("first EnsureCluster: %v", err)
	}
	if adopted {
		t.Error("first call reported adopted; it created the cluster")
	}

	second, adopted, err := register.EnsureCluster(ctx, f, "org-1", "prod-1", "")
	if err != nil {
		t.Fatalf("second EnsureCluster: %v", err)
	}
	if !adopted {
		t.Error("second call did not report adopted")
	}
	if first.ID != second.ID {
		t.Errorf("second call returned a different cluster: %s vs %s", first.ID, second.ID)
	}
	if f.createCalls != 1 {
		t.Errorf("create was called %d times; a second register must not create a second cluster", f.createCalls)
	}
}

func TestEnsureClusterAdoptsAfterLosingARace(t *testing.T) {
	// The lookup finds nothing, then the create collides: another run got there
	// first. Adopting is the whole point of create-or-adopt.
	f := &fakeAPI{
		orgs:            oneOrg(),
		clusters:        []api.Cluster{{ID: "cluster-9", Name: "prod-1", OrgID: "org-1"}},
		hideFromListing: "prod-1",
	}

	// hideOnce hides the name from the first listing only, so the lookup misses
	// and the post-collision re-read finds it.
	hiding := &hideOnce{fakeAPI: f, name: "prod-1"}

	cluster, adopted, err := register.EnsureCluster(context.Background(), hiding, "org-1", "prod-1", "")
	if err != nil {
		t.Fatalf("EnsureCluster: %v", err)
	}
	if !adopted {
		t.Error("lost race should adopt, not report creation")
	}
	if cluster.ID != "cluster-9" {
		t.Errorf("adopted the wrong cluster: %s", cluster.ID)
	}
}

// hideOnce hides a cluster name from the first listing only.
type hideOnce struct {
	*fakeAPI
	name  string
	calls int
}

func (h *hideOnce) ListOrgClusters(ctx context.Context, orgID string) ([]api.Cluster, error) {
	h.calls++
	if h.calls == 1 {
		h.fakeAPI.hideFromListing = h.name
	} else {
		h.fakeAPI.hideFromListing = ""
	}
	return h.fakeAPI.ListOrgClusters(ctx, orgID)
}

func TestEnsureClusterRefusesANameOwnedByAnotherOrg(t *testing.T) {
	f := &fakeAPI{
		orgs:     oneOrg(),
		clusters: []api.Cluster{{ID: "cluster-x", Name: "prod-1", OrgID: "org-other"}},
	}
	_, _, err := register.EnsureCluster(context.Background(), f, "org-1", "prod-1", "")
	var taken *register.ErrNameTakenElsewhere
	if !errors.As(err, &taken) {
		t.Fatalf("expected ErrNameTakenElsewhere, got %v", err)
	}
	if !strings.Contains(err.Error(), "unique across KubeNest") {
		t.Errorf("error should explain why, got: %v", err)
	}
}

func TestEnsureClusterDoesNotDoubleCreateWhenTakenButInvisible(t *testing.T) {
	// Taken in our own org, yet absent from our listing. Creating a second
	// cluster here is the bug this guards; so is reporting success.
	f := &fakeAPI{
		orgs:            oneOrg(),
		clusters:        []api.Cluster{{ID: "cluster-7", Name: "prod-1", OrgID: "org-1"}},
		hideFromListing: "prod-1",
	}
	_, _, err := register.EnsureCluster(context.Background(), f, "org-1", "prod-1", "")
	if err == nil {
		t.Fatal("expected an error, got success")
	}
	if f.createCalls != 1 {
		t.Errorf("create called %d times; must not retry into a duplicate", f.createCalls)
	}
	if !strings.Contains(err.Error(), "not in this organization's cluster list") {
		t.Errorf("error should name the contradiction, got: %v", err)
	}
}

func TestResolveOrg(t *testing.T) {
	ctx := context.Background()

	t.Run("single org is implicit", func(t *testing.T) {
		org, err := register.ResolveOrg(ctx, &fakeAPI{orgs: oneOrg()}, "")
		if err != nil || org.ID != "org-1" {
			t.Fatalf("got %v, %v", org, err)
		}
	})

	t.Run("several orgs must be named", func(t *testing.T) {
		f := &fakeAPI{orgs: []api.Org{
			{ID: "org-1", Slug: "crest"}, {ID: "org-2", Slug: "acme"},
		}}
		_, err := register.ResolveOrg(ctx, f, "")
		var ambiguous *register.ErrAmbiguousOrg
		if !errors.As(err, &ambiguous) {
			t.Fatalf("expected ErrAmbiguousOrg, got %v", err)
		}
		if !strings.Contains(err.Error(), "--org") {
			t.Errorf("error should name the fix, got: %v", err)
		}
	})

	t.Run("slug selects", func(t *testing.T) {
		f := &fakeAPI{orgs: []api.Org{
			{ID: "org-1", Slug: "crest"}, {ID: "org-2", Slug: "acme"},
		}}
		org, err := register.ResolveOrg(ctx, f, "acme")
		if err != nil || org.ID != "org-2" {
			t.Fatalf("got %v, %v", org, err)
		}
	})

	t.Run("no orgs is its own error", func(t *testing.T) {
		_, err := register.ResolveOrg(ctx, &fakeAPI{}, "")
		if !errors.Is(err, register.ErrNoOrgs) {
			t.Fatalf("expected ErrNoOrgs, got %v", err)
		}
	})
}

func TestRegisterIsIdempotentInTheClusterAndRotatingInTheCredentials(t *testing.T) {
	// The property the wave gate asks for, stated as a test: registering twice
	// leaves ONE cluster, and the second mint supersedes the first rather than
	// repeating it.
	f := &fakeAPI{orgs: oneOrg()}
	ctx := context.Background()
	opts := register.Options{ClusterName: "prod-1"}

	first, err := register.Register(ctx, f, opts)
	if err != nil {
		t.Fatalf("first Register: %v", err)
	}
	second, err := register.Register(ctx, f, opts)
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}

	if first.Cluster.ID != second.Cluster.ID {
		t.Errorf("two clusters created: %s and %s", first.Cluster.ID, second.Cluster.ID)
	}
	if f.createCalls != 1 {
		t.Errorf("create called %d times, want 1", f.createCalls)
	}
	if first.Adopted {
		t.Error("first register should not report adopted")
	}
	if !second.Adopted {
		t.Error("second register should report adopted")
	}
	if f.mintCalls != 2 {
		t.Errorf("mint called %d times, want 2 — Register always mints", f.mintCalls)
	}
	if second.TokenVersion() <= first.TokenVersion() {
		t.Errorf("token_version did not advance: %d then %d",
			first.TokenVersion(), second.TokenVersion())
	}
}

func TestBundleCannotBeSerialized(t *testing.T) {
	// The in-memory-only rule, enforced rather than documented: if a caller
	// ever tries to write the bundle to a journal, it fails loudly.
	f := &fakeAPI{orgs: oneOrg()}
	bundle, err := register.Register(context.Background(), f, register.Options{ClusterName: "prod-1"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	if _, err := json.Marshal(bundle); err == nil {
		t.Fatal("the credential bundle marshalled to JSON; it must refuse")
	}

	// And it does not leak through formatting either.
	for _, rendered := range []string{
		fmt.Sprintf("%v", bundle.Credentials.AgentJWT.Token),
		fmt.Sprintf("%s", bundle.Credentials.AgentJWT.Token),
		fmt.Sprintf("%+v", bundle.Credentials.AgentJWT),
		fmt.Sprintf("%#v", bundle.Credentials.RepoCredential),
	} {
		if strings.Contains(rendered, "jwt-") || strings.Contains(rendered, "PRIVATE KEY") {
			t.Errorf("credential leaked into a formatted string: %q", rendered)
		}
	}

	// Reveal is the only way out, and it still works.
	if bundle.Credentials.AgentJWT.Token.Reveal() == "" {
		t.Error("Reveal returned nothing; the credential must still be usable")
	}
}

func TestMintCredentialsRequiresACluster(t *testing.T) {
	if _, err := register.MintCredentials(context.Background(), &fakeAPI{}, ""); err == nil {
		t.Fatal("expected an error for an empty cluster id")
	}
}
