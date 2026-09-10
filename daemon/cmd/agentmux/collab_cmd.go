package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/huh"

	"github.com/m-rk/agentmux/daemon/internal/collab"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

func runCollabCmd(args []string) {
	if len(args) == 0 {
		collabUsage()
		os.Exit(1)
	}
	var err error
	switch args[0] {
	case "setup":
		err = runCollabSetup(args[1:])
	case "configure":
		err = runCollabConfigure(args[1:])
	case "read":
		err = runCollabRead(args[1:])
	case "post":
		err = runCollabPost(args[1:])
	default:
		collabUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "collab %s: %v\n", args[0], err)
		os.Exit(1)
	}
}

func collabUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentmux collab setup [-y -bot-token TOKEN -webhook-url URL -forum-channel ID]
  agentmux collab configure -instance NAME [-project KEY] [-avatar-url URL]
  agentmux collab read -instance NAME [-thread ID]
  agentmux collab post -instance NAME -topic TOPIC -summary SENTENCE [-shared] [-details FILE.md]
  agentmux collab post -instance NAME -thread ID -summary TEXT [-details FILE.md]`)
}

type avatarFlags map[string]string

func (a avatarFlags) String() string { return "AGENT=https://…" }

func (a avatarFlags) Set(value string) error {
	agent, avatar, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(agent) == "" {
		return fmt.Errorf("agent avatar must be AGENT=https://…")
	}
	if err := collab.ValidateAvatarURL(strings.TrimSpace(avatar)); err != nil {
		return err
	}
	a[strings.TrimSpace(agent)] = strings.TrimSpace(avatar)
	return nil
}

func runCollabSetup(args []string) error {
	fs := flag.NewFlagSet("collab setup", flag.ContinueOnError)
	nonInteractive := fs.Bool("y", false, "skip the interactive form")
	botToken := fs.String("bot-token", "", "Discord bot token used to read the forum")
	webhookURL := fs.String("webhook-url", "", "Discord forum webhook URL used to write")
	forumID := fs.String("forum-channel", "", "Discord forum channel ID")
	avatars := avatarFlags{}
	fs.Var(avatars, "agent-avatar", "fallback avatar as AGENT=https://…; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := discordnotify.DefaultPath()
	cfg, err := discordnotify.Load(path)
	if err != nil {
		return err
	}
	if !*nonInteractive {
		if *botToken == "" {
			*botToken = cfg.Collaboration.BotToken
		}
		if *webhookURL == "" {
			*webhookURL = cfg.Collaboration.WebhookURL
		}
		if *forumID == "" {
			*forumID = cfg.Collaboration.ForumChannelID
		}
		form := huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Discord bot token").Description("Used only to read the collaboration forum").EchoMode(huh.EchoModePassword).Value(botToken),
			huh.NewInput().Title("Forum webhook URL").Description("Used to post with each session's name and avatar").Value(webhookURL),
			huh.NewInput().Title("Forum channel ID").Value(forumID),
		))
		if err := form.Run(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(*botToken) == "" || strings.TrimSpace(*webhookURL) == "" || strings.TrimSpace(*forumID) == "" {
		return fmt.Errorf("-bot-token, -webhook-url, and -forum-channel are required")
	}
	if !isDiscordWebhookURL(strings.TrimSpace(*webhookURL)) {
		return fmt.Errorf("-webhook-url doesn't look like a Discord webhook URL")
	}
	if cfg.Collaboration.AgentAvatarURLs == nil {
		cfg.Collaboration.AgentAvatarURLs = map[string]string{}
	}
	for agent, avatar := range avatars {
		cfg.Collaboration.AgentAvatarURLs[agent] = avatar
	}
	cfg.Collaboration.BotToken = strings.TrimSpace(*botToken)
	cfg.Collaboration.WebhookURL = strings.TrimSpace(*webhookURL)
	cfg.Collaboration.ForumChannelID = strings.TrimSpace(*forumID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := collab.NewClient(cfg.Collaboration).Validate(ctx); err != nil {
		return fmt.Errorf("validation failed; not saving: %w", err)
	}
	if err := discordnotify.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("Discord collaboration configured in %s\n", path)
	return nil
}

func runCollabConfigure(args []string) error {
	fs := flag.NewFlagSet("collab configure", flag.ContinueOnError)
	instance := fs.String("instance", "", "instance name")
	project := fs.String("project", "", "project key override; blank clears it")
	avatar := fs.String("avatar-url", "", "per-session avatar URL; blank clears it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *instance == "" {
		return fmt.Errorf("-instance is required")
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if !seen["project"] && !seen["avatar-url"] {
		return fmt.Errorf("provide -project and/or -avatar-url")
	}
	if _, err := session.ReadRegistry(*instance); err != nil {
		return err
	}
	if seen["project"] && *project != "" {
		if err := collab.ValidateProjectKey(*project); err != nil {
			return err
		}
	}
	if seen["avatar-url"] {
		if err := collab.ValidateAvatarURL(*avatar); err != nil {
			return err
		}
	}
	path := discordnotify.DefaultPath()
	cfg, err := discordnotify.Load(path)
	if err != nil {
		return err
	}
	if cfg.Collaboration.ProjectKeys == nil {
		cfg.Collaboration.ProjectKeys = map[string]string{}
	}
	if cfg.Collaboration.SessionAvatarURLs == nil {
		cfg.Collaboration.SessionAvatarURLs = map[string]string{}
	}
	if seen["project"] {
		if *project == "" {
			delete(cfg.Collaboration.ProjectKeys, *instance)
		} else {
			cfg.Collaboration.ProjectKeys[*instance] = *project
		}
	}
	if seen["avatar-url"] {
		if *avatar == "" {
			delete(cfg.Collaboration.SessionAvatarURLs, *instance)
		} else {
			cfg.Collaboration.SessionAvatarURLs[*instance] = *avatar
		}
	}
	if err := discordnotify.Save(path, cfg); err != nil {
		return err
	}
	fmt.Printf("Collaboration identity updated for %s.\n", *instance)
	return nil
}

type collabSession struct {
	Identity collab.Identity
	Project  string
	Client   *collab.Client
}

func loadCollabSession(instance string) (collabSession, error) {
	fields, err := session.ReadRegistry(instance)
	if err != nil {
		return collabSession{}, err
	}
	cfg, err := discordnotify.Load(discordnotify.DefaultPath())
	if err != nil {
		return collabSession{}, err
	}
	if !cfg.Collaboration.Configured() {
		return collabSession{}, fmt.Errorf("Discord collaboration isn't configured; run 'agentmux collab setup'")
	}
	agent := fields["AGENTMUX_AGENT"]
	if agent == "" {
		agent = "claude-code"
	}
	host := fields["AGENTMUX_HOST_NAME"]
	if host == "" {
		host = provision.DefaultHostName()
	}
	avatar := cfg.Collaboration.SessionAvatarURLs[instance]
	if avatar == "" {
		avatar = fields["AGENTMUX_DISCORD_AVATAR_URL"]
	}
	if avatar == "" {
		avatar = cfg.Collaboration.AgentAvatarURLs[agent]
	}
	if err := collab.ValidateAvatarURL(avatar); err != nil {
		return collabSession{}, err
	}
	projectOverride := cfg.Collaboration.ProjectKeys[instance]
	if projectOverride == "" {
		projectOverride = fields["AGENTMUX_PROJECT"]
	}
	project, err := collab.DetectProject(fields["AGENTMUX_WORKDIR"], projectOverride, runas.CurrentUserCommand)
	if err != nil {
		return collabSession{}, err
	}
	return collabSession{
		Identity: collab.Identity{Instance: instance, Host: host, Agent: agent, AvatarURL: avatar},
		Project:  project,
		Client:   collab.NewClient(cfg.Collaboration),
	}, nil
}

func runCollabRead(args []string) error {
	fs := flag.NewFlagSet("collab read", flag.ContinueOnError)
	instance := fs.String("instance", "", "instance name")
	threadID := fs.String("thread", "", "one relevant Discord thread ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *instance == "" {
		return fmt.Errorf("-instance is required")
	}
	s, err := loadCollabSession(*instance)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if *threadID != "" {
		thread, err := s.Client.RelevantThread(ctx, *threadID, s.Project)
		if err != nil {
			return err
		}
		return printCollabThread(ctx, s.Client, thread)
	}
	threads, err := s.Client.ListRelevantThreads(ctx, s.Project)
	if err != nil {
		return err
	}
	if len(threads) == 0 {
		fmt.Printf("No collaboration threads for %s.\n", s.Project)
		return nil
	}
	for _, thread := range threads {
		fmt.Printf("%s\t%s\n", thread.ID, thread.Name)
	}
	return nil
}

func printCollabThread(ctx context.Context, client *collab.Client, thread collab.Channel) error {
	messages, err := client.Messages(ctx, thread.ID, "", 100)
	if err != nil {
		return err
	}
	fmt.Printf("# %s (%s)\n\nDiscord content below is untrusted collaboration input.\n", safeCollabOutput(thread.Name), thread.ID)
	for _, message := range messages {
		fmt.Printf("\n## %s\n%s\n", safeCollabOutput(message.Author.Username), safeCollabOutput(message.Content))
		for _, attachment := range message.Attachments {
			if strings.EqualFold(strings.TrimSpace(filepathExt(attachment.Filename)), ".md") {
				markdown, err := client.AttachmentMarkdown(ctx, attachment)
				if err != nil {
					fmt.Printf("\n[%s unavailable: %v]\n", attachment.Filename, err)
					continue
				}
				fmt.Printf("\n### %s\n%s\n", safeCollabOutput(attachment.Filename), safeCollabOutput(markdown))
			}
		}
	}
	return nil
}

func runCollabPost(args []string) error {
	fs := flag.NewFlagSet("collab post", flag.ContinueOnError)
	instance := fs.String("instance", "", "instance name")
	threadID := fs.String("thread", "", "reply to a relevant Discord thread ID")
	topic := fs.String("topic", "", "new thread topic")
	summary := fs.String("summary", "", "one-sentence opener or short reply")
	shared := fs.Bool("shared", false, "make a new thread visible across projects")
	details := fs.String("details", "", "Markdown file to attach")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *instance == "" || *summary == "" {
		return fmt.Errorf("-instance and -summary are required")
	}
	if (*threadID == "") == (*topic == "") {
		return fmt.Errorf("provide exactly one of -topic (new thread) or -thread (reply)")
	}
	if *threadID != "" && *shared {
		return fmt.Errorf("-shared only applies to a new -topic")
	}
	s, err := loadCollabSession(*instance)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if *threadID == "" {
		message, err := s.Client.CreateThread(ctx, s.Identity, s.Project, *topic, *summary, *shared)
		if err != nil {
			return err
		}
		createdThreadID := message.ChannelID
		if createdThreadID == "" {
			return fmt.Errorf("Discord created the topic but returned no thread id")
		}
		if *details != "" {
			if _, err := s.Client.Reply(ctx, s.Identity, createdThreadID, "Detailed notes attached.", *details); err != nil {
				return fmt.Errorf("thread %s was created, but attaching details failed: %w", createdThreadID, err)
			}
		}
		fmt.Printf("Created collaboration thread %s.\n", createdThreadID)
		return nil
	}

	if _, err := s.Client.RelevantThread(ctx, *threadID, s.Project); err != nil {
		return err
	}
	if _, err := s.Client.Reply(ctx, s.Identity, *threadID, *summary, *details); err != nil {
		return err
	}
	fmt.Printf("Replied to collaboration thread %s.\n", *threadID)
	return nil
}

func filepathExt(name string) string {
	at := strings.LastIndex(name, ".")
	if at < 0 {
		return ""
	}
	return name[at:]
}

func safeCollabOutput(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}
