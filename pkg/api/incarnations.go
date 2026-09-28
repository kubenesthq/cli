package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ClusterIncarnation is one physical incarnation of a cluster: the machine (or
// set of machines) that ran it from a point in time.
//
// A cluster's identity is the cluster's, never the host's. Rebuilding a lost
// host registers a NEW incarnation under the SAME cluster id, and that is what
// lets the control plane retire the old machine's credentials without the
// cluster losing its projects, its members or its history.
type ClusterIncarnation struct {
	ID        string `json:"id"`
	ClusterID string `json:"cluster_id"`
	// Ordinal counts the rebuilds recorded for one cluster, from 1. The host
	// the cluster was first installed on has no row, so ordinal 1 is the
	// second machine to run the cluster.
	Ordinal int `json:"ordinal"`
	// Reason is why the incarnation was recorded, e.g. "recovery".
	Reason     string `json:"reason"`
	RecordedAt string `json:"recorded_at"`
	// TokenFloor is the cluster's revocation floor after this incarnation was
	// recorded: every agent token below it is refused at the hub.
	TokenFloor *int `json:"token_floor,omitempty"`
}

// RecordIncarnation registers a new physical incarnation of an existing cluster
// (POST /api/v1/clusters/{id}/incarnations).
//
// It is the recovery path's adoption step. The control plane raises the
// cluster's revocation floor to the version its next credentials will carry, so
// the machine that is gone cannot reconnect once the new one is registered —
// the identity belongs to the cluster, and an incarnation boundary is exactly
// the moment the old machine stops being part of it.
func (c *Client) RecordIncarnation(ctx context.Context, clusterID, reason, note string) (*ClusterIncarnation, error) {
	body, err := json.Marshal(map[string]any{"reason": reason, "note": note})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/incarnations"), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	var out ClusterIncarnation
	if err := c.do(req, &out); err != nil {
		return nil, fmt.Errorf("recording a new incarnation of cluster %s: %w", clusterID, err)
	}
	if out.Ordinal == 0 || out.ClusterID != clusterID {
		return nil, fmt.Errorf("the control plane returned an unusable incarnation for cluster %s (ordinal %d, cluster %q): the server rebuilds from nothing until this is right, so the install cannot continue", clusterID, out.Ordinal, out.ClusterID)
	}
	return &out, nil
}

// ClusterIncarnationList is every recorded incarnation of one cluster.
type ClusterIncarnationList struct {
	ClusterID    string               `json:"cluster_id"`
	Incarnations []ClusterIncarnation `json:"incarnations"`
}

// ListIncarnations reads a cluster's incarnations, oldest first
// (GET /api/v1/clusters/{id}/incarnations).
//
// It is how a recovery is CHECKED rather than asserted: the rebuilt host is a
// new ordinal, and the floor on the newest one is the version the machine it
// replaced can no longer use.
func (c *Client) ListIncarnations(ctx context.Context, clusterID string) ([]ClusterIncarnation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/incarnations"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	var out ClusterIncarnationList
	if err := c.do(req, &out); err != nil {
		return nil, fmt.Errorf("reading the incarnations of cluster %s: %w", clusterID, err)
	}
	return out.Incarnations, nil
}
