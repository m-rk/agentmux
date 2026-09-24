package threadwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrSourceMissing means the opencode database (or the sqlite3 binary
// needed to read it) isn't available for this instance. It is not a
// collection failure: callers should treat it as "nothing to collect from
// this source" rather than logging or alerting on it.
var ErrSourceMissing = errors.New("threadwatch: opencode source missing")

// opencodeRunner executes `sqlite3 <args...>` and returns stdout. It is a
// field on OpencodeCollector so tests can substitute a fixture without
// touching a real database or PATH.
type opencodeRunner func(ctx context.Context, sqlitePath string, args ...string) ([]byte, error)

// OpencodeCollector reads opencode's own SQLite state
// (`<inst.Home>/.local/share/opencode/opencode.db`) and turns new
// session/message/part rows into Events. It never links a SQLite driver in;
// it shells out to the `sqlite3` CLI in read-only, JSON-output mode.
type OpencodeCollector struct {
	// SqlitePath is the sqlite3 binary to exec. Defaults to "sqlite3"
	// (resolved via PATH) when empty.
	SqlitePath string

	// Backfill, when true, emits events for everything already in the
	// database the first time an instance is polled (no stored offset).
	// The default (false) starts from the current max row so history
	// never replays as a flood of events on first run.
	Backfill bool

	// run executes sqlite3. Overridden in tests; defaults to execSqlite3.
	run opencodeRunner
}

func (c *OpencodeCollector) runner() opencodeRunner {
	if c.run != nil {
		return c.run
	}
	return execSqlite3
}

func (c *OpencodeCollector) sqlitePath() string {
	if c.SqlitePath != "" {
		return c.SqlitePath
	}
	return "sqlite3"
}

// execSqlite3 is the default opencodeRunner: it shells out to the real
// sqlite3 CLI. A missing binary is reported as ErrSourceMissing so callers
// don't treat "sqlite3 isn't installed on this host" as an alertable error.
func execSqlite3(ctx context.Context, sqlitePath string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, sqlitePath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A bare command name that isn't on PATH surfaces as *exec.Error;
		// a path (absolute or relative) that doesn't exist surfaces as a
		// wrapped os.ErrNotExist from the fork/exec syscall. Either way,
		// sqlite3 isn't usable on this host: report it as a missing
		// source, not a collection failure worth alerting on.
		var execErr *exec.Error
		if errors.As(err, &execErr) || errors.Is(err, os.ErrNotExist) {
			return nil, ErrSourceMissing
		}
		return nil, fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func opencodeDBPath(inst Instance) string {
	return filepath.Join(inst.Home, ".local", "share", "opencode", "opencode.db")
}

// Poll implements Collector.
func (c *OpencodeCollector) Poll(ctx context.Context, inst Instance, offsets OffsetStore) ([]Event, error) {
	dbPath := opencodeDBPath(inst)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, ErrSourceMissing
	}
	run := c.runner()
	sqlitePath := c.sqlitePath()

	sessionIDs, err := c.sessionIDs(ctx, run, sqlitePath, dbPath, inst.Workdir)
	if err != nil {
		return nil, err
	}
	if len(sessionIDs) == 0 {
		return nil, nil
	}

	key := "opencode:" + dbPath + ":" + inst.Workdir
	floor, hadOffset := offsets.Get(key)
	if !hadOffset && !c.Backfill {
		maxTU, err := c.maxTimeUpdated(ctx, run, sqlitePath, dbPath, sessionIDs)
		if err != nil {
			return nil, err
		}
		floor = maxTU
	}

	msgRows, err := c.queryMessages(ctx, run, sqlitePath, dbPath, sessionIDs, floor)
	if err != nil {
		return nil, err
	}
	partRows, err := c.queryParts(ctx, run, sqlitePath, dbPath, sessionIDs, floor)
	if err != nil {
		return nil, err
	}
	turnRows, err := c.queryTurns(ctx, run, sqlitePath, dbPath, sessionIDs, floor)
	if err != nil {
		return nil, err
	}

	events, newOffset := buildOpencodeEvents(inst, msgRows, partRows, turnRows)
	if newOffset < floor {
		newOffset = floor
	}
	offsets.Set(key, newOffset)
	return events, nil
}

// --- SQLite access -----------------------------------------------------

func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func sqlQuoteList(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = sqlQuote(s)
	}
	return strings.Join(quoted, ",")
}

func (c *OpencodeCollector) query(ctx context.Context, run opencodeRunner, sqlitePath, dbPath, sql string, out any) error {
	stdout, err := run(ctx, sqlitePath, "-readonly", "-json", dbPath, sql)
	if err != nil {
		return err
	}
	stdout = bytes.TrimSpace(stdout)
	if len(stdout) == 0 {
		return nil
	}
	return json.Unmarshal(stdout, out)
}

// sessionIDs returns the opencode session ids whose working directory is
// inst.Workdir, matched either on session.directory directly or, when a
// session doesn't record it, on its project's worktree.
func (c *OpencodeCollector) sessionIDs(ctx context.Context, run opencodeRunner, sqlitePath, dbPath, workdir string) ([]string, error) {
	sql := fmt.Sprintf(
		`SELECT DISTINCT session.id AS id FROM session LEFT JOIN project ON session.project_id = project.id WHERE session.directory = %s OR project.worktree = %s`,
		sqlQuote(workdir), sqlQuote(workdir))
	var rows []struct {
		ID string `json:"id"`
	}
	if err := c.query(ctx, run, sqlitePath, dbPath, sql, &rows); err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids, nil
}

// maxTimeUpdated returns the highest message/part time_updated currently in
// the sessions given, or 0 if there are none. It's the floor used the first
// time an instance is polled and Backfill is false.
func (c *OpencodeCollector) maxTimeUpdated(ctx context.Context, run opencodeRunner, sqlitePath, dbPath string, sessionIDs []string) (int64, error) {
	ids := sqlQuoteList(sessionIDs)
	sql := fmt.Sprintf(
		`SELECT COALESCE(MAX(mx),0) AS mx FROM (
			SELECT MAX(time_updated) AS mx FROM message WHERE session_id IN (%s)
			UNION ALL
			SELECT MAX(time_updated) AS mx FROM part WHERE session_id IN (%s)
		)`, ids, ids)
	var rows []struct {
		MX int64 `json:"mx"`
	}
	if err := c.query(ctx, run, sqlitePath, dbPath, sql, &rows); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].MX, nil
}

type opencodeMessageRow struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	TimeCreated int64  `json:"time_created"`
	TimeUpdated int64  `json:"time_updated"`
	Data        string `json:"data"`
}

func (c *OpencodeCollector) queryMessages(ctx context.Context, run opencodeRunner, sqlitePath, dbPath string, sessionIDs []string, floor int64) ([]opencodeMessageRow, error) {
	sql := fmt.Sprintf(
		`SELECT id, session_id, time_created, time_updated, data FROM message WHERE session_id IN (%s) AND time_updated > %d ORDER BY time_updated ASC, rowid ASC`,
		sqlQuoteList(sessionIDs), floor)
	var rows []opencodeMessageRow
	if err := c.query(ctx, run, sqlitePath, dbPath, sql, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

type opencodePartRow struct {
	ID          string `json:"id"`
	MessageID   string `json:"message_id"`
	SessionID   string `json:"session_id"`
	TimeCreated int64  `json:"time_created"`
	TimeUpdated int64  `json:"time_updated"`
	Data        string `json:"data"`
	MsgData     string `json:"msg_data"`
}

func (c *OpencodeCollector) queryParts(ctx context.Context, run opencodeRunner, sqlitePath, dbPath string, sessionIDs []string, floor int64) ([]opencodePartRow, error) {
	sql := fmt.Sprintf(
		`SELECT part.id AS id, part.message_id AS message_id, part.session_id AS session_id,
			part.time_created AS time_created, part.time_updated AS time_updated,
			part.data AS data, message.data AS msg_data
		FROM part JOIN message ON part.message_id = message.id
		WHERE part.session_id IN (%s) AND part.time_updated > %d
		ORDER BY part.time_updated ASC, part.rowid ASC`,
		sqlQuoteList(sessionIDs), floor)
	var rows []opencodePartRow
	if err := c.query(ctx, run, sqlitePath, dbPath, sql, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// opencodeTurnRow is one completed turn: a run of assistant "steps" (each
// its own message row, opencode's usual shape for tool-call round trips)
// ending in the first step whose finish reason isn't "tool-calls" (stop,
// length, unknown, or absent-but-completed all end a turn; see
// isTurnTerminal). Duration and token/cost totals are summed in SQL across
// every step in the run, not just the terminal row, and the run's start is
// taken from the preceding user message's created time when there is one
// (falling back to the first step's own created time) — computing this
// against the full session history (not just rows newer than the poll
// floor) so a restart mid-turn still reports the right duration and totals.
type opencodeTurnRow struct {
	SessionID           string  `json:"session_id"`
	TerminalID          string  `json:"terminal_id"`
	TerminalTimeUpdated int64   `json:"terminal_time_updated"`
	Completed           int64   `json:"completed"`
	StartTime           int64   `json:"start_time"`
	ErrorName           string  `json:"error_name"`
	ErrorMessage        string  `json:"error_message"`
	SumIn               int64   `json:"sum_in"`
	SumOut              int64   `json:"sum_out"`
	SumCacheWrite       int64   `json:"sum_cw"`
	SumCacheRead        int64   `json:"sum_cr"`
	SumCost             float64 `json:"sum_cost"`
}

// queryTurns groups every assistant message in sessionIDs into turns (a run
// of "tool-calls" steps followed by a terminal step) and returns only the
// turns whose terminal step is newer than floor. The grouping and
// aggregation happen entirely in SQL over each session's full history so
// the result doesn't depend on which rows a previous poll already
// consumed.
func (c *OpencodeCollector) queryTurns(ctx context.Context, run opencodeRunner, sqlitePath, dbPath string, sessionIDs []string, floor int64) ([]opencodeTurnRow, error) {
	ids := sqlQuoteList(sessionIDs)
	sql := fmt.Sprintf(`
WITH assistant AS (
  SELECT id, session_id, rowid,
         time_created, time_updated,
         json_extract(data,'$.time.completed') AS completed,
         json_extract(data,'$.finish') AS finish,
         COALESCE(json_extract(data,'$.tokens.input'),0) AS tok_in,
         COALESCE(json_extract(data,'$.tokens.output'),0) AS tok_out,
         COALESCE(json_extract(data,'$.tokens.cache.write'),0) AS tok_cw,
         COALESCE(json_extract(data,'$.tokens.cache.read'),0) AS tok_cr,
         COALESCE(json_extract(data,'$.cost'),0) AS cost,
         json_extract(data,'$.error.name') AS err_name,
         json_extract(data,'$.error.data.message') AS err_msg
  FROM message
  WHERE session_id IN (%s) AND json_extract(data,'$.role') = 'assistant'
),
flagged AS (
  SELECT *,
    CASE WHEN completed IS NOT NULL AND (finish IS NULL OR finish <> 'tool-calls') THEN 1 ELSE 0 END AS is_terminal
  FROM assistant
),
grouped AS (
  SELECT *,
    COALESCE(SUM(is_terminal) OVER (
      PARTITION BY session_id ORDER BY rowid ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING
    ), 0) AS turn_group
  FROM flagged
),
turns AS (
  SELECT session_id, turn_group,
    MIN(rowid) AS first_rowid,
    MIN(time_created) AS first_created,
    MAX(CASE WHEN is_terminal = 1 THEN id END) AS terminal_id,
    MAX(CASE WHEN is_terminal = 1 THEN time_updated END) AS terminal_time_updated,
    MAX(CASE WHEN is_terminal = 1 THEN completed END) AS completed,
    MAX(CASE WHEN is_terminal = 1 THEN err_name END) AS error_name,
    MAX(CASE WHEN is_terminal = 1 THEN err_msg END) AS error_message,
    SUM(tok_in) AS sum_in, SUM(tok_out) AS sum_out,
    SUM(tok_cw) AS sum_cw, SUM(tok_cr) AS sum_cr, SUM(cost) AS sum_cost
  FROM grouped
  GROUP BY session_id, turn_group
  HAVING terminal_id IS NOT NULL
),
results AS (
  SELECT t.session_id AS session_id, t.terminal_id AS terminal_id,
    t.terminal_time_updated AS terminal_time_updated,
    t.completed AS completed,
    COALESCE(
      (SELECT m2.time_created FROM message m2
       WHERE m2.session_id = t.session_id AND m2.rowid < t.first_rowid
         AND json_extract(m2.data,'$.role') = 'user'
       ORDER BY m2.rowid DESC LIMIT 1),
      t.first_created
    ) AS start_time,
    t.error_name AS error_name, t.error_message AS error_message,
    t.sum_in AS sum_in, t.sum_out AS sum_out,
    t.sum_cw AS sum_cw, t.sum_cr AS sum_cr, t.sum_cost AS sum_cost
  FROM turns t
)
SELECT * FROM results WHERE terminal_time_updated > %d ORDER BY terminal_time_updated ASC`,
		ids, floor)

	var rows []opencodeTurnRow
	if err := c.query(ctx, run, sqlitePath, dbPath, sql, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// --- row -> Event mapping ------------------------------------------------

// opencodeMessageData mirrors the shape of message.data. Field names match
// opencode's on-disk JSON; only the fields threadwatch needs are captured.
type opencodeMessageData struct {
	Role   string `json:"role"`
	Finish string `json:"finish"` // assistant only: "tool-calls" for a step, otherwise (stop/length/unknown/absent) ends the turn
	Time   struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// isStep reports whether md is a completed intermediate step of a turn
// (opencode's per-tool-call-round assistant message) rather than the
// message that ends the turn. Turn detection/aggregation — including
// tokens, cost and error text, all summed/read from the terminal step —
// happens in SQL (queryTurns); this only classifies the non-terminal steps
// so they're mapped to KindActivity instead.
func (md opencodeMessageData) isStep() bool {
	return md.Time.Completed > 0 && md.Finish == "tool-calls"
}

// opencodePartData mirrors the shape of part.data.
type opencodePartData struct {
	Type  string `json:"type"`
	Tool  string `json:"tool"`
	Text  string `json:"text"`
	State *struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"state"`
}

var authErrorHints = []string{
	"unauthorized", "401", "403", "forbidden", "invalid token", "invalid api key",
	"invalid_api_key", "session expired", "re-authenticate", "reauthenticate",
	"authentication", "not authenticated", "credentials",
}

// isAuthErrorText reports whether an error name/message looks like an
// auth/login failure rather than a generic API error.
func isAuthErrorText(s string) bool {
	s = strings.ToLower(s)
	for _, hint := range authErrorHints {
		if strings.Contains(s, hint) {
			return true
		}
	}
	return false
}

// usageLimitErrorPattern matches an opencode turn's error name/message that
// reads as a usage/session/credit/rate limit rather than a generic API
// error. Matched case-insensitively.
var usageLimitErrorPattern = regexp.MustCompile(`(?i)hit your (?:session|usage|weekly|daily)? ?limit|usage limit|out of (?:usage )?credits|insufficient credit|credit balance is too low|quota exceeded|quota`)

// isUsageLimitErrorText reports whether an error name/message looks like a
// usage/credit/rate limit rather than a generic API failure.
func isUsageLimitErrorText(s string) bool {
	return usageLimitErrorPattern.MatchString(s)
}

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

type opencodeTextPart struct {
	Text        string
	TimeUpdated int64
	SessionID   string
}

// buildOpencodeEvents maps new message/part rows, plus turns already
// aggregated in SQL (see queryTurns), to Events. It returns the highest
// time_updated/terminal_time_updated seen so the caller can advance the
// offset.
func buildOpencodeEvents(inst Instance, msgRows []opencodeMessageRow, partRows []opencodePartRow, turnRows []opencodeTurnRow) ([]Event, int64) {
	var events []Event
	var newOffset int64

	base := func(kind, sessionID string, ts time.Time) Event {
		return Event{
			Time:     ts,
			Instance: inst.Name,
			Agent:    "opencode",
			Thread:   sessionID,
			Kind:     kind,
		}
	}

	textByMsg := map[string][]opencodeTextPart{}

	// Parts drive tool errors, compaction and generic activity directly.
	// Text parts are buffered per message: whether a given text part is
	// "the final assistant message" or just interim activity depends on
	// the owning message, which is resolved in the pass below.
	for _, row := range partRows {
		if row.TimeUpdated > newOffset {
			newOffset = row.TimeUpdated
		}
		var pd opencodePartData
		if err := json.Unmarshal([]byte(row.Data), &pd); err != nil {
			continue
		}
		ts := msTime(row.TimeUpdated)
		if ts.IsZero() {
			ts = msTime(row.TimeCreated)
		}

		switch pd.Type {
		case "text":
			textByMsg[row.MessageID] = append(textByMsg[row.MessageID], opencodeTextPart{
				Text:        pd.Text,
				TimeUpdated: row.TimeUpdated,
				SessionID:   row.SessionID,
			})
		case "tool":
			if pd.State != nil && pd.State.Status == "error" {
				ev := base(KindToolError, row.SessionID, ts)
				ev.Tool = pd.Tool
				ev.Excerpt = Excerpt(pd.State.Error)
				events = append(events, ev)
				continue
			}
			ev := base(KindActivity, row.SessionID, ts)
			ev.Tool = pd.Tool
			events = append(events, ev)
		case "compaction":
			events = append(events, base(KindCompaction, row.SessionID, ts))
		default:
			// reasoning, step-start, step-finish, file, patch, ...
			events = append(events, base(KindActivity, row.SessionID, ts))
		}
	}

	emitText := func(kind, sessionID string, ts time.Time, texts []opencodeTextPart) {
		if len(texts) == 0 {
			return
		}
		last := texts[len(texts)-1]
		ev := base(kind, sessionID, ts)
		ev.Excerpt = Excerpt(last.Text)
		events = append(events, ev)
		for _, t := range texts[:len(texts)-1] {
			act := base(KindActivity, sessionID, msTime(t.TimeUpdated))
			act.Excerpt = Excerpt(t.Text)
			events = append(events, act)
		}
	}

	// Turns (see queryTurns) are the authority on where a turn started, how
	// long it took and what it cost: those are summed in SQL across every
	// "tool-calls" step of the turn, not just its terminal message. Each
	// turn's terminal message id is recorded in terminalIDs so the message
	// loop below skips it (it must not also be treated as a lone step).
	terminalIDs := make(map[string]bool, len(turnRows))
	for _, t := range turnRows {
		if t.TerminalTimeUpdated > newOffset {
			newOffset = t.TerminalTimeUpdated
		}
		terminalIDs[t.TerminalID] = true

		texts := textByMsg[t.TerminalID]
		delete(textByMsg, t.TerminalID)

		completedAt := msTime(t.Completed)
		dur := time.Duration(t.Completed-t.StartTime) * time.Millisecond
		if dur < 0 {
			dur = 0
		}
		tokens := Usage{
			InputTokens:      t.SumIn,
			OutputTokens:     t.SumOut,
			CacheWriteTokens: t.SumCacheWrite,
			CacheReadTokens:  t.SumCacheRead,
			CostUSD:          t.SumCost,
		}

		if t.ErrorName != "" || t.ErrorMessage != "" {
			kind := KindAPIError
			switch {
			case isAuthErrorText(t.ErrorName) || isAuthErrorText(t.ErrorMessage):
				kind = KindAuthError
			case isUsageLimitErrorText(t.ErrorName) || isUsageLimitErrorText(t.ErrorMessage):
				kind = KindUsageLimit
			}
			errText := t.ErrorMessage
			if errText == "" {
				errText = t.ErrorName
			}
			ev := base(kind, t.SessionID, completedAt)
			ev.Duration = dur
			ev.Tokens = tokens
			ev.Excerpt = Excerpt(errText)
			events = append(events, ev)
			// Any interim text before the error is still useful context.
			for _, tp := range texts {
				act := base(KindActivity, t.SessionID, msTime(tp.TimeUpdated))
				act.Excerpt = Excerpt(tp.Text)
				events = append(events, act)
			}
		} else {
			ev := base(KindTurnEnd, t.SessionID, completedAt)
			ev.Duration = dur
			ev.Tokens = tokens
			events = append(events, ev)
			emitText(KindAssistantMsg, t.SessionID, completedAt, texts)
		}
	}

	for _, row := range msgRows {
		if row.TimeUpdated > newOffset {
			newOffset = row.TimeUpdated
		}
		if terminalIDs[row.ID] {
			// Already handled above with turn-aggregated duration/tokens.
			continue
		}
		var md opencodeMessageData
		if err := json.Unmarshal([]byte(row.Data), &md); err != nil {
			continue
		}
		texts := textByMsg[row.ID]
		delete(textByMsg, row.ID)

		switch md.Role {
		case "user":
			ts := msTime(md.Time.Created)
			if ts.IsZero() {
				ts = msTime(row.TimeUpdated)
			}
			emitText(KindUserMessage, row.SessionID, ts, texts)
		case "assistant":
			// Not a turn-ending message: either a "tool-calls" step
			// (finish=="tool-calls", completed) or a turn still in
			// progress (not completed yet). Either way this isn't a
			// final answer, so any text is interim, not KindAssistantMsg.
			if md.isStep() {
				events = append(events, base(KindActivity, row.SessionID, msTime(md.Time.Completed)))
			}
			for _, t := range texts {
				act := base(KindActivity, row.SessionID, msTime(t.TimeUpdated))
				act.Excerpt = Excerpt(t.Text)
				events = append(events, act)
			}
		default:
			for _, t := range texts {
				act := base(KindActivity, row.SessionID, msTime(t.TimeUpdated))
				act.Excerpt = Excerpt(t.Text)
				events = append(events, act)
			}
		}
	}

	// Text parts whose owning message wasn't itself updated this poll
	// (message row unchanged, but a part attached to it is new).
	for _, texts := range textByMsg {
		for _, t := range texts {
			act := base(KindActivity, t.SessionID, msTime(t.TimeUpdated))
			act.Excerpt = Excerpt(t.Text)
			events = append(events, act)
		}
	}

	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Time.Before(events[j].Time)
	})

	return events, newOffset
}
