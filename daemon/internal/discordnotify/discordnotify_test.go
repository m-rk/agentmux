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
