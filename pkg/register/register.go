// Package register implements stage 2 of `kubenest platform install`.
//
// Stage 2 writes only to the control plane — the first change to a customer
// machine is stage 3 (install.mdx). What it produces is a credential bundle
// held in memory for the rest of the run and never written anywhere.
//
// The stage has two halves with opposite properties, which is why they are
// separate functions:
//
//   - EnsureCluster is idempotent. Run it a hundred times; it creates one
//     cluster and adopts it thereafter.
//   - MintCredentials is NOT idempotent, and cannot be. Each call rotates:
//     new agent JWT at a higher token_version, new repository deploy key, and
//     the superseded key deleted after the control plane's grace window. A
//     resume that re-mints after hosts already hold the previous credentials
//     invalidates them mid-install.
//
// Register runs both, which is correct for a first install and wrong for a
// blind resume. Callers holding a journal must decide; this package will not
// guess for them.
package register

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"kubenest.io/cli/pkg/api"
)

// ErrNoOrgs means the credential can see no organization to register under.
var ErrNoOrgs = errors.New("this credential is not a member of any organization")

// ErrAmbiguousOrg means the caller must choose; the CLI must not pick for them.
type ErrAmbiguousOrg struct {
	Orgs []api.Org
}

func (e *ErrAmbiguousOrg) Error() string {
	slugs := make([]string, 0, len(e.Orgs))
	for _, o := range e.Orgs {
		slugs = append(slugs, o.Slug)
	}
	return fmt.Sprintf("this credential can see %d organizations (%s): "+
		"name one with --org", len(e.Orgs), strings.Join(slugs, ", "))
}

// ErrNameTakenElsewhere means the cluster name belongs to an organization this
// credential cannot see, so neither create nor adopt can succeed.
type ErrNameTakenElsewhere struct {
	Name string
}

func (e *ErrNameTakenElsewhere) Error() string {
	return fmt.Sprintf("cluster name %q is already registered in another organization: "+
		"names are unique across KubeNest, so choose a different --cluster-name", e.Name)
}

// API is the control-plane surface stage 2 uses. Narrow on purpose: it is the
// exact list of calls the register stage is allowed to make, and it keeps the
// tests honest without mocking HTTP.
type API interface {
	ListOrgs(ctx context.Context) ([]api.Org, error)
	ListOrgClusters(ctx context.Context, orgID string) ([]api.Cluster, error)
	CreateCluster(ctx context.Context, orgID, name, description string) (*api.Cluster, error)
	MintAgentCredentials(ctx context.Context, clusterID string) (*api.AgentCredentials, error)
}

// ResolveOrg picks the organization to register under.
//
// An explicit slug or id wins. With none given, a credential that can see
// exactly one organization uses it — the common case, and what an org-bound
// token always produces. Anything else is ambiguous and asks rather than
// guesses: registering a customer's cluster under the wrong org is a support
// call, not a typo.
func ResolveOrg(ctx context.Context, client API, want string) (*api.Org, error) {
	orgs, err := client.ListOrgs(ctx)
	if err != nil {
		return nil, err
	}
	if want != "" {
		for i := range orgs {
			if orgs[i].ID == want || strings.EqualFold(orgs[i].Slug, want) {
				return &orgs[i], nil
			}
		}
		return nil, fmt.Errorf("no organization %q is visible to this credential: "+
			"check the name, or that your token is bound to the right organization", want)
	}
	switch len(orgs) {
	case 0:
		return nil, ErrNoOrgs
	case 1:
		return &orgs[0], nil
	default:
		return nil, &ErrAmbiguousOrg{Orgs: orgs}
	}
}

// EnsureCluster returns the org's cluster with this name, creating it only if
// it does not exist. Idempotent: the second run adopts what the first created.
//
// adopted reports which happened, because the caller's progress output and its
// journal both care ("registered" vs "adopted existing").
func EnsureCluster(
	ctx context.Context, client API, orgID, name, description string,
) (cluster *api.Cluster, adopted bool, err error) {
	if orgID == "" {
		return nil, false, errors.New("register: no organization resolved")
	}
	if name == "" {
		return nil, false, errors.New("register: cluster name is required")
	}

	existing, err := findByName(ctx, client, orgID, name)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, true, nil
	}

	created, err := client.CreateCluster(ctx, orgID, name, description)
	if err == nil {
		return created, false, nil
	}

	// A name collision here is one of two things. Either another run created
	// the cluster between our lookup and our create — adopt it, that is what
	// idempotent means — or the name belongs to an org we cannot see, which no
	// retry fixes.
	if !api.IsNameTaken(err) {
		return nil, false, err
	}
	if api.IsNameTakenInAnotherOrg(err) {
		return nil, false, &ErrNameTakenElsewhere{Name: name}
	}

	raced, lookupErr := findByName(ctx, client, orgID, name)
	if lookupErr != nil {
		return nil, false, fmt.Errorf("cluster %q already exists but could not be read back: %w", name, lookupErr)
	}
	if raced == nil {
		// Taken, in our org, yet absent from our own listing. Do not create a
		// second one and do not claim success.
		return nil, false, fmt.Errorf(
			"control plane refused cluster name %q as already taken, but it is not in this "+
				"organization's cluster list: %w", name, err)
	}
	return raced, true, nil
}

func findByName(ctx context.Context, client API, orgID, name string) (*api.Cluster, error) {
	clusters, err := client.ListOrgClusters(ctx, orgID)
	if err != nil {
		return nil, err
	}
	for i := range clusters {
		if clusters[i].Name == name {
			return &clusters[i], nil
		}
	}
	return nil, nil
}

// Bundle is what stage 2 hands to the rest of the install: the cluster record
// and the credentials that were minted for it.
//
// IT LIVES IN MEMORY ONLY. The secrets inside are api.Secret values, which
// refuse to marshal and print as [redacted], so the bundle cannot reach the
// journal, a log line or a progress event by accident. Go gives no way to wipe
// a string from memory, so the guarantee here is "never written down", not
// "erased after use" — do not claim the stronger one.
type Bundle struct {
	Cluster *api.Cluster
	// Adopted reports that the cluster already existed.
	Adopted bool
	// Credentials is the mint response. Never nil on success.
	Credentials *api.AgentCredentials
}

// TokenVersion is the version stamped on the minted agent JWT. Safe to journal
// and to log: it identifies which mint is live without being a credential.
func (b *Bundle) TokenVersion() int {
	if b == nil || b.Credentials == nil {
		return 0
	}
	return b.Credentials.AgentJWT.TokenVersion
}

// HasRepoCredential reports whether the control plane issued a GitOps
// repository credential. False is legitimate: a control plane without Gitea
// configured mints the agent JWT alone (contract v1.12.0).
func (b *Bundle) HasRepoCredential() bool {
	return b != nil && b.Credentials != nil && b.Credentials.RepoCredential != nil
}

// MintCredentials issues the cluster's install-time credentials.
//
// NOT IDEMPOTENT, deliberately: every call rotates. Call it once per install
// run, before anything is written to a host, and never again in that run.
func MintCredentials(ctx context.Context, client API, clusterID string) (*api.AgentCredentials, error) {
	if clusterID == "" {
		return nil, errors.New("register: cluster id is required to mint credentials")
	}
	creds, err := client.MintAgentCredentials(ctx, clusterID)
	if err != nil {
		return nil, fmt.Errorf("minting install credentials for cluster %s: %w", clusterID, err)
	}
	return creds, nil
}

// Options configure the register stage.
type Options struct {
	// Org is a slug or id. Empty means "the only one I can see".
	Org string
	// ClusterName is the name to create or adopt.
	ClusterName string
	// Description is recorded on creation and ignored when adopting.
	Description string
}

// Register runs stage 2 end to end: resolve the org, create-or-adopt the
// cluster, mint the credentials.
//
// Idempotent in the half that can be (the cluster record) and rotating in the
// half that cannot (the credentials). A resume must NOT call this blindly: if
// the journal says stage 2 completed and hosts already carry the credentials,
// re-running invalidates them. Adopt with EnsureCluster instead, or re-mint
// knowingly and redo every host step that consumed the old material.
func Register(ctx context.Context, client API, opts Options) (*Bundle, error) {
	org, err := ResolveOrg(ctx, client, opts.Org)
	if err != nil {
		return nil, err
	}

	cluster, adopted, err := EnsureCluster(ctx, client, org.ID, opts.ClusterName, opts.Description)
	if err != nil {
		return nil, err
	}

	creds, err := MintCredentials(ctx, client, cluster.ID)
	if err != nil {
		return nil, err
	}

	return &Bundle{Cluster: cluster, Adopted: adopted, Credentials: creds}, nil
}
