package collab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Asks are messages in threads in the collaboration forum that @-mention
// one configured user. Thread tags describe the thread, not the ask: every
// task thread carries exactly one type tag and exactly one state tag (see
// MERG-37). Collab read and the digest use the type tags to keep task
// threads out of session context.
const (
	// AskTagName is the retired marker tag. Threads created before the
	// MERG-37 retag still carry it; the forum gate and the digest filter
	// honour it until Mark deletes it from the forum.
	AskTagName = "ask"
	// DefaultCloseTag leaves a closed thread as done.
	DefaultCloseTag = "done"
	asksUsername    = "agentmux asks"
	maxAppliedTags  = 5
)

// AskTypeTags marks a thread as a task thread: exactly one per thread, set
// at creation. The asks gate, `asks list`, and the collab digest filter all
// key on these (plus the retired `ask` tag while migration is in flight).
var AskTypeTags = []string{"task", "epic", "idea"}

// AskStateTags is the thread's current state: exactly one per thread, kept
// current by whoever owns the thread. Close and post-in-thread swap these.
var AskStateTags = []string{"needs me", "working", "blocked", "parked", "not now", "done", "failed"}

// RetiredAskTags are the pre-retag outcome tags. They are stripped wherever
// state tags are swapped, so partially migrated threads converge.
var RetiredAskTags = []string{"ask", "pending", "launched", "answered"}

// DefaultOpenTag is the state a new ask in an existing thread leaves it in
// when the caller passes no state tag: a fresh question needs Mark.
const DefaultOpenTag = "needs me"

var snowflakeRE = regexp.MustCompile(`^[0-9]{1,25}$`)

// AskAnswer is a one-tap answer to an ask, from the configured user only:
// an emoji reaction (Value is the emoji) or a button click (Value is the
// button label). MessageID is the message that was reacted to or clicked.
type AskAnswer struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	MessageID string `json:"message_id"`
	Timestamp string `json:"timestamp,omitempty"`
}

type AskMessage struct {
	ID                     string      `json:"id"`
	AuthorID               string      `json:"author_id"`
	AuthorName             string      `json:"author_name"`
	AuthorIsConfiguredUser bool        `json:"author_is_configured_user"`
	Text                   string      `json:"text"`
	Timestamp              string      `json:"timestamp"`
	Answers                []AskAnswer `json:"answers,omitempty"`
}

func (c *Client) mentionUser() (string, error) {
	id := c.Config.AskMentionUserID
	if id == "" {
		return "", fmt.Errorf("ask_mention_user_id isn't set in the collaboration section of discord.yaml")
	}
	if !snowflakeRE.MatchString(id) {
		return "", fmt.Errorf("ask_mention_user_id must be a numeric Discord user id")
	}
	return id, nil
}

func (c *Client) forum(ctx context.Context) (Channel, error) {
	var forum Channel
	if err := c.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(c.Config.ForumChannelID), &forum); err != nil {
		return Channel{}, fmt.Errorf("reading Discord forum channel: %w", err)
	}
	return forum, nil
}

func tagID(tags []ForumTag, name string) string {
	for _, t := range tags {
		if strings.EqualFold(t.Name, name) {
			return t.ID
		}
	}
	return ""
}

func hasTag(applied []string, id string) bool {
	if id == "" {
		return false
	}
	for _, a := range applied {
		if a == id {
			return true
		}
	}
	return false
}

// tagNames lists the forum's available tag names for error messages.
func tagNames(forum Channel) []string {
	names := make([]string, 0, len(forum.AvailableTags))
	for _, t := range forum.AvailableTags {
		names = append(names, t.Name)
	}
	return names
}

// resolveTags resolves tag names (case-insensitive) to their ids,
// deduplicated. An unknown name fails naming the valid ones, so the caller
// can refresh the forum (a tag created after the first fetch) and retry.
func resolveTags(forum Channel, names []string) ([]string, error) {
	var ids []string
	var missing []string
	for _, name := range names {
		id := tagID(forum.AvailableTags, name)
		if id == "" {
			missing = append(missing, name)
			continue
		}
		if !hasTag(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(missing) > 0 {
		return nil, &UnknownTagError{Names: missing, Valid: tagNames(forum)}
	}
	return ids, nil
}

// UnknownTagError is a tag name the forum has no tag for. Callers refresh
// the forum and retry once (the tag may have been created since), then
// surface this with the valid names.
type UnknownTagError struct {
	Names []string
	Valid []string
}

func (e *UnknownTagError) Error() string {
	return fmt.Sprintf("the forum has no %s tag; valid tags: %s (create it by hand under Edit Channel → Tags)",
		quoteAll(e.Names), strings.Join(e.Valid, ", "))
}

func quoteAll(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	return strings.Join(quoted, ", ")
}

// isAskThread reports whether the thread is a task thread: it lives in the
// asks forum and carries a type tag (or the retired `ask` tag while
// migration is in flight). Collab threads share the same forum but never
// carry these tags.
func isAskThread(forum Channel, thread Channel) bool {
	if thread.ParentID == "" {
		return false
	}
	if forum.ID != "" && thread.ParentID != forum.ID {
		return false
	}
	for _, name := range append([]string{AskTagName}, AskTypeTags...) {
		if hasTag(thread.AppliedTags, tagID(forum.AvailableTags, name)) {
			return true
		}
	}
	return false
}

func (c *Client) mentionPayload(user string, mention bool) map[string]any {
	if mention {
		return map[string]any{"parse": []string{}, "users": []string{user}}
	}
	return map[string]any{"parse": []string{}}
}

func askContent(user string, mention bool, body string) (string, error) {
	body = strings.TrimSpace(body)
	if mention {
		body = "<@" + user + ">\n" + body
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("body is empty")
	}
	if utf8.RuneCountInString(body) > maxDiscordContentRunes {
		return "", fmt.Errorf("body exceeds Discord's %d-character limit", maxDiscordContentRunes)
	}
	return body, nil
}

// PostAsk creates a forum post tagged `task` (plus `needs me` and extraTags)
// whose body opens with a mention of the configured user. Extra tags are
// free-form: a project tag, or a state override such as `blocked` (which
// replaces the default `needs me`; at most one state tag survives —
// swapStateTags drops the rest).
//
// With opts.Buttons the bot posts the message (webhooks can't carry
// components); otherwise the webhook does. opts.Reactions are seeded by the
// bot afterwards; if only that step fails the ids are returned with a
// *ReactionSeedError.
func (c *Client) PostAsk(ctx context.Context, title, body string, extraTags []string, opts AskOptions) (threadID, messageID string, err error) {
	if err := opts.validate(); err != nil {
		return "", "", err
	}
	user, err := c.mentionUser()
	if err != nil {
		return "", "", err
	}
	title = cleanOneLine(title)
	if title == "" {
		return "", "", fmt.Errorf("title is required")
	}
	if utf8.RuneCountInString(title) > 100 {
		return "", "", fmt.Errorf("title is longer than Discord's 100-character limit")
	}
	content, err := askContent(user, true, body)
	if err != nil {
		return "", "", err
	}
	var tags []string
	var guildID string
	if _, err := c.withRefreshedForum(ctx, func(f Channel) ([]string, error) {
		var err error
		guildID = f.GuildID
		tags, err = swapStateTags(f, nil, "task", extraTags)
		if err != nil {
			return nil, err
		}
		// A lone state extra would leave the thread stateless; default it.
		if !hasStateTag(f, tags) {
			tags, err = swapStateTags(f, tags, DefaultOpenTag, nil)
		}
		return tags, err
	}); err != nil {
		return "", "", err
	}
	if len(tags) > maxAppliedTags {
		return "", "", fmt.Errorf("a forum post takes at most %d tags", maxAppliedTags)
	}
	payload := map[string]any{
		"content":          content,
		"thread_name":      title,
		"username":         asksUsername,
		"applied_tags":     tags,
		"allowed_mentions": c.mentionPayload(user, true),
	}
	if len(opts.Buttons) > 0 {
		// A forum post's starter message shares the thread's id.
		botPayload := map[string]any{
			"name":         title,
			"applied_tags": tags,
			"message": map[string]any{
				"content":          content,
				"allowed_mentions": c.mentionPayload(user, true),
				"components":       c.buttonRowsWithGuild(ctx, guildID, opts.Buttons),
			},
		}
		var thread Channel
		if err := c.botJSONBody(ctx, http.MethodPost, "/channels/"+url.PathEscape(c.Config.ForumChannelID)+"/threads", botPayload, &thread); err != nil {
			return "", "", fmt.Errorf("creating Discord ask with buttons (the bot needs Create Posts on the forum): %w", err)
		}
		if thread.ID == "" {
			return "", "", fmt.Errorf("Discord created the ask but returned no thread id")
		}
		return thread.ID, thread.ID, c.seedReactions(ctx, thread.ID, thread.ID, opts.Reactions)
	}
	var message Message
	if err := c.webhookJSON(ctx, http.MethodPost, url.Values{"wait": {"true"}}, payload, &message); err != nil {
		return "", "", fmt.Errorf("creating Discord ask: %w", err)
	}
	if message.ChannelID == "" {
		return "", "", fmt.Errorf("Discord created the ask but returned no thread id")
	}
	return message.ChannelID, message.ID, c.seedReactions(ctx, message.ChannelID, message.ID, opts.Reactions)
}

// PostAskInThread adds an ask message to an existing task thread instead of
// creating a post. It unarchives the thread (unlocked only), swaps any state
// tag for `needs me` (adding extraTags), optionally renames it, then posts
// the message with a mention of the configured user. Like close, the thread
// edit needs the bot (Manage Threads on the forum).
func (c *Client) PostAskInThread(ctx context.Context, threadID, title, body string, extraTags []string, opts AskOptions) (string, error) {
	if err := opts.validate(); err != nil {
		return "", err
	}
	user, err := c.mentionUser()
	if err != nil {
		return "", err
	}
	content, err := askContent(user, true, body)
	if err != nil {
		return "", err
	}
	title = cleanOneLine(title)
	if utf8.RuneCountInString(title) > 100 {
		return "", fmt.Errorf("title is longer than Discord's 100-character limit")
	}
	thread, forum, err := c.askThread(ctx, threadID)
	if err != nil {
		return "", err
	}
	var tags []string
	if _, err := c.withRefreshedForum(ctx, func(f Channel) ([]string, error) {
		var err error
		tags, err = swapStateTags(f, thread.AppliedTags, DefaultOpenTag, extraTags)
		return tags, err
	}); err != nil {
		return "", err
	}
	patch := map[string]any{"applied_tags": tags, "archived": false, "auto_archive_duration": 10080}
	if title != "" {
		patch["name"] = title
	}
	if err := c.botJSONBody(ctx, http.MethodPatch, "/channels/"+url.PathEscape(threadID), patch, nil); err != nil {
		return "", fmt.Errorf("reopening Discord ask thread (the bot needs Manage Threads on the forum): %w", err)
	}
	var message Message
	if len(opts.Buttons) > 0 {
		botPayload := map[string]any{
			"content":          content,
			"allowed_mentions": c.mentionPayload(user, true),
			"components":       c.buttonRowsWithGuild(ctx, forum.GuildID, opts.Buttons),
		}
		if err := c.botJSONBody(ctx, http.MethodPost, "/channels/"+url.PathEscape(threadID)+"/messages", botPayload, &message); err != nil {
			return "", fmt.Errorf("posting ask with buttons into Discord thread (the bot needs Send Messages in Threads): %w", err)
		}
	} else {
		payload := map[string]any{
			"content":          content,
			"username":         asksUsername,
			"allowed_mentions": c.mentionPayload(user, true),
		}
		query := url.Values{"wait": {"true"}, "thread_id": {threadID}}
		if err := c.webhookJSON(ctx, http.MethodPost, query, payload, &message); err != nil {
			return "", fmt.Errorf("posting ask into Discord thread: %w", err)
		}
	}
	return message.ID, c.seedReactions(ctx, threadID, message.ID, opts.Reactions)
}

// hasStateTag reports whether any of the applied ids is a state tag.
func hasStateTag(forum Channel, applied []string) bool {
	for _, name := range AskStateTags {
		if hasTag(applied, tagID(forum.AvailableTags, name)) {
			return true
		}
	}
	return false
}

// withRefreshedForum runs resolve with the forum, and on an unknown tag
// re-reads the forum once (a tag created after the first fetch) before
// retrying. Anything else returns as-is.
func (c *Client) withRefreshedForum(ctx context.Context, resolve func(Channel) ([]string, error)) ([]string, error) {
	forum, err := c.forum(ctx)
	if err != nil {
		return nil, err
	}
	tags, err := resolve(forum)
	var unknown *UnknownTagError
	if errors.As(err, &unknown) {
		forum, ferr := c.forum(ctx)
		if ferr != nil {
			return nil, err
		}
		return resolve(forum)
	}
	return tags, err
}

// ForumForPreview fetches the forum channel for -dry-run tag resolution.
// It is a read (no post, reply, reaction, or edit), so -dry-run never
// touches live Discord even though it needs the network.
func (c *Client) ForumForPreview(ctx context.Context) (Channel, error) {
	return c.forum(ctx)
}

// swapStateTags returns the applied tag IDs with every state tag (and the
// retired outcome tags) removed, then outcome and extra appended (without
// duplicates). Type and project tags survive untouched.
func swapStateTags(forum Channel, applied []string, outcome string, extra []string) ([]string, error) {
	add, err := resolveTags(forum, append([]string{outcome}, extra...))
	if err != nil {
		return nil, err
	}
	drop := map[string]bool{}
	for _, name := range append(append([]string{}, AskStateTags...), RetiredAskTags...) {
		if id := tagID(forum.AvailableTags, name); id != "" {
			drop[id] = true
		}
	}
	tags := []string{}
	for _, id := range applied {
		if !drop[id] {
			tags = append(tags, id)
		}
	}
	for _, id := range add {
		if !hasTag(tags, id) {
			tags = append(tags, id)
		}
	}
	if len(tags) > maxAppliedTags {
		return nil, fmt.Errorf("a forum post takes at most %d tags", maxAppliedTags)
	}
	return tags, nil
}

// askThread fetches a thread and refuses anything that isn't an ask post in
// the configured forum, so asks commands can't touch collaboration threads.
func (c *Client) askThread(ctx context.Context, threadID string) (Channel, Channel, error) {
	if !snowflakeRE.MatchString(threadID) {
		return Channel{}, Channel{}, fmt.Errorf("thread id must be numeric")
	}
	var thread Channel
	if err := c.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(threadID), &thread); err != nil {
		return Channel{}, Channel{}, fmt.Errorf("reading Discord thread %s: %w", threadID, err)
	}
	forum, err := c.forum(ctx)
	if err != nil {
		return Channel{}, Channel{}, err
	}
	if thread.ParentID != c.Config.ForumChannelID || !isAskThread(forum, thread) {
		return Channel{}, Channel{}, fmt.Errorf("thread %s isn't an ask post", threadID)
	}
	return thread, forum, nil
}

// ReplyAsk posts into an existing ask thread, mentioning the configured user
// only when mention is true.
func (c *Client) ReplyAsk(ctx context.Context, threadID, body string, mention bool) (string, error) {
	user := ""
	if mention {
		var err error
		if user, err = c.mentionUser(); err != nil {
			return "", err
		}
	}
	content, err := askContent(user, mention, body)
	if err != nil {
		return "", err
	}
	if _, _, err := c.askThread(ctx, threadID); err != nil {
		return "", err
	}
	payload := map[string]any{
		"content":          content,
		"username":         asksUsername,
		"allowed_mentions": c.mentionPayload(user, mention),
	}
	var message Message
	query := url.Values{"wait": {"true"}, "thread_id": {threadID}}
	if err := c.webhookJSON(ctx, http.MethodPost, query, payload, &message); err != nil {
		return "", fmt.Errorf("replying to Discord ask: %w", err)
	}
	return message.ID, nil
}

// ReadAsk returns messages in an ask thread after the given message ID
// (oldest first), paging through Discord's 100-message limit.
func (c *Client) ReadAsk(ctx context.Context, threadID, after string) ([]AskMessage, error) {
	user, err := c.mentionUser()
	if err != nil {
		return nil, err
	}
	if _, _, err := c.askThread(ctx, threadID); err != nil {
		return nil, err
	}
	clicks, err := c.clicksFor(threadID, user)
	if err != nil {
		return nil, err
	}
	var out []AskMessage
	if after != "" {
		// Answers to the ask itself sit on the message the caller reads
		// after; surface that message only when it carries any.
		if anchor, err := c.message(ctx, threadID, after); err == nil {
			if m, err := c.askMessage(ctx, threadID, anchor, user, clicks); err != nil {
				return nil, err
			} else if len(m.Answers) > 0 {
				out = append(out, m)
			}
		}
	}
	for {
		batch, err := c.Messages(ctx, threadID, after, 100)
		if err != nil {
			return nil, err
		}
		for _, m := range batch {
			am, err := c.askMessage(ctx, threadID, m, user, clicks)
			if err != nil {
				return nil, err
			}
			out = append(out, am)
		}
		if len(batch) < 100 {
			return out, nil
		}
		after = batch[len(batch)-1].ID
	}
}

func (c *Client) askMessage(ctx context.Context, threadID string, m Message, user string, clicks map[string][]Click) (AskMessage, error) {
	am := AskMessage{
		ID:                     m.ID,
		AuthorID:               m.Author.ID,
		AuthorName:             m.Author.Username,
		AuthorIsConfiguredUser: m.WebhookID == "" && m.Author.ID == user,
		Text:                   m.Content,
		Timestamp:              m.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
	}
	answers, err := c.userReactions(ctx, threadID, m, user)
	if err != nil {
		return AskMessage{}, err
	}
	am.Answers = answers
	for _, click := range clicks[m.ID] {
		am.Answers = append(am.Answers, AskAnswer{Kind: "button", Value: click.Label, MessageID: m.ID, Timestamp: click.Timestamp.Format("2006-01-02T15:04:05Z07:00")})
	}
	return am, nil
}

// CloseAsk replaces the thread's state tag with outcome (any state tag by
// name; empty means `done`), then archives it, and locks it only when lock
// is true so a later ask can reopen the thread. Thread edits (this, tag,
// and post-in-thread) need the bot (Manage Threads on the forum): webhooks
// can't edit threads.
func (c *Client) CloseAsk(ctx context.Context, threadID, outcome string, lock bool) error {
	thread, _, err := c.askThread(ctx, threadID)
	if err != nil {
		return err
	}
	if outcome == "" {
		outcome = DefaultCloseTag
	}
	known := false
	for _, name := range AskStateTags {
		known = known || strings.EqualFold(name, outcome)
	}
	if !known {
		return fmt.Errorf("unknown state tag %q; valid tags: %s", outcome, strings.Join(AskStateTags, ", "))
	}
	var tags []string
	if _, err := c.withRefreshedForum(ctx, func(f Channel) ([]string, error) {
		var err error
		tags, err = swapStateTags(f, thread.AppliedTags, outcome, nil)
		return tags, err
	}); err != nil {
		return err
	}
	body := map[string]any{"applied_tags": tags, "archived": true, "locked": lock}
	if err := c.botJSONBody(ctx, http.MethodPatch, "/channels/"+url.PathEscape(threadID), body, nil); err != nil {
		return fmt.Errorf("closing Discord ask (the bot needs Manage Threads on the forum): %w", err)
	}
	return nil
}

// TagOptions changes how TagAsk touches an archived thread.
type TagOptions struct {
	// Unarchive unarchives the thread first (a retag never posts, so it
	// needs Manage Threads, not just Send Messages in Threads), applies the
	// tags, then re-archives when the thread started archived.
	Unarchive bool
}

// TagAsk replaces the thread's applied tags with exactly names, resolved by
// name from the forum's available tags. An unknown name fails with the valid
// ones. Discord allows at most five tags per thread. Applying tags to an
// archived thread fails (Discord error 50083); pass TagOptions{Unarchive:
// true} to unarchive first and re-archive after.
func (c *Client) TagAsk(ctx context.Context, threadID string, names []string, opts TagOptions) error {
	thread, _, err := c.askThread(ctx, threadID)
	if err != nil {
		return err
	}
	var tags []string
	if _, err := c.withRefreshedForum(ctx, func(f Channel) ([]string, error) {
		var err error
		tags, err = resolveTags(f, names)
		return tags, err
	}); err != nil {
		return err
	}
	if len(tags) > maxAppliedTags {
		return fmt.Errorf("a forum post takes at most %d tags", maxAppliedTags)
	}
	if thread.ThreadMeta.Archived && !opts.Unarchive {
		return fmt.Errorf("thread %s is archived; pass -unarchive to retag it (unarchives, applies tags, re-archives)", threadID)
	}
	patch := func(archived bool) map[string]any {
		return map[string]any{"applied_tags": tags, "archived": archived}
	}
	if thread.ThreadMeta.Archived {
		if thread.ThreadMeta.Locked {
			return fmt.Errorf("thread %s is archived and locked; unlock it by hand before retagging", threadID)
		}
		if err := c.botJSONBody(ctx, http.MethodPatch, "/channels/"+url.PathEscape(threadID), patch(false), nil); err != nil {
			return fmt.Errorf("unarchiving Discord thread %s for retag (the bot needs Manage Threads on the forum): %w", threadID, err)
		}
	}
	if err := c.botJSONBody(ctx, http.MethodPatch, "/channels/"+url.PathEscape(threadID), patch(false), nil); err != nil {
		return fmt.Errorf("retagging Discord thread %s (the bot needs Manage Threads on the forum): %w", threadID, err)
	}
	if thread.ThreadMeta.Archived {
		if err := c.botJSONBody(ctx, http.MethodPatch, "/channels/"+url.PathEscape(threadID), patch(true), nil); err != nil {
			return fmt.Errorf("re-archiving Discord thread %s after retag (the bot needs Manage Threads on the forum): %w", threadID, err)
		}
	}
	return nil
}
