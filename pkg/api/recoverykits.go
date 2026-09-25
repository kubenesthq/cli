package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// RecoveryKitRecord is what the control plane records about a recovery kit it
// will never hold: the immutable artifact id, when it was written, and a
// fingerprint of each key it carries.
//
// There is deliberately no field a key, a value or the sealed document could
// travel in, and the backend refuses a body that tries to smuggle one in by
// name. The control plane's job here is to answer "which kit exists for what",
// so that an export can be authorised; it cannot re-create a kit's private
// material, because it never had any.
type RecoveryKitRecord struct {
	ArtifactID string `json:"artifact_id"`
	// Fingerprints maps each key's NAME to its sha256 fingerprint. Never a
	// value, and never the value's hash without the domain separation
	// recoverykit.Fingerprint applies.
	Fingerprints map[string]string `json:"fingerprints"`
	WrittenAt    time.Time         `json:"written_at"`
	// ClusterID names the management cluster on a control-plane kit — the
	// cluster the control plane runs in — and is the path on a cluster kit.
	ClusterID          string         `json:"cluster_id,omitempty"`
	InstanceID         string         `json:"instance_id,omitempty"`
	VeleroRepositoryID string         `json:"velero_repository_id,omitempty"`
	S3Location         map[string]any `json:"s3_location,omitempty"`
}

// RecordRecoveryKit tells the control plane that a kit exists, by fingerprint.
//
// kind is the kit's scope. A cluster kit posts under its cluster, which the
// route requires the body to agree with; the instance's control-plane kit is
// an instance resource and posts on its own route, where the body names the
// management cluster so the control plane can tell which cluster is not an
// organisation's business.
func (c *Client) RecordRecoveryKit(ctx context.Context, orgID, clusterID, kind string, rec RecoveryKitRecord) error {
	path := "/api/v1/orgs/" + url.PathEscape(orgID) + "/clusters/" + url.PathEscape(clusterID) + "/recovery-kits"
	if kind == "control-plane" {
		path = "/api/v1/orgs/" + url.PathEscape(orgID) + "/recovery-kits/control-plane"
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}
