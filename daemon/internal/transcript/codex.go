package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/session"
)

// codex runs are headless `codex exec --json` processes whose JSONL stream
// agentmux logs per thread (session.CodexRunLogPath), so this reader works
// from those logs rather than codex's own rollout files. Only the agent's
// messages are recovered; tool output is not reproduced.

func init() { register("codex", codexReader{}) }

type codexReader struct{}

func codexLogs(src Source) []string {
	matches, _ := filepath.Glob(filepath.Join(session.CodexRunStateDir(src.Home, src.Instance), "codex-run-*.jsonl"))
	return matches
}

func codexLogThread(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "codex-run-"), ".jsonl")
}

func (codexReader) Threads(_ context.Context, src Source) ([]Thread, error) {
	var out []Thread
	for _, p := range codexLogs(src) {
		id := codexLogThread(p)
		info, err := os.Stat(p)
		if err != nil || !session.ValidCodexThreadID(id) {
			continue
		}
		out = append(out, Thread{ID: id, Updated: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func (r codexReader) Read(ctx context.Context, src Source, thread, cursor string, limit int) (Page, error) {
	if thread == "" {
		threads, _ := r.Threads(ctx, src)
		if len(threads) == 0 {
			return Page{}, ErrNoThread
		}
		thread = threads[0].ID
	}
	if !session.ValidCodexThreadID(thread) {
		return Page{}, ErrNoThread
	}
	if cursor != "" {
		return Page{}, ErrBadCursor
	}
	f, err := os.Open(session.CodexRunLogPath(src.Home, src.Instance, thread))
	if err != nil {
		return Page{}, ErrNoThread
	}
	defer f.Close()
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			Item *struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "item.completed" || ev.Item == nil || ev.Item.Type != "agent_message" {
			continue
		}
		msgs = append(msgs, Message{
			ID: ev.Item.ID, Thread: thread, Role: RoleAssistant,
			Text: CleanText(ev.Item.Text), Untrusted: true,
		})
	}
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return Page{Thread: thread, Messages: msgs}, nil
}
