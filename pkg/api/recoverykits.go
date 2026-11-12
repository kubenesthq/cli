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

// RecoveryKitLocation is the non-secret bucket coordinate returned by a
// recorded cluster kit. It lives here so API reads do not import the kit
// package, which would create a cycle through operation's API client.
type RecoveryKitLocation struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	Region   string `json:"region"`
	Prefix   string `json:"prefix"`
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

// KitLocationsForOtherClusters reads the non-secret locations of every kit
// recorded for other clusters in this organisation. The scope check uses these
// coordinates to probe actual peer prefixes even when the S3 principal denies
// an unprefixed ListBucket. An unreadable record is an error: silently dropping
// one would turn a missing permission into a passing credential check.
func (c *Client) KitLocationsForOtherClusters(ctx context.Context, orgID, ownClusterID string) ([]RecoveryKitLocation, error) {
	endpoint := "/api/v1/orgs/" + url.PathEscape(orgID) + "/recovery-kits/locations"
	status, body, err := c.Get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("reading recovery-kit locations before checking backup credential scope: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("reading recovery-kit locations returned HTTP %d; cannot prove backup credential isolation", status)
	}
	var records struct {
		Data *[]struct {
			ClusterID  string               `json:"cluster_id"`
			S3Location *RecoveryKitLocation `json:"s3_location"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("decoding recovery-kit locations: %w", err)
	}
	if records.Data == nil {
		return nil, fmt.Errorf("recovery-kit locations response has no data array; cannot prove backup credential isolation")
	}
	var locations []RecoveryKitLocation
	for _, record := range *records.Data {
		if record.ClusterID == "" {
			return nil, fmt.Errorf("recovery-kit location has no cluster id; cannot prove backup credential isolation")
		}
		if record.ClusterID == ownClusterID {
			continue
		}
		if record.S3Location == nil || record.S3Location.Endpoint == "" || record.S3Location.Bucket == "" || record.S3Location.Prefix == "" {
			return nil, fmt.Errorf("cluster %s has a recovery kit without a complete S3 location; cannot prove backup credential isolation", record.ClusterID)
		}
		locations = append(locations, *record.S3Location)
	}
	return locations, nil
}
