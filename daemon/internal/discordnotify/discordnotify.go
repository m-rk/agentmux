// Package discordnotify sends one-shot notification messages to a Discord
// incoming webhook, so agentmux can proactively reach the user (e.g. a
// Claude Code refresh token about to expire) instead of only being
// reachable by attaching to a session.
package discordnotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk shape of ~/.config/agentmux/discord.yaml.
type Config struct {
	WebhookURL    string              `yaml:"webhook_url"`
	Collaboration CollaborationConfig `yaml:"collaboration,omitempty"`
}

// CollaborationConfig is deliberately separate from the notification
// webhook above. Collaboration needs a bot credential to read a forum and a
// forum-owned webhook to write with per-session names and avatars.
type CollaborationConfig struct {
	BotToken          string            `yaml:"bot_token,omitempty"`
	WebhookURL        string            `yaml:"webhook_url,omitempty"`
	ForumChannelID    string            `yaml:"forum_channel_id,omitempty"`
	AgentAvatarURLs   map[string]string `yaml:"agent_avatar_urls,omitempty"`
	ProjectKeys       map[string]string `yaml:"project_keys,omitempty"`
	SessionAvatarURLs map[string]string `yaml:"session_avatar_urls,omitempty"`
	// AskMentionUserID is the one Discord user that `agentmux discord` may
	// @-mention, and whose replies are flagged when reading an ask.
	AskMentionUserID string `yaml:"ask_mention_user_id,omitempty"`
	// TestThreadID is the one reusable thread in the asks forum that task
	// sessions may write to. From a task session `discord post` becomes a
	// reply into this thread (never a new forum post) and reply/react/edit
	// work only here; close/tag/list are no-ops. Empty means task sessions
	// refuse every sending command. Stays in host config, never in a repo.
	TestThreadID string `yaml:"test_thread,omitempty"`
	// UnlockLog is a file `discord tag/close -unlock` appends one line to per
	// unlock (thread, who, time) — the autopilot log. Empty falls back to
	// the process log. Stays in host config.
	UnlockLog string `yaml:"unlock_log,omitempty"`
	// KindTags are the forum tag names that mark a thread as an ask (task)
	// thread: a thread carrying any one of them is an ask post, and `asks
	// tag` refuses a set with none of them. Unset (or empty) means
	// DefaultKindTags. A spike task's thread carries `spike` instead of
	// `task`.
	KindTags []string `yaml:"kind_tags,omitempty"`
}

// DefaultKindTags mark a thread as an ask (task) thread when kind_tags is
// unset in discord.yaml.
var DefaultKindTags = []string{"task", "epic", "idea", "spike"}

// KindTagNames returns the configured kind tags, or DefaultKindTags when
// kind_tags is unset.
func (c CollaborationConfig) KindTagNames() []string {
	if len(c.KindTags) == 0 {
		return DefaultKindTags
	}
	return c.KindTags
}

func (c CollaborationConfig) Configured() bool {
	return c.BotToken != "" && c.WebhookURL != "" && c.ForumChannelID != ""
}

// DefaultPath returns ~/.config/agentmux/discord.yaml.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return PathForHome(home)
}

// PathForHome returns the Discord config path for a particular user's home.
// The Linux doctor runs from the root daemon's systemd unit but invokes
// Claude as an instance owner, so it also needs to load that owner's webhook
// rather than root's unrelated configuration.
func PathForHome(home string) string {
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

var resolvedRefs sync.Map
var readOpRef = readOnePasswordRef

func readOnePasswordRef(ref string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "op", "read", ref)
	if token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN"); token != "" {
		cmd.Env = os.Environ()
	} else if home, err := os.UserHomeDir(); err == nil {
		if token, err := os.ReadFile(filepath.Join(home, ".config", "op", "service_account_token")); err == nil {
			cmd.Env = append(os.Environ(), "OP_SERVICE_ACCOUNT_TOKEN="+strings.TrimSpace(string(token)))
		}
	}
	return cmd.Output()
}

// LoadResolved loads the config and resolves scoped 1Password references in
// collaboration credentials. Resolved values are cached for this process.
func LoadResolved(path string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := ResolveCollaboration(&cfg.Collaboration); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ResolveCollaboration resolves credential references in place.
func ResolveCollaboration(c *CollaborationConfig) error {
	resolve := func(key string, value *string) error {
		if !strings.HasPrefix(*value, "op://") {
			return nil
		}
		if cached, ok := resolvedRefs.Load(*value); ok {
			*value = cached.(string)
			return nil
		}
		out, err := readOpRef(*value)
		if err != nil {
			return fmt.Errorf("resolving collaboration %s from 1Password (op read failed)", key)
		}
		secret := strings.TrimSpace(string(out))
		if secret == "" {
			return fmt.Errorf("resolving collaboration %s from 1Password: empty value", key)
		}
		resolvedRefs.Store(*value, secret)
		*value = secret
		return nil
	}
	if err := resolve("bot_token", &c.BotToken); err != nil {
		return err
	}
	if err := resolve("webhook_url", &c.WebhookURL); err != nil {
		return err
	}
	return nil
}

// MissingCollaborationKeys names the required settings without exposing values.
func (c CollaborationConfig) MissingKeys() []string {
	var missing []string
	if strings.TrimSpace(c.BotToken) == "" {
		missing = append(missing, "bot_token")
	}
	if strings.TrimSpace(c.WebhookURL) == "" {
		missing = append(missing, "webhook_url")
	}
	if strings.TrimSpace(c.ForumChannelID) == "" {
		missing = append(missing, "forum_channel_id")
	}
	return missing
}

// Save writes cfg to path with 0600 permissions. Unlike hosts.yaml (which
// holds no secrets), Discord webhook URLs and bot tokens are bearer
// credentials, so the file and its parent directory must not be
// group/world-readable.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("securing %s: %w", filepath.Dir(path), err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".discord-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temporary Discord config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("securing temporary Discord config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temporary Discord config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temporary Discord config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("saving %s: %w", path, err)
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
