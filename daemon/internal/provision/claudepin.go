package provision

import (
	"fmt"
	"regexp"
	"strings"
)

// Registry keys holding a claude-code instance's pinned model and effort.
// They ride `claude --model/--effort` on every launch (including restarts),
// so a pin survives a restart instead of falling back to the template's or
// the account's default.
const (
	ClaudeModelKey  = "AGENTMUX_CLAUDE_MODEL"
	ClaudeEffortKey = "AGENTMUX_CLAUDE_EFFORT"
)

var (
	// A claude model is an alias (opus, sonnet, haiku, with an optional
	// [1m] suffix) or a full id (claude-<family>-<version>...). Anything
	// else is refused up front rather than left for claude to reject
	// inside a detached tmux pane, or to fall back silently.
	claudeAliasRE = regexp.MustCompile(`^(opus|sonnet|haiku|best|opusplan)(\[1m\])?$`)
	claudeIDRE    = regexp.MustCompile(`^claude-[a-z0-9][a-z0-9.-]{0,63}(\[1m\])?$`)
)

// claudeEfforts are the levels `claude --effort` accepts.
var claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// CleanClaudeModel validates a pinned claude model; empty means no pin.
func CleanClaudeModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || claudeAliasRE.MatchString(model) || claudeIDRE.MatchString(model) {
		return model, nil
	}
	return "", fmt.Errorf("unsupported claude model %q: want an alias such as opus or sonnet, or a full id such as claude-<family>-<version>", model)
}

// CleanClaudeEffort validates a pinned claude effort; empty means no pin.
func CleanClaudeEffort(effort string) (string, error) {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return "", nil
	}
	for _, e := range claudeEfforts {
		if effort == e {
			return effort, nil
		}
	}
	return "", fmt.Errorf("unsupported claude effort %q: want one of %s", effort, strings.Join(claudeEfforts, ", "))
}

// ClaudeModelMatches reports whether the model a session actually ran
// satisfies a pin: an exact id, an alias naming the id's family, or the
// pinned id as a prefix of a dated id.
func ClaudeModelMatches(pin, actual string) bool {
	pin, actual = strings.TrimSuffix(pin, "[1m]"), strings.TrimSpace(actual)
	switch {
	case pin == "" || actual == "":
		return true
	case pin == actual, strings.HasPrefix(actual, pin+"-"):
		return true
	case claudeAliasRE.MatchString(pin):
		return strings.Contains(actual, "-"+pin+"-") || strings.HasPrefix(actual, "claude-"+pin)
	}
	return false
}
