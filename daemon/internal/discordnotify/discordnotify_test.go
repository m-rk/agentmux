package discordnotify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveSecuresConfigContainingBotToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "discord.yaml")
	cfg := &Config{
		WebhookURL: "https://discord.com/api/webhooks/notify/token",
		Collaboration: CollaborationConfig{
			BotToken:       "secret",
			WebhookURL:     "https://discord.com/api/webhooks/collab/token",
			ForumChannelID: "123",
		},
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WebhookURL != cfg.WebhookURL || loaded.Collaboration.BotToken != "secret" {
		t.Fatalf("round trip = %#v", loaded)
	}
}

func TestLoadResolvedKeepsReferencesOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discord.yaml")
	content := "collaboration:\n  bot_token: op://vault/item/token\n  webhook_url: op://vault/item/webhook\n  forum_channel_id: forum\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := readOpRef
	readOpRef = func(ref string) ([]byte, error) {
		if ref == "op://vault/item/token" {
			return []byte("private-token"), nil
		}
		if ref == "op://vault/item/webhook" {
			return []byte("https://discord.com/api/webhooks/123/secret"), nil
		}
		t.Fatalf("unexpected reference %q", ref)
		return nil, nil
	}
	t.Cleanup(func() { readOpRef = old })
	cfg, err := LoadResolved(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Collaboration.BotToken != "private-token" {
		t.Fatal("bot token was not resolved")
	}
	if cfg.Collaboration.WebhookURL == "op://vault/item/webhook" {
		t.Fatal("webhook was not resolved")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-token") || !strings.Contains(string(data), "op://vault/item/token") {
		t.Fatal("config did not retain refs safely")
	}
}

func TestMissingCollaborationKeys(t *testing.T) {
	got := (CollaborationConfig{}).MissingKeys()
	if strings.Join(got, ",") != "bot_token,webhook_url,forum_channel_id" {
		t.Fatalf("MissingKeys() = %v", got)
	}
}

func TestKindTagsDefaultAndOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "discord.yaml")
	var cfg Config
	if err := Save(path, &cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Collaboration.KindTagNames(); len(got) != 4 || got[3] != "spike" {
		t.Fatalf("default kind tags = %#v", got)
	}
	loaded.Collaboration.KindTags = []string{"spike"}
	if err := Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Collaboration.KindTagNames(); len(got) != 1 || got[0] != "spike" {
		t.Fatalf("override kind tags = %#v", got)
	}
}
