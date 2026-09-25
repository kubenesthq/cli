package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// The two states a mirrored record can be in, as the contract names them. The
// route refuses a state that disagrees with `terminal`, and this client refuses
// to send one: a mirror that holds a different derivation of the pair would be
// a record lying about its own state, and the refusal is cheaper before the
// wire than after it.
const (
	ClusterOperationRunning  = "running"
	ClusterOperationTerminal = "terminal"
)

// ClusterOperationState is one revision of an operation record as the control
// plane mirrors it, for display and for `check_upgrade`.
//
// It is the record's DISPLAY state, never its authority. The lock is the
// `kubenest-operation` ConfigMap in the customer's cluster, and it keeps
// working while this control plane is unreachable — which is the case the whole
// design exists for. So no caller may read a failed mirror write as a failed
// operation.
//
// There is deliberately no field the request itself could travel in, only its
// digest, and none a credential could sit in: this record is copied
// off-cluster and mirrored, and the backend refuses an unknown field
// (`extra="forbid"`) rather than storing one.
type ClusterOperationState struct {
	// OperationID names the operation, and RequestDigest identifies the
	// immutable request it is running — so a record whose request changed
	// after it was acquired is visible as such.
	OperationID   string `json:"operation_id"`
	RequestDigest string `json:"request_digest"`
	// Terminal and State travel together, and must agree.
	Terminal bool   `json:"terminal"`
	State    string `json:"state"`
	// Stage is where the operation got to, which is what an interrupted record
	// names instead of "unknown".
	Stage string `json:"stage,omitempty"`
	// ResumeCommand is the command that continues it. Empty is allowed and
	// meaningful: the control plane then says the record names no resume
	// command, rather than printing one that does not exist.
	ResumeCommand string `json:"resume_command,omitempty"`
	// ExecutorHeartbeatAt is the last time the executor wrote anything. Stale
	// is how the control plane reads "interrupted"; it never permits a take-over.
	ExecutorHeartbeatAt *time.Time `json:"executor_heartbeat_at,omitempty"`
	// Revision is the ConfigMap resourceVersion this record was read at. The
	// route only moves forward and refuses an older revision, so a delayed
	// retry from an executor that no longer holds the lock cannot resurrect
	// the operation it lost.
	Revision string `json:"revision"`
}

// ClusterOperation is the mirror as the control plane holds it: the state that
// was written, plus the control plane's own columns. It is what a read
// returns, and it is never sent back: the route forbids a field it does not
// know, and `cluster_id` and `updated_at` are the control plane's.
type ClusterOperation struct {
	ClusterOperationState
	ClusterID string     `json:"cluster_id"`
	UpdatedAt *time.Time `json:"updated_at"`
}

// validate refuses a state the route is known to reject, so the CLI's own
// mirror post fails with the reason instead of a 422 whose text says the same
// thing.
func (s ClusterOperationState) validate() error {
	switch {
	case s.OperationID == "":
		return errors.New("a mirrored record needs the operation id it mirrors")
	case s.RequestDigest == "":
		return errors.New("a mirrored record needs the digest of the request it is running")
	case s.Revision == "":
		return errors.New("a mirrored record needs the resourceVersion it was read at: the mirror only moves forward, and a write without one cannot be placed")
	}
	want := ClusterOperationRunning
	if s.Terminal {
		want = ClusterOperationTerminal
	}
	if s.State != want {
		return fmt.Errorf("a mirrored record with terminal=%t must carry state %q, not %q", s.Terminal, want, s.State)
	}
	return nil
}

// PutClusterOperation mirrors one revision of a cluster's operation record.
// Scope: install:report — the scope the CLI reports its operations under.
//
// The write is idempotent, and that is deliberate: a CLI that retried after a
// timeout sends the same revision and the same content, and must not be told
// its own write is a conflict. What the route DOES refuse is a revision older
// than the one it holds, and the same revision with different content.
func (c *Client) PutClusterOperation(ctx context.Context, clusterID string, state ClusterOperationState) error {
	if err := state.validate(); err != nil {
		return err
	}
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/operation"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

// GetClusterOperation reads a cluster's mirrored record.
//
// A cluster that has never run a disruptive operation has no record, and the
// control plane answers 404 for exactly that. nil says so: it is a normal
// state, not an error about this control plane, and `check_upgrade` reads the
// same absence as "no upgrade performed".
func (c *Client) GetClusterOperation(ctx context.Context, clusterID string) (*ClusterOperation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/operation"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	var out ClusterOperation
	if err := c.do(req, &out); err != nil {
		var refused *Error
		if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}
