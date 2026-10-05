package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func init() { register("opencode", &opencodeReader{}) }

// sqliteRunner executes `sqlite3 args...` and returns stdout.
type sqliteRunner func(ctx context.Context, args ...string) ([]byte, error)

// opencodeReader reads opencode's SQLite database through the sqlite3 CLI in
// read-only JSON mode (no cgo or driver), like threadwatch's collector.
type opencodeReader struct {
	// run executes sqlite3; nil means the real binary. Tests inject one.
	run sqliteRunner
}

func execSqlite3(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, errors.New("transcript: sqlite3 binary not found")
		}
		return nil, fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// query runs sql against the instance's database and decodes the JSON rows.
func (r *opencodeReader) query(ctx context.Context, src Source, sql string, out any) error {
	db := filepath.Join(src.Home, ".local", "share", "opencode", "opencode.db")
	if _, err := os.Stat(db); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: opencode database not found", ErrNoThread)
		}
		return err
	}
	run := r.run
	if run == nil {
		run = execSqlite3
	}
	b, err := run(ctx, "-readonly", "-json", db, sql)
	if err != nil {
		return err
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

// Threads lists the sessions whose directory is the instance's workdir.
func (r *opencodeReader) Threads(ctx context.Context, src Source) ([]Thread, error) {
	var rows []struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Updated  int64  `json:"time_updated"`
		Messages int    `json:"messages"`
	}
	sql := `SELECT id, title, time_updated, (SELECT COUNT(*) FROM message WHERE message.session_id = session.id) AS messages FROM session WHERE directory = ` +
		sqlQuote(src.Workdir) + ` ORDER BY time_updated DESC, id DESC`
	if err := r.query(ctx, src, sql, &rows); err != nil {
		return nil, err
	}
	out := make([]Thread, 0, len(rows))
	for _, row := range rows {
		out = append(out, Thread{ID: row.ID, Title: CleanSummary(row.Title), Updated: time.UnixMilli(row.Updated), Messages: row.Messages})
	}
	return out, nil
}

// Read returns a page of the session's messages. The whole session is
// loaded (text and tool parts only; reasoning and bookkeeping parts are
// filtered in SQL) and paged with index cursors.
func (r *opencodeReader) Read(ctx context.Context, src Source, thread, cursor string, limit int) (Page, error) {
	if thread == "" {
		threads, err := r.Threads(ctx, src)
		if err != nil {
			return Page{}, err
		}
		if len(threads) == 0 {
			return Page{}, ErrNoThread
		}
		thread = threads[0].ID
	}
	var found []struct {
		ID string `json:"id"`
	}
	err := r.query(ctx, src, `SELECT id FROM session WHERE id = `+sqlQuote(thread)+` AND directory = `+sqlQuote(src.Workdir), &found)
	if err != nil {
		return Page{}, err
	}
	if len(found) == 0 {
		return Page{}, fmt.Errorf("%w: %q", ErrNoThread, thread)
	}

	var msgs []struct {
		ID      string `json:"id"`
		Created int64  `json:"time_created"`
		Role    string `json:"role"`
	}
	err = r.query(ctx, src, `SELECT id, time_created, json_extract(data, '$.role') AS role FROM message WHERE session_id = `+
		sqlQuote(thread)+` ORDER BY time_created ASC, id ASC`, &msgs)
	if err != nil {
		return Page{}, err
	}
	var parts []struct {
		MessageID string `json:"message_id"`
		Type      string `json:"type"`
		Text      string `json:"text"`
		Tool      string `json:"tool"`
		Status    string `json:"status"`
		Input     string `json:"input"`
	}
	err = r.query(ctx, src, `SELECT message_id, json_extract(data, '$.type') AS type, json_extract(data, '$.text') AS text,
		json_extract(data, '$.tool') AS tool, json_extract(data, '$.state.status') AS status,
		json_extract(data, '$.state.input') AS input
		FROM part WHERE session_id = `+sqlQuote(thread)+` AND json_extract(data, '$.type') IN ('text', 'tool')
		ORDER BY time_created ASC, id ASC`, &parts)
	if err != nil {
		return Page{}, err
	}

	byID := make(map[string]int, len(msgs))
	all := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		role := RoleSystem
		switch m.Role {
		case "user":
			role = RoleUser
		case "assistant":
			role = RoleAssistant
		}
		byID[m.ID] = len(all)
		all = append(all, Message{ID: m.ID, Thread: thread, Role: role, Time: time.UnixMilli(m.Created), Untrusted: true})
	}
	texts := make([][]string, len(all))
	for _, p := range parts {
		i, ok := byID[p.MessageID]
		if !ok {
			continue
		}
		switch p.Type {
		case "text":
			if t := CleanText(p.Text); t != "" {
				texts[i] = append(texts[i], t)
			}
		case "tool":
			all[i].Tools = append(all[i].Tools, ToolCall{Name: p.Tool, Summary: toolInputSummary(json.RawMessage(p.Input)), Error: p.Status == "error"})
		}
	}
	// Drop messages with nothing to show (reasoning-only steps).
	kept := all[:0]
	for i, m := range all {
		m.Text = strings.Join(texts[i], "\n")
		if m.Text != "" || len(m.Tools) > 0 {
			kept = append(kept, m)
		}
	}
	return PageFrom(thread, kept, cursor, limit)
}
