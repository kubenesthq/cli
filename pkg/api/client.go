package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"kubenest.io/cli/pkg/config"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
	teamUUID   string
}

func NewClient() *Client {
	return &Client{
		baseURL:    config.GetConfig().APIURL,
		httpClient: &http.Client{},
	}
}

func (c *Client) SetToken(token string) {
	c.token = token
}

func (c *Client) SetTeamUUID(teamUUID string) {
	c.teamUUID = teamUUID
}

func (c *Client) doRequest(method, path string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.teamUUID != "" {
		req.Header.Set("X-Team-UUID", c.teamUUID)
	}

	return c.httpClient.Do(req)
}

// Login authenticates with the backend
func (c *Client) Login(email, password string) (*LoginResponse, error) {
	type loginRequest struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	resp, err := c.doRequest("POST", "/api/v1/auth/login", loginRequest{
		Email:    email,
		Password: password,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("login failed: %s", resp.Status)
	}

	var result LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

// ListTeams returns all teams for the authenticated user
func (c *Client) ListTeams() ([]Team, error) {
	resp, err := c.doRequest("GET", "/api/v1/teams", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var teams []Team
	if err := json.NewDecoder(resp.Body).Decode(&teams); err != nil {
		return nil, err
	}

	return teams, nil
}

// ListClusters returns all clusters for the current team
func (c *Client) ListClusters() ([]Cluster, error) {
	resp, err := c.doRequest("GET", "/api/v1/clusters", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var clusters []Cluster
	if err := json.NewDecoder(resp.Body).Decode(&clusters); err != nil {
		return nil, err
	}

	return clusters, nil
}

// ListProjects returns all projects for the current team
func (c *Client) ListProjects() ([]Project, error) {
	resp, err := c.doRequest("GET", "/api/v1/projects", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var projects []Project
	if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
		return nil, err
	}

	return projects, nil
}

// ListStackDeploys returns all stackdeploys for the current team
func (c *Client) ListStackDeploys() ([]StackDeploy, error) {
	resp, err := c.doRequest("GET", "/api/v1/stackdeploys", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var stackdeploys []StackDeploy
	if err := json.NewDecoder(resp.Body).Decode(&stackdeploys); err != nil {
		return nil, err
	}

	return stackdeploys, nil
}

// ListApps returns all deployed applications
func (c *Client) ListApps() ([]App, error) {
	resp, err := c.doRequest("GET", "/api/v1/apps", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var apps []App
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return nil, err
	}

	return apps, nil
}

// GetLogs retrieves application logs
func (c *Client) GetLogs(appID string) (io.ReadCloser, error) {
	resp, err := c.doRequest("GET", fmt.Sprintf("/api/v1/apps/%s/logs", appID), nil)
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

	resp, err := c.doRequest("POST", fmt.Sprintf("/api/v1/apps/%s/pods/%s/exec", appID, podName), execRequest{
		Command: command,
	})
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

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/apps/%s/pods/%s/copy", c.baseURL, appID, podName), file)
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
	resp, err := c.doRequest("GET", fmt.Sprintf("/api/v1/apps/%s/pods", appID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var pods []Pod
	if err := json.NewDecoder(resp.Body).Decode(&pods); err != nil {
		return nil, err
	}

	return pods, nil
}
