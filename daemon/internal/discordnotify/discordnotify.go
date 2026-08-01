// Package discordnotify sends one-shot notification messages to a Discord
// incoming webhook, so agentmux can proactively reach the user (e.g. a
// Claude Code refresh token about to expire) instead of only being
// reachable by attaching to a session.
package discordnotify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk shape of ~/.config/agentmux/discord.yaml.
type Config struct {
	WebhookURL string `yaml:"webhook_url"`
}

// DefaultPath returns ~/.config/agentmux/discord.yaml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "agentmux", "discord.yaml")
}

// Load reads path. A missing file is not an error — it just means
// notifications aren't configured yet, matching hostsconfig.Load's
// missing-file convention. Callers should treat a blank WebhookURL as "not
// configured."
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes cfg to path with 0600 permissions. Unlike hosts.yaml (which
// holds no secrets), the webhook URL is itself a bearer credential — anyone
// who has it can post messages as this webhook — so the file and its parent
// directory must not be group/world-readable.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// httpClient has a bounded timeout since Send can run inside a Type=oneshot
// systemd unit with its own TimeoutStartSec — an unbounded POST could wedge
// the periodic tick that calls it.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// Send posts message to webhookURL as a Discord incoming-webhook message
// (the {"content": "..."} shape Discord's webhook API expects).
func Send(webhookURL, message string) error {
	if webhookURL == "" {
		return fmt.Errorf("no Discord webhook URL configured")
	}
	body, err := json.Marshal(map[string]string{"content": message})
	if err != nil {
		return err
	}
	resp, err := httpClient.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("posting to Discord webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("Discord webhook returned %s", resp.Status)
	}
	return nil
}
