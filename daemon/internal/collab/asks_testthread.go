package collab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The reusable agent test thread: one dedicated thread in the asks forum
// whose id lives in the host's discord.yaml as test_thread. From a task
// session `asks post` becomes a reply into this thread — never a new forum
// post — and reply/react/edit work only here; close/tag/list are no-ops.
// The id stays in host config, never in a repo or its tests; tests use a
// fake id. See AMUX-39.
//
// Every test send goes through the bot token (never the webhook), so post,
// reply, react and edit all act as the same Discord identity and a worker
// that posts a test message can edit it afterwards. A webhook-posted
// message is authored by a different app identity than a bot-posted one,
// and Discord only lets an app edit its own messages — which is why the
// test thread's starter (posted from a plain shell through the webhook)
// could not be edited by the bot until the sends were unified here.

// TestThreadID returns the configured reusable test thread, or an error
// when none is configured — then task sessions refuse every sending
// command.
func (c *Client) TestThreadID() (string, error) {
	id := strings.TrimSpace(c.Config.TestThreadID)
	if id == "" {
		return "", fmt.Errorf("no test_thread in the collaboration section of discord.yaml: task sessions can't test Discord rendering without it")
	}
	if !snowflakeRE.MatchString(id) {
		return "", fmt.Errorf("test_thread must be a numeric Discord thread id")
	}
	return id, nil
}

// IsTestThread reports whether threadID is the configured test thread. An
// unconfigured client (or any other id) reports false.
func (c *Client) IsTestThread(threadID string) bool {
	id := strings.TrimSpace(c.Config.TestThreadID)
	return id != "" && threadID == id
}

var (
	testMentionRE = regexp.MustCompile(`<@!?\d+>|<@&\d+>`)
	testPingRE    = regexp.MustCompile(`@(?:everyone|here)\b`)
)

// scrubTestBody strips every mention from a test body: user/role markup,
// @everyone and @here. Test sends run with allowed mentions off, so this
// is belt and suspenders — nothing a worker pastes into a test body can
// ever ping anyone.
func scrubTestBody(body string) string {
	body = testMentionRE.ReplaceAllString(body, "")
	return testPingRE.ReplaceAllString(body, "")
}

// testContent prefixes the body with the worker's task id (so interleaved
// workers stay readable) and enforces Discord's length limit. Mentions are
// stripped, never sent: test sends run with allowed mentions off.
func testContent(task, body string) (string, error) {
	body = strings.TrimSpace(scrubTestBody(body))
	if body == "" {
		return "", fmt.Errorf("body is empty")
	}
	if task = strings.TrimSpace(task); task != "" {
		body = "[" + task + "] " + body
	}
	if utf8.RuneCountInString(body) > maxDiscordContentRunes {
		return "", fmt.Errorf("body exceeds Discord's %d-character limit", maxDiscordContentRunes)
	}
	return body, nil
}

// sendTestMessage posts body into the test thread as the bot (the same
// identity react and edit use), with allowed mentions off. Posting reopens
// an archived test thread on its own, so no thread edit is needed — and
// nothing here can retag, rename, close, lock or archive it.
func (c *Client) sendTestMessage(ctx context.Context, task, body string, opts AskOptions) (string, error) {
	if err := opts.validate(); err != nil {
		return "", err
	}
	thread, err := c.TestThreadID()
	if err != nil {
		return "", err
	}
	content, err := testContent(task, body)
	if err != nil {
		return "", err
	}
	guildID := ""
	if needsGuildEmojis(opts.Buttons) {
		forum, err := c.forum(ctx)
		if err != nil {
			return "", err
		}
		guildID = forum.GuildID
	}
	payload := map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	AskOptions{Embeds: opts.Embeds}.applyEmbedFlag(payload)
	if len(opts.Buttons) > 0 {
		payload["components"] = c.buttonRowsWithGuild(ctx, guildID, opts.Buttons)
	}
	var message Message
	if err := c.botJSONBody(ctx, http.MethodPost, "/channels/"+url.PathEscape(thread)+"/messages", payload, &message); err != nil {
		return "", fmt.Errorf("posting test message into Discord thread %s (the bot needs Send Messages in Threads on the forum): %w", thread, err)
	}
	if message.ID == "" {
		return "", fmt.Errorf("Discord accepted the test message but returned no message id")
	}
	return message.ID, c.seedReactions(ctx, thread, message.ID, opts.Reactions)
}

// PostTestMessage tests post rendering (body, custom emoji resolution,
// buttons, reactions) by replying into the test thread. It creates no
// forum post, sets no tags, and mentions nobody.
func (c *Client) PostTestMessage(ctx context.Context, task, body string, opts AskOptions) (string, error) {
	return c.sendTestMessage(ctx, task, body, opts)
}

// ReplyTestMessage tests reply rendering the same way: a bot message in
// the test thread, mentions off.
func (c *Client) ReplyTestMessage(ctx context.Context, task, body string) (string, error) {
	return c.sendTestMessage(ctx, task, body, AskOptions{})
}

// ReactTestMessage has the bot add one reaction to a test message, so a
// worker can test reactions as Discord renders them. Only the test
// thread's messages are reachable.
func (c *Client) ReactTestMessage(ctx context.Context, messageID, emoji string) error {
	return c.ReactTestMessageWithReplace(ctx, messageID, emoji, false)
}

// ReactTestMessageWithReplace optionally removes the bot's existing reactions
// before adding the requested reaction.
func (c *Client) ReactTestMessageWithReplace(ctx context.Context, messageID, emoji string, replace bool) error {
	thread, err := c.TestThreadID()
	if err != nil {
		return err
	}
	if !snowflakeRE.MatchString(messageID) {
		return fmt.Errorf("message id must be numeric")
	}
	if err := validateReactionEmoji(emoji); err != nil {
		return err
	}
	if replace {
		if err := c.removeOwnReactions(ctx, thread, messageID); err != nil {
			return fmt.Errorf("removing previous bot reactions from Discord test message %s: %w", messageID, err)
		}
	}
	if err := c.putReaction(ctx, thread, messageID, emoji); err != nil {
		return fmt.Errorf("adding reaction to Discord test message %s (the bot needs Add Reactions on the forum): %w", messageID, err)
	}
	return nil
}

// EditTestMessage replaces the body and/or settles the buttons of a test
// message, as the same identity that posted it. The body is scrubbed like
// a test post, so an edited body never pings anyone.
func (c *Client) EditTestMessage(ctx context.Context, messageID string, opts EditAskOptions) error {
	thread, err := c.TestThreadID()
	if err != nil {
		return err
	}
	if err := opts.validate(); err != nil {
		return err
	}
	if !snowflakeRE.MatchString(messageID) {
		return fmt.Errorf("message id must be numeric")
	}
	payload := map[string]any{}
	if opts.DisableButtons || opts.Chosen != "" {
		rows, err := c.settleMessageButtons(ctx, thread, messageID, opts.Chosen)
		if err != nil {
			return err
		}
		payload["components"] = rows
	}
	payload["allowed_mentions"] = map[string]any{"parse": []string{}}
	opts.applyEditEmbedFlag(payload)
	if opts.Body != "" {
		content := strings.TrimSpace(scrubTestBody(opts.Body))
		if content == "" {
			return fmt.Errorf("body is empty")
		}
		payload["content"] = content
	}
	if err := c.botJSONBody(ctx, http.MethodPatch,
		"/channels/"+url.PathEscape(thread)+"/messages/"+url.PathEscape(messageID), payload, nil); err != nil {
		return fmt.Errorf("editing Discord test message %s (the bot needs Send Messages in Threads on the forum): %w", messageID, err)
	}
	return nil
}

// PruneTestMessages deletes the bot's own messages in thread older than
// cut, keeping the starter message (a forum post's first message shares
// the thread's id). Messages from anyone else are left alone, and so is
// anything whose age can't be told. With dryRun nothing is deleted. It
// returns the deleted (or would-be-deleted) message ids.
// This is what `asks prune` calls — with the test thread by default, so
// yesterday's tests don't pile up — and through it the reconcile job.
func (c *Client) PruneTestMessages(ctx context.Context, thread string, cut time.Time, dryRun bool) ([]string, error) {
	if !snowflakeRE.MatchString(thread) {
		return nil, fmt.Errorf("thread id must be numeric")
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := c.botJSON(ctx, http.MethodGet, "/users/@me", &me); err != nil {
		return nil, fmt.Errorf("reading Discord bot user: %w", err)
	}
	if me.ID == "" {
		return nil, fmt.Errorf("Discord returned no bot user id")
	}
	var pruned []string
	before := ""
	for {
		path := "/channels/" + url.PathEscape(thread) + "/messages?limit=100"
		if before != "" {
			path += "&before=" + url.QueryEscape(before)
		}
		var batch []Message
		if err := c.botJSON(ctx, http.MethodGet, path, &batch); err != nil {
			return pruned, fmt.Errorf("reading Discord test thread %s: %w", thread, err)
		}
		if len(batch) == 0 {
			return pruned, nil
		}
		for _, m := range batch {
			if m.ID == thread || m.Author.ID != me.ID || m.Timestamp.IsZero() || !m.Timestamp.Before(cut) {
				continue
			}
			if !dryRun {
				if err := c.botJSONBody(ctx, http.MethodDelete,
					"/channels/"+url.PathEscape(thread)+"/messages/"+url.PathEscape(m.ID), nil, nil); err != nil {
					return pruned, fmt.Errorf("deleting Discord test message %s: %w", m.ID, err)
				}
			}
			pruned = append(pruned, m.ID)
		}
		if len(batch) < 100 {
			return pruned, nil
		}
		before = batch[len(batch)-1].ID
	}
}
