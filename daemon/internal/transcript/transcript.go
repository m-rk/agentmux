// Package transcript reads an agentmux session's conversation from the
// runtime's own records, as structured messages an orchestrator can page
// through: Claude Code's JSONL, opencode's SQLite, and amp's server-side
// threads through the amp CLI. It is gateway phase 2; see
// docs/design/gateway.md ("Transcript read does not exist").
//
// Everything returned is untrusted data written by an agent or a tool, and
// all text is redacted with threadwatch.Redact before it leaves this
// package, since transcripts are meant to leave the host.
package transcript

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/threadwatch"
)

// Source says where one instance's records live. The CLI or gateway fills it
// from the instance's registry entry.
type Source struct {
	Instance string
	Agent    string // claude-code, opencode, amp
	Workdir  string
	Home     string // run user's home directory

	// AmpRunnerID is the instance's AGENTMUX_AMP_RUNNER_ID; amp only.
	AmpRunnerID string
	// AmpEnvFile is the instance's op env-file supplying AMP_API_KEY
	// (~/.agentmux/env/<instance>.env), if any; amp only. See
	// docs/amp-secrets.md.
	AmpEnvFile string
}

// Thread is one conversation inside a session: a Claude session id, an
// opencode session id, or an amp thread id.
type Thread struct {
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	Updated  time.Time `json:"updated"`
	Messages int       `json:"messages,omitempty"` // 0 when the source can't say cheaply
}

// Message is one turn's worth of content.
type Message struct {
	ID     string     `json:"id,omitempty"`
	Thread string     `json:"thread"`
	Role   string     `json:"role"` // RoleUser, RoleAssistant, RoleTool, RoleSystem
	Time   time.Time  `json:"time,omitempty"`
	Text   string     `json:"text,omitempty"` // redacted, capped at MaxTextBytes
	Tools  []ToolCall `json:"tools,omitempty"`
	// Untrusted is always true: the text was written by an agent, a tool, or
	// a person in another session, never by the caller. Consumers render it
	// as quoted data.
	Untrusted bool `json:"untrusted"`
}

// ToolCall summarizes one tool use inside a message. Input and output are
// not reproduced in full; Summary is a short redacted description.
type ToolCall struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	Error   bool   `json:"error,omitempty"`
}

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

const (
	// MaxTextBytes caps one message's text. Longer text keeps its head and
	// tail around a marker, since both ends tend to matter.
	MaxTextBytes = 8 * 1024
	// MaxSummaryBytes caps a tool call summary.
	MaxSummaryBytes = 300
	// DefaultLimit and MaxLimit bound how many messages one Read returns.
	DefaultLimit = 20
	MaxLimit     = 200
)

// Page is one Read result, newest message last. Older is the cursor for the
// page before this one; empty when there is nothing older.
type Page struct {
	Thread   string    `json:"thread"`
	Messages []Message `json:"messages"`
	Older    string    `json:"older,omitempty"`
}

// Reader reads one runtime's records.
type Reader interface {
	// Threads lists the session's threads, most recently updated first.
	Threads(ctx context.Context, src Source) ([]Thread, error)
	// Read returns up to limit messages of thread, ending just before cursor
	// (cursor "" means the newest message). thread "" means the most
	// recently updated thread. Cursors are opaque and only valid for the
	// same Reader and thread.
	Read(ctx context.Context, src Source, thread, cursor string, limit int) (Page, error)
}

// ErrUnsupported is returned by For for an agent with no transcript source.
var ErrUnsupported = errors.New("transcript: unsupported agent")

// ErrNoThread is returned when the session has no threads yet, or the
// requested thread is not one of the session's.
var ErrNoThread = errors.New("transcript: no such thread")

// ErrBadCursor is returned for a cursor the reader did not issue.
var ErrBadCursor = errors.New("transcript: invalid cursor")

var (
	readersMu sync.Mutex
	readers   = map[string]Reader{}
)

// register is called from each reader's file at init.
func register(agent string, r Reader) {
	readersMu.Lock()
	defer readersMu.Unlock()
	readers[agent] = r
}

// For returns the Reader for agent.
func For(agent string) (Reader, error) {
	readersMu.Lock()
	defer readersMu.Unlock()
	if r, ok := readers[agent]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnsupported, agent)
}

// Agents lists the agents with a registered reader, sorted.
func Agents() []string {
	readersMu.Lock()
	defer readersMu.Unlock()
	var out []string
	for a := range readers {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// ClampLimit applies DefaultLimit and MaxLimit.
func ClampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

// CleanText redacts s and caps it at MaxTextBytes, keeping head and tail.
// Every reader passes message text through it.
func CleanText(s string) string {
	return capMiddle(threadwatch.Redact(strings.TrimSpace(s)), MaxTextBytes)
}

// CleanSummary redacts a tool summary and caps it at MaxSummaryBytes.
func CleanSummary(s string) string {
	s = strings.Join(strings.Fields(threadwatch.Redact(s)), " ")
	if len(s) <= MaxSummaryBytes {
		return s
	}
	return s[:runeStart(s, MaxSummaryBytes)] + "…"
}

func capMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const marker = "\n…[truncated]…\n"
	half := (max - len(marker)) / 2
	head := s[:runeStart(s, half)]
	tail := s[runeStart(s, len(s)-half):]
	return head + marker + tail
}

// runeStart moves i back to the start of the UTF-8 rune containing it.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && (s[i]&0xC0) == 0x80 {
		i--
	}
	return i
}

// PageFrom builds a Page from all of a thread's messages (oldest first) using
// index cursors: the cursor is the decimal index of the first message after
// the page. Readers that load a whole thread anyway (amp exports, a Claude
// JSONL file) use it so cursor handling lives in one place.
func PageFrom(thread string, all []Message, cursor string, limit int) (Page, error) {
	end := len(all)
	if cursor != "" {
		var n int
		if _, err := fmt.Sscanf(cursor, "%d", &n); err != nil || n < 0 || n > len(all) || fmt.Sprint(n) != cursor {
			return Page{}, fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
		end = n
	}
	start := end - ClampLimit(limit)
	if start < 0 {
		start = 0
	}
	p := Page{Thread: thread, Messages: append([]Message(nil), all[start:end]...)}
	if start > 0 {
		p.Older = fmt.Sprint(start)
	}
	return p, nil
}
