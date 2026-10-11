package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
)

func setupFakeOp(t *testing.T, home string) {
	t.Helper()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bin, "op")
	script := "#!/bin/sh\ncase \"$2\" in\n  */token) printf 'synthetic-bot-token\\n' ;;\n  */webhook) printf 'https://discord.com/api/webhooks/123/synthetic-webhook-token\\n' ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCollabSetupStoresRefsWithoutCollaborationBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setupFakeOp(t, home)
	path := discordnotify.PathForHome(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("webhook_url: https://discord.com/api/webhooks/notice/token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldValidate := validateCollaborationSetup
	validateCollaborationSetup = func(cfg discordnotify.CollaborationConfig) error {
		if cfg.BotToken != "synthetic-bot-token" || !strings.Contains(cfg.WebhookURL, "synthetic-webhook-token") {
			t.Fatalf("validation did not receive resolved credentials")
		}
		if cfg.ForumChannelID != "shared-forum" {
			t.Fatalf("forum channel = %q", cfg.ForumChannelID)
		}
		return nil
	}
	t.Cleanup(func() { validateCollaborationSetup = oldValidate })

	output, err := captureStdout(t, func() error {
		return runCollabSetup([]string{"-y", "-bot-token", "op://vault/item/token", "-webhook-url", "op://vault/item/webhook", "-forum-channel", "shared-forum"})
	})
	if err != nil {
		t.Fatalf("runCollabSetup: %v", err)
	}
	if strings.Contains(output, "synthetic-bot-token") || strings.Contains(output, "synthetic-webhook-token") {
		t.Fatalf("setup output exposed a credential: %q", output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"op://vault/item/token", "op://vault/item/webhook", "forum_channel_id: shared-forum"} {
		if !strings.Contains(text, want) {
			t.Fatalf("saved config missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "synthetic-bot-token") || strings.Contains(text, "synthetic-webhook-token") {
		t.Fatalf("saved config exposed a credential: %s", text)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
}

func TestAsksClientResolvesCredentialRefs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setupFakeOp(t, home)
	path := discordnotify.PathForHome(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "collaboration:\n  bot_token: op://vault/item/token\n  webhook_url: op://vault/item/webhook\n  forum_channel_id: shared-forum\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := asksClient()
	if err != nil {
		t.Fatal(err)
	}
	if client.Config.BotToken != "synthetic-bot-token" || !strings.Contains(client.Config.WebhookURL, "synthetic-webhook-token") {
		t.Fatal("asksClient did not resolve credential refs")
	}
}

func TestAsksClientNamesMissingKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := asksClient()
	if err == nil {
		t.Fatal("asksClient unexpectedly succeeded")
	}
	for _, key := range []string{"bot_token", "webhook_url", "forum_channel_id", "agentmux collab setup"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q missing %q", err, key)
		}
	}
}
