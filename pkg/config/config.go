// Package config stores the CLI's configuration and credential in
// ~/.kubenest/config.json.
//
// The file holds the control-plane token, so it is written 0600 inside a 0700
// directory, and permissions are re-tightened on every save even if the file
// already existed looser.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

type Config struct {
	// ControlPlaneURL is the base URL of the KubeNest control plane,
	// e.g. https://api.your-domain.com.
	ControlPlaneURL string `json:"control_plane_url,omitempty"`
	// Token is the control-plane credential obtained by `kubenest login`.
	// Today this is the backend's bearer JWT; it becomes the revocable CLI
	// token when the control plane ships one (kn-odqp).
	Token     string `json:"token,omitempty"`
	UserEmail string `json:"email,omitempty"`

	// LegacyAPIURL is read (never written) so a config written by the
	// pre-platform CLI still logs in against the same control plane.
	LegacyAPIURL string `json:"api_url,omitempty"`
}

const (
	dirName  = ".kubenest"
	fileName = "config.json"

	dirMode  = 0o700
	fileMode = 0o600
)

// Path returns the config file path without creating anything.
func Path() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, dirName, fileName), nil
}

// Load reads the configuration. A missing file yields an empty config, not an
// error. A legacy api_url is folded into ControlPlaneURL.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return loadFrom(path)
}

func loadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.ControlPlaneURL == "" && cfg.LegacyAPIURL != "" {
		cfg.ControlPlaneURL = cfg.LegacyAPIURL
	}
	cfg.LegacyAPIURL = ""
	return &cfg, nil
}

// Save writes the configuration with owner-only permissions.
func Save(cfg *Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return saveTo(path, cfg)
}

func saveTo(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}

	out := *cfg
	out.LegacyAPIURL = ""
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, data, fileMode); err != nil {
		return err
	}
	// os.WriteFile does not change the mode of a pre-existing file, and the
	// directory may predate this CLI with looser permissions.
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
