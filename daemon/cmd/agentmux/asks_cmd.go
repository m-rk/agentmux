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
	"github.com/m-rk/agentmux/daemon/internal/liveguard"
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
	case "tag":
		err = runAsksTag(args[1:])
	case "close":
		err = runAsksClose(args[1:])
	case "list":
		err = runAsksList(args[1:])
	case "serve":
		err = runAsksServe(args[1:])
	case "prune":
		err = runAsksPrune(args[1:])
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
  agentmux asks post (-title T | -thread ID [-title T]) -body-file F [-tag NAME ...] [-react EMOJI,EMOJI,...] [-button LABEL ...] [-buttons-json FILE] [-embeds] [-json] [-dry-run]
  agentmux asks reply -thread ID -body-file F [-mention] [-embeds] [-dry-run]
  agentmux asks read -thread ID [-after MESSAGE_ID] [-json]
  agentmux asks react -thread ID -message ID -emoji EMOJI
  agentmux asks edit -thread ID -message ID [-body-file F] [-disable-buttons] [-chosen LABEL] [-embeds]
  agentmux asks tag -thread ID -set "task,working" [-unarchive]
  agentmux asks close -thread ID [-tag NAME] [-lock]
  agentmux asks list [-open|-archived|-all] [-tag NAME] [-since DUR] [-json]
  agentmux asks serve                      hold the Discord gateway open to record button clicks
  agentmux asks prune [-thread ID] [-older-than DUR] [-dry-run] [-json]
                       delete old test-thread messages (task sessions: only the test thread)

-dry-run prints the Discord payload instead of sending it.
From a task session the sending commands reroute into the reusable test
thread from discord.yaml (test thread: …) instead of touching live asks:
post and reply post there, react and edit work only there, close, tag and
list are dry-run style no-ops. Without a test thread configured, task
sessions refuse the sending commands.`)
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
	// AGENTMUX_DISCORD_API_BASE points the bot API at a fake gateway in
	// tests. Never set in production: it would redirect bot calls.
	if base := strings.TrimSpace(os.Getenv("AGENTMUX_DISCORD_API_BASE")); base != "" {
		client.APIBaseURL = base
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
	embeds := fs.Bool("embeds", false, "keep link unfurls (embeds) on this send; default suppresses them")
	dryRun := fs.Bool("dry-run", false, "print the Discord payload instead of sending it")
	var tags, buttons tagFlags
	fs.Var(&tags, "tag", "extra forum tag; repeatable")
	fs.Var(&buttons, "button", "button label (needs 'asks serve' running to record clicks); repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := collab.AskOptions{Embeds: *embeds}
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
	if *dryRun {
		return printAsksPostPreview(client, *title, *thread, body, tags, opts, *asJSON)
	}
	if liveguard.IsTaskSession() {
		return runAsksPostAsTest(client, *title, *thread, tags, body, opts, *asJSON)
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

// printAsksPostPreview resolves the forum's tags and prints the payload a
// post would send, without touching Discord. -thread posts are shown as a
// reply-style preview: the live path renames the thread and reopens it, so
// the preview names the target instead of rendering those edits.
func printAsksPostPreview(client *collab.Client, title, thread, body string, tags []string, opts collab.AskOptions, asJSON bool) error {
	if thread != "" {
		preview := collab.AskPreview{Kind: "post-in-thread", Title: title, Content: body, ThreadID: thread, Tags: tags, Options: opts}
		if asJSON {
			js, err := preview.Marshal()
			if err != nil {
				return err
			}
			fmt.Println(js)
			return nil
		}
		fmt.Print(preview.Format())
		return nil
	}
	forum, err := client.ForumForPreview(context.Background())
	if err != nil {
		return err
	}
	names := make([]string, 0, len(forum.AvailableTags))
	for _, t := range forum.AvailableTags {
		names = append(names, t.Name)
	}
	preview, err := client.PreviewPost(title, body, tags, opts, names)
	if err != nil {
		return err
	}
	if asJSON {
		js, err := preview.Marshal()
		if err != nil {
			return err
		}
		fmt.Println(js)
		return nil
	}
	fmt.Print(preview.Format())
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
	embeds := fs.Bool("embeds", false, "keep link unfurls (embeds) on this send; default suppresses them")
	dryRun := fs.Bool("dry-run", false, "print the Discord payload instead of sending it")
	asJSON := fs.Bool("json", false, "print JSON")
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
	if *dryRun {
		preview, err := client.PreviewReply(*thread, body, *mention)
		if err != nil {
			return err
		}
		if *asJSON {
			js, err := preview.Marshal()
			if err != nil {
				return err
			}
			fmt.Println(js)
			return nil
		}
		fmt.Print(preview.Format())
		return nil
	}
	if liveguard.IsTaskSession() {
		return runAsksReplyAsTest(client, *thread, *mention, body, *asJSON)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := client.ReplyAsk(ctx, *thread, body, *mention, *embeds)
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

// runAsksTag replaces a task thread's tags with exactly -set, resolved by
// name from the forum's available tags. A retag never posts.
func runAsksTag(args []string) error {
	fs := flag.NewFlagSet("asks tag", flag.ContinueOnError)
	thread := fs.String("thread", "", "task thread ID")
	set := fs.String("set", "", `comma-separated tag names, e.g. "task,working"`)
	unarchive := fs.Bool("unarchive", false, "unarchive an archived thread first, re-archive after")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *thread == "" {
		return fmt.Errorf("-thread is required")
	}
	var names []string
	for _, name := range strings.Split(*set, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf(`-set is required, e.g. -set "task,working"`)
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	if liveguard.IsTaskSession() {
		if _, terr := client.TestThreadID(); terr != nil {
			return liveguard.Check()
		}
		fmt.Printf("test thread: would retag thread %s as %s (no-op: tag never touches the test thread or a real one).\n", *thread, strings.Join(names, ","))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.TagAsk(ctx, *thread, names, collab.TagOptions{Unarchive: *unarchive}); err != nil {
		return err
	}
	fmt.Printf("Retagged thread %s as %s.\n", *thread, strings.Join(names, ","))
	return nil
}

func runAsksClose(args []string) error {
	fs := flag.NewFlagSet("asks close", flag.ContinueOnError)
	thread := fs.String("thread", "", "ask thread ID")
	tag := fs.String("tag", collab.DefaultCloseTag, "state tag (any forum state tag by name)")
	lock := fs.Bool("lock", false, "also lock the thread (default: archive only, so a later ask can reopen it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	if liveguard.IsTaskSession() {
		if _, terr := client.TestThreadID(); terr != nil {
			return liveguard.Check()
		}
		fmt.Printf("test thread: would close ask thread %s as %q (no-op: close never touches the test thread or a real one).\n", *thread, *tag)
		return nil
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
	if liveguard.IsTaskSession() {
		test, terr := client.TestThreadID()
		if terr != nil {
			return liveguard.Check()
		}
		if *thread != "" && *thread != test {
			return fmt.Errorf("task sessions can only react in the test thread %s", test)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.ReactTestMessage(ctx, *message, *emoji); err != nil {
			return err
		}
		fmt.Printf("test thread: reacted %s to message %s in test thread %s.\n", *emoji, *message, test)
		return nil
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
	embeds := fs.Bool("embeds", false, "keep link unfurls (embeds) on the edit; default keeps them suppressed")
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
	if liveguard.IsTaskSession() {
		test, terr := client.TestThreadID()
		if terr != nil {
			return liveguard.Check()
		}
		if *thread != "" && *thread != test {
			return fmt.Errorf("task sessions can only edit in the test thread %s", test)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.EditTestMessage(ctx, *message, collab.EditAskOptions{
			Body:           body,
			DisableButtons: *disable,
			Chosen:         *chosen,
			Embeds:         *embeds,
		}); err != nil {
			return err
		}
		fmt.Printf("test thread: edited message %s in test thread %s.\n", *message, test)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.EditAsk(ctx, *thread, *message, collab.EditAskOptions{
		Body:           body,
		DisableButtons: *disable,
		Chosen:         *chosen,
		Embeds:         *embeds,
	}); err != nil {
		return err
	}
	fmt.Printf("Edited message %s in ask thread %s.\n", *message, *thread)
	return nil
}

// runAsksList prints the asks forum's threads. Listing is read-only, but
// from a task session it is a dry-run style no-op that names the test
// thread instead: nothing in a task session may enumerate live asks.
func runAsksList(args []string) error {
	fs := flag.NewFlagSet("asks list", flag.ContinueOnError)
	open := fs.Bool("open", false, "only open (unarchived) threads (default)")
	archived := fs.Bool("archived", false, "only archived threads")
	all := fs.Bool("all", false, "open and archived threads")
	tag := fs.String("tag", "", "only threads carrying this forum tag")
	since := fs.String("since", "", "only threads active since this long ago (e.g. 24h, 30m)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	state := "open"
	switch {
	case *all:
		state = "all"
	case *archived:
		state = "archived"
	case *open:
		state = "open"
	}
	if n := boolCount(*open, *archived, *all); n > 1 {
		return fmt.Errorf("pick at most one of -open, -archived, -all")
	}
	var sinceTime time.Time
	if *since != "" {
		dur, err := time.ParseDuration(*since)
		if err != nil {
			return fmt.Errorf("bad -since: %w", err)
		}
		sinceTime = time.Now().Add(-dur)
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	if liveguard.IsTaskSession() {
		if _, terr := client.TestThreadID(); terr != nil {
			return liveguard.Check()
		}
		filters := []string{"state=" + state}
		if *tag != "" {
			filters = append(filters, "tag="+*tag)
		}
		if *since != "" {
			filters = append(filters, "since="+*since)
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode([]collab.AskThread{})
		}
		fmt.Printf("test thread: would list asks (%s) (no-op: list never enumerates live asks).\n", strings.Join(filters, ", "))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	threads, err := client.ListAsks(ctx, collab.ListAsksOptions{State: state, Tag: *tag, Since: sinceTime})
	if err != nil {
		return err
	}
	if threads == nil {
		threads = []collab.AskThread{}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(threads)
	}
	for _, th := range threads {
		status := "open"
		if th.Archived {
			status = "archived"
		}
		if th.Locked {
			status += ",locked"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", th.ID, safeCollabOutput(th.Title), status, strings.Join(th.Tags, ","), th.LastMessageTime)
	}
	return nil
}

func boolCount(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
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

// testTaskID names the worker in its test messages ([<task id>] prefix),
// from the task-session identity the guard already trusts: the task flag
// and instance name `sessions run` stamps, falling back to the
// *-worktrees/task-* directory. Empty when nothing identifies the worker.
func testTaskID() string {
	if inst := os.Getenv(liveguard.InstanceEnv); strings.HasPrefix(inst, liveguard.TaskPrefix) {
		return strings.TrimPrefix(inst, liveguard.TaskPrefix)
	}
	if wd, err := os.Getwd(); err == nil {
		for _, seg := range strings.Split(wd, string(os.PathSeparator)) {
			if rest, ok := strings.CutPrefix(seg, "task-"); ok && rest != "" {
				return rest
			}
		}
	}
	return ""
}

// runAsksPostAsTest routes a task session's `asks post` into the reusable
// test thread: a bot reply there (mentions off, prefixed with the task
// id), never a new forum post. Title, thread and tags are dropped — test
// messages carry none — and the output says so when any were passed.
// Needs test_thread in discord.yaml; without it the command refuses as
// before.
func runAsksPostAsTest(client *collab.Client, title, thread string, tags []string, body string, opts collab.AskOptions, asJSON bool) error {
	test, terr := client.TestThreadID()
	if terr != nil {
		return liveguard.Check()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	messageID, err := client.PostTestMessage(ctx, testTaskID(), body, opts)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"thread_id": test, "message_id": messageID})
	}
	dropped := []string{}
	if title != "" {
		dropped = append(dropped, "title")
	}
	if thread != "" {
		dropped = append(dropped, "-thread")
	}
	if len(tags) > 0 {
		dropped = append(dropped, "tags")
	}
	extra := ""
	if len(dropped) > 0 {
		extra = " (" + strings.Join(dropped, ", ") + " ignored: test messages carry no title or tags)"
	}
	fmt.Printf("test thread: posted test message %s in test thread %s%s.\n", messageID, test, extra)
	return nil
}

// runAsksReplyAsTest routes a task session's `asks reply` the same way:
// a bot message in the test thread, never touching the named thread and
// never mentioning anyone.
func runAsksReplyAsTest(client *collab.Client, thread string, mention bool, body string, asJSON bool) error {
	test, terr := client.TestThreadID()
	if terr != nil {
		return liveguard.Check()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	messageID, err := client.ReplyTestMessage(ctx, testTaskID(), body)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"thread_id": test, "message_id": messageID})
	}
	extra := ""
	if thread != "" && thread != test {
		extra = " (the named thread is untouched)"
	}
	if mention {
		extra += " (-mention ignored: test messages mention nobody)"
	}
	fmt.Printf("test thread: replied with test message %s in test thread %s%s.\n", messageID, test, extra)
	return nil
}

// runAsksPrune is `agentmux asks prune`: delete the bot's own messages in
// a thread older than -older-than (default 24h), keeping the starter
// message. From a task session only the configured test thread is
// reachable (any other -thread is refused); a person may name another
// ask thread explicitly, and the default is the test thread when one is
// configured. The reconcile job calls this against the test thread so
// yesterday's tests don't pile up; it never takes a thread id from a
// thread listing.
func runAsksPrune(args []string) error {
	fs := flag.NewFlagSet("asks prune", flag.ContinueOnError)
	thread := fs.String("thread", "", "thread to prune (default: the configured test thread)")
	olderThan := fs.String("older-than", "24h", "delete the bot's own messages older than this (e.g. 24h, 30m)")
	dryRun := fs.Bool("dry-run", false, "list what would go without deleting anything")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dur, err := time.ParseDuration(*olderThan)
	if err != nil {
		return fmt.Errorf("bad -older-than: %w", err)
	}
	if dur < 0 {
		return fmt.Errorf("-older-than must not be negative")
	}
	client, err := asksClient()
	if err != nil {
		return err
	}
	target := *thread
	if target == "" {
		target, err = client.TestThreadID()
		if err != nil {
			return err
		}
	}
	if liveguard.IsTaskSession() && !client.IsTestThread(target) {
		return fmt.Errorf("task sessions can only prune the test thread %s", strings.TrimSpace(client.Config.TestThreadID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cut := time.Now().Add(-dur)
	pruned, err := client.PruneTestMessages(ctx, target, cut, *dryRun)
	if err != nil {
		return err
	}
	if pruned == nil {
		pruned = []string{}
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"thread_id": target, "pruned": pruned, "dry_run": *dryRun})
	}
	verb := "deleted"
	if *dryRun {
		verb = "would delete"
	}
	if len(pruned) == 0 {
		fmt.Printf("test thread: nothing to prune in thread %s.\n", target)
		return nil
	}
	fmt.Printf("test thread: %s %d message(s) in thread %s: %s.\n", verb, len(pruned), target, strings.Join(pruned, ", "))
	return nil
}
