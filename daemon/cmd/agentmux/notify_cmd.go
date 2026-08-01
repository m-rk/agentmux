package main

import (
	"flag"
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
		runNotifyDiscordSetup(args[2:])
	case "test":
		runNotifyDiscordTest()
	default:
		fmt.Fprintf(os.Stderr, "unknown notify discord subcommand %q\n", args[1])
		os.Exit(1)
	}
}

// isDiscordWebhookURL is the same shape check the interactive form's
// Validate uses — shared so -y scripting gets the same guard against an
// obviously-wrong value (e.g. a pasted channel link instead of a webhook).
func isDiscordWebhookURL(s string) bool {
	return strings.HasPrefix(s, "https://discord.com/api/webhooks/") || strings.HasPrefix(s, "https://discordapp.com/api/webhooks/")
}

// runNotifyDiscordSetup handles both the interactive form (see
// runDiscordSetupForm) and -y non-interactive scripting (-webhook-url),
// e.g. for provisioning agentmux unattended.
func runNotifyDiscordSetup(args []string) {
	fs := flag.NewFlagSet("notify discord setup", flag.ExitOnError)
	nonInteractive := fs.Bool("y", false, "skip the interactive form; save -webhook-url directly")
	webhookURL := fs.String("webhook-url", "", "-y only: the Discord incoming-webhook URL")
	fs.Parse(args)

	if *nonInteractive {
		if *webhookURL == "" {
			fmt.Fprintln(os.Stderr, "notify discord setup -y: -webhook-url is required")
			os.Exit(1)
		}
		if !isDiscordWebhookURL(*webhookURL) {
			fmt.Fprintln(os.Stderr, "notify discord setup -y: -webhook-url doesn't look like a Discord webhook URL")
			os.Exit(1)
		}
		if err := discordnotify.Send(*webhookURL, "✅ agentmux: Discord notifications configured."); err != nil {
			fmt.Fprintf(os.Stderr, "notify discord setup: test message failed, not saving: %v\n", err)
			os.Exit(1)
		}
		path := discordnotify.DefaultPath()
		if err := discordnotify.Save(path, &discordnotify.Config{WebhookURL: *webhookURL}); err != nil {
			fmt.Fprintf(os.Stderr, "notify discord setup: saving config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Test message sent and webhook saved to %s\n", path)
		return
	}

	if _, err := runDiscordSetupForm(); err != nil {
		fmt.Fprintf(os.Stderr, "notify discord setup: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Test message sent and webhook saved to %s\n", discordnotify.DefaultPath())
}

// runDiscordSetupForm prompts for a Discord incoming-webhook URL, sends a
// test message to confirm it actually works before saving anything, then
// writes it to disk. See Discord's own docs for creating one: Server
// Settings -> Integrations -> Webhooks -> New Webhook -> Copy Webhook URL.
// Shared by the standalone CLI command, the `new` wizard's optional
// onboarding step, and the TUI's own "D" key action — none of which should
// os.Exit on failure, so this returns an error instead.
func runDiscordSetupForm() (string, error) {
	path := discordnotify.DefaultPath()
	existing, err := discordnotify.Load(path)
	if err != nil {
		return "", fmt.Errorf("reading existing config: %w", err)
	}

	webhookURL := existing.WebhookURL
	field := huh.NewInput().
		Title("Discord webhook URL").
		Description("Server Settings -> Integrations -> Webhooks -> New Webhook -> Copy Webhook URL").
		Value(&webhookURL).
		Validate(func(s string) error {
			if !isDiscordWebhookURL(s) {
				return fmt.Errorf("doesn't look like a Discord webhook URL")
			}
			return nil
		})
	if err := huh.NewForm(huh.NewGroup(field)).Run(); err != nil {
		return "", err
	}

	if err := discordnotify.Send(webhookURL, "✅ agentmux: Discord notifications configured."); err != nil {
		return "", fmt.Errorf("test message failed, not saving: %w", err)
	}
	if err := discordnotify.Save(path, &discordnotify.Config{WebhookURL: webhookURL}); err != nil {
		return "", fmt.Errorf("saving config: %w", err)
	}
	return webhookURL, nil
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
