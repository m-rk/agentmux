// Package retention loads the host-local gc retention setting from
// ~/.config/agentmux/retention.yaml. It says how long a retired task
// session's leftovers (archived amp threads, stored opencode sessions)
// are kept before `agentmux gc` deletes them. The file is host-local on
// purpose: retention is a per-machine housekeeping choice, not repo
// policy. Docs and tests use small placeholder values — never anything
// identifying a real host.
package retention

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultDays is the retention when no file exists: retired sessions are
// kept two weeks before gc deletes their threads.
const DefaultDays = 14

// configFile is the file name under ~/.config/agentmux.
const configFile = "retention.yaml"

// Config is the parsed retention.yaml.
type Config struct {
	// RetentionDays bounds how long a retired session waits for gc.
	RetentionDays int `yaml:"retention_days"`
}

// DefaultPath returns ~/.config/agentmux/retention.yaml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agentmux", configFile)
}

// Load reads and parses the file at path. A missing file is not an error:
// it means the default retention applies. A present file must name a
// positive retention_days — a zero or negative value almost always means
// somebody commented out the value and left the key, and silently keeping
// everything (or deleting immediately) would hide that mistake.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{RetentionDays: DefaultDays}, nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var raw Config
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if raw.RetentionDays <= 0 {
		return Config{}, fmt.Errorf("parsing %s: retention_days must be positive", path)
	}
	return raw, nil
}
