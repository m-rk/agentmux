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
func ampCommand(ctx context.Context, src Source, args ...string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if src.AmpEnvFile == "" {
		cmd = exec.CommandContext(ctx, "amp", args...)
	} else {
		tok, err := ampOpToken(src.Home)
		if err != nil {
			return nil, err
		}
		argv := append([]string{"run", "--env-file=" + src.AmpEnvFile, "--",
			"/usr/bin/env", "-u", ampOpTokenEnv, "amp"}, args...)
		cmd = exec.CommandContext(ctx, "op", argv...)
		cmd.Env = append(os.Environ(), ampOpTokenEnv+"="+tok)
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
		return Page{}, err
	}
	return PageFrom(ex.ID, ampMessages(ex), cursor, limit)
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
