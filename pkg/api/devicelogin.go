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

// CliTokenScopes are the scopes a plain `kubenest login` requests: what the
// installer needs, nothing else (CliTokenScope in the contract). This request
// is unchanged by --scope, which ADDS to it.
var CliTokenScopes = []string{
	"clusters:read",
	"clusters:register",
	"bundles:read",
	"install:report",
}

// CliTokenScopeRotate is the one scope that can disconnect a cluster, so it is
// deliberately NOT in CliTokenScopes: `kubenest login --scope clusters:rotate`
// asks for it by name, and that login is what makes
// `kubenest cluster rotate-token` runnable. The control plane names it in the
// 403 insufficient_scope body when a token lacks it.
const CliTokenScopeRotate = "clusters:rotate"

// IsKnownCliTokenScope reports whether the contract's CliTokenScope enum
// carries `scope`. `kubenest login --scope` refuses anything else before the
// device flow starts, so a typo is an error on this machine naming the flag
// rather than a rejection of an authorization the human has already approved.
func IsKnownCliTokenScope(scope string) bool {
	if scope == CliTokenScopeRotate {
		return true
	}
	for _, known := range CliTokenScopes {
		if scope == known {
			return true
		}
	}
	return false
}

// KnownCliTokenScopes is every scope the contract's CliTokenScope enum carries:
// the default request followed by the opt-in ones. A caller naming the choices
// (a refusal in `kubenest login --scope`) reads them from here rather than
// keeping a second list that can fall behind the contract.
func KnownCliTokenScopes() []string {
	return append(append([]string{}, CliTokenScopes...), CliTokenScopeRotate)
}

// requestedScopes is the default request with `extra` folded in: the defaults
// in their own order, then anything new, no value repeated.
func requestedScopes(extra []string) []string {
	scopes := append([]string{}, CliTokenScopes...)
	seen := make(map[string]bool, len(scopes)+len(extra))
	for _, scope := range scopes {
		seen[scope] = true
	}
	for _, scope := range extra {
		if scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		scopes = append(scopes, scope)
	}
	return scopes
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
//
// extraScopes are added to the default request (CliTokenScopes), in order,
// ignoring duplicates — `kubenest login --scope clusters:rotate` is how a
// credential gets a scope a default login does not ask for. Passing none is the
// default request, unchanged.
func (c *Client) StartDeviceAuth(ctx context.Context, clientName string, extraScopes ...string) (DeviceAuth, error) {
	body, err := json.Marshal(map[string]any{
		"requested_scopes": requestedScopes(extraScopes),
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
