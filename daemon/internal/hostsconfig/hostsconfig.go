// Package hostsconfig loads the TUI's list of known agentmuxd hosts from
// ~/.config/agentmux/hosts.yaml, so the TUI can connect to more than one
// daemon (phase 2: multi-host over Tailscale) instead of just the local
// Unix socket (phase 1).
package hostsconfig

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/m-rk/agentmux/daemon/internal/address"
	"gopkg.in/yaml.v3"
)

// Host is one entry in hosts.yaml. Address is a dial target:
//   - "unix:///run/agentmux/agentmuxd.sock" for a local daemon
//   - "tcp://100.x.y.z:4287" for a daemon reachable over Tailscale
//
// Gateway, if set, is the base URL of that host's gateway, e.g.
// "http://100.x.y.z:4288". `agentmux sessions` uses it to reach the host's
// sessions; plain HTTP is fine on a tailnet, which encrypts the link and
// supplies the caller's identity.
type Host struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
	Gateway string `yaml:"gateway"`
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
		if h.Gateway != "" {
			if err := CheckGatewayURL(h.Gateway); err != nil {
				return nil, fmt.Errorf("%s: host %q: %w", path, h.Name, err)
			}
		}
	}
	return &cfg, nil
}

// CheckGatewayURL requires an http or https URL with a host and nothing else
// (no path, query, fragment or userinfo).
func CheckGatewayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("gateway %q: %v", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("gateway %q: want http://host:port or https://host:port", raw)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("gateway %q: must be a bare base URL with no path, query or credentials", raw)
	}
	return nil
}

// CheckUnique fails when two hosts share a name once canonicalized for
// addresses ("local" becomes this machine's host name, case is folded), since
// <instance>@<host> would then name two sessions.
func CheckUnique(hosts []Host) error {
	seen := map[string]string{}
	for _, h := range hosts {
		c := address.Canonical(h.Name)
		if prev, ok := seen[c]; ok {
			return fmt.Errorf("hosts %q and %q both resolve to host name %q; addresses would be ambiguous", prev, h.Name, c)
		}
		seen[c] = h.Name
	}
	return nil
}
