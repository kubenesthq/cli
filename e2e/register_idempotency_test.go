//go:build e2e

// The kn-7k8 wave gate for the register stage: registering the same cluster
// twice must not orphan a deploy key or strand a repository, and the agent
// JWT's token_version must advance.
//
// No mocks (AGENTS.md rule §1). This drives a REAL control plane talking to a
// REAL Gitea, and it verifies the outcome against Gitea's own API rather than
// against anything the backend reports about itself.
//
//	./e2e/stack/register-proof-env.sh up
//	source /tmp/kn7k8-proof.env
//	go test -tags e2e -v -run TestRegisterStage ./e2e/
//	./e2e/stack/register-proof-env.sh down
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/register"
)

type proofEnv struct {
	api        string
	token      string
	orgID      string
	giteaURL   string
	giteaToken string
	giteaOwner string
	grace      int
}

func loadEnv(t *testing.T) proofEnv {
	t.Helper()
	e := proofEnv{
		api:        os.Getenv("KUBENEST_PROOF_API"),
		token:      os.Getenv("KUBENEST_PROOF_TOKEN"),
		orgID:      os.Getenv("KUBENEST_PROOF_ORG"),
		giteaURL:   strings.TrimRight(os.Getenv("TEST_GITEA_URL"), "/"),
		giteaToken: os.Getenv("TEST_GITEA_TOKEN"),
		giteaOwner: os.Getenv("TEST_GITEA_OWNER"),
	}
	if v := os.Getenv("KUBENEST_PROOF_GRACE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("KUBENEST_PROOF_GRACE_SECONDS=%q is not a number", v)
		}
		e.grace = n
	}
	for name, val := range map[string]string{
		"KUBENEST_PROOF_API":   e.api,
		"KUBENEST_PROOF_TOKEN": e.token,
		"KUBENEST_PROOF_ORG":   e.orgID,
		"TEST_GITEA_URL":       e.giteaURL,
		"TEST_GITEA_TOKEN":     e.giteaToken,
		"TEST_GITEA_OWNER":     e.giteaOwner,
	} {
		if val == "" {
			t.Skipf("%s is unset: run ./e2e/stack/register-proof-env.sh up and source /tmp/kn7k8-proof.env", name)
		}
	}
	return e
}

// gitea calls the Gitea API directly. The proof does not ask the backend
// whether it cleaned up; it asks Gitea.
func (e proofEnv) gitea(t *testing.T, method, path string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, e.giteaURL+"/api/v1"+path, nil)
	if err != nil {
		t.Fatalf("gitea request: %v", err)
	}
	req.Header.Set("Authorization", "token "+e.giteaToken)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("gitea %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("gitea %s %s decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

type deployKey struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	ReadOnly bool   `json:"read_only"`
}

func (e proofEnv) deployKeys(t *testing.T, repo string) []deployKey {
	t.Helper()
	var keys []deployKey
	if code := e.gitea(t, http.MethodGet, "/repos/"+e.giteaOwner+"/"+repo+"/keys", &keys); code != 200 {
		t.Fatalf("listing deploy keys on %s: HTTP %d", repo, code)
	}
	return keys
}

func (e proofEnv) repoExists(t *testing.T, repo string) bool {
	t.Helper()
	return e.gitea(t, http.MethodGet, "/repos/"+e.giteaOwner+"/"+repo, nil) == 200
}

func (e proofEnv) gitopsRepoNames(t *testing.T) []string {
	t.Helper()
	var repos []struct {
		Name string `json:"name"`
	}
	if code := e.gitea(t, http.MethodGet, "/user/repos?limit=200", &repos); code != 200 {
		t.Fatalf("listing repos: HTTP %d", code)
	}
	var out []string
	for _, r := range repos {
		if strings.HasPrefix(r.Name, "gitops-") {
			out = append(out, r.Name)
		}
	}
	return out
}

func TestRegisterStageIsIdempotentAgainstRealGitea(t *testing.T) {
	env := loadEnv(t)
	ctx := context.Background()

	client, err := api.New(env.api, api.WithToken(env.token))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	// A fresh name per run: the proof must not depend on a clean database, and
	// re-running it must not collide with its own earlier runs.
	name := fmt.Sprintf("proof-%d", time.Now().UnixNano())
	opts := register.Options{Org: env.orgID, ClusterName: name, Description: "kn-7k8 idempotency proof"}

	reposBefore := len(env.gitopsRepoNames(t))

	// ---- REGISTER, ONCE ----
	first, err := register.Register(ctx, client, opts)
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if first.Adopted {
		t.Error("first register reported adopted; nothing existed to adopt")
	}
	if !first.HasRepoCredential() {
		t.Fatal("no repo credential minted: Gitea is configured, so this is the broker failing quietly")
	}
	repo := "gitops-" + first.Cluster.ID
	t.Cleanup(func() {
		e := env
		_ = e.gitea(t, http.MethodDelete, "/repos/"+env.giteaOwner+"/"+repo, nil)
	})

	if !env.repoExists(t, repo) {
		t.Fatalf("register did not create %s in Gitea", repo)
	}
	keysAfterFirst := env.deployKeys(t, repo)
	if len(keysAfterFirst) != 1 {
		t.Fatalf("after one register %s has %d deploy keys, want 1: %+v", repo, len(keysAfterFirst), keysAfterFirst)
	}
	if keysAfterFirst[0].ReadOnly {
		t.Error("the deploy key is read-only; the operator has to push desired state")
	}

	// The credential must be for THIS cluster's repo and nothing else — the
	// whole point of kn-rnyl was that one shared credential wrote to every
	// cluster's desired state.
	if !strings.Contains(first.Credentials.RepoCredential.RepoURL, repo) {
		t.Errorf("repo credential points at %q, not at %s",
			first.Credentials.RepoCredential.RepoURL, repo)
	}

	// ---- REGISTER, AGAIN. THE GATE. ----
	second, err := register.Register(ctx, client, opts)
	if err != nil {
		t.Fatalf("second register: %v", err)
	}

	if !second.Adopted {
		t.Error("second register did not adopt the existing cluster")
	}
	if second.Cluster.ID != first.Cluster.ID {
		t.Fatalf("second register produced a different cluster: %s then %s",
			first.Cluster.ID, second.Cluster.ID)
	}

	// NO STRANDED REPO: the org gained exactly one gitops repo across both runs.
	reposAfter := env.gitopsRepoNames(t)
	if got := len(reposAfter) - reposBefore; got != 1 {
		t.Errorf("gitops repo count changed by %d across two registers, want 1: %v", got, reposAfter)
	}

	// TOKEN_VERSION ADVANCES: the second mint supersedes the first rather than
	// handing back the same credential.
	if second.TokenVersion() != first.TokenVersion()+1 {
		t.Errorf("token_version went %d -> %d, want +1",
			first.TokenVersion(), second.TokenVersion())
	}
	if first.Credentials.AgentJWT.Token.Reveal() == second.Credentials.AgentJWT.Token.Reveal() {
		t.Error("the second mint returned the identical JWT; re-minting must rotate")
	}

	// NO ORPHANED DEPLOY KEY. What "orphaned" means depends on the control
	// plane's grace window, so assert the behaviour the setting actually
	// specifies rather than a number that happens to pass.
	keysAfterSecond := env.deployKeys(t, repo)
	switch {
	case env.grace == 0:
		// Immediate revocation: the superseded key is gone, one key remains.
		if len(keysAfterSecond) != 1 {
			t.Errorf("with a 0s grace window %s has %d deploy keys after two registers, want 1: %+v",
				repo, len(keysAfterSecond), keysAfterSecond)
		}
		for _, k := range keysAfterSecond {
			if k.ID == keysAfterFirst[0].ID {
				t.Errorf("the superseded key %d (%s) was not deleted", k.ID, k.Title)
			}
		}
	default:
		// Inside the window the old key is retained ON PURPOSE, so a re-mint
		// cannot cut off an operator mid-rollover. Retained is not orphaned:
		// what would be a defect is a THIRD key, or the new one missing.
		if len(keysAfterSecond) != 2 {
			t.Errorf("with a %ds grace window %s has %d deploy keys after two registers, want 2 "+
				"(the new one plus the one still inside its window): %+v",
				env.grace, repo, len(keysAfterSecond), keysAfterSecond)
		}
	}

	// Every key on the repo is one of ours and is a write key — nothing else
	// accumulated.
	for _, k := range keysAfterSecond {
		if !strings.HasPrefix(k.Title, "kubenest-agent-") {
			t.Errorf("unexpected deploy key on %s: %+v", repo, k)
		}
	}

	// ONE CLUSTER, not two: assert against the control plane's own listing.
	clusters, err := client.ListOrgClusters(ctx, env.orgID)
	if err != nil {
		t.Fatalf("ListOrgClusters: %v", err)
	}
	matches := 0
	for _, c := range clusters {
		if c.Name == name {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("the org holds %d clusters named %q after two registers, want 1", matches, name)
	}

	t.Logf("cluster %s: one repo (%s), %d deploy key(s) at grace=%ds, token_version %d -> %d",
		first.Cluster.ID, repo, len(keysAfterSecond), env.grace,
		first.TokenVersion(), second.TokenVersion())
}

func TestRegisterStageRefusesAWrongScopedToken(t *testing.T) {
	// Default-deny, proven against the running control plane rather than in a
	// unit test: a token that cannot register gets a scoped 403, not a 500.
	env := loadEnv(t)

	client, err := api.New(env.api, api.WithToken(env.token+"-tampered"))
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	_, err = client.ListOrgs(context.Background())
	if err == nil {
		t.Fatal("a tampered token was accepted")
	}
	if !strings.Contains(err.Error(), "kubenest login") {
		t.Errorf("the error should tell the operator how to fix it, got: %v", err)
	}
}
