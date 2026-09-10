package discordnotify

import (
	"os"
	"path/filepath"
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
