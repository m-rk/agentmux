package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/threadwatch"
)

// amp keeps thread content on ampcode.com, so this reader goes through the amp
// CLI: `amp threads list --json` for the account's threads and
// `amp threads export <id>` for one thread's messages. Both need AMP_API_KEY,
// which comes from the instance's op env-file (docs/amp-secrets.md).

func init() { register("amp", ampReader{}) }

const (
	// ampCmdTimeout bounds one amp invocation.
	ampCmdTimeout = 90 * time.Second

	ampOpTokenRelPath = ".config/op/service_account_token"
	ampOpTokenEnv     = "OP_SERVICE_ACCOUNT_TOKEN"
	ampCacheRelPath   = ".cache/agentmux/amp-thread-runners.json"
)

// ampMaxExports bounds the exports one Threads call makes to learn thread
// runners (newest threads first). Exports of long threads are multi-MB, so a
// first scan of a large account fills the cache over several calls instead of
// stalling one.
var ampMaxExports = 20

// ampThreadID is the shape of an amp thread id: T- plus a UUID.
var ampThreadID = regexp.MustCompile(`^T-[0-9A-Fa-f-]+$`)

// ampExec runs `amp args...` for src and returns stdout. Replaceable in tests.
var ampExec = func(ctx context.Context, src Source, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ampCmdTimeout)
	defer cancel()
	cmd, err := ampCommand(ctx, src, args...)
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// stderr is redacted and capped; it is the only hint why the
			// CLI failed (expired key, unknown thread).
			msg := strings.Join(strings.Fields(threadwatch.Redact(string(ee.Stderr))), " ")
			if len(msg) > 200 {
				msg = msg[:runeStart(msg, 200)] + "…"
			}
			return nil, fmt.Errorf("amp %s: %w: %s", args[0], err, msg)
		}
		return nil, fmt.Errorf("amp %s: %w", args[0], err)
	}
	return out, nil
}

// ampCommand builds the amp invocation. With src.AmpEnvFile it runs amp under
// `op run`, as openv.go does for the runner itself: the service account token
// is set on the child's environment only (never argv) and stripped again by
// `env -u` before amp starts. stdin is empty because with stdin open the CLI
// waits and fails with "Timeout while reading from stdin".
//
// The binaries resolve the way session launch resolves them —
// runas.Command for the run user (privilege-dropped, run-user PATH) or
// runas.CurrentUserCommand when no run user is set — not a bare
// exec.Command: the daemon runs with a minimal ambient PATH, while run
// users install amp into ~/.npm-global/bin, so a bare lookup fails with
// "executable file not found in $PATH" for exactly the users retire
// must serve.
func ampCommand(ctx context.Context, src Source, args ...string) (*exec.Cmd, error) {
	build := func(name string, args ...string) *exec.Cmd {
		if src.RunUser != "" {
			return runas.CommandContext(ctx, src.RunUser, name, args...)
		}
		return runas.CurrentUserCommandContext(ctx, name, args...)
	}
	var cmd *exec.Cmd
	if src.AmpEnvFile == "" {
		cmd = build("amp", args...)
	} else {
		tok, err := ampOpToken(src.Home)
		if err != nil {
			return nil, err
		}
		argv := append([]string{"run", "--env-file=" + src.AmpEnvFile, "--",
			"/usr/bin/env", "-u", ampOpTokenEnv, "amp"}, args...)
		cmd = build("op", argv...)
		cmd.Env = append(cmd.Env, ampOpTokenEnv+"="+tok)
	}
	cmd.Stdin = bytes.NewReader(nil)
	cmd.WaitDelay = time.Second
	return cmd, nil
}

// ampOpToken reads the service account token, refusing a missing or empty
// file with an error that names the path, never the contents.
func ampOpToken(home string) (string, error) {
	if home == "" {
		return "", errors.New("amp: no home directory to find the 1Password service account token")
	}
	p := filepath.Join(home, ampOpTokenRelPath)
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("reading 1Password service account token %s: %w", p, err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("1Password service account token %s is empty", p)
	}
	return tok, nil
}

type ampReader struct{}

// ampListed is one `amp threads list --json` row.
type ampListed struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Updated      string `json:"updated"`
	Tree         string `json:"tree"`
	MessageCount int    `json:"messageCount"`
}

// ampExport is the part of `amp threads export` this package uses.
type ampExport struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Env   struct {
		Initial struct {
			RunnerID string `json:"runnerID"`
		} `json:"initial"`
	} `json:"env"`
	Meta struct {
		LastKnownAgentState struct {
			State string `json:"state"`
		} `json:"lastKnownAgentState"`
	} `json:"meta"`
	Messages []ampMessage `json:"messages"`
}

type ampMessage struct {
	Role      string          `json:"role"`
	MessageID ampScalar       `json:"messageId"`
	CreatedAt ampScalar       `json:"createdAt"`
	Content   json.RawMessage `json:"content"`
}

// ampScalar reads a JSON string or number as text: messageId is a string in
// some threads and a number in others.
type ampScalar string

func (s *ampScalar) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) == nil {
		*s = ampScalar(str)
	} else if string(b) != "null" {
		*s = ampScalar(b)
	}
	return nil
}

type ampBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"toolUseID"`
	Run       struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	} `json:"run"`
}

func (ampReader) Threads(ctx context.Context, src Source) ([]Thread, error) {
	out, err := ampExec(ctx, src, "threads", "list", "--json")
	if err != nil {
		return nil, err
	}
	var listed []ampListed
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("amp threads list: %w", err)
	}
	// Several runners can serve overlapping directories, so the tree only
	// narrows the candidates; the runner id decides.
	var cand []ampListed
	for _, t := range listed {
		if ampThreadID.MatchString(t.ID) && ampInTree(t.Tree, src.Workdir) {
			cand = append(cand, t)
		}
	}
	sort.SliceStable(cand, func(i, j int) bool { return cand[i].Updated > cand[j].Updated })

	cache := loadAmpCache(src.Home)
	learned := map[string]ampCacheEntry{}
	var out2 []Thread
	var firstErr error
	exports := 0
	for _, t := range cand {
		runner, ok := cache[t.ID]
		if !ok {
			if exports >= ampMaxExports {
				continue
			}
			exports++
			ex, err := ampFetch(ctx, src, t.ID)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			runner = ampCacheEntry{Runner: ex.Env.Initial.RunnerID, Updated: parseAmpTime(t.Updated)}
			cache[t.ID] = runner
			learned[t.ID] = runner
		}
		if runner.Runner != src.AmpRunnerID {
			continue
		}
		out2 = append(out2, Thread{ID: t.ID, Title: t.Title, Updated: parseAmpTime(t.Updated), Messages: t.MessageCount})
	}
	saveAmpCache(src.Home, learned)
	if len(out2) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out2, nil
}

func (r ampReader) Read(ctx context.Context, src Source, thread, cursor string, limit int) (Page, error) {
	ex, err := r.export(ctx, src, thread)
	if err != nil {
		// A `sessions run` thread keeps its own stream log under the
		// state dir, readable without amp auth: prefer it when the
		// export fails (expired key, CLI gone) and the log exists.
		if logPage, ok := ampRunLogPage(src, thread, cursor, limit); ok {
			return logPage, nil
		}
		return Page{}, err
	}
	return PageFrom(ex.ID, ampMessages(ex), cursor, limit)
}

// ampRunLogPage reads a `sessions run` thread from its stream log under
// the state dir (~/.local/state/agentmux/sessions/<instance>/), without
// touching the amp CLI. It reports false when there is no such log, so
// the caller falls through to the export path. Stream records map to
// messages: user records stay user text, assistant text blocks become one
// assistant message, and the final result record becomes a closing
// assistant message (its result text) so the turn's outcome is visible. A
// pending `ask_user_choice` tool_use with no result record after it
// appends a closing assistant message naming the question and options, so
// the read surfaces the stuck question.
func ampRunLogPage(src Source, thread, cursor string, limit int) (Page, bool) {
	if src.Home == "" || src.Instance == "" {
		return Page{}, false
	}
	want := thread
	if want == "" {
		want = newestAmpRunThread(src.Home, src.Instance)
	}
	if want == "" || !ampThreadID.MatchString(want) {
		return Page{}, false
	}
	path := filepath.Join(src.Home, ".local", "state", "agentmux", "sessions", src.Instance, "amp-run-"+want+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		return Page{}, false
	}
	var msgs []Message
	scanner := lineScanner(data)
	var pendingAsk *askPending
	for scanner.Scan() {
		line := scanner.Bytes()
		if m, ok := ampStreamMessage(want, line); ok {
			msgs = append(msgs, m)
		}
		pendingAsk = trackAskPending(pendingAsk, line)
	}
	if q := pendingAskMessage(want, pendingAsk); q != nil {
		msgs = append(msgs, *q)
	}
	if len(msgs) == 0 {
		return Page{}, false
	}
	page, err := PageFrom(want, msgs, cursor, limit)
	if err != nil {
		return Page{}, false
	}
	return page, true
}

// askPending is a pending `ask_user_choice` question seen in a stream
// log: the tool_use id plus the question, options, and allow_other from
// its input.
type askPending struct {
	toolUseID  string
	question   string
	options    []string
	allowOther bool
}

// trackAskPending follows one stream-log line: an assistant record with an
// `ask_user_choice` tool_use sets the pending question (its tool_use id,
// question, options, allow_other from the input); a tool_result user
// record or a result record clears it. Callers keep the returned value
// across lines and render what remains at the end.
func trackAskPending(cur *askPending, line []byte) *askPending {
	var rec struct {
		Type    string `json:"type"`
		Message *struct {
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"toolUseID"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Message == nil {
		return cur
	}
	switch rec.Type {
	case "assistant":
		for _, b := range rec.Message.Content {
			if b.Type != "tool_use" || b.Name != "ask_user_choice" || b.ID == "" {
				continue
			}
			p := &askPending{toolUseID: b.ID}
			var in struct {
				Question   string   `json:"question"`
				Options    []string `json:"options"`
				AllowOther bool     `json:"allowOther"`
			}
			if json.Unmarshal(b.Input, &in) == nil {
				p.question, p.options, p.allowOther = in.Question, in.Options, in.AllowOther
			}
			return p
		}
	case "user":
		for _, b := range rec.Message.Content {
			if b.Type == "tool_result" {
				return nil
			}
		}
	case "result":
		return nil
	}
	return cur
}

// ampStreamToolSummary is the one-line summary for a stream-log tool_use:
// the ask_user_choice question, else the amp input summary (command, path,
// pattern, …). Stream records carry the tool input, not rendered text,
// and b.Text is empty there.
func ampStreamToolSummary(name string, raw json.RawMessage) string {
	if name == "ask_user_choice" {
		var in struct {
			Question string `json:"question"`
		}
		if json.Unmarshal(raw, &in) == nil && in.Question != "" {
			return in.Question
		}
		return ""
	}
	return ampInputSummary(raw)
}

// pendingAskMessage renders the still-pending question as a closing
// assistant message, or nil when nothing is pending. The ToolCall names
// the tool so the question is visible in both the human-readable read and
// the JSON page.
func pendingAskMessage(thread string, pending *askPending) *Message {
	if pending == nil {
		return nil
	}
	text := "Waiting on a question (ask_user_choice)."
	if q := strings.TrimSpace(pending.question); q != "" {
		text = "Waiting on a question: " + q
	}
	if len(pending.options) > 0 {
		text += "\nOptions: " + strings.Join(pending.options, " / ")
	}
	summary := pending.question
	if summary == "" {
		summary = pending.toolUseID
	}
	m := &Message{Thread: thread, Role: RoleAssistant, Text: CleanText(text), Untrusted: true,
		Tools: []ToolCall{{Name: "ask_user_choice", Summary: CleanSummary(summary)}}}
	return m
}

// newestAmpRunThread is the most recently modified run log's thread, or
// "" when none exists: the `read` default when no thread is named.
func newestAmpRunThread(home, instance string) string {
	dir := filepath.Join(home, ".local", "state", "agentmux", "sessions", instance)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "amp-run-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "amp-run-"), ".jsonl")
		if id == "pending" || !ampThreadID.MatchString(id) {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(bestMod) {
			best, bestMod = id, info.ModTime()
		}
	}
	return best
}

// lineScanner scans newline-separated records in data.
func lineScanner(data []byte) *lineScannerT {
	return &lineScannerT{lines: strings.Split(string(data), "\n")}
}

type lineScannerT struct {
	lines []string
	pos   int
	cur   string
}

func (s *lineScannerT) Scan() bool {
	for s.pos < len(s.lines) {
		s.cur = s.lines[s.pos]
		s.pos++
		if strings.TrimSpace(s.cur) != "" {
			return true
		}
	}
	return false
}

func (s *lineScannerT) Bytes() []byte { return []byte(s.cur) }

// ampStreamMessage maps one stream-json line to a transcript message.
// Assistant records carry content blocks (text, tool_use); user records
// carry the echoed prompt; the result record closes the turn with its
// result text. Init and other system records have no message content. A
// pending `ask_user_choice` tool_use with no result record after it
// becomes a closing assistant message naming the question and options, so
// `sessions read` surfaces the stuck question the same way `sessions
// status` does through waiting_on.
func ampStreamMessage(thread string, line []byte) (Message, bool) {
	var base struct {
		Type    string `json:"type"`
		Message *struct {
			Role    string `json:"role"`
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if json.Unmarshal(line, &base) != nil {
		return Message{}, false
	}
	m := Message{Thread: thread, Untrusted: true}
	switch base.Type {
	case "user":
		if base.Message == nil {
			return Message{}, false
		}
		var texts []string
		for _, b := range base.Message.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				texts = append(texts, strings.TrimSpace(b.Text))
			}
		}
		if len(texts) == 0 {
			return Message{}, false
		}
		m.Role, m.Text = RoleUser, CleanText(strings.Join(texts, "\n\n"))
	case "assistant":
		if base.Message == nil {
			return Message{}, false
		}
		var texts []string
		var calls []ToolCall
		for _, b := range base.Message.Content {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					texts = append(texts, strings.TrimSpace(b.Text))
				}
			case "tool_use":
				calls = append(calls, ToolCall{Name: b.Name, Summary: CleanSummary(ampStreamToolSummary(b.Name, b.Input))})
			}
		}
		if len(texts) == 0 && len(calls) == 0 {
			return Message{}, false
		}
		m.Role, m.Text, m.Tools = RoleAssistant, CleanText(strings.Join(texts, "\n\n")), calls
	case "result":
		if strings.TrimSpace(base.Result) == "" {
			return Message{}, false
		}
		m.Role, m.Text = RoleAssistant, CleanText(base.Result)
		if base.IsError {
			m.Tools = []ToolCall{{Name: "error", Summary: CleanSummary(base.Subtype)}}
		}
	default:
		return Message{}, false
	}
	return m, true
}

// AmpThreadState returns the agent state amp last recorded for thread
// ("idle", for example); thread "" means the instance's newest thread. It
// costs a full export, so callers poll it sparingly.
func AmpThreadState(ctx context.Context, src Source, thread string) (string, error) {
	ex, err := ampReader{}.export(ctx, src, thread)
	if err != nil {
		return "", err
	}
	return ex.Meta.LastKnownAgentState.State, nil
}

// export fetches thread (or the newest one) and checks it belongs to the
// instance's runner.
func (r ampReader) export(ctx context.Context, src Source, thread string) (*ampExport, error) {
	if thread == "" {
		ts, err := r.Threads(ctx, src)
		if err != nil {
			return nil, err
		}
		if len(ts) == 0 {
			return nil, ErrNoThread
		}
		thread = ts[0].ID
	}
	if !ampThreadID.MatchString(thread) {
		return nil, fmt.Errorf("%w: invalid thread id", ErrNoThread)
	}
	ex, err := ampFetch(ctx, src, thread)
	if err != nil {
		return nil, err
	}
	saveAmpCache(src.Home, map[string]ampCacheEntry{
		thread: {Runner: ex.Env.Initial.RunnerID, Updated: time.Now().UTC()},
	})
	if ex.Env.Initial.RunnerID != src.AmpRunnerID {
		return nil, ErrNoThread
	}
	if ex.ID == "" {
		ex.ID = thread
	}
	return ex, nil
}

func ampFetch(ctx context.Context, src Source, id string) (*ampExport, error) {
	out, err := ampExec(ctx, src, "threads", "export", id)
	if err != nil {
		return nil, err
	}
	var ex ampExport
	if err := json.Unmarshal(out, &ex); err != nil {
		return nil, fmt.Errorf("amp threads export: %w", err)
	}
	return &ex, nil
}

// ampInTree reports whether tree (a file:// URL) is workdir or inside it. An
// empty workdir matches everything.
func ampInTree(tree, workdir string) bool {
	if workdir == "" {
		return true
	}
	u, err := url.Parse(tree)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return false
	}
	p, w := path.Clean(u.Path), path.Clean(workdir)
	return p == w || w == "/" || strings.HasPrefix(p, w+"/")
}

func parseAmpTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// ampMessages maps an export to messages, oldest first. Tool results arrive
// as user messages of tool_result blocks; they become RoleTool messages whose
// ToolCall names the tool that was called. Thinking blocks are dropped, as are
// messages left empty by that.
func ampMessages(ex *ampExport) []Message {
	names := map[string]string{}
	for _, m := range ex.Messages {
		for _, b := range ampBlocks(m.Content) {
			if b.Type == "tool_use" {
				names[b.ID] = b.Name
			}
		}
	}
	var out []Message
	for i, m := range ex.Messages {
		base := Message{ID: string(m.MessageID), Thread: ex.ID, Time: parseAmpTime(string(m.CreatedAt)), Untrusted: true}
		if base.ID == "" {
			base.ID = fmt.Sprint(i)
		}
		var texts []string
		var calls, results []ToolCall
		for _, b := range ampBlocks(m.Content) {
			switch b.Type {
			case "text":
				if s := strings.TrimSpace(b.Text); s != "" {
					texts = append(texts, s)
				}
			case "tool_use":
				calls = append(calls, ToolCall{Name: b.Name, Summary: CleanSummary(ampInputSummary(b.Input))})
			case "tool_result":
				name := names[b.ToolUseID]
				if name == "" {
					name = "tool"
				}
				results = append(results, ToolCall{
					Name:    name,
					Summary: CleanSummary(ampResultSummary(b)),
					Error:   ampResultFailed(b),
				})
			}
		}
		role := RoleUser
		if m.Role == "assistant" {
			role = RoleAssistant
		}
		if len(texts) > 0 || len(calls) > 0 {
			msg := base
			msg.Role = role
			msg.Text = CleanText(strings.Join(texts, "\n\n"))
			msg.Tools = calls
			out = append(out, msg)
		}
		if len(results) > 0 {
			msg := base
			msg.Role = RoleTool
			msg.Tools = results
			if len(texts) > 0 || len(calls) > 0 {
				msg.ID += "/result"
			}
			out = append(out, msg)
		}
	}
	return out
}

// ampBlocks decodes a message's content, which is a block array (a bare
// string is tolerated as one text block).
func ampBlocks(raw json.RawMessage) []ampBlock {
	var blocks []ampBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return []ampBlock{{Type: "text", Text: s}}
	}
	return nil
}

// ampSummaryKeys are the tool input fields that say most about a call, in
// preference order.
var ampSummaryKeys = []string{"cmd", "command", "path", "filePath", "file_path", "pattern", "query", "url", "threadID", "thread", "question", "prompt", "description"}

func ampInputSummary(raw json.RawMessage) string {
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range ampSummaryKeys {
		var s string
		if v, ok := in[k]; ok && json.Unmarshal(v, &s) == nil && s != "" {
			return s
		}
	}
	// Unknown tool: the first string field in key order.
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		var s string
		if json.Unmarshal(in[k], &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// ampResultSummary prefers a result's output text; results are strings or
// objects with tool-specific fields.
func ampResultSummary(b ampBlock) string {
	raw := b.Run.Result
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"output", "stdout", "content", "message", "error"} {
			if json.Unmarshal(obj[k], &s) == nil && s != "" {
				return s
			}
		}
	}
	return string(raw)
}

func ampResultFailed(b ampBlock) bool {
	switch b.Run.Status {
	case "error", "cancelled", "rejected-by-user":
		return true
	}
	return len(b.Run.Error) > 0 && string(b.Run.Error) != "null"
}

// ampCacheEntry records which runner a thread belongs to. A thread's runner
// never changes, so an entry stays valid forever.
type ampCacheEntry struct {
	Runner  string    `json:"runner"`
	Updated time.Time `json:"updated"`
}

var ampCacheMu sync.Mutex

func ampCachePath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ampCacheRelPath)
}

// loadAmpCache returns the thread-to-runner map; a missing or unreadable file
// is an empty cache, since it only saves exports.
func loadAmpCache(home string) map[string]ampCacheEntry {
	ampCacheMu.Lock()
	defer ampCacheMu.Unlock()
	return readAmpCache(ampCachePath(home))
}

func readAmpCache(p string) map[string]ampCacheEntry {
	m := map[string]ampCacheEntry{}
	if p == "" {
		return m
	}
	if data, err := os.ReadFile(p); err == nil {
		if json.Unmarshal(data, &m) != nil || m == nil {
			m = map[string]ampCacheEntry{}
		}
	}
	return m
}

// saveAmpCache merges entries into the cache file atomically (temp file and
// rename, 0600 in a 0700 directory). Failures are ignored: the cache is an
// optimization.
func saveAmpCache(home string, entries map[string]ampCacheEntry) {
	p := ampCachePath(home)
	if p == "" || len(entries) == 0 {
		return
	}
	ampCacheMu.Lock()
	defer ampCacheMu.Unlock()
	m := readAmpCache(p)
	for k, v := range entries {
		m[k] = v
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	dir := filepath.Dir(p)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".amp-thread-runners-*")
	if err != nil {
		return
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(f.Name(), 0o600) != nil || os.Rename(f.Name(), p) != nil {
		os.Remove(f.Name())
	}
}

// AmpRun runs `amp <args>` the way the reader does: empty stdin, and
// AMP_API_KEY through the instance's op env-file. `agentmux sessions send`
// uses it to post to runner threads. It returns stdout.
func AmpRun(ctx context.Context, src Source, args ...string) ([]byte, error) {
	return ampExec(ctx, src, args...)
}

// ValidAmpThreadID reports whether id has the shape of an amp thread id.
func ValidAmpThreadID(id string) bool { return ampThreadID.MatchString(id) }
