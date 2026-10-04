// Package safesend holds the checks and records around `agentmux sessions
// send`: whether a session can take a message right now, the provenance
// prefix every relayed message carries, and the audit log. It is gateway
// phase 3; see docs/design/gateway.md ("Send is raw keystrokes").
package safesend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Reason is a stable, machine-readable refusal code. Callers branch on it,
// so existing values never change meaning.
type Reason string

const (
	ReasonInvalid     Reason = "invalid"     // bad address, provenance, or text
	ReasonNotFound    Reason = "not_found"   // no such instance, or no such thread
	ReasonNotLocal    Reason = "not_local"   // another host; needs the gateway
	ReasonDead        Reason = "dead"        // instance has no live session
	ReasonBusy        Reason = "busy"        // mid-turn
	ReasonPrompt      Reason = "prompt"      // showing a choice/permission prompt; needs a person
	ReasonDraft       Reason = "draft"       // someone has unsent text in the input box
	ReasonUnsupported Reason = "unsupported" // agent has no safe send path
	ReasonFailed      Reason = "failed"      // delivery itself failed
	// Gateway only.
	ReasonForbidden   Reason = "forbidden"    // caller lacks the capability for this op/session
	ReasonRateLimited Reason = "rate_limited" // caller is over its send rate
)

// Retryable reports whether the same send may succeed later without anyone
// intervening. A prompt or draft needs a person first.
func (r Reason) Retryable() bool {
	return r == ReasonBusy || r == ReasonFailed || r == ReasonRateLimited
}

// State is what a session's pane says about taking input.
type State string

const (
	StateReady   State = "ready"
	StateBusy    State = "busy"
	StatePrompt  State = "prompt"
	StateDraft   State = "draft"
	StateUnknown State = "unknown"
)

var (
	// Footer text each TUI shows only while a turn runs.
	busyMarkers = map[string][]string{
		"claude-code": {"esc to interrupt"},
		"opencode":    {"esc interrupt"},
		"kilo":        {"esc interrupt"},
	}
	// A numbered choice menu with the cursor on it, as Claude Code draws
	// permission and plan prompts.
	claudeMenu = regexp.MustCompile(`(?m)^\s*❯\s+\d+\.\s`)
	// Prompt text looked for only near the bottom, since the same words turn
	// up in ordinary replies ("Do you want to ...").
	footerPromptText = []string{"esc to cancel"}
	dialogPromptText = map[string][]string{
		"opencode": {"permission required", "allow once", "allow always"},
		"kilo":     {"permission required", "allow once", "allow always"},
	}
	// Claude Code's input line; a placeholder suggestion starts with Try ".
	claudeInput = regexp.MustCompile(`(?m)^❯ ?(.*)$`)
)

// footerLines and promptLines bound how far up the pane the markers are
// looked for, so the same words in the conversation above don't count.
const (
	footerLines = 6
	dialogLines = 12
	promptLines = 25
)

// Classify reads a pane snapshot. Unknown means the agent has no markers
// here; callers treat it as not ready.
func Classify(agent, pane string) State {
	markers, ok := busyMarkers[agent]
	if !ok {
		return StateUnknown
	}
	// A capture taken with escapes keeps styling only for the input line;
	// everything else is matched on plain text.
	styled := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	lines := make([]string, len(styled))
	for i, l := range styled {
		lines[i] = ansiSeq.ReplaceAllString(l, "")
	}
	footer := strings.ToLower(lastLines(lines, footerLines))
	for _, m := range markers {
		if strings.Contains(footer, m) {
			return StateBusy
		}
	}
	bottom := lastLines(lines, promptLines)
	if agent == "claude-code" && claudeMenu.MatchString(bottom) {
		return StatePrompt
	}
	for _, p := range footerPromptText {
		if strings.Contains(footer, p) {
			return StatePrompt
		}
	}
	dialog := strings.ToLower(lastLines(lines, dialogLines))
	for _, p := range dialogPromptText[agent] {
		if strings.Contains(dialog, p) {
			return StatePrompt
		}
	}
	if agent == "claude-code" {
		if m := claudeInput.FindAllStringSubmatch(bottom, -1); len(m) > 0 {
			draft := strings.TrimSpace(m[len(m)-1][1])
			if draft != "" && !strings.HasPrefix(draft, `Try "`) && !suggestionOnly(styled, lines) {
				return StateDraft
			}
		}
	}
	return StateReady
}

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;:?]*[ -/]*[@-~]`)

// suggestionOnly reports whether the last input line holds nothing but
// Claude Code's prompt suggestion: greyed ghost text, with the cursor
// drawn in reverse video over its first character. Typed text is neither
// dim nor reversed. Without styling (a plain capture) it is false.
func suggestionOnly(styled, plain []string) bool {
	idx := -1
	for i := len(plain) - 1; i >= 0 && i >= len(plain)-promptLines; i-- {
		if claudeInput.MatchString(plain[i]) {
			idx = i
			break
		}
	}
	if idx < 0 || !strings.Contains(styled[idx], "\x1b[") {
		return false
	}
	line := styled[idx]
	// Skip the prompt glyph itself, which is styled differently.
	at := strings.Index(line, "❯")
	if at < 0 {
		return false
	}
	line = line[at+len("❯"):]
	var dim, reverse bool
	seen := false
	for i := 0; i < len(line); {
		if loc := ansiSeq.FindStringIndex(line[i:]); loc != nil && loc[0] == 0 {
			seq := line[i : i+loc[1]]
			i += loc[1]
			if strings.HasSuffix(seq, "m") {
				dim, reverse = applySGR(seq[2:len(seq)-1], dim, reverse)
			}
			continue
		}
		r, n := utf8.DecodeRuneInString(line[i:])
		i += n
		if r == ' ' || r == '\u00a0' || reverse {
			continue
		}
		if !dim {
			return false
		}
		seen = true
	}
	return seen
}

// applySGR folds one SGR parameter list into the dim and reverse state.
// Grey foregrounds count as dim: 90, 38;5;8 and the 38;5;232-255 ramp.
func applySGR(params string, dim, reverse bool) (bool, bool) {
	if params == "" {
		return false, false
	}
	p := strings.Split(params, ";")
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case "0":
			dim, reverse = false, false
		case "2", "90":
			dim = true
		case "22", "39":
			dim = false
		case "7":
			reverse = true
		case "27":
			reverse = false
		case "38":
			if i+2 < len(p) && p[i+1] == "5" {
				n, _ := strconv.Atoi(p[i+2])
				dim = n == 8 || n >= 232
				i += 2
			} else if i+4 < len(p) && p[i+1] == "2" {
				r, _ := strconv.Atoi(p[i+2])
				g, _ := strconv.Atoi(p[i+3])
				b, _ := strconv.Atoi(p[i+4])
				dim = r == g && g == b && r < 200
				i += 4
			}
		}
	}
	return dim, reverse
}

func lastLines(lines []string, n int) string {
	// Skip trailing blank lines: the pane is padded to its height.
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	start := end - n
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:end], "\n")
}

// Provenance says who relayed a message and why, so the receiving agent can
// tell it from the person typing. Rendered as "[<via> by <by> from <from>]".
type Provenance struct {
	Via  string // one of Vias
	By   string // the sending principal, e.g. orchestrator
	From string // optional reference: a task id, a person, a thread
}

// Vias are the allowed verbs. Fixed so a message can't dress itself up as
// something else ("[approved by the user]").
var Vias = []string{"relayed", "dispatched", "sent"}

var token = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,63}$`)

// ValidToken reports whether s may appear in a provenance prefix or as a
// correlation id: 1 to 64 characters of letters, digits and ._:@/- .
func ValidToken(s string) bool { return token.MatchString(s) }

func (p Provenance) Validate() error {
	okVia := false
	for _, v := range Vias {
		okVia = okVia || p.Via == v
	}
	if !okVia {
		return fmt.Errorf("provenance verb %q must be one of %s", p.Via, strings.Join(Vias, ", "))
	}
	if !ValidToken(p.By) {
		return fmt.Errorf("provenance principal %q: want 1-64 of letters, digits and ._:@/-", p.By)
	}
	if p.From != "" && !ValidToken(p.From) {
		return fmt.Errorf("provenance source %q: want 1-64 of letters, digits and ._:@/-", p.From)
	}
	return nil
}

func (p Provenance) Prefix() string {
	s := "[" + p.Via + " by " + p.By
	if p.From != "" {
		s += " from " + p.From
	}
	return s + "]"
}

// Compose puts the prefix on its own line above the text.
func (p Provenance) Compose(text string) string {
	return p.Prefix() + "\n" + text
}

// MaxTextBytes bounds one message.
const MaxTextBytes = 64 * 1024

// AuditEntry is one line of the send audit log. It never holds message
// text: only its size and SHA-256 (of the delivered text, prefix included).
type AuditEntry struct {
	Time        time.Time `json:"time"`
	Principal   string    `json:"principal"`
	Via         string    `json:"via"`
	From        string    `json:"from,omitempty"`
	Address     string    `json:"address"`
	Thread      string    `json:"thread,omitempty"`
	Correlation string    `json:"correlation,omitempty"`
	Bytes       int       `json:"bytes"`
	SHA256      string    `json:"sha256"`
	Outcome     string    `json:"outcome"` // "delivered" or the refusal Reason
	Detail      string    `json:"detail,omitempty"`
}

// Digest returns the size and hex SHA-256 recorded for text.
func Digest(text string) (int, string) {
	sum := sha256.Sum256([]byte(text))
	return len(text), hex.EncodeToString(sum[:])
}

// AuditPath is ~/.local/state/agentmux/send-audit.jsonl for home.
func AuditPath(home string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "send-audit.jsonl")
}

// Append writes one entry, creating the log 0600 in a 0700 directory.
func Append(path string, e AuditEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
