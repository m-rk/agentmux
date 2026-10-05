package retire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// lookupUser is user.Lookup as a var so tests don't need real accounts.
var lookupUser = user.Lookup

// --- amp threads ---

// ampSource builds the transcript Source for an amp instance from its
// registry fields, mirroring ops.Source: the run user's home plus the
// instance's op env-file when one exists.
func ampSource(instance string, fields map[string]string) transcript.Source {
	src := transcript.Source{
		Instance:    instance,
		Agent:       "amp",
		Workdir:     fields["AGENTMUX_WORKDIR"],
		RunUser:     fields["AGENTMUX_RUN_USER"],
		AmpRunnerID: fields["AGENTMUX_AMP_RUNNER_ID"],
	}
	home := ""
	if runUser := fields["AGENTMUX_RUN_USER"]; runUser != "" {
		if u, err := lookupUser(runUser); err == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	src.Home = home
	if home != "" {
		envFile := filepath.Join(home, ".agentmux", "env", instance+".env")
		if info, err := os.Stat(envFile); err == nil && info.Mode().IsRegular() {
			src.AmpEnvFile = envFile
		}
	}
	return src
}

// ampThreads lists the instance's amp threads through the transcript
// reader (which maps threads to the runner, so overlapping checkouts
// don't leak another runner's threads in).
var listAmpThreads = func(ctx context.Context, src transcript.Source) ([]transcript.Thread, error) {
	r, err := transcript.For("amp")
	if err != nil {
		return nil, err
	}
	return r.Threads(ctx, src)
}

// ampArchiveRun runs `amp threads archive <id>` the way the transcript
// reader runs amp (empty stdin, through the instance's op env-file when
// one exists). A var so tests substitute a fake.
var ampArchiveRun = func(ctx context.Context, src transcript.Source, args ...string) ([]byte, error) {
	return transcript.AmpRun(ctx, src, args...)
}

// liveAmpThread resolves the instance's amp thread: the newest thread on
// its runner. Task instances run exactly one thread (started by
// `sessions run` with the task title), so the newest is the task's.
func liveAmpThread(ctx context.Context, instance string, fields map[string]string) (string, error) {
	src := ampSource(instance, fields)
	threads, err := listAmpThreads(ctx, src)
	if err != nil {
		return "", errorf(safesend.ReasonFailed, "listing amp threads for %s: %v", instance, err)
	}
	if len(threads) == 0 {
		return "", errorf(safesend.ReasonNotFound, "no amp thread found for %s", instance)
	}
	return threads[0].ID, nil
}

// ampArchive archives one amp thread. The thread stays readable on
// ampcode.com; gc deletes it after retention.
func ampArchive(ctx context.Context, instance string, fields map[string]string, thread string) error {
	if !transcript.ValidAmpThreadID(thread) {
		return errorf(safesend.ReasonInvalid, "%q is not an amp thread id", thread)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := ampArchiveRun(ctx, ampSource(instance, fields), "threads", "archive", thread); err != nil {
		return errorf(safesend.ReasonFailed, "archiving amp thread %s: %v", thread, ampErr(err))
	}
	return nil
}

// ampDelete permanently deletes one amp thread, local and server-side.
func ampDelete(ctx context.Context, instance, thread string) error {
	if !transcript.ValidAmpThreadID(thread) {
		return errorf(safesend.ReasonInvalid, "%q is not an amp thread id", thread)
	}
	src := transcript.Source{Instance: instance, Agent: "amp"}
	if u, err := user.Current(); err == nil {
		src.Home = u.HomeDir
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := ampArchiveRun(ctx, src, "threads", "delete", thread); err != nil {
		return errorf(safesend.ReasonFailed, "deleting amp thread %s: %v", thread, ampErr(err))
	}
	return nil
}

// ampErr trims CLI stderr to one line for the refusal detail.
func ampErr(err error) error {
	var ee *exec.ExitError
	if e, ok := err.(*exec.ExitError); ok {
		ee = e
		_ = ee
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return fmt.Errorf("%s", msg)
}

// --- opencode sessions ---

// liveOpencodeSessions lists the stored opencode session ids whose
// directory is the workdir, through the sqlite3 CLI in read-only JSON
// mode — the same read path as the transcript reader, but ids only, so
// no message bodies are ever loaded.
func liveOpencodeSessions(ctx context.Context, instance string, fields map[string]string) ([]string, error) {
	home := ""
	if runUser := fields["AGENTMUX_RUN_USER"]; runUser != "" {
		if u, err := lookupUser(runUser); err == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	db := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	if _, err := os.Stat(db); err != nil {
		// No database means no stored sessions to retire: a task
		// instance that never got going leaves nothing behind.
		return nil, nil
	}
	workdir := fields["AGENTMUX_WORKDIR"]
	out, err := sqliteQuery(ctx, db, `SELECT id FROM session WHERE directory = `+sqlQuote(workdir))
	if err != nil {
		return nil, errorf(safesend.ReasonFailed, "listing opencode sessions for %s: %v", instance, err)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &rows); err != nil {
		return nil, errorf(safesend.ReasonFailed, "listing opencode sessions for %s: %v", instance, err)
	}
	var ids []string
	for _, r := range rows {
		if r.ID != "" {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// opencodeDeleteSessions deletes stored opencode sessions by id: the
// session row plus its messages and parts. Scoped to the exact ids the
// retire recorded, never the whole database.
func opencodeDeleteSessions(rec Record) error {
	if len(rec.OpencodeSessions) == 0 {
		return nil
	}
	home := ""
	if u, err := user.Current(); err == nil {
		home = u.HomeDir
	}
	db := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, id := range rec.OpencodeSessions {
		q := sqlQuote(id)
		for _, sql := range []string{
			`DELETE FROM part WHERE session_id = ` + q,
			`DELETE FROM message WHERE session_id = ` + q,
			`DELETE FROM session WHERE id = ` + q,
		} {
			if err := sqliteExec(ctx, db, sql); err != nil {
				return errorf(safesend.ReasonFailed, "deleting opencode session %s: %v", id, err)
			}
		}
	}
	return nil
}

// sqliteQuery runs sql against db and returns stdout. A var so tests
// substitute a fake without touching a real database or PATH.
var sqliteQuery = func(ctx context.Context, db, sql string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", db, sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// sqliteExec runs a write against db. A var so tests substitute a fake.
var sqliteExec = func(ctx context.Context, db, sql string) error {
	cmd := exec.CommandContext(ctx, "sqlite3", db, sql)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sqlite3: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
