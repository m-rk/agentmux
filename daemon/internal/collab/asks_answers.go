package collab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// One-tap answers to an ask: emoji reactions the bot seeds and the
// configured user taps, or buttons the bot posts and a gateway listener
// (`agentmux asks serve`) records. Both surface in `asks read` as
// AskMessage.Answers.

const (
	maxReactions    = 20
	maxButtons      = 25
	maxButtonLabel  = 80
	buttonIDPrefix  = "ask:"
	buttonsPerRow   = 5
	buttonStyleGrey = 2
)

// AskOptions are the optional one-tap answer choices on a posted ask.
type AskOptions struct {
	Reactions []string // emoji the bot adds, in order
	Buttons   []string // button labels, in order
}

// ReactionSeedError means the ask was posted but adding the seed reactions
// failed (typically the bot lacks Add Reactions).
type ReactionSeedError struct{ Err error }

func (e *ReactionSeedError) Error() string {
	return "ask posted but seeding reactions failed (the bot needs Add Reactions on the forum): " + e.Err.Error()
}
func (e *ReactionSeedError) Unwrap() error { return e.Err }

func (o AskOptions) validate() error {
	if len(o.Reactions) > maxReactions {
		return fmt.Errorf("at most %d reactions", maxReactions)
	}
	seen := map[string]bool{}
	for _, r := range o.Reactions {
		if r == "" || strings.ContainsAny(r, "/?#%\\") || strings.IndexFunc(r, unicode.IsSpace) >= 0 {
			return fmt.Errorf("invalid reaction %q", r)
		}
		if seen[r] {
			return fmt.Errorf("duplicate reaction %q", r)
		}
		seen[r] = true
	}
	if len(o.Buttons) > maxButtons {
		return fmt.Errorf("at most %d buttons", maxButtons)
	}
	seen = map[string]bool{}
	for _, b := range o.Buttons {
		if strings.TrimSpace(b) == "" || utf8.RuneCountInString(b) > maxButtonLabel {
			return fmt.Errorf("button label %q must be 1-%d characters", b, maxButtonLabel)
		}
		if seen[b] {
			return fmt.Errorf("duplicate button %q", b)
		}
		seen[b] = true
	}
	return nil
}

// buttonRows lays labels out as action rows of buttons. The custom id carries
// the label, so a click is self-describing.
func buttonRows(labels []string) []map[string]any {
	var rows []map[string]any
	for i := 0; i < len(labels); i += buttonsPerRow {
		end := min(i+buttonsPerRow, len(labels))
		var buttons []map[string]any
		for _, label := range labels[i:end] {
			buttons = append(buttons, map[string]any{"type": 2, "style": buttonStyleGrey, "label": label, "custom_id": buttonIDPrefix + label})
		}
		rows = append(rows, map[string]any{"type": 1, "components": buttons})
	}
	return rows
}

func emojiPath(channelID, messageID, emoji string) string {
	return "/channels/" + url.PathEscape(channelID) + "/messages/" + url.PathEscape(messageID) + "/reactions/" + url.PathEscape(emoji)
}

func (c *Client) seedReactions(ctx context.Context, channelID, messageID string, emojis []string) error {
	for _, emoji := range emojis {
		if err := c.putReaction(ctx, channelID, messageID, emoji); err != nil {
			return &ReactionSeedError{Err: err}
		}
	}
	return nil
}

// putReaction retries once when Discord's reaction rate limit (about one per
// quarter second) answers 429.
func (c *Client) putReaction(ctx context.Context, channelID, messageID, emoji string) error {
	path := emojiPath(channelID, messageID, emoji) + "/@me"
	err := c.botJSONBody(ctx, http.MethodPut, path, nil, nil)
	var api *apiError
	if errors.As(err, &api) && api.Status == http.StatusTooManyRequests {
		var rl struct {
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal([]byte(api.Body), &rl)
		wait := time.Duration(rl.RetryAfter*float64(time.Second)) + 50*time.Millisecond
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		err = c.botJSONBody(ctx, http.MethodPut, path, nil, nil)
	}
	return err
}

func (c *Client) message(ctx context.Context, channelID, messageID string) (Message, error) {
	var m Message
	if !snowflakeRE.MatchString(messageID) {
		return m, fmt.Errorf("message id must be numeric")
	}
	err := c.botJSON(ctx, http.MethodGet, "/channels/"+url.PathEscape(channelID)+"/messages/"+url.PathEscape(messageID), &m)
	return m, err
}

// userReactions returns the configured user's reactions on m. The bot's own
// seed reactions are ignored, and the per-emoji reactor lookup is skipped
// when the bot is the only reactor.
func (c *Client) userReactions(ctx context.Context, channelID string, m Message, user string) ([]AskAnswer, error) {
	var out []AskAnswer
	for _, r := range m.Reactions {
		others := r.Count
		if r.Me {
			others--
		}
		if others <= 0 || r.Emoji.Name == "" {
			continue
		}
		id := r.Emoji.Name
		if r.Emoji.ID != "" {
			id += ":" + r.Emoji.ID
		}
		var users []Author
		if err := c.botJSON(ctx, http.MethodGet, emojiPath(channelID, m.ID, id)+"?limit=100", &users); err != nil {
			return nil, fmt.Errorf("reading reactions on message %s: %w", m.ID, err)
		}
		for _, u := range users {
			if u.ID == user {
				out = append(out, AskAnswer{Kind: "reaction", Value: r.Emoji.Name, MessageID: m.ID})
				break
			}
		}
	}
	return out, nil
}

// Click is one recorded button press.
type Click struct {
	ThreadID  string    `json:"thread_id"`
	MessageID string    `json:"message_id"`
	Label     string    `json:"label"`
	UserID    string    `json:"user_id"`
	Timestamp time.Time `json:"timestamp"`
}

// ClickStore is an append-only JSONL file shared by the listener (writer)
// and `asks read` (reader). The first click on a message wins.
type ClickStore struct {
	Path string
	mu   sync.Mutex
}

func DefaultClicksPath(home string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "asks", "clicks.jsonl")
}

func (s *ClickStore) load() ([]Click, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var clicks []Click
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c Click
		if json.Unmarshal(sc.Bytes(), &c) == nil && c.MessageID != "" {
			clicks = append(clicks, c)
		}
	}
	return clicks, sc.Err()
}

// Append records c, returning false (and writing nothing) when the message
// already has a click.
func (s *ClickStore) Append(c Click) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.load()
	if err != nil {
		return false, err
	}
	for _, e := range existing {
		if e.MessageID == c.MessageID {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	line, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) clicksFor(threadID, user string) (map[string][]Click, error) {
	if c.ClicksPath == "" {
		return nil, nil
	}
	all, err := (&ClickStore{Path: c.ClicksPath}).load()
	if err != nil {
		return nil, fmt.Errorf("reading recorded button clicks: %w", err)
	}
	out := map[string][]Click{}
	for _, click := range all {
		if click.ThreadID == threadID && click.UserID == user {
			out[click.MessageID] = append(out[click.MessageID], click)
		}
	}
	return out, nil
}
