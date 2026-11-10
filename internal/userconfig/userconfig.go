// Package userconfig persists SV Multi-Agent CLI settings in the user's config
// directory (like opencode does), e.g.:
//
//	Windows: %AppData%\svma\config.json
//	Linux:   ~/.config/svma/config.json
//	macOS:   ~/Library/Application Support/svma/config.json
//
// Environment variables still take precedence over these values.
package userconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LLM holds the OpenAI-compatible provider settings collected by the wizard.
type LLM struct {
	Provider string `json:"provider,omitempty"` // ollama | lmstudio | opencode | custom | mock
	Mode     string `json:"mode,omitempty"`     // mock | openai-compatible
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Config is the persisted user configuration.
type Config struct {
	LLM  LLM    `json:"llm"`
	Addr string `json:"addr,omitempty"`
}

// Dir returns the per-user configuration directory for svma.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(base, "svma"), nil
}

// Path returns the absolute path of the user config file.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the user config. A missing file is reported as os.ErrNotExist.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the user config with restrictive permissions and returns its path.
func Save(cfg Config) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// Exists reports whether a user config file is present.
func Exists() bool {
	path, err := Path()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
