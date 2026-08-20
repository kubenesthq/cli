package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// CLI device authorization per the contract (openapi.yaml /auth/cli/device*):
// the CLI starts an authorization, the human approves the user code in the
// console from any browser, and the CLI polls until it receives the knp_*
// token — exactly once; the plaintext is never retrievable again.

// Scopes the installer needs, nothing else (CliTokenScope in the contract).
var CliTokenScopes = []string{
	"clusters:read",
	"clusters:register",
	"bundles:read",
	"install:report",
}

// DeviceAuth is the response of POST /auth/cli/device.
type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// StartDeviceAuth begins a device authorization.
func (c *Client) StartDeviceAuth(ctx context.Context, clientName string) (DeviceAuth, error) {
	body, err := json.Marshal(map[string]any{
		"requested_scopes": CliTokenScopes,
		"client_name":      clientName,
	})
	if err != nil {
		return DeviceAuth{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/auth/cli/device"), bytes.NewReader(body))
	if err != nil {
		return DeviceAuth{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	var auth DeviceAuth
	if err := c.do(req, &auth); err != nil {
		return DeviceAuth{}, err
	}
	if auth.DeviceCode == "" || auth.UserCode == "" || auth.VerificationURI == "" {
		return DeviceAuth{}, fmt.Errorf("control plane returned an incomplete device authorization")
	}
	return auth, nil
}

// Sentinel results of one token poll, per the contract's error codes.
var (
	errAuthorizationPending = errors.New("authorization_pending")
	errSlowDown             = errors.New("slow_down")
	// ErrAccessDenied: the user rejected the authorization in the console.
	ErrAccessDenied = errors.New("the authorization was denied in the console")
	// ErrExpiredToken: the authorization expired before approval.
	ErrExpiredToken = errors.New("the authorization expired before it was approved: run `kubenest login` again")
)

// pollDeviceToken performs one poll of POST /auth/cli/device/token.
func (c *Client) pollDeviceToken(ctx context.Context, deviceCode string) (string, error) {
	body, err := json.Marshal(map[string]string{"device_code": deviceCode})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/auth/cli/device/token"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	c.debugf(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("control plane %s is unreachable: %w", c.baseURL.Host, err)
	}
	defer resp.Body.Close()

	var payload struct {
		Token   string `json:"token"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("control plane returned an unexpected response (%s): %w", resp.Status, err)
	}

	switch {
	case resp.StatusCode == http.StatusOK && payload.Token != "":
		return payload.Token, nil
	case payload.Error == "authorization_pending":
		return "", errAuthorizationPending
	case payload.Error == "slow_down":
		return "", errSlowDown
	case payload.Error == "access_denied":
		return "", ErrAccessDenied
	case payload.Error == "expired_token":
		return "", ErrExpiredToken
	default:
		return "", fmt.Errorf("device token poll failed: %s (HTTP %d)", payload.Message, resp.StatusCode)
	}
}

// WaitForDeviceToken polls until the authorization is approved, denied or
// expired, honoring the server's interval and slow_down responses. On
// success it returns the knp_* token — the only time it is ever available.
func (c *Client) WaitForDeviceToken(ctx context.Context, auth DeviceAuth) (string, error) {
	interval := time.Duration(auth.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	deadline := time.Now().Add(time.Duration(auth.ExpiresIn) * time.Second)
	for {
		token, err := c.pollDeviceToken(ctx, auth.DeviceCode)
		switch {
		case err == nil:
			return token, nil
		case errors.Is(err, errAuthorizationPending):
			// keep polling
		case errors.Is(err, errSlowDown):
			interval += 5 * time.Second // RFC 8628 §3.5
		default:
			return "", err
		}

		if auth.ExpiresIn > 0 && time.Now().After(deadline) {
			return "", ErrExpiredToken
		}
		if err := c.sleep(ctx, interval); err != nil {
			return "", err
		}
	}
}

// sleepFn waits or returns early on cancellation; replaced in tests.
type sleepFn func(ctx context.Context, d time.Duration) error

func realSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
