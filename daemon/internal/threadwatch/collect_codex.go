package threadwatch

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/session"
)

// CodexCollector reads the per-thread JSONL logs agentmux keeps for headless
// `codex exec --json` runs (session.CodexRunLogPath). The stream has no
// timestamps, so events carry the poll time. Turn boundaries, agent
// messages, failures and usage limits map to events; a silent running turn
// is the stall detector's job (no events arrive), as for the other agents.
type CodexCollector struct{}

// Poll implements Collector.
func (c *CodexCollector) Poll(ctx context.Context, inst Instance, offsets OffsetStore) ([]Event, error) {
	logs, _ := filepath.Glob(filepath.Join(session.CodexRunStateDir(inst.Home, inst.Name), "codex-run-*.jsonl"))
	var events []Event
	for _, path := range logs {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		default:
		}
		thread := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "codex-run-"), ".jsonl")
		if !session.ValidCodexThreadID(thread) {
			continue
		}
		events = append(events, c.pollFile(path, thread, inst, offsets)...)
	}
	return events, nil
}

func (c *CodexCollector) pollFile(path, thread string, inst Instance, offsets OffsetStore) []Event {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	key := "codex:" + path
	offset, known := offsets.Get(key)
	if !known || info.Size() < offset {
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
	var events []Event
	r := bufio.NewReader(f)
	consumed := offset
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			break
		}
		consumed += int64(len(line))
		if ev, ok := c.mapLine(strings.TrimSpace(line), thread, inst); ok {
			events = append(events, ev)
		}
	}
	offsets.Set(key, consumed)
	return events
}

func (c *CodexCollector) mapLine(line, thread string, inst Instance) (Event, bool) {
	var rec struct {
		Type  string `json:"type"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Item *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
		Usage *struct {
			Input  int64 `json:"input_tokens"`
			Cached int64 `json:"cached_input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if line == "" || json.Unmarshal([]byte(line), &rec) != nil {
		return Event{}, false
	}
	ev := Event{Time: time.Now().UTC(), Instance: inst.Name, Agent: "codex", Thread: thread}
	switch rec.Type {
	case "turn.started":
		ev.Kind = KindActivity
	case "item.completed":
		if rec.Item == nil || rec.Item.Type != "agent_message" {
			ev.Kind = KindActivity
			break
		}
		ev.Kind = KindAssistantMsg
		ev.Excerpt = capExcerpt(rec.Item.Text, 400)
	case "turn.completed":
		ev.Kind = KindTurnEnd
		if rec.Usage != nil {
			ev.Tokens = Usage{InputTokens: rec.Usage.Input, OutputTokens: rec.Usage.Output, CacheReadTokens: rec.Usage.Cached}
		}
	case "turn.failed":
		msg := ""
		if rec.Error != nil {
			msg = rec.Error.Message
		}
		ev.Excerpt = capExcerpt(msg, 400)
		ev.Kind = KindAPIError
		if session.CodexErrorIsRateLimit(msg) {
			ev.Kind = KindUsageLimit
		}
	case "agentmux.exit":
		ev.Kind = KindSessionExit
	default:
		return Event{}, false
	}
	return ev, true
}
