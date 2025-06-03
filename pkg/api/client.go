package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"time"
	"kubenest.io/cli/pkg/config"
)

const (
	defaultTimeout = 30 * time.Second
)

var defaultBaseURL, _ = url.Parse("https://api.kubenest.io")

// Client handles HTTP communication with the Kubenest API
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	token      string
	teamUUID   string
}

// ClientOption is a function that configures a Client
type ClientOption func(*Client)

// WithBaseURL sets the base URL for the client
func WithBaseURL(baseURL string) ClientOption {
	return func(c *Client) {
		parsedURL, _ := url.Parse(baseURL)
		c.baseURL = parsedURL
	}
}

// WithToken sets the authentication token for the client
func WithToken(token string) ClientOption {
	return func(c *Client) {
		c.token = token
	}
}

// WithTimeout sets the timeout for HTTP requests
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		c.httpClient.Timeout = timeout
	}
}

// NewClient creates a new API client with the given options
func NewClient(opts ...ClientOption) (*Client, error) {
	client := &Client{
		httpClient: &http.Client{
			Timeout: defaultTimeout,
		},
		baseURL: defaultBaseURL,
	}

	for _, opt := range opts {
		opt(client)
	}

	return client, nil
}

func (c *Client) SetToken(token string) {
	c.token = token
}

func (c *Client) SetTeamUUID(teamUUID string) {
	c.teamUUID = teamUUID
}

// Get performs a GET request to the specified endpoint
func (c *Client) Get(ctx context.Context, endpoint string) (*http.Response, error) {
	url := *c.baseURL
	url.Path = path.Join(url.Path, endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
	req.Header.Set("Accept", "application/json")

	// Set X-Team-UUID header for all endpoints except /api/v1/teams and /api/v1/auth/login
	if c.teamUUID != "" && !isTeamsOrLoginEndpoint(endpoint) {
		req.Header.Set("X-Team-UUID", c.teamUUID)
	}

	// Print the equivalent curl command
	if os.Getenv("DEBUG") == "1" {
		curlCmd := "curl -X GET "
		for key, values := range req.Header {
			for _, value := range values {
				curlCmd += fmt.Sprintf("-H '%s: %s' ", key, value)
			}
		}
		curlCmd += fmt.Sprintf("'%s'", url.String())
		fmt.Println("[DEBUG] Equivalent curl command:", curlCmd)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return resp, nil
}

// isTeamsOrLoginEndpoint returns true if the endpoint is /api/v1/teams or /api/v1/auth/login
func isTeamsOrLoginEndpoint(endpoint string) bool {
	return endpoint == "/api/v1/teams" || endpoint == "/api/v1/auth/login"
}

// Post performs a POST request to the specified endpoint
func (c *Client) Post(endpoint string, body interface{}) ([]byte, error) {
	return c.doRequest(http.MethodPost, endpoint, body)
}

// Put performs a PUT request to the specified endpoint
func (c *Client) Put(endpoint string, body interface{}) ([]byte, error) {
	return c.doRequest(http.MethodPut, endpoint, body)
}

// Delete performs a DELETE request to the specified endpoint
func (c *Client) Delete(endpoint string) ([]byte, error) {
	return c.doRequest(http.MethodDelete, endpoint, nil)
}

// doRequest performs the HTTP request and handles the response
func (c *Client) doRequest(method, endpoint string, body interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequest(method, c.baseURL.String()+endpoint, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if c.teamUUID != "" {
		req.Header.Set("X-Team-UUID", c.teamUUID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error       string `json:"error"`
			Code        int    `json:"code"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(respBody, &errResp); err != nil {
			return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(respBody))
		}
		return nil, fmt.Errorf("request failed: %s (code: %d, description: %s)", errResp.Error, errResp.Code, errResp.Description)
	}

	return respBody, nil
}

// Login authenticates with the backend
func (c *Client) Login(email, password string) (*LoginResponse, error) {
	type loginRequest struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	respBody, err := c.doRequest("POST", "/api/v1/auth/login", loginRequest{
		Email:    email,
		Password: password,
	})
	if err != nil {
		return nil, err
	}

	var result LoginResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// ListTeams returns all teams for the authenticated user
func (c *Client) ListTeams(ctx context.Context) ([]Team, error) {
	resp, err := c.Get(ctx, "/api/v1/teams")
	if err != nil {
		return nil, fmt.Errorf("failed to list teams: %w", err)
	}
	defer resp.Body.Close()

	var teams []Team
	if err := json.NewDecoder(resp.Body).Decode(&teams); err != nil {
		return nil, fmt.Errorf("failed to decode teams response: %w", err)
	}

	return teams, nil
}

// ListClusters returns all clusters for the current team
func (c *Client) ListClusters(ctx context.Context) ([]Cluster, error) {
	resp, err := c.Get(ctx, "/api/v1/clusters")
	if err != nil {
		return nil, fmt.Errorf("failed to list clusters: %w", err)
	}
	defer resp.Body.Close()

	var clusters []Cluster
	if err := json.NewDecoder(resp.Body).Decode(&clusters); err != nil {
		return nil, fmt.Errorf("failed to decode clusters response: %w", err)
	}

	return clusters, nil
}

// ListProjects returns all projects for the current team
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	resp, err := c.Get(ctx, "/api/v1/projects")
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	defer resp.Body.Close()

	var projects []Project
	if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
		return nil, fmt.Errorf("failed to decode projects response: %w", err)
	}

	return projects, nil
}

// ListStackDeploys returns all stackdeploys for the current team
func (c *Client) ListStackDeploys(ctx context.Context) ([]StackDeploy, error) {
	resp, err := c.Get(ctx, "/api/v1/stack-deploys")
	if err != nil {
		return nil, fmt.Errorf("failed to list stack deploys: %w", err)
	}
	defer resp.Body.Close()

	var stackdeploys []StackDeploy
	if err := json.NewDecoder(resp.Body).Decode(&stackdeploys); err != nil {
		return nil, fmt.Errorf("failed to decode stack deploys response: %w", err)
	}

	return stackdeploys, nil
}

// ListApps returns all stackdeploy apps for the current team
func (c *Client) ListApps(ctx context.Context) ([]StackDeployApp, error) {
	resp, err := c.Get(ctx, "/api/v1/stackdeploys")
	if err != nil {
		return nil, fmt.Errorf("failed to list apps: %w", err)
	}
	defer resp.Body.Close()

	var apps []StackDeployApp
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, fmt.Errorf("failed to decode apps response: %w", err)
	}

	return apps, nil
}

// GetLogs retrieves application logs
func (c *Client) GetLogs(appID string) (io.ReadCloser, error) {
	url := *c.baseURL
	url.Path = path.Join(url.Path, "/api/v1/apps/", appID, "/logs")

	req, err := http.NewRequest(http.MethodGet, url.String(), nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.teamUUID != "" {
		req.Header.Set("X-Team-UUID", c.teamUUID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("failed to get logs: %s", resp.Status)
	}

	return resp.Body, nil
}

// ExecCommand executes a command in a pod
func (c *Client) ExecCommand(appID, podName, command string) (io.ReadCloser, error) {
	type execRequest struct {
		Command string `json:"command"`
	}

	url := *c.baseURL
	url.Path = path.Join(url.Path, "/api/v1/apps/", appID, "/pods/", podName, "/exec")

	bodyBytes, err := json.Marshal(execRequest{Command: command})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, url.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.teamUUID != "" {
		req.Header.Set("X-Team-UUID", c.teamUUID)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("exec failed: %s", resp.Status)
	}

	return resp.Body, nil
}

// CopyFile copies files to/from a pod
func (c *Client) CopyFile(appID, podName, srcPath, destPath string, isUpload bool) error {
	file, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer file.Close()

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/apps/%s/pods/%s/copy", c.baseURL.String(), appID, podName), file)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.teamUUID != "" {
		req.Header.Set("X-Team-UUID", c.teamUUID)
	}

	query := req.URL.Query()
	query.Add("dest", destPath)
	if isUpload {
		query.Add("direction", "upload")
	} else {
		query.Add("direction", "download")
	}
	req.URL.RawQuery = query.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("copy failed: %s", resp.Status)
	}

	if !isUpload {
		// For downloads, save the file
		destFile, err := os.Create(destPath)
		if err != nil {
			return err
		}
		defer destFile.Close()

		_, err = io.Copy(destFile, resp.Body)
		return err
	}

	return nil
}

// ListPods returns all pods for a given application
func (c *Client) ListPods(appID string) ([]Pod, error) {
	respBody, err := c.doRequest("GET", fmt.Sprintf("/api/v1/apps/%s/pods", appID), nil)
	if err != nil {
		return nil, err
	}

	var pods []Pod
	if err := json.Unmarshal(respBody, &pods); err != nil {
		return nil, err
	}

	return pods, nil
}

type UserInfo struct {
	UUID      string `json:"uuid"`
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// GetUser fetches the current user's info
func (c *Client) GetUser(ctx context.Context) (*UserInfo, error) {
	resp, err := c.Get(ctx, "/api/v1/user")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var userInfo UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return nil, err
	}
	return &userInfo, nil
}

// GetStackDeployDetail fetches stackdeploy details by uuid
func (c *Client) GetStackDeployDetail(ctx context.Context, uuid string) (*StackDeployDetail, error) {
	resp, err := c.Get(ctx, "/api/v1/stackdeploys/"+uuid)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var detail StackDeployDetail
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// GetProjectKubeconfig fetches the kubeconfig for a project by uuid
func (c *Client) GetProjectKubeconfig(ctx context.Context, projectUUID string) (string, string, error) {
	resp, err := c.Get(ctx, "/api/v1/projects/"+projectUUID+"/kubeconfig")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var kc KubeconfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&kc); err != nil {
		return "", "", err
	}
	return kc.Kubeconfig, kc.Namespace, nil
}

// NewClientFromConfig creates a new API client using the stored configuration
func NewClientFromConfig() (*Client, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	opts := []ClientOption{}
	if cfg.APIURL != "" {
		opts = append(opts, WithBaseURL(cfg.APIURL))
	}
	if cfg.Token != "" {
		opts = append(opts, WithToken(cfg.Token))
	}

	return NewClient(opts...)
}

// DoRequestWithMethod allows making a request with a custom HTTP method (e.g., PATCH)
func (c *Client) DoRequestWithMethod(method, endpoint string, body interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequest(method, c.baseURL.String()+endpoint, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if c.teamUUID != "" {
		req.Header.Set("X-Team-UUID", c.teamUUID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error       string `json:"error"`
			Code        int    `json:"code"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(respBody, &errResp); err != nil {
			return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(respBody))
		}
		return nil, fmt.Errorf("request failed: %s (code: %d, description: %s)", errResp.Error, errResp.Code, errResp.Description)
	}

	return respBody, nil
}

// GetStackDeployDetailWithComponents fetches stackdeploy details by uuid, including components
func (c *Client) GetStackDeployDetailWithComponents(ctx context.Context, uuid string) (*StackDeployDetailWithComponents, error) {
	resp, err := c.Get(ctx, "/api/v1/stackdeploys/"+uuid)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var detail StackDeployDetailWithComponents
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// ListRegistries returns all registries for a given project
func (c *Client) ListRegistries(ctx context.Context, projectUUID string) ([]Registry, error) {
	resp, err := c.Get(ctx, "/api/v1/projects/"+projectUUID+"/registries")
	if err != nil {
		return nil, fmt.Errorf("failed to list registries: %w", err)
	}
	defer resp.Body.Close()

	var registries []Registry
	if err := json.NewDecoder(resp.Body).Decode(&registries); err != nil {
		return nil, fmt.Errorf("failed to decode registries response: %w", err)
	}

	return registries, nil
}

// AddRegistry creates a new registry for a given project
func (c *Client) AddRegistry(ctx context.Context, projectUUID, name, url, username, password string) error {
	type RegistryCreateRequest struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	body := RegistryCreateRequest{
		Name:     name,
		URL:      url,
		Username: username,
		Password: password,
	}
	endpoint := "/api/v1/projects/" + projectUUID + "/registries"
	_, err := c.Post(endpoint, body)
	if err != nil {
		return err
	}
	return nil
}

// DeleteRegistry deletes a registry by UUID for a given project
func (c *Client) DeleteRegistry(ctx context.Context, projectUUID, registryUUID string) error {
	endpoint := "/api/v1/projects/" + projectUUID + "/registries/" + registryUUID
	_, err := c.Delete(endpoint)
	if err != nil {
		return err
	}
	return nil
}

// DeleteApp deletes an app (stackdeploy) by UUID
func (c *Client) DeleteApp(ctx context.Context, appUUID string) error {
	endpoint := "/api/v1/stackdeploys/" + appUUID
	_, err := c.Delete(endpoint)
	if err != nil {
		return err
	}
	return nil
}
