package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Instance alert destinations (T2.5).
//
// The control plane stores a destination's URL encrypted and NEVER returns it:
// a webhook path is routinely the credential itself, so the API answers with the
// host and nothing else. `kubenest alerts list-destinations` therefore prints
// hosts, and `test` reports whether each destination accepted a test alert.
//
// These calls carry the operator's CLI token. The authority is the instance
// administrator flag on the control plane, not anything the CLI asserts.

// AlertDestination is one destination as the control plane describes it.
type AlertDestination struct {
	ID        string    `json:"id"`
	Format    string    `json:"format"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	// URLHost is the host (with a non-default port). The path is never returned.
	URLHost string `json:"url_host"`
}

// AlertDestinationResult is one destination's answer to a test alert.
type AlertDestinationResult struct {
	ID        string `json:"id"`
	Format    string `json:"format"`
	URLHost   string `json:"url_host"`
	Delivered bool   `json:"delivered"`
	Error     string `json:"error,omitempty"`
}

// AlertDestinationTest is the answer to `kubenest alerts test`.
type AlertDestinationTest struct {
	Destinations []AlertDestinationResult `json:"destinations"`
	Delivered    int                      `json:"delivered"`
	Failed       int                      `json:"failed"`
}

// AlertingBacklog is how far behind alert delivery is.
type AlertingBacklog struct {
	Pending          int  `json:"pending"`
	OldestAgeSeconds *int `json:"oldest_age_seconds"`
}

// InstanceAlerting is the instance's alerting status: counts and booleans only.
type InstanceAlerting struct {
	Destinations        int             `json:"destinations"`
	HeartbeatConfigured bool            `json:"heartbeat_configured"`
	Backlog             AlertingBacklog `json:"backlog"`
	FailedDestinations  int             `json:"failed_destinations"`
}

// AddAlertDestination adds an instance destination. format is "slack" or
// "generic"; the control plane refuses an address it must not connect to.
func (c *Client) AddAlertDestination(ctx context.Context, destinationURL, format string) (AlertDestination, error) {
	body, err := json.Marshal(map[string]string{"url": destinationURL, "format": format})
	if err != nil {
		return AlertDestination{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/instance/alert-destinations"), bytes.NewReader(body))
	if err != nil {
		return AlertDestination{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	var out AlertDestination
	if err := c.do(req, &out); err != nil {
		return AlertDestination{}, err
	}
	return out, nil
}

// ListAlertDestinations lists every destination. Hosts only — never a URL.
func (c *Client) ListAlertDestinations(ctx context.Context) ([]AlertDestination, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/instance/alert-destinations"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	var out []AlertDestination
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RemoveAlertDestination removes a destination and its undelivered outbox rows.
func (c *Client) RemoveAlertDestination(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.endpoint("/api/v1/instance/alert-destinations/"+url.PathEscape(id)), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// TestAlertDestinations fires a test alert to every enabled destination.
func (c *Client) TestAlertDestinations(ctx context.Context) (AlertDestinationTest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/instance/alert-destinations/test"), nil)
	if err != nil {
		return AlertDestinationTest{}, err
	}
	req.Header.Set("Accept", "application/json")

	var out AlertDestinationTest
	if err := c.do(req, &out); err != nil {
		return AlertDestinationTest{}, err
	}
	return out, nil
}

// SetHeartbeat stores the dead-man's-switch URL. Storage only: the control plane
// requests it after a healthy evaluation and delivery sweep (T2.8).
func (c *Client) SetHeartbeat(ctx context.Context, heartbeatURL string) error {
	body, err := json.Marshal(map[string]string{"url": heartbeatURL})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.endpoint("/api/v1/instance/heartbeat"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, nil)
}

// ClearHeartbeat removes the heartbeat URL.
func (c *Client) ClearHeartbeat(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.endpoint("/api/v1/instance/heartbeat"), nil)
	if err != nil {
		return err
	}
	return c.do(req, nil)
}

// InstanceAlerting reads the instance's alerting status. Readable by any
// signed-in caller, so `kubenest health` can say "no destination is configured".
func (c *Client) InstanceAlerting(ctx context.Context) (InstanceAlerting, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.endpoint("/api/v1/instance/alerting"), nil)
	if err != nil {
		return InstanceAlerting{}, err
	}
	req.Header.Set("Accept", "application/json")

	var out InstanceAlerting
	if err := c.do(req, &out); err != nil {
		return InstanceAlerting{}, err
	}
	return out, nil
}
