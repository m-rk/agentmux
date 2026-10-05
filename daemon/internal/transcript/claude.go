package transcript

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func init() { register("claude-code", claudeReader{}) }

// claudeReader reads Claude Code's JSONL transcripts under
// <Home>/.claude/projects/<slug>/*.jsonl, one file per Claude session.
type claudeReader struct{}

const (
	// claudeMaxLine caps one JSONL line. Longer lines (huge tool results,
	// pasted files) are skipped rather than failing the read.
	claudeMaxLine = 32 << 20
	// claudeTitleScan bounds how many of the newest threads Threads scans
	// for a title; older ones are listed without one to keep listing cheap.
	claudeTitleScan = 50
)

// claudeProjectSlug mirrors threadwatch's: the working directory with '/'
// and '.' replaced by '-', which is how Claude Code names its project dir.
func claudeProjectSlug(workdir string) string {
	slug := strings.ReplaceAll(workdir, "/", "-")
	return strings.ReplaceAll(slug, ".", "-")
}

func claudeDir(src Source) string {
	return filepath.Join(src.Home, ".claude", "projects", claudeProjectSlug(src.Workdir))
}

// claudeLine is the subset of a JSONL record the reader uses.
type claudeLine struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	UUID        string `json:"uuid"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	// IsCompactSummary marks the synthetic user message holding the summary
	// Claude Code writes when it compacts a conversation.
	IsCompactSummary bool   `json:"isCompactSummary"`
	Summary          string `json:"summary"`
	CustomTitle      string `json:"customTitle"`
	AITitle          string `json:"aiTitle"`
	Message          struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

// eachLine calls fn for every line of r, tolerating lines of any length up
// to max (longer ones are skipped). fn returning false stops the walk.
func eachLine(r io.Reader, max int, fn func([]byte) bool) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var buf []byte
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !skipping {
			buf = append(buf, chunk...)
			if len(buf) > max {
				buf, skipping = nil, true
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if len(buf) > 0 && !skipping {
			if !fn(bytes.TrimRight(buf, "\r\n")) {
				return nil
			}
		}
		buf, skipping = buf[:0], false
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// claudeFiles lists the session's transcript files, newest first.
func claudeFiles(src Source) (paths []string, mod map[string]time.Time, err error) {
	entries, err := os.ReadDir(claudeDir(src))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	mod = map[string]time.Time{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		paths = append(paths, id)
		mod[id] = info.ModTime()
	}
	sort.Slice(paths, func(i, j int) bool {
		if !mod[paths[i]].Equal(mod[paths[j]]) {
			return mod[paths[i]].After(mod[paths[j]])
		}
		return paths[i] < paths[j]
	})
	return paths, mod, nil
}

// Threads lists one thread per file. Messages is always 0: counting would
// mean a full scan of every file. Titles come from custom-title, ai-title or
// summary records, else the first real user message, for the newest
// claudeTitleScan threads only.
func (claudeReader) Threads(ctx context.Context, src Source) ([]Thread, error) {
	ids, mod, err := claudeFiles(src)
	if err != nil {
		return nil, err
	}
	out := make([]Thread, 0, len(ids))
	for i, id := range ids {
		t := Thread{ID: id, Updated: mod[id]}
		if i < claudeTitleScan {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			t.Title = claudeTitle(filepath.Join(claudeDir(src), id+".jsonl"))
		}
		out = append(out, t)
	}
	return out, nil
}

func claudeTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var custom, ai, summary, first string
	eachLine(f, claudeMaxLine, func(b []byte) bool {
		// Cheap prefilter before parsing: only title records and (until
		// found) user lines matter.
		isTitle := bytes.Contains(b, []byte(`"customTitle"`)) || bytes.Contains(b, []byte(`"aiTitle"`)) || bytes.Contains(b, []byte(`"type":"summary"`))
		if !isTitle && (first != "" || !bytes.Contains(b, []byte(`"type":"user"`))) {
			return true
		}
		var l claudeLine
		if json.Unmarshal(b, &l) != nil {
			return true
		}
		switch {
		case l.CustomTitle != "":
			custom = l.CustomTitle
		case l.AITitle != "":
			ai = l.AITitle
		case l.Type == "summary" && l.Summary != "":
			summary = l.Summary
		case l.Type == "user" && first == "" && !l.IsMeta && !l.IsSidechain && !l.IsCompactSummary:
			if text, _, _ := claudeUserParts(l.Message.Content); text != "" && !claudeCommandRecord(text) && !claudeHarnessNote(text) {
				first = text
			}
		}
		return true
	})
	for _, t := range []string{custom, ai, summary, first} {
		if t = CleanSummary(t); t != "" {
			return t
		}
	}
	return ""
}

// validThreadID rejects ids that could escape the session directory.
func validThreadID(id string) bool {
	return id != "" && id != "." && !strings.Contains(id, "..") && !strings.ContainsAny(id, `/\`) && !strings.ContainsRune(id, 0)
}

// Read returns a page of the thread's messages. Tool results become their
// own RoleTool messages carrying a ToolCall named after the originating
// tool use (Summary holds a short error excerpt only when Error is set).
func (claudeReader) Read(ctx context.Context, src Source, thread, cursor string, limit int) (Page, error) {
	if thread == "" {
		ids, _, err := claudeFiles(src)
		if err != nil {
			return Page{}, err
		}
		if len(ids) == 0 {
			return Page{}, ErrNoThread
		}
		thread = ids[0]
	}
	if !validThreadID(thread) {
		return Page{}, fmt.Errorf("%w: %q", ErrNoThread, thread)
	}
	f, err := os.Open(filepath.Join(claudeDir(src), thread+".jsonl"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Page{}, fmt.Errorf("%w: %q", ErrNoThread, thread)
		}
		return Page{}, err
	}
	defer f.Close()

	var (
		all     []Message
		byMsgID = map[string]int{}    // assistant message.id -> index in all
		toolIDs = map[string]string{} // tool_use id -> tool name
	)
	err = eachLine(f, claudeMaxLine, func(b []byte) bool {
		if ctx.Err() != nil {
			return false
		}
		var l claudeLine
		if json.Unmarshal(b, &l) != nil || l.IsSidechain || l.IsMeta {
			return true
		}
		ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
		switch l.Type {
		case "assistant":
			text, tools := claudeAssistantParts(l.Message.Content, toolIDs)
			if text == "" && len(tools) == 0 {
				return true
			}
			if i, ok := byMsgID[l.Message.ID]; ok && l.Message.ID != "" {
				m := &all[i]
				if text != "" {
					if m.Text != "" {
						text = m.Text + "\n" + text
					}
					m.Text = text
				}
				m.Tools = append(m.Tools, tools...)
				return true
			}
			if l.Message.ID != "" {
				byMsgID[l.Message.ID] = len(all)
			}
			all = append(all, Message{ID: firstNonEmpty(l.Message.ID, l.UUID), Thread: thread, Role: RoleAssistant, Time: ts, Text: text, Tools: tools, Untrusted: true})
		case "user":
			text, results, hasResult := claudeUserParts(l.Message.Content)
			if hasResult {
				var tools []ToolCall
				for _, r := range results {
					tc := ToolCall{Name: toolIDs[r.ToolUseID], Error: r.IsError}
					if tc.Name == "" {
						tc.Name = "tool"
					}
					tools = append(tools, tc)
				}
				all = append(all, Message{ID: l.UUID, Thread: thread, Role: RoleTool, Time: ts, Tools: tools, Untrusted: true})
			}
			if text != "" && !claudeCommandRecord(text) {
				role := RoleUser
				if l.IsCompactSummary || claudeHarnessNote(text) {
					role = RoleSystem
				}
				all = append(all, Message{ID: l.UUID, Thread: thread, Role: role, Time: ts, Text: CleanText(text), Untrusted: true})
			}
		case "system":
			if text := claudeContentString(l.Message.Content); text != "" {
				all = append(all, Message{ID: l.UUID, Thread: thread, Role: RoleSystem, Time: ts, Text: CleanText(text), Untrusted: true})
			}
		}
		return true
	})
	if err != nil {
		return Page{}, err
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	return PageFrom(thread, all, cursor, limit)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// claudeContentString returns content when it is a plain JSON string.
func claudeContentString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// claudeAssistantParts extracts a line's text and tool uses, dropping
// thinking blocks, and records tool names by id for later results. Text is
// already cleaned.
func claudeAssistantParts(raw json.RawMessage, toolIDs map[string]string) (string, []ToolCall) {
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		if s := claudeContentString(raw); s != "" {
			return CleanText(s), nil
		}
		return "", nil
	}
	var texts []string
	var tools []ToolCall
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := CleanText(b.Text); t != "" {
				texts = append(texts, t)
			}
		case "tool_use":
			toolIDs[b.ID] = b.Name
			tools = append(tools, ToolCall{Name: b.Name, Summary: toolInputSummary(b.Input)})
		}
	}
	return strings.Join(texts, "\n"), tools
}

// claudeUserParts splits user content into plain text (uncleaned) and
// tool_result blocks.
func claudeUserParts(raw json.RawMessage) (text string, results []claudeBlock, hasResult bool) {
	if s := claudeContentString(raw); s != "" {
		return s, nil, false
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil, false
	}
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				texts = append(texts, b.Text)
			}
		case "tool_result":
			results = append(results, b)
		}
	}
	return strings.Join(texts, "\n"), results, len(results) > 0
}

// toolSummaryKeys are the input fields that best describe a tool call, in
// priority order. Both Claude Code (file_path) and opencode (filePath)
// spellings are listed.
var toolSummaryKeys = []string{"command", "file_path", "filePath", "path", "pattern", "description", "query", "url", "prompt", "subagent_type", "name"}

// toolInputSummary picks the most telling string field of a tool input.
// Never the whole input.
func toolInputSummary(raw json.RawMessage) string {
	var in map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range toolSummaryKeys {
		if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
			return CleanSummary(s)
		}
	}
	return ""
}

// claudeCommandRecord reports a user-role line that records a local slash
// command (its name, its output, or the caveat Claude Code writes around
// them) rather than anything a person or agent said.
func claudeCommandRecord(text string) bool {
	for _, p := range []string{"<command-name>", "<command-message>", "<local-command-"} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// claudeHarnessNote reports a user-role line the harness injected, such as a
// background task notification; it is kept, as a system message.
func claudeHarnessNote(text string) bool {
	return strings.HasPrefix(text, "<task-notification>")
}
