// Package api is the CLI's HTTP client for the KubeNest control plane.
//
// It deliberately covers only what the platform CLI needs: authentication and
// the calls the install/upgrade/backup paths make. It knows nothing about SSH;
// key material must never reach this package (enforced by a test in pkg/sshx).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"kubenest.io/cli/pkg/version"
)

const defaultTimeout = 30 * time.Second

// Client talks to one control plane.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	token      string
	// debugw receives redacted request traces when KUBENEST_DEBUG=1.
	debugw io.Writer
}

type Option func(*Client)

// WithToken sets the bearer credential for authenticated calls.
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithTimeout overrides the default request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithDebugWriter routes debug traces somewhere other than stderr (tests).
func WithDebugWriter(w io.Writer) Option {
	return func(c *Client) { c.debugw = w }
}

// New builds a client for the given control-plane base URL.
func New(controlPlane string, opts ...Option) (*Client, error) {
	if controlPlane == "" {
		return nil, fmt.Errorf("no control plane configured: run `kubenest login --control-plane https://...` first")
	}
	base, err := url.Parse(controlPlane)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("control plane URL %q is not a valid absolute URL", controlPlane)
	}
	c := &Client{
		baseURL:    base,
		httpClient: &http.Client{Timeout: defaultTimeout},
		debugw:     os.Stderr,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// BaseURL reports the control plane this client targets.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// SetToken replaces the bearer credential (after login).
func (c *Client) SetToken(token string) { c.token = token }

// Token is the login response of POST /api/v1/login.
type Token struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// Login authenticates with email and password and returns the bearer token.
// The control plane expects OAuth2 password-form encoding with the email in
// the username field.
func (c *Client) Login(ctx context.Context, email, password string) (Token, error) {
	form := url.Values{}
	form.Set("username", email)
	form.Set("password", password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.endpoint("/api/v1/login"), strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	var tok Token
	if err := c.do(req, &tok); err != nil {
		return Token{}, err
	}
	if tok.AccessToken == "" {
		return Token{}, fmt.Errorf("control plane returned no access token")
	}
	return tok, nil
}

// User is the shape of GET /api/v1/user/me/.
type User struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// CurrentUser returns the identity the stored credential belongs to.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/api/v1/user/me/"), nil)
	if err != nil {
		return User{}, err
	}
	req.Header.Set("Accept", "application/json")

	var u User
	if err := c.do(req, &u); err != nil {
		return User{}, err
	}
	return u, nil
}

func (c *Client) endpoint(p string) string {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + p
	return u.String()
}

// do executes the request, decodes a JSON response into out (if non-nil), and
// converts non-2xx responses into actionable errors.
func (c *Client) do(req *http.Request, out any) error {
	if c.token != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", "kubenest-cli/"+version.Version)

	c.debugf(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("control plane %s is unreachable: %w", c.baseURL.Host, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("not authenticated (or the session expired): run `kubenest login --control-plane %s`", c.BaseURL())
	case resp.StatusCode >= 400:
		return fmt.Errorf("%s %s: %s", req.Method, req.URL.Path, apiErrorDetail(resp.StatusCode, body))
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("control plane returned an unexpected response (%s): %w", resp.Status, err)
	}
	return nil
}

// apiErrorDetail extracts FastAPI's {"detail": ...} when present.
func apiErrorDetail(status int, body []byte) string {
	var e struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &e) == nil && len(e.Detail) > 0 {
		var s string
		if json.Unmarshal(e.Detail, &s) == nil {
			return fmt.Sprintf("%s (HTTP %d)", s, status)
		}
		return fmt.Sprintf("%s (HTTP %d)", e.Detail, status)
	}
	return fmt.Sprintf("HTTP %d", status)
}

// debugf writes a redacted request trace when KUBENEST_DEBUG=1. Credentials
// never appear: the Authorization header is masked and request bodies are not
// printed at all (the login body carries the password).
func (c *Client) debugf(req *http.Request) {
	if os.Getenv("KUBENEST_DEBUG") != "1" {
		return
	}
	fmt.Fprintf(c.debugw, "[debug] %s %s", req.Method, req.URL.String())
	for key := range req.Header {
		val := req.Header.Get(key)
		if strings.EqualFold(key, "Authorization") {
			val = "Bearer [redacted]"
		}
		fmt.Fprintf(c.debugw, " %s=%q", key, val)
	}
	fmt.Fprintln(c.debugw)
}
