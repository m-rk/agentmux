package ampsweep

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// CLIRunner runs amp the way the transcript reader does: as the run
// user (or the current user), with empty stdin (with stdin open the CLI
// waits and fails), bounded by one timeout per invocation. Exports of
// long threads are multi-MB, so Sweep caps how many it fetches: the
// list already skipped threads whose listed count exceeds the archive
// ceiling, and the cap only bounds the remaining small-thread exports.
type CLIRunner struct {
	// RunUser runs amp as this user; empty means the current user.
	RunUser string
	// MaxExports bounds the exports one Sweep makes; 0 means DefaultMaxExports.
	MaxExports int
	// Timeout bounds one amp invocation; 0 means DefaultTimeout.
	Timeout time.Duration
	// archived records the ids archived, for the report.
	archived []string
}

// DefaultMaxExports bounds the exports one Sweep makes.
const DefaultMaxExports = 200

// DefaultTimeout bounds one amp invocation.
const DefaultTimeout = 90 * time.Second

// listRow is one `amp threads list --json` row.
type listRow struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Updated      string `json:"updated"`
	MessageCount int    `json:"messageCount"`
}

// exportDoc is the part of `amp threads export` the rules need.
type exportDoc struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Created   int64  `json:"created"`
	UpdatedAt string `json:"updatedAt"`
	Env       struct {
		Initial struct {
			RunnerID string `json:"runnerID"`
		} `json:"initial"`
	} `json:"env"`
	Meta struct {
		LastKnownAgentState struct {
			State string `json:"state"`
		} `json:"lastKnownAgentState"`
	} `json:"meta"`
	Messages []exportMsg `json:"messages"`
}

type exportMsg struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (c *CLIRunner) command(ctx context.Context, args ...string) *exec.Cmd {
	if c.RunUser != "" {
		return runas.CommandContext(ctx, c.RunUser, "amp", args...)
	}
	return runas.CurrentUserCommandContext(ctx, "amp", args...)
}

func (c *CLIRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := c.command(ctx, args...)
	cmd.Stdin = bytes.NewReader(nil)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			msg := strings.Join(strings.Fields(string(ee.Stderr)), " ")
			if len(msg) > 200 {
				msg = msg[:200] + "…"
			}
			return nil, fmt.Errorf("amp %s: %w: %s", args[0], err, msg)
		}
		return nil, fmt.Errorf("amp %s: %w", args[0], err)
	}
	return out, nil
}

// List runs `amp threads list --json`: the account's live threads (no
// --include-archived — archived threads are already where the sweep
// wants them, and gc will delete them after retention).
func (c *CLIRunner) List(ctx context.Context) ([]ListedThread, error) {
	out, err := c.run(ctx, "threads", "list", "--json")
	if err != nil {
		return nil, err
	}
	var rows []listRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("amp threads list: %w", err)
	}
	var threads []ListedThread
	for _, row := range rows {
		if !transcript.ValidAmpThreadID(row.ID) {
			continue
		}
		var updated time.Time
		if row.Updated != "" {
			updated, _ = time.Parse(time.RFC3339Nano, row.Updated)
		}
		threads = append(threads, ListedThread{
			ID: row.ID, Title: row.Title, Updated: updated,
			MessageCount: row.MessageCount,
		})
	}
	return threads, nil
}

// Export runs `amp threads export` for one thread and extracts the
// rules' signal: the first user message's text, the agent state, and
// the true message count. The first user message is the thread's
// opening prompt — the provenance prefix ("[relayed by ...]",
// "[dispatched by ...]") or the reviewer's system prompt lives there,
// never in a later turn.
func (c *CLIRunner) Export(ctx context.Context, id string) (*ExportedThread, error) {
	out, err := c.run(ctx, "threads", "export", id)
	if err != nil {
		return nil, err
	}
	var doc exportDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("amp threads export: %w", err)
	}
	ex := &ExportedThread{ID: doc.ID, Title: doc.Title, State: doc.Meta.LastKnownAgentState.State, Messages: len(doc.Messages)}
	if ex.ID == "" {
		ex.ID = id
	}
	if doc.UpdatedAt != "" {
		ex.Updated, _ = time.Parse(time.RFC3339Nano, doc.UpdatedAt)
	}
	for _, m := range doc.Messages {
		if m.Role != "user" {
			continue
		}
		var sb strings.Builder
		for _, b := range m.Content {
			if b.Type == "" || b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		if text := strings.TrimSpace(sb.String()); text != "" {
			ex.FirstText = text
			break
		}
	}
	return ex, nil
}

// Archive runs `amp threads archive`: reversible, unlike Delete.
func (c *CLIRunner) Archive(ctx context.Context, id string) error {
	if !transcript.ValidAmpThreadID(id) {
		return fmt.Errorf("%q is not an amp thread id", id)
	}
	if _, err := c.run(ctx, "threads", "archive", id); err != nil {
		return err
	}
	c.archived = append(c.archived, id)
	return nil
}

// Delete runs `amp threads delete`: permanent, local and server-side.
// Only the sweep's own swept records call it, after retention expires.
func (c *CLIRunner) Delete(ctx context.Context, id string) error {
	if !transcript.ValidAmpThreadID(id) {
		return fmt.Errorf("%q is not an amp thread id", id)
	}
	_, err := c.run(ctx, "threads", "delete", id)
	return err
}
