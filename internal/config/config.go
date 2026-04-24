// Package config loads Nucleus gateway settings from TOML.
//
// In M2 there are no required settings — the file is optional. Keeping the
// package in place gives later milestones a single home for gateway-wide
// options (log level, idle reaper timeout, workspace resolution rules, ...)
// without another package shuffle.
//
// Credentials and profiles are NOT stored here. See:
//   - internal/registry (profiles)
//   - internal/vault (credentials)
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Config is the top-level gateway config. All fields are optional.
type Config struct {
	// Reserved for M3+ — e.g. LogLevel, IdleTimeoutSec, WorkspaceRules, ...
}

// DefaultPath returns ~/.nucleusmcp/config.toml.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".nucleusmcp", "config.toml"), nil
}

// Load reads and parses the config file at path. If the file does not
// exist, returns a zero-value Config and no error — the file is optional.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}
