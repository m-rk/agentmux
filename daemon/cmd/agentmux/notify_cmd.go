package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
)

// runNotifyCmd is `agentmux notify discord <setup|test>`: configures (or
// tests) the Discord webhook agentmux uses to reach the user proactively
// (e.g. when a Claude Code instance's refresh token is about to expire —
// see ensureClaudeAuthNotified in daemon/internal/session/claudecode.go).
func runNotifyCmd(args []string) {
	if len(args) < 2 || args[0] != "discord" {
		fmt.Fprintln(os.Stderr, "usage: agentmux notify discord <setup|test>")
		os.Exit(1)
	}
	switch args[1] {
	case "setup":
		runNotifyDiscordSetup()
	case "test":
		runNotifyDiscordTest()
	default:
		fmt.Fprintf(os.Stderr, "unknown notify discord subcommand %q\n", args[1])
		os.Exit(1)
	}
}

// runNotifyDiscordSetup prompts for a Discord incoming-webhook URL, sends a
// test message to confirm it actually works before saving anything, then
// writes it to disk. See Discord's own docs for creating one: Server
// Settings -> Integrations -> Webhooks -> New Webhook -> Copy Webhook URL.
func runNotifyDiscordSetup() {
	path := discordnotify.DefaultPath()
	existing, err := discordnotify.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "notify discord setup: reading existing config: %v\n", err)
		os.Exit(1)
	}

	webhookURL := existing.WebhookURL
	field := huh.NewInput().
		Title("Discord webhook URL").
		Description("Server Settings -> Integrations -> Webhooks -> New Webhook -> Copy Webhook URL").
		Value(&webhookURL).
		Validate(func(s string) error {
			if !strings.HasPrefix(s, "https://discord.com/api/webhooks/") && !strings.HasPrefix(s, "https://discordapp.com/api/webhooks/") {
				return fmt.Errorf("doesn't look like a Discord webhook URL")
			}
			return nil
		})
	if err := huh.NewForm(huh.NewGroup(field)).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "notify discord setup: %v\n", err)
		os.Exit(1)
	}

	if err := discordnotify.Send(webhookURL, "✅ agentmux: Discord notifications configured."); err != nil {
		fmt.Fprintf(os.Stderr, "notify discord setup: test message failed, not saving: %v\n", err)
		os.Exit(1)
	}

	if err := discordnotify.Save(path, &discordnotify.Config{WebhookURL: webhookURL}); err != nil {
		fmt.Fprintf(os.Stderr, "notify discord setup: saving config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Test message sent and webhook saved to %s\n", path)
}

// runNotifyDiscordTest resends a test message using the already-saved
// config, without going through the setup form — useful for confirming
// notifications still work after e.g. rotating the webhook in Discord.
func runNotifyDiscordTest() {
	cfg, err := discordnotify.Load(discordnotify.DefaultPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "notify discord test: %v\n", err)
		os.Exit(1)
	}
	if cfg.WebhookURL == "" {
		fmt.Fprintln(os.Stderr, "notify discord test: no webhook configured yet; run 'agentmux notify discord setup' first")
		os.Exit(1)
	}
	if err := discordnotify.Send(cfg.WebhookURL, "✅ agentmux: test notification."); err != nil {
		fmt.Fprintf(os.Stderr, "notify discord test: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Test message sent.")
}
