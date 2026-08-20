package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Credentials is the CLI token store: ~/.kubenest/credentials.json, keyed by
// control-plane URL as the contract specifies (openapi.yaml CliTokenCreated).
// The plaintext token is returned by the control plane exactly once, so this
// file is the only copy — 0600 in a 0700 dir, enforced on every save.
type Credentials struct {
	Tokens map[string]StoredCredential `json:"tokens"`
}

type StoredCredential struct {
	// Token is the knp_* CLI token.
	Token string `json:"token"`
}

const credentialsFile = "credentials.json"

// CredentialsPath returns the credentials file path without creating anything.
func CredentialsPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, dirName, credentialsFile), nil
}

// normalizeKey canonicalizes a control-plane URL for use as a store key.
func normalizeKey(controlPlane string) string {
	return strings.TrimRight(strings.TrimSpace(controlPlane), "/")
}

// LoadCredentials reads the store; a missing file is an empty store.
func LoadCredentials() (*Credentials, error) {
	path, err := CredentialsPath()
	if err != nil {
		return nil, err
	}
	return loadCredentialsFrom(path)
}

func loadCredentialsFrom(path string) (*Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Credentials{Tokens: map[string]StoredCredential{}}, nil
		}
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Tokens == nil {
		c.Tokens = map[string]StoredCredential{}
	}
	return &c, nil
}

// TokenFor returns the stored token for a control plane, or "".
func (c *Credentials) TokenFor(controlPlane string) string {
	return c.Tokens[normalizeKey(controlPlane)].Token
}

// Set stores a token for a control plane.
func (c *Credentials) Set(controlPlane, token string) {
	if c.Tokens == nil {
		c.Tokens = map[string]StoredCredential{}
	}
	c.Tokens[normalizeKey(controlPlane)] = StoredCredential{Token: token}
}

// Delete removes the token for a control plane.
func (c *Credentials) Delete(controlPlane string) {
	delete(c.Tokens, normalizeKey(controlPlane))
}

// SaveCredentials writes the store with owner-only permissions.
func SaveCredentials(c *Credentials) error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	return saveCredentialsTo(path, c)
}

func saveCredentialsTo(path string, c *Credentials) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, fileMode); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, fileMode); err != nil {
			return err
		}
		if err := os.Chmod(dir, dirMode); err != nil {
			return err
		}
	}
	return nil
}
