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

// HostRecord is one host in a cluster's inventory (kn-t50): the contract's
// HostEntry.
//
// This is what makes a node verb possible from a second laptop. The install
// journal lives on the machine that ran the install; this record lives in the
// control plane, and every node verb reads its targets from here.
type HostRecord struct {
	HostID string `json:"host_id"`
	// NodeUID is the Kubernetes Node's metadata.uid, and it is empty until
	// the node exists — a host can be in the inventory before it is a node.
	NodeUID string `json:"node_uid"`
	// SSHAddress, SSHPort and SSHUser are where and as whom the CLI reaches
	// the host over SSH.
	SSHAddress string `json:"ssh_address"`
	SSHPort    int    `json:"ssh_port"`
	SSHUser    string `json:"ssh_user"`
	// HostKeyFingerprint is the SHA-256 of the host key seen when the host
	// joined, in OpenSSH's SHA256:<base64> form. Recorded, not independently
	// verified: SSH trust-on-first-use is unchanged. Not a secret.
	HostKeyFingerprint string `json:"host_key_fingerprint"`
	// JoinAddress is the address the host joined the cluster through, which
	// may be a private address different from SSHAddress.
	JoinAddress string `json:"join_address"`
	// Role is "server" (a k3s control-plane node) or "agent" (a worker).
	Role string `json:"role"`
	// StorageDevice is the stable /dev/disk/by-id/... path, or empty for a
	// cluster installed without one. A bare device name is never accepted.
	StorageDevice string `json:"storage_device"`
	// VolumeGroupOwnership is "customer-created" or "installer-created":
	// what an uninstall or a node removal may destroy on THIS host.
	VolumeGroupOwnership string `json:"volume_group_ownership"`
	// LifecycleState is "joining", "active", "removing" or "removed".
	LifecycleState string `json:"lifecycle_state"`
}

// BundleRecord is stage 12's write: what was installed, which profiles, which
// tier, and who owns the volume group.
//
// The volume-group ownership is not bookkeeping. It is the input uninstall
// reads to decide whether it may remove a volume group, and a wrong value
// there is the difference between a clean teardown and destroying a
// customer's data.
//
// Revision is the compare-and-swap this write is based on: it is the revision
// the caller READ, and the control plane refuses a write that is not at the
// current one (409) rather than applying it. Two operators editing one
// cluster from two laptops is the ordinary case.
type BundleRecord struct {
	BundleVersion        string                `json:"bundle_version"`
	Profiles             []string              `json:"profiles"`
	HATier               string                `json:"ha_tier"`
	VolumeGroupOwnership string                `json:"volume_group_ownership"`
	InstallJournal       []InstallJournalEntry `json:"install_journal,omitempty"`
	// Hosts is the inventory this write asserts. Omitted means "leave the
	// stored inventory alone": an upgrade rewrites this record and carries no
	// host list, and must not erase one.
	Hosts []HostRecord `json:"hosts,omitempty"`
	// Revision is the revision the write is based on. Required by the
	// contract: a body that does not carry one is refused.
	Revision int `json:"revision"`
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

// ClusterBundle is the cluster's recorded bundle: what is installed on it,
// which profiles, which tier, who owns the volume group, and which machines
// it is.
//
// An upgrade reads it rather than being told: the record is the authority on
// what the cluster IS, and an upgrade that took its starting point from a
// command-line argument could move a cluster it had misidentified.
type ClusterBundle struct {
	BundleVersion        string                `json:"bundle_version"`
	Profiles             []string              `json:"profiles"`
	HATier               string                `json:"ha_tier"`
	VolumeGroupOwnership string                `json:"volume_group_ownership"`
	InstallJournal       []InstallJournalEntry `json:"install_journal"`
	// Hosts is the cluster's host inventory. Null in the record means no
	// inventory has ever been recorded — NOT an empty cluster — and a node
	// verb must refuse that rather than guess the machines from flags.
	Hosts []HostRecord `json:"hosts"`
	// Revision is what a write must carry back (see BundleRecord.Revision).
	Revision int `json:"revision"`
}

// BundleRecord reads the cluster's recorded bundle.
func (c *Client) BundleRecord(ctx context.Context, clusterID string) (ClusterBundle, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/bundle"), nil)
	if err != nil {
		return ClusterBundle{}, err
	}
	req.Header.Set("Accept", "application/json")
	var out ClusterBundle
	if err := c.do(req, &out); err != nil {
		return ClusterBundle{}, err
	}
	return out, nil
}

// MaintenanceWindow is the cluster's recurring window: the one definition
// shared by everything that must not act outside it — bundle upgrades
// (kn-fuo) and OS reboots (kn-nqj).
//
// The timezone is an IANA name and never an offset: offsets move twice a year
// and a window that silently shifts by an hour is worse than no window, since
// the customer set their local time and local is what they meant.
type MaintenanceWindow struct {
	Days     []string `json:"days"`
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Timezone string   `json:"timezone"`
}

// The four states a stored window is in, as the control plane reports them,
// and as the command surface prints them. They are distinct values and are
// never collapsed (kn-t31): `none` means no window has ever been written,
// `stored` is written to the control plane, `applying` has been handed to the
// cluster and not yet acknowledged, and `active` has been acknowledged at the
// current revision. A window that is stored but not yet active is NOT in force,
// and `none` is not `stored` — a cluster with no window must not have to be
// recognised by its null field (kn-nqj.1).
const (
	WindowStateNone     = "none"
	WindowStateStored   = "stored"
	WindowStateApplying = "applying"
	WindowStateActive   = "active"
)

// WindowBackupUnknown is what the control plane puts in Warning when the
// operator's backup duration has never been measured. It is a wire value and
// not a message: a client must be able to tell "no overlap" (null) from "we
// could not tell" (this), because an unmeasured backup is not a passing check.
const WindowBackupUnknown = "unknown"

// MaintenanceWindowRecord is what both maintenance-window routes answer: the
// stored window, the revision it is at, and which of the three states it is in.
//
// A NIL Window IS A REFUSAL, NOT PERMISSION. The cluster's window is unset and
// no code may read that as "any time is inside it" — that was the kn-nqj
// defect, where the upgrade's window gate approved every cluster because a
// missing window was treated as a passing check. A caller with a nil window
// must refuse and name `kubenest cluster set-window`.
type MaintenanceWindowRecord struct {
	Window   *MaintenanceWindow `json:"window"`
	Revision *int               `json:"revision"`
	State    string             `json:"state"`
	// AppliedRevision is the revision the operator last acknowledged. Lower
	// than Revision — or nil — means the cluster is still running the previous
	// window (or none), which is how a disconnected cluster is reported rather
	// than as the new one being in force.
	AppliedRevision *int    `json:"applied_revision"`
	RejectReason    *string `json:"reject_reason"`
	// Warning is the backup-overlap warning, the literal "unknown" when the
	// operator's backup evidence has never been measured, or nil when the
	// evidence was read and there is no overlap. An unmeasured backup is not a
	// passing check, which is why the control plane answers "unknown" rather
	// than nothing.
	Warning *string `json:"warning"`
}

// CurrentRevision is the revision a write must carry: the one this record was
// read at. A cluster that has never had a window is at revision 0, which is
// what its first write is based on.
func (r MaintenanceWindowRecord) CurrentRevision() int {
	if r.Revision == nil {
		return 0
	}
	return *r.Revision
}

// PutMaintenanceWindow stores the cluster's window. Scope: install:report.
//
// revision is the compare-and-swap: the revision the caller READ, from
// MaintenanceWindow. A write that is not at the stored window's current
// revision is refused with 409 and the stored window is left alone — two
// operators setting one cluster's window from two laptops is the ordinary
// case, and which window is in force is not something to be ambiguous about.
//
// The stored window comes back with its new revision and its state, which is
// `stored` or `applying`: a freshly written window is NOT active until the
// operator has acknowledged it, and the caller must not report it as in force.
func (c *Client) PutMaintenanceWindow(ctx context.Context, clusterID string, w MaintenanceWindow, revision int) (MaintenanceWindowRecord, error) {
	body, err := json.Marshal(struct {
		MaintenanceWindow
		Revision int `json:"revision"`
	}{MaintenanceWindow: w, Revision: revision})
	if err != nil {
		return MaintenanceWindowRecord{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/maintenance-window"), bytes.NewReader(body))
	if err != nil {
		return MaintenanceWindowRecord{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	var out MaintenanceWindowRecord
	if err := c.do(req, &out); err != nil {
		return MaintenanceWindowRecord{}, windowRefusal(err)
	}
	return out, nil
}

// MaintenanceWindow reads the cluster's window, its revision and its state.
// Scope: clusters:read.
//
// A cluster with no window answers 200 with a nil Window, a nil Revision and
// state "none". A control plane too old to serve the route answers 404, which
// is an ERROR and not a nil record: an unroutable read is a failed read, and a
// failed read that looked like "no window" would also be a failed read that
// looked like permission.
func (c *Client) MaintenanceWindow(ctx context.Context, clusterID string) (MaintenanceWindowRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/maintenance-window"), nil)
	if err != nil {
		return MaintenanceWindowRecord{}, err
	}
	req.Header.Set("Accept", "application/json")
	var out MaintenanceWindowRecord
	if err := c.do(req, &out); err != nil {
		return MaintenanceWindowRecord{}, windowRefusal(err)
	}
	return out, nil
}

// windowRefusal turns the two window-route refusals into the operator's next
// action.
//
// 409 is the compare-and-swap: the window moved since this caller read it, so
// the fix is to re-read and re-apply, never to retry the same body. 404 is a
// control plane that does not serve the route, which is reported as an
// inability to read the window rather than as a missing one.
func windowRefusal(err error) error {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Status {
	case http.StatusConflict:
		return fmt.Errorf("someone changed the window; re-run: %w", err)
	case http.StatusNotFound:
		return fmt.Errorf("this control plane does not serve the cluster's maintenance-window route, so its window cannot be read — that is not the same as no window being set; upgrade the control plane before acting on the cluster: %w", err)
	}
	return err
}

// AgentTokenRotation is the result of POST /clusters/{id}/rotate-token (kn-i3c).
//
// IT CARRIES THE NEW JWT AND NOTHING ELSE THAT VALUES RENDERING NEEDS — no repo
// credential, no operator install info. That is deliberate on the backend side
// and it is why delivery patches the cluster's existing agent manifest rather
// than re-rendering it: see agent.ReplaceJWTSecret. Re-minting to recover the
// missing fields would rotate the token a second time.
type AgentTokenRotation struct {
	ClusterID string   `json:"cluster_id"`
	AgentJWT  AgentJWT `json:"agent_jwt"`
	// Enforcement is "in_force" when the hub confirmed the new revocation floor
	// before the response was sent, or "pending" when it did not. Pending is not
	// an error: the rotation already happened and every snapshot carries the
	// floor, so enforcement follows within one snapshot interval.
	Enforcement string `json:"enforcement"`
	Detail      string `json:"detail"`
	// ConnectionDropped says whether the hub closed a live operator connection.
	// False does NOT mean the old token still works — usually it means the
	// cluster was not connected at the time.
	ConnectionDropped bool `json:"connection_dropped"`
}

// RotateAgentToken rotates the cluster's agent JWT and revokes every older one.
//
// THIS DISCONNECTS THE CLUSTER. The new token reaches the agent through chart
// values, so the cluster stays disconnected until the token in the response is
// delivered to it. Callers that stop here have not finished a rotation; they
// have taken a cluster offline.
func (c *Client) RotateAgentToken(ctx context.Context, clusterID string) (AgentTokenRotation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/rotate-token"), nil)
	if err != nil {
		return AgentTokenRotation{}, err
	}
	var out AgentTokenRotation
	if err := c.do(req, &out); err != nil {
		return AgentTokenRotation{}, err
	}
	if out.AgentJWT.Token.IsZero() {
		// The floor has already been raised by the time a response is written,
		// so a response with no token is a rotation whose result was lost. Say
		// that rather than delivering an empty secret.
		return AgentTokenRotation{}, fmt.Errorf(
			"the control plane rotated %s but returned no token: the cluster is now disconnected and the new token is not recoverable — re-run to rotate again", clusterID)
	}
	return out, nil
}

// Cluster reads one cluster's record, including the status the hub maintains.
// Used to prove a rotated cluster came back rather than assuming it did.
func (c *Client) Cluster(ctx context.Context, clusterID string) (Cluster, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)), nil)
	if err != nil {
		return Cluster{}, err
	}
	var out Cluster
	if err := c.do(req, &out); err != nil {
		return Cluster{}, err
	}
	return out, nil
}
