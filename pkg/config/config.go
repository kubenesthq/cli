package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

type Config struct {
	APIURL     string `json:"api_url"`
	Token      string `json:"token"`
	TeamUUID   string `json:"team_uuid"`
	ConfigPath string `json:"-"`
}

var (
	config *Config
)

func Init() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	configPath := filepath.Join(home, ".kubenest")
	if err := os.MkdirAll(configPath, 0755); err != nil {
		return err
	}

	viper.SetConfigName("config")
	viper.SetConfigType("json")
	viper.AddConfigPath(configPath)

	config = &Config{
		APIURL:     "http://localhost:3000", // Default API URL
		ConfigPath: configPath,
	}

	if err := viper.ReadInConfig(); err == nil {
		if err := viper.Unmarshal(config); err != nil {
			return err
		}
	}

	return nil
}

func GetConfig() *Config {
	return config
}

func SaveConfig() error {
	configFile := filepath.Join(config.ConfigPath, "config.json")
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configFile, data, 0600)
}

func SetToken(token string) {
	config.Token = token
	SaveConfig()
}

func SetTeamUUID(teamUUID string) {
	config.TeamUUID = teamUUID
	SaveConfig()
}

func ClearToken() {
	config.Token = ""
	config.TeamUUID = ""
	SaveConfig()
}
