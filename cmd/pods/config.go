package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// defaultEndpoint is the hosted Happy Pods instance the CLI talks to when no
// endpoint is configured, so `pods login` just works out of the box.
const defaultEndpoint = "https://podbay.dev"

// config is the persisted CLI configuration (~/.config/pods/config.json).
// Secret is kept as the JSON field name for compatibility; it stores an API token.
type config struct {
	Endpoint string `json:"endpoint"`
	Secret   string `json:"secret"`
}

// configPath returns the path of the config file, ~/.config/pods/config.json.
func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".config", "pods", "config.json"), nil
}

// loadConfigFile reads the config file at path. A missing file is not an
// error; it yields the zero config.
func loadConfigFile(path string) (config, error) {
	var cfg config
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// saveConfigFile writes cfg to path with 0600 permissions, creating parent
// directories as needed.
func saveConfigFile(path string, cfg config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	// os.WriteFile does not change the mode of a pre-existing file.
	return os.Chmod(path, 0o600)
}

// resolveConfig applies the configuration precedence: flags beat the
// PODS_ENDPOINT/PODS_TOKEN/PODS_SECRET environment variables, which beat the config
// file. Endpoint and secret are resolved independently.
func resolveConfig(flagEndpoint, flagSecret string, getenv func(string) string, file config) config {
	cfg := file
	if v := getenv("PODS_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if v := getenv("PODS_SECRET"); v != "" {
		cfg.Secret = v
	}
	if v := getenv("PODS_TOKEN"); v != "" {
		cfg.Secret = v
	}
	if flagEndpoint != "" {
		cfg.Endpoint = flagEndpoint
	}
	if flagSecret != "" {
		cfg.Secret = flagSecret
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	return cfg
}
