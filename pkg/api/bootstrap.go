package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Bootstrapping the CLI's own access to a control plane it just installed:
// log in with the admin email and password, then exchange that session for a
// revocable CLI token. Both calls are in the contract's openapi.yaml.

// PasswordLogin logs in with an email and password and returns the backend's
// session access token (POST /api/v1/login, form-encoded — FastAPI's
// OAuth2PasswordRequestForm, which is why the field is `username` and not
// `email`).
//
// The session token is short-lived and is used for one thing: minting a CLI
// token. The password travels in the request body, which the debug trace
// never prints.
func (c *Client) PasswordLogin(ctx context.Context, email, password string) (string, error) {
	form := url.Values{"username": {email}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/login"), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("control plane accepted the login but returned no access_token")
	}
	return out.AccessToken, nil
}

// CreateCLIToken mints a CLI token (POST /api/v1/user/me/cli-tokens) with the
// client's bearer, which must be the session token from PasswordLogin. The
// returned knp_* token is shown once and is never retrievable again; the CLI
// stores it in ~/.kubenest/config.json.
func (c *Client) CreateCLIToken(ctx context.Context, name string, scopes []string, ttlDays int) (string, error) {
	body, err := json.Marshal(map[string]any{
		"name":     name,
		"scopes":   scopes,
		"ttl_days": ttlDays,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/user/me/cli-tokens"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("control plane accepted the request but returned no CLI token")
	}
	return out.Token, nil
}
