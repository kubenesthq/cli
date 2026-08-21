// Register-stage calls: stage 2 of `kubenest platform install`.
//
// Everything here runs on the knp_ CLI token from `kubenest login`. The scopes
// the control plane requires (DESIGN-INSTALL-CREDENTIALS.md §3) are named on
// each method, because a 403 here is a login problem, not a bug.
//
// One rule governs this file: the mint response carries live credentials, so
// those fields use types that cannot be printed or serialized. They exist to
// be handed to the host installer and to nothing else.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Org is one organization the caller can see (GET /api/v1/orgs).
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Cluster is the control plane's cluster record. It is secret-free by
// contract: kn-rnyl removed install_command and connection_token from every
// browser-reachable response, and nothing replaced them here.
type Cluster struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	OrgID  string `json:"org_id"`
}

// ListOrgs returns the organizations this credential can see. A token bound to
// one org returns exactly that org, which is how `install` resolves the target
// without asking. Scope: clusters:read.
func (c *Client) ListOrgs(ctx context.Context) ([]Org, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/api/v1/orgs"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	var out []Org
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListOrgClusters returns the org's clusters. Scope: clusters:read.
//
// Paginated server-side; this walks every page, because the caller is looking
// for one name and a cluster missing from page 2 would be created twice.
func (c *Client) ListOrgClusters(ctx context.Context, orgID string) ([]Cluster, error) {
	const perPage = 100

	var all []Cluster
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("items_per_page", strconv.Itoa(perPage))
		endpoint := c.endpoint("/api/v1/orgs/"+url.PathEscape(orgID)+"/clusters") + "?" + q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")

		var out struct {
			Data        []Cluster `json:"data"`
			HasMore     bool      `json:"has_more"`
			TotalCount  int       `json:"total_count"`
			CurrentPage int       `json:"page"`
		}
		if err := c.do(req, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data...)

		// Stop on the server's own signal; fall back to a short page, and
		// hard-stop when a page adds nothing so a lying has_more cannot spin.
		if !out.HasMore || len(out.Data) == 0 || len(out.Data) < perPage {
			return all, nil
		}
	}
}

// CreateCluster registers a new cluster under the org. Scope: clusters:register.
//
// Cluster names are unique across the whole control plane, not per org, so a
// name taken elsewhere fails here even though the caller's own org lookup found
// nothing. IsNameTaken reports that case.
func (c *Client) CreateCluster(ctx context.Context, orgID, name, description string) (*Cluster, error) {
	body, err := json.Marshal(map[string]any{"name": name, "description": description})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/orgs/"+url.PathEscape(orgID)+"/clusters"), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	var out Cluster
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCluster reads one cluster record. Scope: clusters:read.
func (c *Client) GetCluster(ctx context.Context, clusterID string) (*Cluster, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	var out Cluster
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IsNameTaken reports whether err is the control plane refusing a cluster name
// that already exists. The backend raises its house DuplicateValueException,
// which maps to 422 rather than 409, and the two cases — taken in this org,
// taken in another — differ only in the detail text. Both are pinned by
// kubenest-backend tests/api/test_register_stage_auth.py.
func IsNameTaken(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Status != http.StatusUnprocessableEntity && apiErr.Status != http.StatusConflict {
		return false
	}
	d := strings.ToLower(apiErr.Detail)
	return strings.Contains(d, "already exists") || strings.Contains(d, "already taken")
}

// IsNameTakenInAnotherOrg narrows IsNameTaken to the case the caller cannot
// fix by adopting: the name belongs to an org this credential cannot see.
func IsNameTakenInAnotherOrg(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return IsNameTaken(err) && strings.Contains(strings.ToLower(apiErr.Detail), "another organization")
}

// Secret is a credential the control plane returned exactly once. Every
// rendering and serialization path is overridden, so it cannot reach a log
// line, a progress event, or the install journal by accident. Reveal is the
// only way out and its call sites are the ones worth reviewing.
//
// Same construction as pkg/sshx's keyMaterial, for the same reason.
type Secret struct {
	raw string
}

// NewSecret wraps a value that must not be printed. Exported for tests and for
// callers assembling a credential bundle from another source.
func NewSecret(raw string) Secret { return Secret{raw: raw} }

// Reveal returns the plaintext. Call it at the point of use and never store
// the result.
func (s Secret) Reveal() string { return s.raw }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.raw == "" }

func (s Secret) String() string   { return "[redacted]" }
func (s Secret) GoString() string { return "[redacted]" }

// Format covers every fmt verb, including %+v and field-by-field dumps.
func (s Secret) Format(f fmt.State, verb rune) { fmt.Fprint(f, "[redacted]") }

var errSecretNotSerializable = errors.New("api: install credentials must never be serialized")

// MarshalJSON refuses: a credential must not be encoded into a journal, a
// progress event, or any request body the CLI builds.
func (s Secret) MarshalJSON() ([]byte, error) { return nil, errSecretNotSerializable }

// MarshalText refuses likewise (covers YAML, XML and text-based encoders).
func (s Secret) MarshalText() ([]byte, error) { return nil, errSecretNotSerializable }

// MarshalBinary refuses likewise (covers gob and binary encoders).
func (s Secret) MarshalBinary() ([]byte, error) { return nil, errSecretNotSerializable }

// UnmarshalJSON accepts the wire form, so the mint response decodes normally
// while the value stays unprintable from then on.
func (s *Secret) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.raw = raw
	return nil
}

// AgentJWT is the cluster's hub credential.
type AgentJWT struct {
	Token        Secret    `json:"token"`
	ExpiresAt    time.Time `json:"expires_at"`
	HubURL       string    `json:"hub_url"`
	TokenVersion int       `json:"token_version"`
}

// RepoCredential is the per-cluster GitOps repository credential: a write
// deploy key on this cluster's own repo, never a shared token (kn-rnyl §4).
// Absent when the control plane has no GitOps configured.
type RepoCredential struct {
	PrivateKey Secret `json:"private_key"`
	RepoURL    string `json:"repo_url"`
	Branch     string `json:"branch"`
}

// OperatorInstallInfo is where and what to install — no secrets.
type OperatorInstallInfo struct {
	Namespace string `json:"namespace"`
	ChartRef  string `json:"chart_ref"`
}

// AgentCredentials is the mint response. Returned exactly once: there is no
// endpoint that reads these back, and re-minting rotates rather than repeats.
type AgentCredentials struct {
	ClusterID      string              `json:"cluster_id"`
	AgentJWT       AgentJWT            `json:"agent_jwt"`
	RepoCredential *RepoCredential     `json:"repo_credential"`
	Operator       OperatorInstallInfo `json:"operator"`
}

// MintAgentCredentials issues the cluster's install-time credentials.
// Scope: clusters:register.
//
// CALLING THIS ROTATES. Every call increments token_version, replaces the
// stored agent JWT, and registers a fresh deploy key while deleting superseded
// ones after the control plane's grace window. Calling it a second time during
// a resume invalidates credentials an earlier run already placed on hosts —
// see pkg/register.
func (c *Client) MintAgentCredentials(ctx context.Context, clusterID string) (*AgentCredentials, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/agent-credentials"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	var out AgentCredentials
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	if out.AgentJWT.Token.IsZero() {
		return nil, fmt.Errorf("control plane returned no agent token for cluster %s: "+
			"the mint succeeded but the response is unusable, so the install cannot continue", clusterID)
	}
	return &out, nil
}
