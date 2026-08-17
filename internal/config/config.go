package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds jmon configuration
type Config struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// DefaultConfig returns default configuration
func DefaultConfig() *Config {
	return &Config{
		Username: "jmon",
		Password: "jmon",
	}
}

func configDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".jmon")
}

func configFile() string {
	return filepath.Join(configDir(), "config.json")
}

// Load loads config from ~/.jmon/config.json, creating it with defaults if missing
func Load() (*Config, error) {
	dir := configDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return DefaultConfig(), nil
	}

	path := configFile()
	data, err := os.ReadFile(path)
	if err != nil {
		// File doesn't exist, create with defaults
		cfg := DefaultConfig()
		_ = Save(cfg)
		return cfg, nil
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return DefaultConfig(), nil
	}
	return cfg, nil
}

// Save writes config to ~/.jmon/config.json
func Save(cfg *Config) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configFile(), data, 0644)
}
