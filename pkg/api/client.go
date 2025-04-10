package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/kubenesthq/cli/pkg/config"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
	token      string
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

	return c.httpClient.Do(req)
}

// Login authenticates with the backend
func (c *Client) Login(username, password string) (string, error) {
	type loginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	resp, err := c.doRequest("POST", "/auth/login", loginRequest{
		Username: username,
		Password: password,
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login failed: %s", resp.Status)
	}

	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.Token, nil
}

// ListApps returns all deployed applications
func (c *Client) ListApps() ([]App, error) {
	resp, err := c.doRequest("GET", "/apps", nil)
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

// DeployApp deploys a new application
func (c *Client) DeployApp(appConfig AppConfig) error {
	resp, err := c.doRequest("POST", "/apps", appConfig)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("deployment failed: %s", resp.Status)
	}

	return nil
}

// GetLogs retrieves application logs
func (c *Client) GetLogs(appID string) (io.ReadCloser, error) {
	resp, err := c.doRequest("GET", fmt.Sprintf("/apps/%s/logs", appID), nil)
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

	resp, err := c.doRequest("POST", fmt.Sprintf("/apps/%s/pods/%s/exec", appID, podName), execRequest{
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

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/apps/%s/pods/%s/copy", c.baseURL, appID, podName), file)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
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
