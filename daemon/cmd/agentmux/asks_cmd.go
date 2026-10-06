package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/collab"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
)

func runAsksCmd(args []string) {
	if len(args) == 0 {
		asksUsage()
		os.Exit(1)
	}
	var err error
	switch args[0] {
	case "post":
		err = runAsksPost(args[1:])
	case "reply":
		err = runAsksReply(args[1:])
	case "read":
		err = runAsksRead(args[1:])
	case "react":
		err = runAsksReact(args[1:])
	case "edit":
		err = runAsksEdit(args[1:])
	case "close":
		err = runAsksClose(args[1:])
	case "serve":
		err = runAsksServe(args[1:])
	default:
		asksUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "asks %s: %v\n", args[0], err)
		os.Exit(1)
	}
}

func asksUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentmux asks post (-title T | -thread ID [-title T]) -body-file F [-tag NAME ...] [-react EMOJI,EMOJI,...] [-button LABEL ...] [-buttons-json FILE] [-json]
  agentmux asks reply -thread ID -body-file F [-mention]
  agentmux asks read -thread ID [-after MESSAGE_ID] [-json]
  agentmux asks react -thread ID -message ID -emoji EMOJI
  agentmux asks edit -thread ID -message ID [-body-file F] [-disable-buttons] [-chosen LABEL]
  agentmux asks close -thread ID [-tag NAME] [-lock]
  agentmux asks serve                      hold the Discord gateway open to record button clicks`)
}

type tagFlags []string

func (t *tagFlags) String() string     { return "NAME" }
func (t *tagFlags) Set(v string) error { *t = append(*t, v); return nil }

func asksClient() (*collab.Client, error) {
	cfg, err := discordnotify.Load(discordnotify.DefaultPath())
	if err != nil {
		return nil, err
	}
	if !cfg.Collaboration.Configured() {
		return nil, fmt.Errorf("Discord collaboration isn't configured; run 'agentmux collab setup'")
	}
	client := collab.NewClient(cfg.Collaboration)
	if home, err := os.UserHomeDir(); err == nil {
		client.ClicksPath = collab.DefaultClicksPath(home)
	}
	return client, nil
}

func readBodyFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("-body-file is required")
	}
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	return string(data), err
}

func runAsksPost(args []string) error {
	fs := flag.NewFlagSet("asks post", flag.ContinueOnError)
	title := fs.String("title", "", "forum post title (with -thread: rename the thread)")
	thread := fs.String("thread", "", "add the ask to this existing ask thread instead of creating a post")
	bodyFile := fs.String("body-file", "", "file with the post body ('-' for stdin)")
	asJSON := fs.Bool("json", false, "print JSON")
	react := fs.String("react", "", "comma-separated emoji the bot adds as reactions, in order (e.g. 1️⃣,2️⃣,⏸️)")
	buttonsJSON := fs.String("buttons-json", "", "file with a JSON array of {label, emoji?, style?} buttons ('-' for stdin); appended after -button labels")
	var tags, buttons tagFlags
	fs.Var(&tags, "tag", "extra forum tag; repeatable")
	fs.Var(&buttons, "button", "button label (needs 'asks serve' running to record clicks); repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := collab.AskOptions{}
	for _, label := range buttons {
		opts.Buttons = append(opts.Buttons, collab.AskButton{Label: label})
	}
	if *buttonsJSON != "" {
		rich, err := readButtonsJSON(*buttonsJSON)
		if err != nil {
			return err
		}
		opts.Buttons = append(opts.Buttons, rich...)
	}
	for _, e := range strings.Split(*react, ",") {
		if e = strings.TrimSpace(e); e != "" {
			opts.Reactions = append(opts.Reactions, e)
		}
	}
	body, err := readBodyFile(*bodyFile)
	if err != nil {
		return err
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var threadID, messageID string
	if *thread != "" {
		threadID = *thread
		messageID, err = client.PostAskInThread(ctx, threadID, *title, body, tags, opts)
	} else {
		threadID, messageID, err = client.PostAsk(ctx, *title, body, tags, opts)
	}
	result := map[string]string{"thread_id": threadID, "message_id": messageID}
	var seedErr *collab.ReactionSeedError
	if errors.As(err, &seedErr) && messageID != "" {
		// The ask exists; failing now would invite a duplicate post.
		result["reactions_error"] = seedErr.Error()
		fmt.Fprintln(os.Stderr, "warning:", seedErr)
	} else if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	fmt.Printf("Ask posted in thread %s (message %s).\n", threadID, messageID)
	return nil
}

// readButtonsJSON reads a JSON array of {label, emoji?, style?} buttons from
// a file ('-' for stdin). -button LABEL stays valid for plain labels; this
// is the richer form for emoji and style.
func readButtonsJSON(path string) ([]collab.AskButton, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	var buttons []collab.AskButton
	if err := json.Unmarshal(data, &buttons); err != nil {
		return nil, fmt.Errorf("parsing -buttons-json: %w", err)
	}
	return buttons, nil
}

func runAsksReply(args []string) error {
	fs := flag.NewFlagSet("asks reply", flag.ContinueOnError)
	thread := fs.String("thread", "", "ask thread ID")
	bodyFile := fs.String("body-file", "", "file with the reply body ('-' for stdin)")
	mention := fs.Bool("mention", false, "mention the configured user")
	if err := fs.Parse(args); err != nil {
		return err
	}
	body, err := readBodyFile(*bodyFile)
	if err != nil {
		return err
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := client.ReplyAsk(ctx, *thread, body, *mention)
	if err != nil {
		return err
	}
	fmt.Printf("Replied to ask thread %s (message %s).\n", *thread, id)
	return nil
}

func runAsksRead(args []string) error {
	fs := flag.NewFlagSet("asks read", flag.ContinueOnError)
	thread := fs.String("thread", "", "ask thread ID")
	after := fs.String("after", "", "only messages after this message ID")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	messages, err := client.ReadAsk(ctx, *thread, *after)
	if err != nil {
		return err
	}
	if messages == nil {
		messages = []collab.AskMessage{}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(messages)
	}
	for _, m := range messages {
		who := m.AuthorName
		if m.AuthorIsConfiguredUser {
			who += " (you)"
		}
		fmt.Printf("%s\t%s\t%s\n", m.ID, safeCollabOutput(who), safeCollabOutput(m.Text))
		for _, a := range m.Answers {
			fmt.Printf("%s\t(answer)\t%s %s\n", a.MessageID, a.Kind, safeCollabOutput(a.Value))
		}
	}
	return nil
}

func runAsksClose(args []string) error {
	fs := flag.NewFlagSet("asks close", flag.ContinueOnError)
	thread := fs.String("thread", "", "ask thread ID")
	tag := fs.String("tag", collab.DefaultCloseTag, "outcome tag")
	lock := fs.Bool("lock", false, "also lock the thread (default: archive only, so a later ask can reopen it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.CloseAsk(ctx, *thread, *tag, *lock); err != nil {
		return err
	}
	fmt.Printf("Closed ask thread %s as %q.\n", *thread, *tag)
	return nil
}

func runAsksReact(args []string) error {
	fs := flag.NewFlagSet("asks react", flag.ContinueOnError)
	thread := fs.String("thread", "", "ask thread ID")
	message := fs.String("message", "", "message ID to react to")
	emoji := fs.String("emoji", "", "emoji for the bot to add (e.g. 🤖)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.ReactAsk(ctx, *thread, *message, *emoji); err != nil {
		return err
	}
	fmt.Printf("Reacted %s to message %s in ask thread %s.\n", *emoji, *message, *thread)
	return nil
}

func runAsksEdit(args []string) error {
	fs := flag.NewFlagSet("asks edit", flag.ContinueOnError)
	thread := fs.String("thread", "", "ask thread ID")
	message := fs.String("message", "", "message ID to edit")
	bodyFile := fs.String("body-file", "", "file with the replacement body ('-' for stdin); empty keeps the message")
	disable := fs.Bool("disable-buttons", false, "grey every button out")
	chosen := fs.String("chosen", "", "button label to keep highlighted (success style); the rest go grey")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var body string
	if *bodyFile != "" {
		data, err := readBodyFile(*bodyFile)
		if err != nil {
			return err
		}
		body = data
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.EditAsk(ctx, *thread, *message, collab.EditAskOptions{
		Body:           body,
		DisableButtons: *disable,
		Chosen:         *chosen,
	}); err != nil {
		return err
	}
	fmt.Printf("Edited message %s in ask thread %s.\n", *message, *thread)
	return nil
}

// runAsksServe holds the Discord gateway connection that button clicks need,
// until interrupted. Run it under a service manager; see docs/discord-asks.md.
func runAsksServe(args []string) error {
	fs := flag.NewFlagSet("asks serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	if client.ClicksPath == "" {
		return fmt.Errorf("can't find the home directory to record clicks in")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	l := &collab.Listener{Client: client, Store: &collab.ClickStore{Path: client.ClicksPath}}
	return l.Run(ctx)
}
