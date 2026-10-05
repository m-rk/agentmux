package collab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Asks are forum posts in the collaboration forum that @-mention one
// configured user. They carry the `ask` tag, which collab read and the
// digest use to keep them out of session context.
const (
	AskTagName      = "ask"
	DefaultCloseTag = "answered"
	asksUsername    = "agentmux asks"
	maxAppliedTags  = 5
)

// OutcomeTags are the mutually exclusive status tags: close replaces any of
// these already on the thread with the requested one.
var OutcomeTags = []string{"pending", "launched", "not now", "failed", "answered"}

var snowflakeRE = regexp.MustCompile(`^[0-9]{1,25}$`)

type AskMessage struct {
	ID                     string `json:"id"`
	AuthorID               string `json:"author_id"`
	AuthorName             string `json:"author_name"`
	AuthorIsConfiguredUser bool   `json:"author_is_configured_user"`
	Text                   string `json:"text"`
	Timestamp              string `json:"timestamp"`
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

func resolveTags(forum Channel, names []string) ([]string, error) {
	var ids []string
	for _, name := range names {
		id := tagID(forum.AvailableTags, name)
		if id == "" {
			return nil, fmt.Errorf("the forum has no %q tag; create it by hand (Edit Channel → Tags)", name)
		}
		if !hasTag(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
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

// PostAsk creates a forum post tagged `ask` (plus `pending` and extraTags)
// whose body opens with a mention of the configured user.
func (c *Client) PostAsk(ctx context.Context, title, body string, extraTags []string) (threadID, messageID string, err error) {
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
	forum, err := c.forum(ctx)
	if err != nil {
		return "", "", err
	}
	tags, err := resolveTags(forum, append([]string{AskTagName, "pending"}, extraTags...))
	if err != nil {
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
	var message Message
	if err := c.webhookJSON(ctx, http.MethodPost, url.Values{"wait": {"true"}}, payload, &message); err != nil {
		return "", "", fmt.Errorf("creating Discord ask: %w", err)
	}
	if message.ChannelID == "" {
		return "", "", fmt.Errorf("Discord created the ask but returned no thread id")
	}
	return message.ChannelID, message.ID, nil
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
	if thread.ParentID != c.Config.ForumChannelID || !hasTag(thread.AppliedTags, tagID(forum.AvailableTags, AskTagName)) {
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
	var out []AskMessage
	for {
		batch, err := c.Messages(ctx, threadID, after, 100)
		if err != nil {
			return nil, err
		}
		for _, m := range batch {
			out = append(out, AskMessage{
				ID:                     m.ID,
				AuthorID:               m.Author.ID,
				AuthorName:             m.Author.Username,
				AuthorIsConfiguredUser: m.WebhookID == "" && m.Author.ID == user,
				Text:                   m.Content,
				Timestamp:              m.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
			})
		}
		if len(batch) < 100 {
			return out, nil
		}
		after = batch[len(batch)-1].ID
	}
}

// CloseAsk replaces the thread's outcome tag with outcome, then archives and
// locks it. This is the one write that needs the bot (Manage Threads on the
// forum): webhooks can't edit threads.
func (c *Client) CloseAsk(ctx context.Context, threadID, outcome string) error {
	thread, forum, err := c.askThread(ctx, threadID)
	if err != nil {
		return err
	}
	if outcome == "" {
		outcome = DefaultCloseTag
	}
	known := false
	for _, name := range OutcomeTags {
		known = known || strings.EqualFold(name, outcome)
	}
	if !known {
		return fmt.Errorf("outcome tag must be one of: %s", strings.Join(OutcomeTags, ", "))
	}
	newID, err := resolveTags(forum, []string{outcome})
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, name := range OutcomeTags {
		if id := tagID(forum.AvailableTags, name); id != "" {
			drop[id] = true
		}
	}
	tags := []string{}
	for _, id := range thread.AppliedTags {
		if !drop[id] {
			tags = append(tags, id)
		}
	}
	tags = append(tags, newID...)
	body := map[string]any{"applied_tags": tags, "archived": true, "locked": true}
	if err := c.botJSONBody(ctx, http.MethodPatch, "/channels/"+url.PathEscape(threadID), body, nil); err != nil {
		return fmt.Errorf("closing Discord ask (the bot needs Manage Threads on the forum): %w", err)
	}
	return nil
}
