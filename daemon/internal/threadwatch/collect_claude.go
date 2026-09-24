package threadwatch

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ClaudeCollector reads Claude Code's own JSONL transcripts. Transcripts
// live under <Home>/.claude/projects/<slug>/*.jsonl, one file per resumed
// session in a project, where slug is the working directory with '/' and
// '.' replaced by '-'.
type ClaudeCollector struct {
	// Backfill, when true, replays a transcript file's full history the
	// first time it is seen instead of starting at end-of-file. Defaults
	// to false so a freshly-enabled watch does not flood the event log
	// with old turns.
	Backfill bool

	// state is per-file bookkeeping that must survive across polls but is
	// not part of the persisted OffsetStore contract (tool_use id lookup,
	// usage accumulation, per-thread dedup). Keyed by transcript path.
	state map[string]*claudeFileState
}

// claudeFileState is the in-memory, per-transcript-file state a
// ClaudeCollector keeps between polls. It is deliberately not persisted:
// losing it across a daemon restart only means a handful of tool_result
// events fall back to an unknown tool name and one turn's usage is
// under-counted, which is preferable to persisting raw tool names/ids.
type claudeFileState struct {
	// toolNameByUseID maps a tool_use block id to its tool name, so a
	// later tool_result in a type=user record can be attributed. Bounded
	// to claudeToolIDCacheSize entries, evicted oldest-first.
	toolNameByUseID map[string]string
	toolIDOrder     []string

	// seenMessageIDs dedupes usage accounting: Claude Code can write
	// several records for the same API response (the same message.id),
	// and usage must only be counted once per message.
	seenMessageIDs map[string]struct{}
	messageIDOrder []string

	// pendingUsage accumulates message.usage across assistant records
	// since the last turn_end, to attach to the next KindTurnEnd event.
	pendingUsage Usage
	hasUsage     bool
}

// claudeToolIDCacheSize bounds the per-file tool_use id -> name and
// message id dedup caches.
const claudeToolIDCacheSize = 512

func (s *claudeFileState) rememberTool(id, name string) {
	if id == "" {
		return
	}
	if _, ok := s.toolNameByUseID[id]; !ok {
		s.toolIDOrder = append(s.toolIDOrder, id)
	}
	if s.toolNameByUseID == nil {
		s.toolNameByUseID = map[string]string{}
	}
	s.toolNameByUseID[id] = name
	for len(s.toolIDOrder) > claudeToolIDCacheSize {
		old := s.toolIDOrder[0]
		s.toolIDOrder = s.toolIDOrder[1:]
		delete(s.toolNameByUseID, old)
	}
}

// seenMessage reports whether id was already accounted for, and marks it
// seen. An empty id is never deduped (some records omit message.id).
func (s *claudeFileState) seenMessage(id string) bool {
	if id == "" {
		return false
	}
	if s.seenMessageIDs == nil {
		s.seenMessageIDs = map[string]struct{}{}
	}
	if _, ok := s.seenMessageIDs[id]; ok {
		return true
	}
	s.seenMessageIDs[id] = struct{}{}
	s.messageIDOrder = append(s.messageIDOrder, id)
	for len(s.messageIDOrder) > claudeToolIDCacheSize {
		old := s.messageIDOrder[0]
		s.messageIDOrder = s.messageIDOrder[1:]
		delete(s.seenMessageIDs, old)
	}
	return false
}

func (s *claudeFileState) addUsage(u Usage) {
	s.pendingUsage.InputTokens += u.InputTokens
	s.pendingUsage.OutputTokens += u.OutputTokens
	s.pendingUsage.CacheReadTokens += u.CacheReadTokens
	s.pendingUsage.CacheWriteTokens += u.CacheWriteTokens
	s.hasUsage = true
}

func (s *claudeFileState) takeUsage() Usage {
	u := s.pendingUsage
	s.pendingUsage = Usage{}
	s.hasUsage = false
	return u
}

// claudeProjectSlug turns a working directory into the directory name
// Claude Code stores its transcripts under.
func claudeProjectSlug(workdir string) string {
	slug := strings.ReplaceAll(workdir, "/", "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	return slug
}

// claudeRecord is the subset of a Claude Code JSONL line's top-level shape
// that thread watch cares about. Unrecognised fields are ignored.
type claudeRecord struct {
	Type              string         `json:"type"`
	Subtype           string         `json:"subtype,omitempty"`
	Timestamp         string         `json:"timestamp,omitempty"`
	SessionID         string         `json:"sessionId,omitempty"`
	IsSidechain       bool           `json:"isSidechain,omitempty"`
	IsApiErrorMessage bool           `json:"isApiErrorMessage,omitempty"`
	IsMeta            bool           `json:"isMeta,omitempty"`
	DurationMs        int64          `json:"durationMs,omitempty"`
	Error             string         `json:"error,omitempty"` // isApiErrorMessage records: e.g. "rate_limit"
	Message           *claudeMessage `json:"message,omitempty"`
}

// usageLimitTextPattern matches isApiErrorMessage text that reads as a
// usage/session/rate limit rather than a generic API failure — e.g. "You've
// hit your session limit · resets 7am (UTC)". Matched case-insensitively.
// See the real record shape this was derived from in
// docs/design/thread-watch.md's usage_limit notes.
var usageLimitTextPattern = regexp.MustCompile(`(?i)hit your (?:session|usage|weekly|daily)? ?limit|usage limit|out of (?:usage )?credits|credit balance is too low|quota exceeded`)

// isClaudeUsageLimitError reports whether an isApiErrorMessage record with
// the given top-level `error` field and assistant text describes a
// usage/session/credit/rate limit rather than a generic API error.
func isClaudeUsageLimitError(topLevelError, text string) bool {
	if strings.EqualFold(topLevelError, "rate_limit") {
		return true
	}
	return usageLimitTextPattern.MatchString(text)
}

type claudeMessage struct {
	ID         string          `json:"id,omitempty"`
	Role       string          `json:"role,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"` // string, or []claudeBlock
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      *claudeUsage    `json:"usage,omitempty"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens,omitempty"`
	OutputTokens             int64 `json:"output_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
}

// claudeBlock is one entry of a message's content array: assistant text,
// tool_use, or (in a user message) a tool_result.
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use
	Name      string          `json:"name,omitempty"`        // tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result: string or []claudeBlock
	IsError   bool            `json:"is_error,omitempty"`
}

func (u *claudeUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// claudeBlocks parses a message.content field that may be either a plain
// string or a JSON array of blocks. A bare string comes back as a single
// text block.
func claudeBlocks(raw json.RawMessage) []claudeBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	return nil
}

// claudeText joins every text block's content, in order.
func claudeText(blocks []claudeBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// claudeToolResultText renders a tool_result block's content (string or
// text blocks) as plain text for an excerpt.
func claudeToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return claudeText(claudeBlocks(raw))
}

func claudeParseTime(ts string) time.Time {
	if ts == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t
	}
	return time.Now()
}

// Poll implements Collector.
func (c *ClaudeCollector) Poll(ctx context.Context, inst Instance, offsets OffsetStore) ([]Event, error) {
	dir := filepath.Join(inst.Home, ".claude", "projects", claudeProjectSlug(inst.Workdir))
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(paths) == 0 {
		// No transcripts yet, or an unreadable/absent dir: nothing to
		// report, not an error.
		return nil, nil
	}
	sort.Strings(paths)

	if c.state == nil {
		c.state = map[string]*claudeFileState{}
	}

	var events []Event
	for _, path := range paths {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		default:
		}
		evs := c.pollFile(path, inst, offsets)
		events = append(events, evs...)
	}
	return events, nil
}

func (c *ClaudeCollector) pollFile(path string, inst Instance, offsets OffsetStore) []Event {
	key := "claude:" + path
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}

	offset, known := offsets.Get(key)
	if !known {
		if c.Backfill {
			offset = 0
		} else {
			offset = info.Size()
		}
	} else if info.Size() < offset {
		// Truncated/replaced: restart from the top.
		offset = 0
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	if _, err := f.Seek(offset, 0); err != nil {
		offsets.Set(key, 0)
		return nil
	}

	st := c.state[path]
	if st == nil {
		st = &claudeFileState{}
		c.state[path] = st
	}

	var events []Event
	reader := bufio.NewReader(f)
	consumed := offset
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// No trailing newline yet: leave this partial line for the
			// next poll by not advancing consumed past its start.
			break
		}
		consumed += int64(len(line))
		if ev, ok := claudeMapLine(strings.TrimRight(line, "\n"), inst, st); ok {
			events = append(events, ev...)
		}
	}
	offsets.Set(key, consumed)
	return events
}

// claudeMapLine parses one JSONL line and returns zero or more events.
// Malformed or unrecognised lines are ignored, never an error.
func claudeMapLine(line string, inst Instance, st *claudeFileState) ([]Event, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, false
	}
	var rec claudeRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return nil, false
	}

	base := Event{
		Time:     claudeParseTime(rec.Timestamp),
		Instance: inst.Name,
		Agent:    "claude-code",
		Thread:   rec.SessionID,
	}

	switch rec.Type {
	case "system":
		return claudeMapSystem(rec, base, st)
	case "assistant":
		return claudeMapAssistant(rec, base, st)
	case "user":
		return claudeMapUser(rec, base, st)
	default:
		return nil, false
	}
}

func claudeMapSystem(rec claudeRecord, base Event, st *claudeFileState) ([]Event, bool) {
	if strings.Contains(rec.Subtype, "compact") {
		if rec.IsSidechain {
			return nil, false
		}
		ev := base
		ev.Kind = KindCompaction
		return []Event{ev}, true
	}
	if rec.Subtype == "turn_duration" {
		if rec.IsSidechain {
			return nil, false
		}
		ev := base
		ev.Kind = KindTurnEnd
		ev.Duration = time.Duration(rec.DurationMs) * time.Millisecond
		if st.hasUsage {
			ev.Tokens = st.takeUsage()
		}
		return []Event{ev}, true
	}
	return nil, false
}

func claudeMapAssistant(rec claudeRecord, base Event, st *claudeFileState) ([]Event, bool) {
	if rec.IsApiErrorMessage {
		var text string
		if rec.Message != nil {
			text = claudeText(claudeBlocks(rec.Message.Content))
		}
		ev := base
		if isClaudeUsageLimitError(rec.Error, text) {
			ev.Kind = KindUsageLimit
		} else {
			ev.Kind = KindAPIError
		}
		ev.Excerpt = Excerpt(text)
		return []Event{ev}, true
	}
	if rec.Message == nil {
		return nil, false
	}
	if rec.IsSidechain {
		// Subagent turn: no activity/assistant_msg noise, and its usage
		// is not folded into the parent thread's next turn_end (a
		// subagent bills against its own turn, which is itself skipped
		// below in claudeMapSystem).
		return nil, false
	}

	blocks := claudeBlocks(rec.Message.Content)
	for _, b := range blocks {
		if b.Type == "tool_use" {
			st.rememberTool(b.ID, b.Name)
		}
	}

	if rec.Message.Usage != nil && !st.seenMessage(rec.Message.ID) {
		st.addUsage(rec.Message.Usage.toUsage())
	}

	var events []Event
	activity := base
	activity.Kind = KindActivity
	events = append(events, activity)

	if rec.Message.StopReason == "end_turn" {
		if text := claudeText(blocks); text != "" {
			msg := base
			msg.Kind = KindAssistantMsg
			msg.Excerpt = Excerpt(text)
			events = append(events, msg)
		}
	}
	return events, true
}

func claudeMapUser(rec claudeRecord, base Event, st *claudeFileState) ([]Event, bool) {
	if rec.Message == nil || rec.IsMeta {
		return nil, false
	}
	blocks := claudeBlocks(rec.Message.Content)

	var toolResults []claudeBlock
	for _, b := range blocks {
		if b.Type == "tool_result" {
			toolResults = append(toolResults, b)
		}
	}
	if len(toolResults) > 0 {
		var events []Event
		for _, b := range toolResults {
			if !b.IsError {
				continue
			}
			ev := base
			ev.Kind = KindToolError
			ev.Tool = st.toolNameByUseID[b.ToolUseID]
			ev.Excerpt = Excerpt(claudeToolResultText(b.Content))
			events = append(events, ev)
		}
		return events, len(events) > 0
	}

	if text := claudeText(blocks); text != "" {
		ev := base
		ev.Kind = KindUserMessage
		ev.Excerpt = Excerpt(text)
		return []Event{ev}, true
	}
	return nil, false
}
