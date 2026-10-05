package collab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReactAsk has the bot add one reaction to a message in an ask thread, so a
// caller outside Discord (CLI, autopilot, orchestrator) can mark a posted
// ask without a person tapping anything. The ask-thread check is the same
// gate as post/reply/read/close; the reaction PUT needs the bot (Add
// Reactions on the forum) and gets the same one-retry treatment as seeding.
func (c *Client) ReactAsk(ctx context.Context, threadID, messageID, emoji string) error {
	if _, _, err := c.askThread(ctx, threadID); err != nil {
		return err
	}
	if !snowflakeRE.MatchString(messageID) {
		return fmt.Errorf("message id must be numeric")
	}
	if err := validateReactionEmoji(emoji); err != nil {
		return err
	}
	if err := c.putReaction(ctx, threadID, messageID, emoji); err != nil {
		return fmt.Errorf("adding reaction to Discord message %s (the bot needs Add Reactions on the forum): %w", messageID, err)
	}
	return nil
}

func validateReactionEmoji(emoji string) error {
	if emoji == "" || strings.ContainsAny(emoji, "/?#%\\") || strings.IndexFunc(emoji, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid reaction %q", emoji)
	}
	return nil
}

// EditAskOptions change a posted ask message in place: Body replaces the
// text (empty keeps it), and the button flags restyle the message's buttons
// without touching their labels. DisableButtons greys every button out;
// Chosen keeps that one highlighted (success style, ✓ prefix) and greys the
// rest — the same settled look a click gets from the gateway.
// Message edits need the bot (Send Messages in Threads on the forum).
type EditAskOptions struct {
	Body           string // replacement body; empty keeps the message as-is
	DisableButtons bool   // grey every button out
	Chosen         string // button label to highlight; implies DisableButtons for the rest
}

func (o EditAskOptions) validate() error {
	if o.Body != "" {
		body := strings.TrimSpace(o.Body)
		if body == "" {
			return fmt.Errorf("body is empty")
		}
		if utf8.RuneCountInString(body) > maxDiscordContentRunes {
			return fmt.Errorf("body exceeds Discord's %d-character limit", maxDiscordContentRunes)
		}
	}
	if o.Chosen != "" && strings.TrimSpace(o.Chosen) == "" {
		return fmt.Errorf("chosen button label must not be blank")
	}
	if o.Chosen != "" && utf8.RuneCountInString(o.Chosen) > maxButtonLabel {
		return fmt.Errorf("chosen button label %q must be 1-%d characters", o.Chosen, maxButtonLabel)
	}
	if o.Body == "" && !o.DisableButtons && o.Chosen == "" {
		return fmt.Errorf("nothing to change: pass a body, -disable-buttons, or -chosen")
	}
	return nil
}

// EditAsk replaces the body and/or restyles the buttons of a posted ask
// message. Only the body is rewritten — mentions are not re-added, so an
// edited body never pings anyone again.
func (c *Client) EditAsk(ctx context.Context, threadID, messageID string, opts EditAskOptions) error {
	if err := opts.validate(); err != nil {
		return err
	}
	if _, _, err := c.askThread(ctx, threadID); err != nil {
		return err
	}
	if !snowflakeRE.MatchString(messageID) {
		return fmt.Errorf("message id must be numeric")
	}
	payload := map[string]any{}
	if opts.Body != "" {
		payload["content"] = strings.TrimSpace(opts.Body)
	}
	if opts.DisableButtons || opts.Chosen != "" {
		var current struct {
			Components []map[string]any `json:"components"`
		}
		if err := c.botJSON(ctx, http.MethodGet,
			"/channels/"+url.PathEscape(threadID)+"/messages/"+url.PathEscape(messageID), &current); err != nil {
			return fmt.Errorf("reading Discord message %s: %w", messageID, err)
		}
		rows, err := settleFetchedComponents(current.Components, opts.Chosen)
		if err != nil {
			return err
		}
		payload["components"] = rows
	}
	if err := c.botJSONBody(ctx, http.MethodPatch,
		"/channels/"+url.PathEscape(threadID)+"/messages/"+url.PathEscape(messageID), payload, nil); err != nil {
		return fmt.Errorf("editing Discord message %s (the bot needs Send Messages in Threads on the forum): %w", messageID, err)
	}
	return nil
}

// settleFetchedComponents disables the buttons of a message fetched from
// Discord, highlighting chosen (\"\" disables all without a highlight).
// Buttons carry their label in `ask:<label>` custom ids, so the highlight
// matches on the label. It errors when the message has no buttons or the
// chosen label isn't among them, so a typo can't silently grey everything.
// Only ask: buttons are restyled; foreign components are left alone.
func settleFetchedComponents(rows []map[string]any, chosen string) ([]map[string]any, error) {
	const styleSuccess = 3
	matched := false
	seen := 0
	for _, row := range rows {
		buttons, _ := row["components"].([]any)
		for _, b := range buttons {
			btn, ok := b.(map[string]any)
			if !ok {
				continue
			}
			id, _ := btn["custom_id"].(string)
			if !strings.HasPrefix(id, buttonIDPrefix) {
				continue
			}
			seen++
			btn["disabled"] = true
			if chosen != "" && strings.TrimPrefix(id, buttonIDPrefix) == chosen {
				matched = true
				btn["style"] = styleSuccess
				if label, ok := btn["label"].(string); ok {
					btn["label"] = "✓ " + label
				}
			}
		}
	}
	if seen == 0 {
		return nil, fmt.Errorf("message has no buttons to settle")
	}
	if chosen != "" && !matched {
		return nil, fmt.Errorf("message has no button labelled %q", chosen)
	}
	return rows, nil
}
