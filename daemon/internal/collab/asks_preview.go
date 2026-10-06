package collab

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AskPreview is the Discord payload `-dry-run` prints for an ask post or
// reply instead of sending it, so agents can check the real rendering
// (mention placement, tags, buttons) without touching live Discord.
type AskPreview struct {
	Kind            string         `json:"kind"` // "post", "post-in-thread", "reply"
	Title           string         `json:"title,omitempty"`
	Content         string         `json:"content"`
	ThreadID        string         `json:"thread_id,omitempty"`
	Tags            []string       `json:"tags,omitempty"`
	MentionUserID   string         `json:"mention_user_id,omitempty"`
	AllowedMentions map[string]any `json:"allowed_mentions,omitempty"`
	Options         AskOptions     `json:"options,omitempty"`
}

// Format renders the preview the way the CLI prints it.
func (p AskPreview) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dry-run: would send %s", p.Kind)
	if p.ThreadID != "" {
		fmt.Fprintf(&b, " to thread %s", p.ThreadID)
	}
	b.WriteString("\n")
	if p.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", p.Title)
	}
	if len(p.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(p.Tags, ", "))
	}
	fmt.Fprintf(&b, "content:\n%s\n", p.Content)
	if len(p.Options.Buttons) > 0 {
		labels := make([]string, 0, len(p.Options.Buttons))
		for _, btn := range p.Options.Buttons {
			labels = append(labels, btn.Label)
		}
		fmt.Fprintf(&b, "buttons: %s\n", strings.Join(labels, ", "))
	}
	if len(p.Options.Reactions) > 0 {
		fmt.Fprintf(&b, "reactions: %s\n", strings.Join(p.Options.Reactions, " "))
	}
	return b.String()
}

// Marshal renders the preview as indented JSON for -json output.
func (p AskPreview) Marshal() (string, error) {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// PreviewPost builds the payload `asks post` would send: the same title
// cleaning, mention content and tag list as the live path, resolved against
// the configured forum tags without any network call beyond reading them.
// forumTags are the forum's available tag names; unknown names fail the same
// way the live post would.
func (c *Client) PreviewPost(title, body string, extraTags []string, opts AskOptions, forumTags []string) (AskPreview, error) {
	if err := opts.validate(); err != nil {
		return AskPreview{}, err
	}
	user, err := c.mentionUser()
	if err != nil {
		return AskPreview{}, err
	}
	clean := cleanOneLine(title)
	if clean == "" {
		return AskPreview{}, fmt.Errorf("title is required")
	}
	content, err := askContent(user, true, body)
	if err != nil {
		return AskPreview{}, err
	}
	forum := Channel{AvailableTags: nil}
	for _, name := range forumTags {
		forum.AvailableTags = append(forum.AvailableTags, ForumTag{ID: "preview-" + name, Name: name})
	}
	tags, err := swapStateTags(forum, nil, "task", extraTags)
	if err != nil {
		return AskPreview{}, err
	}
	if !hasStateTag(forum, tags) {
		tags, err = swapStateTags(forum, tags, DefaultOpenTag, nil)
		if err != nil {
			return AskPreview{}, err
		}
	}
	names := make([]string, 0, len(tags))
	byID := map[string]string{}
	for _, t := range forum.AvailableTags {
		byID[t.ID] = t.Name
	}
	for _, id := range tags {
		names = append(names, byID[id])
	}
	tags = names
	return AskPreview{
		Kind:            "post",
		Title:           clean,
		Content:         content,
		Tags:            tags,
		MentionUserID:   user,
		AllowedMentions: c.mentionPayload(user, true),
		Options:         opts,
	}, nil
}

// PreviewReply builds the payload `asks reply` would send.
func (c *Client) PreviewReply(threadID, body string, mention bool) (AskPreview, error) {
	user := ""
	if mention {
		var err error
		if user, err = c.mentionUser(); err != nil {
			return AskPreview{}, err
		}
	}
	content, err := askContent(user, mention, body)
	if err != nil {
		return AskPreview{}, err
	}
	p := AskPreview{Kind: "reply", Content: content, ThreadID: threadID}
	if mention {
		p.MentionUserID = user
		p.AllowedMentions = c.mentionPayload(user, true)
	} else {
		p.AllowedMentions = c.mentionPayload("", false)
	}
	return p, nil
}
