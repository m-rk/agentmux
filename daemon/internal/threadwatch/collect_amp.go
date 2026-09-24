package threadwatch

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AmpCollector reads amp's own runner log,
// <Home>/.cache/amp/logs/no-tui.log. Unlike Claude Code, amp's thread
// content lives server-side, so this collector can only report liveness
// and error signals: auth failures, API errors, turn boundaries from the
// runner's per-thread agent state, compaction, and tool executions.
type AmpCollector struct {
	// turns tracks each thread's last agent state and when its current
	// turn started, so an idle transition can carry the turn's duration.
	turns map[string]ampTurn
}

type ampTurn struct {
	state string
	start time.Time
}

// ampLogPath is amp's runner log, relative to an instance's home directory.
var ampLogPath = filepath.Join(".cache", "amp", "logs", "no-tui.log")

// ampRecord is one JSON line of amp's runner log.
type ampRecord struct {
	Timestamp string    `json:"@timestamp"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	ThreadID  string    `json:"threadId,omitempty"`
	Type      string    `json:"type,omitempty"`
	Subtype   string    `json:"subtype,omitempty"`
	Error     *ampError `json:"error,omitempty"`
}

type ampError struct {
	Message string `json:"message,omitempty"`
}

// ampAuthPatterns match amp log text that indicates the runner's login has
// gone bad and needs re-authentication, rather than a one-off API error.
var ampAuthPatterns = []string{
	"session expired",
	"could not be refreshed",
	"unauthorized",
	" 401",
}

// ampUsageLimitPatterns match amp log text that indicates a usage/credit/
// rate limit rather than a generic API failure.
var ampUsageLimitPatterns = []string{
	"out of credits",
	"insufficient credit",
	"usage limit",
	"quota",
}

// ampAgentStateMessage carries a thread's agent state in its subtype:
// working, streaming, tool_use, running_tools, compacting, then idle when
// the turn ends.
const ampAgentStateMessage = "[observer] onAgentState"

// ampToolMarker precedes the tool name in the runner's tool execution
// lines ("<executor-id> executing tool: Read").
const ampToolMarker = "executing tool: "

func ampParseTime(ts string) time.Time {
	if ts == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t
	}
	return time.Now()
}

// ampFileID returns a stable per-inode identifier for info, or 0 if the
// platform's Sys() shape is unrecognised (rotation detection then falls
// back to the size-shrank check alone).
func ampFileID(info os.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Ino)
	}
	return 0
}

func ampContainsAny(haystack string, needles []string) bool {
	h := strings.ToLower(haystack)
	for _, n := range needles {
		if strings.Contains(h, n) {
			return true
		}
	}
	return false
}

// Poll implements Collector.
func (c *AmpCollector) Poll(ctx context.Context, inst Instance, offsets OffsetStore) ([]Event, error) {
	path := filepath.Join(inst.Home, ampLogPath)
	info, err := os.Stat(path)
	if err != nil {
		// No log yet: nothing to report, not an error.
		return nil, nil
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	offsetKey := "amp:" + path
	inoKey := "amp-ino:" + path

	offset, known := offsets.Get(offsetKey)
	prevIno, inoKnown := offsets.Get(inoKey)
	curIno := ampFileID(info)

	switch {
	case !known:
		offset = 0
	case inoKnown && curIno != 0 && prevIno != curIno:
		// Rotated: a new file took the old name.
		offset = 0
	case info.Size() < offset:
		// Truncated in place.
		offset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()

	if _, err := f.Seek(offset, 0); err != nil {
		offsets.Set(offsetKey, 0)
		offsets.Set(inoKey, curIno)
		return nil, nil
	}

	var events []Event
	reader := bufio.NewReader(f)
	consumed := offset
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		consumed += int64(len(line))
		if ev, ok := c.mapLine(strings.TrimRight(line, "\n"), inst); ok {
			events = append(events, ev)
		}
	}
	offsets.Set(offsetKey, consumed)
	offsets.Set(inoKey, curIno)
	return events, nil
}

// mapLine parses one log line and returns an event if it maps to one.
// Malformed or uninteresting lines are ignored, never an error. Most of the
// runner's thread chatter (websocket and JSON-RPC traffic) is dropped: agent
// state transitions and tool executions are enough to show progress.
func (c *AmpCollector) mapLine(line string, inst Instance) (Event, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Event{}, false
	}
	var rec ampRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return Event{}, false
	}

	base := Event{
		Time:     ampParseTime(rec.Timestamp),
		Instance: inst.Name,
		Agent:    "amp",
		Thread:   rec.ThreadID,
	}

	errText := rec.Message
	if rec.Error != nil && rec.Error.Message != "" {
		if errText != "" {
			errText += ": " + rec.Error.Message
		} else {
			errText = rec.Error.Message
		}
	}

	if ampContainsAny(errText, ampAuthPatterns) {
		ev := base
		ev.Kind = KindAuthError
		ev.Excerpt = Excerpt(errText)
		return ev, true
	}

	if ampContainsAny(errText, ampUsageLimitPatterns) {
		ev := base
		ev.Kind = KindUsageLimit
		ev.Excerpt = Excerpt(errText)
		return ev, true
	}

	level := strings.ToUpper(rec.Level)
	if level == "ERROR" {
		ev := base
		ev.Kind = KindAPIError
		ev.Excerpt = Excerpt(errText)
		return ev, true
	}

	if rec.Message == ampAgentStateMessage && rec.ThreadID != "" && rec.Subtype != "" {
		return c.agentState(base, rec.Subtype)
	}

	if i := strings.Index(rec.Message, ampToolMarker); i >= 0 && rec.ThreadID != "" {
		ev := base
		ev.Kind = KindActivity
		ev.Tool = strings.TrimSpace(rec.Message[i+len(ampToolMarker):])
		return ev, true
	}

	if strings.Contains(strings.ToLower(rec.Message), "reconnecting") {
		// A single reconnect is normal amp behaviour, not a failure.
		// Repeated reconnects are a detector's job (error_loop over
		// these events), not something to page on individually.
		return Event{}, false
	}

	return Event{}, false
}

// agentState turns a thread's agent state into an event on transitions
// only: idle ends the turn, compacting is a compaction, and anything else is
// progress. Repeats of the current state are dropped.
func (c *AmpCollector) agentState(base Event, state string) (Event, bool) {
	if c.turns == nil {
		c.turns = map[string]ampTurn{}
	}
	prev, seen := c.turns[base.Thread]
	if seen && prev.state == state {
		return Event{}, false
	}
	next := ampTurn{state: state, start: prev.start}
	ev := base
	switch {
	case state == "idle":
		delete(c.turns, base.Thread)
		if !seen {
			// A turn that started before this collector began has no
			// known length; it still ends.
			return Event{}, false
		}
		ev.Kind = KindTurnEnd
		if !prev.start.IsZero() {
			ev.Duration = base.Time.Sub(prev.start)
		}
		return ev, true
	case !seen || prev.state == "idle":
		next.start = base.Time
		ev.Kind = KindActivity
	case state == "compacting":
		ev.Kind = KindCompaction
	default:
		ev.Kind = KindActivity
	}
	c.turns[base.Thread] = next
	return ev, true
}
