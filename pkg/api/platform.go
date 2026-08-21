package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// The control-plane calls the platform installer makes, other than register:
// fetching the bundle manifest it installs from, reporting each stage as it
// happens, and writing the record at the end.
//
// They live in their own file so the installer's surface and the register
// stage's surface can grow independently — two seats, two files, no
// contention in one shared client.go.

// BundleManifest fetches one bundle manifest, exactly as authored in
// kubenest-contracts.
//
// The manifest comes from the CONTROL PLANE rather than from a file on the
// operator's machine, because the bundle is a versioned artifact of the
// release and the record written in stage 12 has to be checkable against the
// same document the control plane serves. Returned as raw bytes for
// pkg/manifest to parse — this package does not interpret bundles.
func (c *Client) BundleManifest(ctx context.Context, version string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/bundles/"+url.PathEscape(version)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	var raw json.RawMessage
	if err := c.do(req, &raw); err != nil {
		return nil, fmt.Errorf("fetching bundle %s: %w", version, err)
	}
	return raw, nil
}

// InstallStageStatus is the canonical status vocabulary shared by the local
// journal, this API and the SSE stream (install_journal_entry.json).
type InstallStageStatus string

const (
	StageStarted   InstallStageStatus = "started"
	StageCompleted InstallStageStatus = "completed"
	StageFailed    InstallStageStatus = "failed"
)

// InstallJournalEntry is one stage transition, in the contract's shape.
//
// Note what is NOT here: nothing that can hold a credential. detail is
// user-safe text only — no secrets, no raw command output — and the caller
// sanitizes it before it reaches this struct.
type InstallJournalEntry struct {
	Stage     string             `json:"stage"`
	Component string             `json:"component,omitempty"`
	Status    InstallStageStatus `json:"status"`
	At        *time.Time         `json:"at,omitempty"`
	// ReasonCode is the machine-readable failure reason, required by the
	// taxonomy when Status is failed.
	ReasonCode string `json:"reason_code,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// ReportInstallStage publishes one stage transition. Scope: install:report.
//
// The caller must treat a failure here as non-fatal: an install that
// succeeded on the machines has succeeded even if its telemetry did not
// arrive, and failing it would let the observability path break the thing it
// observes.
func (c *Client) ReportInstallStage(ctx context.Context, clusterID string, entry InstallJournalEntry) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/install-events"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

// BundleRecord is stage 12's write: what was installed, which profiles, which
// tier, and who owns the volume group.
//
// The volume-group ownership is not bookkeeping. It is the input uninstall
// reads to decide whether it may remove a volume group, and a wrong value
// there is the difference between a clean teardown and destroying a
// customer's data.
type BundleRecord struct {
	BundleVersion        string                `json:"bundle_version"`
	Profiles             []string              `json:"profiles"`
	HATier               string                `json:"ha_tier"`
	VolumeGroupOwnership string                `json:"volume_group_ownership"`
	InstallJournal       []InstallJournalEntry `json:"install_journal,omitempty"`
}

// PutBundleRecord writes the cluster's bundle record. Scope: install:report.
//
// The control plane validates every field against the bundle catalog and
// rejects an unknown version, an unknown profile or a tier the bundle does
// not offer. That refusal is wanted: a cluster whose record does not match
// what is on it cannot be safely upgraded, and nothing else in the day-2
// story is trustworthy if the record drifts.
func (c *Client) PutBundleRecord(ctx context.Context, clusterID string, record BundleRecord) error {
	if record.Profiles == nil {
		record.Profiles = []string{}
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/bundle"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

// ClusterHealth is the "has this cluster reported in?" view: the status the
// control plane shows, and whether a fleet-telemetry heartbeat has arrived.
//
// Both are needed, and the heartbeat is the load-bearing one. Status alone is
// not a reliable answer during an install: every install-stage event moves a
// cluster to `installing`, so a stage that runs AFTER the agent connects
// (record, verify) puts a connected cluster back into `installing` until the
// next heartbeat. The heartbeat timestamp cannot be moved backwards by an
// event, which is why the acceptance check reads it.
type ClusterHealth struct {
	Name          string     `json:"name"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"last_heartbeat"`
}

// ClusterHealth reads one cluster's status and last heartbeat.
func (c *Client) ClusterHealth(ctx context.Context, clusterID string) (ClusterHealth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)), nil)
	if err != nil {
		return ClusterHealth{}, err
	}
	req.Header.Set("Accept", "application/json")
	var out ClusterHealth
	if err := c.do(req, &out); err != nil {
		return ClusterHealth{}, err
	}
	return out, nil
}

// InstallJournal reads the SERVER-side install journal for a cluster: the
// terminal transitions the backend persisted, in order.
//
// This is what an acceptance check should assert against. A CLI that checks
// its own error object proves only that it is internally consistent; reading
// the record back proves the whole chain — the CLI emitted, the backend
// persisted, and the record names the stage and component that failed.
func (c *Client) InstallJournal(ctx context.Context, clusterID string) ([]InstallJournalEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/bundle"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	var out struct {
		InstallJournal []InstallJournalEntry `json:"install_journal"`
	}
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out.InstallJournal, nil
}
