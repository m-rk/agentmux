// Package hostsconfig loads the TUI's list of known agentmuxd hosts from
// ~/.config/agentmux/hosts.yaml, so the TUI can connect to more than one
// daemon (phase 2: multi-host over Tailscale) instead of just the local
// Unix socket (phase 1).
package hostsconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Host is one entry in hosts.yaml. Address is a dial target:
//   - "unix:///run/agentmux/agentmuxd.sock" for a local daemon
//   - "tcp://100.x.y.z:4287" for a daemon reachable over Tailscale
type Host struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
}

type Config struct {
	Hosts []Host `yaml:"hosts"`
}

// DefaultPath returns ~/.config/agentmux/hosts.yaml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agentmux", "hosts.yaml")
}

// Load reads and parses the hosts.yaml at path. A missing file is not an
// error: callers should fall back to a single local host in that case
// (check os.IsNotExist on the returned error).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for i, h := range cfg.Hosts {
		if h.Name == "" {
			return nil, fmt.Errorf("%s: host %d is missing a name", path, i)
		}
		if h.Address == "" {
			return nil, fmt.Errorf("%s: host %q is missing an address", path, h.Name)
		}
	}
	return &cfg, nil
}
