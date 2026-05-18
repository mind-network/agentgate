// Package localconfig manages LP configuration under ~/.aicg/.
package localconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Dir is the LP config directory.
const Dir = ".aicg"

// Credentials holds the authentication material stored in ~/.aicg/credentials.
type Credentials struct {
	GwURL   string `yaml:"gateway_url"`
	UserID  string `yaml:"user_id"`
	APIKey  string `yaml:"api_key"`
	TeamID  string `yaml:"team_id,omitempty"`
}

// Config holds local LP settings from ~/.aicg/config.yaml.
type Config struct {
	Port      int    `yaml:"port"`
	GwURL     string `yaml:"gateway_url"`
	LogLevel  string `yaml:"log_level"`
	CAPath    string `yaml:"ca_path,omitempty"`
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Port:     7777,
		LogLevel: "info",
	}
}

// HomeDir resolves the LP config directory path.
func HomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, Dir), nil
}

// LoadCredentials reads ~/.aicg/credentials.
func LoadCredentials() (*Credentials, error) {
	dir, err := HomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "credentials")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w (run `aicg login` first)", err)
	}
	var c Credentials
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("no api_key in credentials")
	}
	return &c, nil
}

// SaveCredentials writes credentials to ~/.aicg/credentials (mode 0600).
func SaveCredentials(c *Credentials) error {
	dir, err := HomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "credentials"), data, 0600)
}

// LoadConfig reads ~/.aicg/config.yaml.
func LoadConfig() (*Config, error) {
	dir, err := HomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Port == 0 {
		c.Port = 7777
	}
	return &c, nil
}

// SaveConfig writes the config to ~/.aicg/config.yaml.
func SaveConfig(c *Config) error {
	dir, err := HomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0600)
}
