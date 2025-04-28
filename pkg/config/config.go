package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	APIURL      string `json:"api_url"`
	Token       string `json:"token"`
	TeamUUID    string `json:"team_uuid"`
	ClusterUUID string `json:"cluster_uuid,omitempty"`
	ProjectUUID string `json:"project_uuid,omitempty"`
}

const (
	configDir  = ".kubenest"
	configFile = "config.json"
)

// GetConfigPath returns the path to the config file
func GetConfigPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	configDirPath := filepath.Join(homeDir, configDir)
	if err := os.MkdirAll(configDirPath, 0755); err != nil {
		return "", err
	}

	return filepath.Join(configDirPath, configFile), nil
}

// LoadConfig loads the configuration from disk
func LoadConfig() (*Config, error) {
	configPath, err := GetConfigPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveConfig saves the configuration to disk
func SaveConfig(config *Config) error {
	configPath, err := GetConfigPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0644)
}
