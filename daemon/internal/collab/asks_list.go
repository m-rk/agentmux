package collab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AskThread is one asks-forum thread: the fields `asks list` shows.
type AskThread struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Tags            []string `json:"tags"`
	Created         string   `json:"created,omitempty"`
	LastMessageID   string   `json:"last_message_id,omitempty"`
	LastMessageTime string   `json:"last_message_time,omitempty"`
	Archived        bool     `json:"archived"`
	Locked          bool     `json:"locked"`
	StarterMessage  string   `json:"starter_message_id,omitempty"`
}

// ListAsksOptions filters which forum threads come back.
type ListAsksOptions struct {
	// State is "open" (default), "archived", or "all". Anything else fails.
	State string
	// Tag keeps only threads carrying this tag (matched by name).
	Tag string
	// Since keeps only threads with activity at or after this time.
	// Zero means no cutoff.
	Since time.Time
}

// ListAsks returns the asks forum's threads: active threads (the guild's
// active list, filtered to the forum) and archived ones (the forum's public
// archived list, paginated to the end). Reads use the bot's existing View
// Channels and Read Message History; no write permission is needed.
func (c *Client) ListAsks(ctx context.Context, opts ListAsksOptions) ([]AskThread, error) {
	switch opts.State {
	case "", "open":
		opts.State = "open"
	case "archived", "all":
	default:
		return nil, fmt.Errorf("state must be open, archived, or all")
	}
	forum, err := c.forum(ctx)
	if err != nil {
		return nil, err
	}
	if forum.GuildID == "" {
		return nil, fmt.Errorf("Discord forum channel has no guild id")
	}
	names := map[string]string{}
	for _, t := range forum.AvailableTags {
		names[t.ID] = t.Name
	}
	var wantTag string
	if opts.Tag != "" {
		wantTag = tagID(forum.AvailableTags, opts.Tag)
		if wantTag == "" {
			return nil, fmt.Errorf("the forum has no %q tag; create it by hand (Edit Channel → Tags)", opts.Tag)
		}
	}

	var chans []Channel
	if opts.State == "open" || opts.State == "all" {
		var active threadList
		if err := c.listJSON(ctx, http.MethodGet, "/guilds/"+url.PathEscape(forum.GuildID)+"/threads/active", &active); err != nil {
			return nil, fmt.Errorf("listing active Discord threads: %w", err)
		}
		for _, th := range active.Threads {
			if th.ParentID != c.Config.ForumChannelID {
				continue
			}
			chans = append(chans, th)
		}
	}
	if opts.State == "archived" || opts.State == "all" {
		before := ""
		for {
			path := "/channels/" + url.PathEscape(c.Config.ForumChannelID) + "/threads/archived/public?limit=100"
			if before != "" {
				path += "&before=" + url.QueryEscape(before)
			}
			var page threadList
			if err := c.listJSON(ctx, http.MethodGet, path, &page); err != nil {
				return nil, fmt.Errorf("listing archived Discord threads: %w", err)
			}
			chans = append(chans, page.Threads...)
			if !page.HasMore || len(page.Threads) == 0 {
				break
			}
			before = page.Threads[len(page.Threads)-1].ThreadMeta.ArchiveTimestamp
			if before == "" {
				break
			}
		}
	}

	seen := map[string]bool{}
	out := []AskThread{}
	for _, th := range chans {
		if th.ParentID != c.Config.ForumChannelID {
			continue
		}
		if seen[th.ID] {
			continue
		}
		seen[th.ID] = true
		if !isAskThread(forum, th) {
			continue
		}
		if wantTag != "" && !hasTag(th.AppliedTags, wantTag) {
			continue
		}
		archived := th.ThreadMeta.Archived
		if opts.State == "open" && archived {
			continue
		}
		if opts.State == "archived" && !archived {
			continue
		}
		created := th.ThreadMeta.CreateTimestamp
		if created == "" {
			created = snowflakeTime(th.ID)
		}
		lastTime := snowflakeTime(th.LastMessageID)
		// Freshness is last-message time when known, else creation.
		stamp := created
		if lastTime != "" {
			stamp = lastTime
		}
		if !opts.Since.IsZero() {
			if t, err := time.Parse(time.RFC3339, stamp); err != nil || t.Before(opts.Since) {
				continue
			}
		}
		var tags []string
		for _, id := range th.AppliedTags {
			if name, ok := names[id]; ok {
				tags = append(tags, name)
			} else {
				tags = append(tags, id)
			}
		}
		sort.Strings(tags)
		thread := AskThread{
			ID:              th.ID,
			Title:           th.Name,
			Tags:            tags,
			Created:         created,
			LastMessageID:   th.LastMessageID,
			LastMessageTime: lastTime,
			Archived:        archived,
			Locked:          th.ThreadMeta.Locked,
		}
		if th.ID != "" {
			thread.StarterMessage = th.ID
		}
		out = append(out, thread)
	}
	// Newest first by thread id (snowflakes grow with time).
	sort.Slice(out, func(i, j int) bool { return snowflakeGreater(out[i].ID, out[j].ID) })
	return out, nil
}

// listJSON is botJSON with one retry when Discord answers 429: a paged
// archived-threads walk is exactly the burst a rate limit would cut short.
func (c *Client) listJSON(ctx context.Context, method, path string, out any) error {
	err := c.botJSON(ctx, method, path, out)
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
		if wait < 0 {
			wait = 0
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		err = c.botJSON(ctx, method, path, out)
	}
	return err
}

// snowflakeTime renders a Discord snowflake id as an RFC3339 timestamp, or
// "" when the id isn't numeric. Discord epoch is 2015-01-01T00:00:00Z.
func snowflakeTime(id string) string {
	n, err := strconv.ParseUint(strings.TrimSpace(id), 10, 64)
	if err != nil || n < 1<<22 {
		return ""
	}
	ms := int64(n>>22) + 1420070400000
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
