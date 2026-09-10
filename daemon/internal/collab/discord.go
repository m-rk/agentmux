package collab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
)

const discordAPIBaseURL = "https://discord.com/api/v10"

type Client struct {
	Config     discordnotify.CollaborationConfig
	HTTPClient *http.Client
	APIBaseURL string
}

type Channel struct {
	ID            string         `json:"id"`
	GuildID       string         `json:"guild_id"`
	ParentID      string         `json:"parent_id"`
	Name          string         `json:"name"`
	Type          int            `json:"type"`
	LastMessageID string         `json:"last_message_id"`
	ThreadMeta    ThreadMetadata `json:"thread_metadata"`
}

type ThreadMetadata struct {
	Archived         bool   `json:"archived"`
	ArchiveTimestamp string `json:"archive_timestamp"`
}

type Author struct {
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type Attachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
	Size     int64  `json:"size"`
}

type Message struct {
	ID          string       `json:"id"`
	ChannelID   string       `json:"channel_id"`
	Content     string       `json:"content"`
	Author      Author       `json:"author"`
	WebhookID   string       `json:"webhook_id"`
	Timestamp   time.Time    `json:"timestamp"`
	Attachments []Attachment `json:"attachments"`
}

type threadList struct {
	Threads []Channel `json:"threads"`
	HasMore bool      `json:"has_more"`
}

type webhookInfo struct {
	ChannelID string `json:"channel_id"`
}

func NewClient(cfg discordnotify.CollaborationConfig) *Client {
	return &Client{
		Config:     cfg,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		APIBaseURL: discordAPIBaseURL,
	}
}

func (c *Client) Validate(ctx context.Context) error {
	if !c.Config.Configured() {
		return fmt.Errorf("Discord collaboration needs a bot token, forum channel id, and webhook URL")
	}
	var forum Channel
	if err := c.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(c.Config.ForumChannelID), &forum); err != nil {
		return fmt.Errorf("reading Discord forum channel: %w", err)
	}
	const guildForum = 15
	if forum.Type != guildForum {
		return fmt.Errorf("Discord channel %s is not a forum channel", c.Config.ForumChannelID)
	}
	var webhook webhookInfo
	if err := c.webhookJSON(ctx, http.MethodGet, nil, nil, &webhook); err != nil {
		return fmt.Errorf("reading Discord collaboration webhook: %w", err)
	}
	if webhook.ChannelID != c.Config.ForumChannelID {
		return fmt.Errorf("Discord webhook belongs to channel %s, not configured forum %s", webhook.ChannelID, c.Config.ForumChannelID)
	}
	return nil
}

func (c *Client) ListRelevantThreads(ctx context.Context, project string) ([]Channel, error) {
	if err := ValidateProjectKey(project); err != nil {
		return nil, err
	}
	var forum Channel
	if err := c.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(c.Config.ForumChannelID), &forum); err != nil {
		return nil, fmt.Errorf("reading Discord forum channel: %w", err)
	}
	if forum.GuildID == "" {
		return nil, fmt.Errorf("Discord forum channel has no guild id")
	}

	var active threadList
	if err := c.botJSON(ctx, http.MethodGet, "/guilds/"+url.PathEscape(forum.GuildID)+"/threads/active", &active); err != nil {
		return nil, fmt.Errorf("listing active Discord threads: %w", err)
	}
	var archived threadList
	if err := c.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(c.Config.ForumChannelID)+"/threads/archived/public?limit=100", &archived); err != nil {
		return nil, fmt.Errorf("listing archived Discord threads: %w", err)
	}

	byID := map[string]Channel{}
	for _, thread := range append(active.Threads, archived.Threads...) {
		if thread.ParentID == c.Config.ForumChannelID && ThreadRelevant(thread.Name, project) {
			byID[thread.ID] = thread
		}
	}
	threads := make([]Channel, 0, len(byID))
	for _, thread := range byID {
		threads = append(threads, thread)
	}
	sort.Slice(threads, func(i, j int) bool { return snowflakeGreater(threads[i].ID, threads[j].ID) })
	return threads, nil
}

// RelevantThread resolves a thread directly so explicit read/reply commands
// still work for topics older than the recent archived-thread listing.
func (c *Client) RelevantThread(ctx context.Context, threadID, project string) (Channel, error) {
	if err := ValidateProjectKey(project); err != nil {
		return Channel{}, err
	}
	var thread Channel
	if err := c.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(threadID), &thread); err != nil {
		return Channel{}, fmt.Errorf("reading Discord thread %s: %w", threadID, err)
	}
	if thread.ParentID != c.Config.ForumChannelID || !ThreadRelevant(thread.Name, project) {
		return Channel{}, fmt.Errorf("thread %s isn't relevant to project %s", threadID, project)
	}
	return thread, nil
}

func (c *Client) Messages(ctx context.Context, threadID, after string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != "" {
		query.Set("after", after)
	}
	var messages []Message
	path := "/channels/" + url.PathEscape(threadID) + "/messages?" + query.Encode()
	if err := c.botJSON(ctx, http.MethodGet, path, &messages); err != nil {
		return nil, fmt.Errorf("reading Discord thread %s: %w", threadID, err)
	}
	sort.Slice(messages, func(i, j int) bool { return snowflakeGreater(messages[j].ID, messages[i].ID) })
	return messages, nil
}

func (c *Client) CreateThread(ctx context.Context, identity Identity, project, topic, summary string, shared bool) (Message, error) {
	name, err := ProjectThreadName(project, topic, shared)
	if err != nil {
		return Message{}, err
	}
	if err := ValidateThreadSummary(summary); err != nil {
		return Message{}, err
	}
	payload := map[string]any{
		"content":          summary,
		"thread_name":      name,
		"username":         identity.DisplayName(),
		"avatar_url":       identity.AvatarURL,
		"allowed_mentions": map[string]any{"parse": []string{}},
	}
	if identity.AvatarURL == "" {
		delete(payload, "avatar_url")
	}
	var message Message
	if err := c.webhookJSON(ctx, http.MethodPost, url.Values{"wait": {"true"}}, payload, &message); err != nil {
		return Message{}, fmt.Errorf("creating Discord collaboration thread: %w", err)
	}
	return message, nil
}

func (c *Client) Reply(ctx context.Context, identity Identity, threadID, summary, markdownPath string) (Message, error) {
	if err := ValidateInlineReply(summary); err != nil {
		return Message{}, err
	}
	query := url.Values{"wait": {"true"}, "thread_id": {threadID}}
	if markdownPath == "" {
		payload := map[string]any{
			"content":          cleanOneLine(summary),
			"username":         identity.DisplayName(),
			"allowed_mentions": map[string]any{"parse": []string{}},
		}
		if identity.AvatarURL != "" {
			payload["avatar_url"] = identity.AvatarURL
		}
		var message Message
		if err := c.webhookJSON(ctx, http.MethodPost, query, payload, &message); err != nil {
			return Message{}, fmt.Errorf("replying to Discord collaboration thread: %w", err)
		}
		return message, nil
	}
	return c.replyWithMarkdown(ctx, identity, threadID, summary, markdownPath)
}

func (c *Client) replyWithMarkdown(ctx context.Context, identity Identity, threadID, summary, markdownPath string) (Message, error) {
	if strings.ToLower(filepath.Ext(markdownPath)) != ".md" {
		return Message{}, fmt.Errorf("collaboration attachments must be Markdown files")
	}
	info, err := os.Stat(markdownPath)
	if err != nil {
		return Message{}, fmt.Errorf("reading Markdown attachment: %w", err)
	}
	if info.Size() > maxMarkdownBytes {
		return Message{}, fmt.Errorf("Markdown attachment exceeds agentmux's %d-byte limit", maxMarkdownBytes)
	}
	data, err := os.ReadFile(markdownPath)
	if err != nil {
		return Message{}, fmt.Errorf("reading Markdown attachment: %w", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	payload := map[string]any{
		"content":          cleanOneLine(summary),
		"username":         identity.DisplayName(),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"attachments":      []map[string]any{{"id": "0", "filename": filepath.Base(markdownPath)}},
	}
	if identity.AvatarURL != "" {
		payload["avatar_url"] = identity.AvatarURL
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	if err := writer.WriteField("payload_json", string(payloadJSON)); err != nil {
		return Message{}, err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[0]"; filename=%q`, filepath.Base(markdownPath)))
	header.Set("Content-Type", "text/markdown; charset=utf-8")
	part, err := writer.CreatePart(header)
	if err != nil {
		return Message{}, err
	}
	if _, err := part.Write(data); err != nil {
		return Message{}, err
	}
	if err := writer.Close(); err != nil {
		return Message{}, err
	}

	endpoint, err := c.webhookEndpoint(url.Values{"wait": {"true"}, "thread_id": {threadID}})
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	var message Message
	if err := c.doJSON(req, &message); err != nil {
		return Message{}, fmt.Errorf("replying with Markdown to Discord collaboration thread: %w", err)
	}
	return message, nil
}

func (c *Client) AttachmentMarkdown(ctx context.Context, attachment Attachment) (string, error) {
	if strings.ToLower(filepath.Ext(attachment.Filename)) != ".md" {
		return "", fmt.Errorf("attachment %s is not Markdown", attachment.Filename)
	}
	if attachment.Size > maxMarkdownBytes {
		return "", fmt.Errorf("attachment %s exceeds agentmux's %d-byte limit", attachment.Filename, maxMarkdownBytes)
	}
	attachmentURL, err := url.Parse(attachment.URL)
	if err != nil || attachmentURL.Scheme != "https" || attachmentURL.Host == "" {
		return "", fmt.Errorf("attachment %s has an invalid or insecure URL", attachment.Filename)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, attachment.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("Discord attachment returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMarkdownBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxMarkdownBytes {
		return "", fmt.Errorf("attachment %s exceeds agentmux's %d-byte limit", attachment.Filename, maxMarkdownBytes)
	}
	return string(data), nil
}

func (c *Client) botJSON(ctx context.Context, method, path string, out any) error {
	base := strings.TrimRight(c.APIBaseURL, "/")
	if base == "" {
		base = discordAPIBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+c.Config.BotToken)
	return c.doJSON(req, out)
}

func (c *Client) webhookJSON(ctx context.Context, method string, query url.Values, payload, out any) error {
	endpoint, err := c.webhookEndpoint(query)
	if err != nil {
		return err
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req, out)
}

func (c *Client) webhookEndpoint(query url.Values) (string, error) {
	u, err := url.Parse(c.Config.WebhookURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid Discord collaboration webhook URL")
	}
	values := u.Query()
	for key, entries := range query {
		for _, value := range entries {
			values.Set(key, value)
		}
	}
	u.RawQuery = values.Encode()
	return u.String(), nil
}

func (c *Client) doJSON(req *http.Request, out any) error {
	req.Header.Set("User-Agent", "agentmux/discord-collaboration")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("Discord returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding Discord response: %w", err)
	}
	return nil
}

func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func snowflakeGreater(a, b string) bool {
	ai, aErr := strconv.ParseUint(a, 10, 64)
	bi, bErr := strconv.ParseUint(b, 10, 64)
	if aErr == nil && bErr == nil {
		return ai > bi
	}
	return a > b
}
