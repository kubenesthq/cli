// Fleet health reads (T2.9): the two control-plane routes behind `kubenest health`.
//
// THE FIELD NAMES ARE THE CONTRACT. `kubenest health --json` is a documented,
// stable format (plan 7.7) so Crest's own monitoring can run it with no new
// integration, and the names below are what that monitoring reads. Renaming a
// tag is therefore a breaking change to an interface we do not own, and the
// tests assert the names rather than the prose around them.
//
// The wire types mirror the backend's response schemas exactly
// (kubenest-backend/app/schemas/cluster_health.py). The view types at the bottom
// are the document this CLI writes: the wire answer plus the two things the
// fleet route does not carry — each cluster's groups, and the extras the plan
// requires, which come from the per-cluster detail call because the fleet
// route's `problems` list holds only the checks that are not `ok`.
//
// `InstanceAlerting` (the first line's source) is not here: T2.5 landed it in
// alerts.go, and a second definition of one response would be a second answer.
package api

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// HealthCheck is one group's verdict: the six-state vocabulary (T2.6) plus the
// fix-shaped message and the evidence envelope behind it.
type HealthCheck struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	// ReasonCode is the machine-readable name of the finding.
	ReasonCode string `json:"reason_code"`
	// Message is the human sentence, and for a problem it is fix-shaped: the
	// control plane does not report a fault without saying what to do about it.
	Message string `json:"message"`
	// Detail carries the facts worth showing next to the verdict (expiry dates,
	// node names, the last restore drill). A key that is absent is absent: it
	// is never a zero, and a reader says "not reported" rather than invent one.
	Detail map[string]any `json:"detail"`
	// Evidence is when the measurement behind this verdict was taken, when the
	// control plane received it, where it came from and which check-set rules
	// produced it. Empty for a check stored before the envelope existed, and a
	// reader must then say the measurement time is unknown, never assume now.
	Evidence map[string]any `json:"evidence"`
}

// FleetClusterHealth is one row of the fleet view.
type FleetClusterHealth struct {
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	// Status is the cluster's own state. A cluster that has never reported is
	// `unknown`, never `ok`.
	Status string `json:"status"`
	// ExitCode is the control plane's process exit code for Status (T2.6):
	// critical 2, unknown 3, warning 1, ok 0. Present on every row, never null,
	// so a consumer that treats a missing field as a hard error still gets an
	// honest number for a cluster that has never reported.
	ExitCode int `json:"exit_code"`
	// Problems is the checks that are not `ok`, and nothing else.
	Problems []HealthCheck `json:"problems"`
	// LastReportedAt is when the control plane last received a report. Nil means
	// never, which is not the same fact as "a while ago".
	LastReportedAt           *time.Time `json:"last_reported_at"`
	BundleVersion            string     `json:"bundle_version"`
	HATier                   string     `json:"ha_tier"`
	NoControlPlaneRedundancy bool       `json:"no_control_plane_redundancy"`
}

// FleetHealth is GET /api/v1/orgs/{org_id}/fleet-health: every cluster, worst first.
type FleetHealth struct {
	OrgID  string         `json:"org_id"`
	Total  int            `json:"total"`
	Counts map[string]int `json:"counts"`
	// ExitCode is the org's own verdict, from the worst STATE among its rows.
	ExitCode int                  `json:"exit_code"`
	Clusters []FleetClusterHealth `json:"clusters"`
	// Sweeper is the watchdog's own liveness. It travels with the answer because
	// a dead sweeper makes every cluster look permanently fine and nothing else
	// would notice.
	Sweeper map[string]any `json:"sweeper"`
}

// ClusterHealthDetail is GET /api/v1/clusters/{cluster_id}/health: everything we
// know about one cluster.
type ClusterHealthDetail struct {
	ClusterID string `json:"cluster_id"`
	// ClusterName is what the record calls it; a caller matching on a name must
	// not have to know the id it resolved the name to.
	ClusterName string `json:"cluster_name"`
	Status      string `json:"status"`
	// ExitCode is the same field and the same four numbers as a fleet row's.
	ExitCode int           `json:"exit_code"`
	Checks   []HealthCheck `json:"checks"`
	// ReportedAt is the cluster's own collection time and ReceivedAt when the
	// control plane took delivery. They differ by the network and by a clock
	// that may itself be the finding.
	ReportedAt            *time.Time `json:"reported_at"`
	ReceivedAt            *time.Time `json:"received_at"`
	ReportIntervalSeconds *int       `json:"report_interval_seconds"`
	BundleVersion         string     `json:"bundle_version"`
	// ThresholdsProvisional is true while the thresholds behind these verdicts
	// are not yet measured, and nil when the control plane could not say.
	ThresholdsProvisional *bool `json:"thresholds_provisional"`
}

// FleetHealth reads the org's fleet view. Scope: clusters:read.
func (c *Client) FleetHealth(ctx context.Context, orgID string) (FleetHealth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/orgs/"+url.PathEscape(orgID)+"/fleet-health"), nil)
	if err != nil {
		return FleetHealth{}, err
	}
	req.Header.Set("Accept", "application/json")

	var out FleetHealth
	if err := c.do(req, &out); err != nil {
		return FleetHealth{}, err
	}
	return out, nil
}

// ClusterHealthDetail reads one cluster's full health. Scope: clusters:read.
func (c *Client) ClusterHealthDetail(ctx context.Context, clusterID string) (ClusterHealthDetail, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/clusters/"+url.PathEscape(clusterID)+"/health"), nil)
	if err != nil {
		return ClusterHealthDetail{}, err
	}
	req.Header.Set("Accept", "application/json")

	var out ClusterHealthDetail
	if err := c.do(req, &out); err != nil {
		return ClusterHealthDetail{}, err
	}
	return out, nil
}

// HealthView is the document `kubenest health --json` writes, and the data both
// renderers draw from: the table is this document laid out for a human, so the
// two can never disagree.
//
// It carries every cluster's groups with their own state and reason, which the
// wire fleet answer does not (it holds only the checks that are not `ok`), and
// the four extras the plan requires per cluster.
type HealthView struct {
	// OrgID is the organisation the fleet was read from. Empty when the
	// credential sees more than one organisation: the document then covers all
	// of them, and naming one of them would be a lie about the other rows.
	OrgID string `json:"org_id"`
	// ExitCode is the code `kubenest health` must exit with: the worst row
	// state mapped once in the fleet view, the control plane's own number for
	// `--cluster C`. A cron line reads this field rather than parsing the table.
	ExitCode int            `json:"exit_code"`
	Total    int            `json:"total"`
	Counts   map[string]int `json:"counts"`
	// Alerting is the T2.5 status behind the first line, or null when it could
	// not be read — in which case AlertingError says why, because a fleet view
	// that cannot tell whether alerts have anywhere to go must say that much.
	Alerting      *InstanceAlerting `json:"alerting"`
	AlertingError string            `json:"alerting_error"`
	Clusters      []ClusterView     `json:"clusters"`
}

// ClusterView is one cluster in the document: the table's own row, its groups,
// and the extras the plan requires.
type ClusterView struct {
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	// Status is the state word exactly as the control plane sent it, so a
	// reader sees `unsupported` or `not_applicable` if one ever arrives as a
	// cluster's state rather than having it silently rewritten.
	Status string `json:"status"`
	// ExitCode is the control plane's own number for this cluster, unchanged:
	// the CLI does not re-derive a single cluster's code.
	ExitCode int `json:"exit_code"`
	// Reason is the sentence the table shows in place of a colour: the worst
	// check's own message, or the recovery fact that makes an `ok` cluster
	// worth calling ok, or the defect the control plane's answer contained.
	Reason        string `json:"reason"`
	BundleVersion string `json:"bundle_version"`
	// HATier is empty in the single-cluster document: the detail route does not
	// carry the cluster's tier (the fleet route does), and an empty string here
	// means "not reported" like every other absent string in this document.
	HATier         string        `json:"ha_tier"`
	LastReportedAt *time.Time    `json:"last_reported_at"`
	Groups         []HealthCheck `json:"groups"`
	Extras         ClusterExtras `json:"extras"`
	// DetailReadError is why Groups and Extras are empty, and empty means the
	// detail was read. The fleet route's answer is complete without it, so a
	// per-cluster read failure is disclosed rather than fatal.
	DetailReadError string `json:"detail_read_error"`
}

// ClusterExtras is the machine-readable part of a row: the four facts the plan
// names, each nil when the control plane did not report it.
//
// A nil is "not reported" and never a zero. `pending_reboot_nodes: 0` and "no
// reboot is pending" are different answers, and a fleet view that prints the
// second for the first is the failure this feature exists to prevent.
type ClusterExtras struct {
	// LastRestoreDrillAt is checks["backup"].detail["last_restore_drill_at"]: a
	// backup nobody has restored is a claim, not a capability.
	LastRestoreDrillAt *time.Time `json:"last_restore_drill_at"`
	// PendingRebootNodes is checks["os_patching"].detail["pending_reboot_nodes"].
	PendingRebootNodes *int `json:"pending_reboot_nodes"`
	// PausedProjects is checks["backup"].detail["paused_projects"]: work the
	// operator has paused, which is a reason a cluster is not converging.
	PausedProjects *int `json:"paused_projects"`
	// Operation is the cluster's in-flight operation (T2.3), or nil when none
	// was reported.
	Operation *RunningOperation `json:"operation"`
}

// RunningOperation is the operation record's projection into the health view:
// what is running, where it got to, and the command that resumes it.
type RunningOperation struct {
	OperationID   string `json:"operation_id"`
	Stage         string `json:"stage"`
	State         string `json:"state"`
	ResumeCommand string `json:"resume_command"`
}
