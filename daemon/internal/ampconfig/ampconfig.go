// Package ampconfig loads the host-local amp settings from
// ~/.config/agentmux/amp.yaml. Only one setting exists: mode, the amp
// agent mode (`-m/--mode`) every amp thread started by `sessions run` uses.
// The file is host-local on purpose: the mode names a model/provider
// choice that belongs to the machine, not the repo. Docs and tests use
// placeholders such as "high" — never put a real user's mode name in the
// repo.
package ampconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvOverride is the per-instance registry value that overrides the host
// config: AGENTMUX_AMP_MODE, set by `agentmux new -amp-mode`.
const EnvOverride = "AGENTMUX_AMP_MODE"

// configFile is the file name under ~/.config/agentmux.
const configFile = "amp.yaml"

// Config is the parsed amp.yaml.
type Config struct {
	// Mode is the amp agent mode key or label passed to `amp -m`, e.g.
	// "high" or a plugin mode's key or label. Empty means no file or no
	// mode set: callers run amp without -m.
	Mode string
}

// DefaultPath returns ~/.config/agentmux/amp.yaml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agentmux", configFile)
}

// Load reads and parses the file at path. A missing file is not an error:
// it means no host default is configured. An empty mode is refused the
// same way an absent file is — "mode:" with nothing after it almost
// always means somebody commented out the value and left the key, and
// silently running amp without -m would hide that mistake.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var raw struct {
		Mode string `yaml:"mode"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	mode := strings.TrimSpace(raw.Mode)
	if mode == "" {
		return Config{}, fmt.Errorf("parsing %s: mode is empty", path)
	}
	return Config{Mode: mode}, nil
}

// Resolve is the effective mode for an instance: the instance's
// AGENTMUX_AMP_MODE registry value wins, otherwise the host config.
// It returns where the value came from for `sessions status -json`:
// "instance", "host", or "" when no mode is configured anywhere.
func Resolve(host Config, instanceMode string) (mode, source string) {
	if m := strings.TrimSpace(instanceMode); m != "" {
		return m, "instance"
	}
	if host.Mode != "" {
		return host.Mode, "host"
	}
	return "", ""
}
