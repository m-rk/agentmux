// Package collab provides agentmux's Discord-backed collaboration protocol.
// Discord is the shared transport; project routing, message limits, session
// identity, and delivery state remain deterministic local policy.
package collab

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxThreadSummaryRunes  = 240
	maxInlineReplyWords    = 200
	maxDiscordContentRunes = 2000
	maxMarkdownBytes       = 512 * 1024
	maxPromptRunes         = 6000
)

var projectKeyRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

type Identity struct {
	Instance  string
	Host      string
	Agent     string
	AvatarURL string
}

func (i Identity) DisplayName() string {
	name := strings.TrimSpace(i.Instance) + " · " + strings.TrimSpace(i.Host)
	if utf8.RuneCountInString(name) <= 80 {
		return name
	}
	runes := []rune(name)
	return string(runes[:79]) + "…"
}

// Address is intentionally not a Discord user mention. It is a stable,
// human-typeable session address that can be recognized across hosts.
func (i Identity) Address() string {
	return "@" + strings.TrimSpace(i.Instance) + "@" + strings.TrimSpace(i.Host)
}

func ValidateProjectKey(project string) error {
	if project == "" {
		return fmt.Errorf("project key is empty")
	}
	if len(project) > 64 || !projectKeyRE.MatchString(project) || strings.Contains(project, "..") {
		return fmt.Errorf("project key must be at most 64 letters, numbers, dots, slashes, underscores, or hyphens")
	}
	return nil
}

func ValidateAvatarURL(value string) error {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("avatar URL must be an absolute https URL")
	}
	return nil
}

func ProjectThreadName(project, topic string, shared bool) (string, error) {
	if err := ValidateProjectKey(project); err != nil {
		return "", err
	}
	topic = cleanOneLine(topic)
	if topic == "" {
		return "", fmt.Errorf("topic is required")
	}
	prefix := "[" + project + "] "
	if shared {
		prefix = "[shared " + project + "] "
	}
	name := prefix + topic
	if utf8.RuneCountInString(name) > 100 {
		return "", fmt.Errorf("thread name is longer than Discord's 100-character limit")
	}
	return name, nil
}

func ThreadRelevant(name, project string) bool {
	return strings.HasPrefix(name, "["+project+"] ") || strings.HasPrefix(name, "[shared ")
}

func ValidateThreadSummary(summary string) error {
	if strings.ContainsAny(summary, "\r\n") || strings.TrimSpace(summary) != summary || summary == "" {
		return fmt.Errorf("a new thread summary must be one non-empty line")
	}
	if utf8.RuneCountInString(summary) > maxThreadSummaryRunes {
		return fmt.Errorf("a new thread summary must be at most %d characters", maxThreadSummaryRunes)
	}
	trimmed := strings.TrimRight(summary, `"')]} `)
	last, _ := utf8.DecodeLastRuneInString(trimmed)
	if trimmed == "" || !strings.ContainsRune(".?!", last) {
		return fmt.Errorf("a new thread summary must be a complete single sentence ending in punctuation")
	}
	return nil
}

func ValidateInlineReply(summary string) error {
	if cleanOneLine(summary) == "" {
		return fmt.Errorf("reply summary is required")
	}
	if len(strings.Fields(summary)) > maxInlineReplyWords {
		return fmt.Errorf("inline replies are limited to %d words; attach Markdown for longer detail", maxInlineReplyWords)
	}
	if utf8.RuneCountInString(cleanOneLine(summary)) > maxDiscordContentRunes {
		return fmt.Errorf("inline reply exceeds Discord's %d-character limit; attach Markdown for longer detail", maxDiscordContentRunes)
	}
	return nil
}

func cleanOneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…"
}
