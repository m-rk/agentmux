package retire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

func withEnvDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := discovery.EnvDir
	discovery.EnvDir = dir
	t.Cleanup(func() { discovery.EnvDir = prev })
	return dir
}

// fakeThreads swaps listAmpThreads with canned threads and records the source.
func fakeThreads(t *testing.T, threads []transcript.Thread, listErr error) *transcript.Source {
	t.Helper()
	var got *transcript.Source
	old := listAmpThreads
	listAmpThreads = func(_ context.Context, src transcript.Source) ([]transcript.Thread, error) {
		c := src
		got = &c
		return threads, listErr
	}
	t.Cleanup(func() { listAmpThreads = old })
	return got
}

func thread(id, title string) transcript.Thread {
	return transcript.Thread{ID: id, Title: title}
}

func TestLiveAmpThreadPicksNewest(t *testing.T) {
	fakeThreads(t, []transcript.Thread{
		thread("T-00000000-0000-4000-8000-000000000002", "second"),
		thread("T-00000000-0000-4000-8000-000000000001", "first"),
	}, nil)
	got, err := liveAmpThread(context.Background(), "task-1", map[string]string{
		"AGENTMUX_WORKDIR": "/w/task-1", "AGENTMUX_AMP_RUNNER_ID": "task-1@host",
	})
	if err != nil {
		t.Fatalf("liveAmpThread: %v", err)
	}
	if got != "T-00000000-0000-4000-8000-000000000002" {
		t.Errorf("thread = %q, want the newest", got)
	}
}

func TestLiveAmpThreadNoThreadsIsNotFound(t *testing.T) {
	fakeThreads(t, nil, nil)
	_, err := liveAmpThread(context.Background(), "task-1", map[string]string{})
	if ReasonOf(err) != "not_found" {
		t.Errorf("reason = %s, want not_found", ReasonOf(err))
	}
}

func TestLiveAmpThreadPropagatesListError(t *testing.T) {
	fakeThreads(t, nil, errors.New("amp exploded"))
	_, err := liveAmpThread(context.Background(), "task-1", map[string]string{})
	if ReasonOf(err) != "failed" {
		t.Errorf("reason = %s, want failed", ReasonOf(err))
	}
}

func TestAmpArchiveRunsTheDocumentedCommand(t *testing.T) {
	var calls [][]string
	old := ampArchiveRun
	ampArchiveRun = func(_ context.Context, _ transcript.Source, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("{}"), nil
	}
	t.Cleanup(func() { ampArchiveRun = old })
	id := "T-00000000-0000-4000-8000-000000000001"
	if err := ampArchive(context.Background(), "task-1", map[string]string{}, id); err != nil {
		t.Fatalf("ampArchive: %v", err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"threads", "archive", id}}) {
		t.Errorf("calls = %v, want threads archive %s", calls, id)
	}
}

func TestAmpArchiveRefusesBadID(t *testing.T) {
	if err := ampArchive(context.Background(), "task-1", map[string]string{}, "nope"); ReasonOf(err) != "invalid" {
		t.Errorf("reason = %s, want invalid", ReasonOf(err))
	}
}

func TestAmpDeleteRunsTheDocumentedCommand(t *testing.T) {
	var calls [][]string
	old := ampArchiveRun
	ampArchiveRun = func(_ context.Context, _ transcript.Source, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("{}"), nil
	}
	t.Cleanup(func() { ampArchiveRun = old })
	id := "T-00000000-0000-4000-8000-000000000001"
	if err := ampDelete(context.Background(), "task-1", id); err != nil {
		t.Fatalf("ampDelete: %v", err)
	}
	if !reflect.DeepEqual(calls, [][]string{{"threads", "delete", id}}) {
		t.Errorf("calls = %v, want threads delete %s", calls, id)
	}
}

func TestLiveOpencodeSessionsListsWorkdirSessions(t *testing.T) {
	var queries []string
	old := sqliteQuery
	sqliteQuery = func(_ context.Context, db, sql string) ([]byte, error) {
		queries = append(queries, sql)
		if !strings.Contains(sql, "FROM session") || !strings.Contains(sql, "/w/task-2") {
			t.Errorf("query = %q, want a session-directory lookup for the workdir", sql)
		}
		if !strings.HasSuffix(db, filepath.Join(".local", "share", "opencode", "opencode.db")) {
			t.Errorf("db = %q, want the opencode database path", db)
		}
		rows, _ := json.Marshal([]map[string]string{{"id": "ses-aaa"}, {"id": "ses-bbb"}})
		return rows, nil
	}
	t.Cleanup(func() { sqliteQuery = old })
	home := t.TempDir()
	dbDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "opencode.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldLookup := lookupUser
	lookupUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, HomeDir: home}, nil
	}
	t.Cleanup(func() { lookupUser = oldLookup })
	got, err := liveOpencodeSessions(context.Background(), "task-2", map[string]string{
		"AGENTMUX_WORKDIR": "/w/task-2", "AGENTMUX_RUN_USER": "taskuser",
	})
	if err != nil {
		t.Fatalf("liveOpencodeSessions: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"ses-aaa", "ses-bbb"}) {
		t.Errorf("sessions = %v", got)
	}
	if len(queries) != 1 {
		t.Errorf("queries = %d, want exactly one id-only lookup (no message bodies loaded)", len(queries))
	}
}

func TestLiveOpencodeSessionsMissingDBIsEmpty(t *testing.T) {
	oldLookup := lookupUser
	lookupUser = func(name string) (*user.User, error) {
		return &user.User{Username: name, HomeDir: t.TempDir()}, nil
	}
	t.Cleanup(func() { lookupUser = oldLookup })
	got, err := liveOpencodeSessions(context.Background(), "task-2", map[string]string{
		"AGENTMUX_WORKDIR": "/w/task-2", "AGENTMUX_RUN_USER": "taskuser",
	})
	if err != nil {
		t.Fatalf("missing db: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("sessions = %v, want none for a missing database", got)
	}
}

func TestOpencodeDeleteSessionsDeletesScopedRows(t *testing.T) {
	var execs []string
	old := sqliteExec
	sqliteExec = func(_ context.Context, _ string, sql string) error {
		execs = append(execs, sql)
		return nil
	}
	t.Cleanup(func() { sqliteExec = old })
	rec := Record{Instance: "task-2", Agent: "opencode", OpencodeSessions: []string{"ses-aaa"}}
	if err := opencodeDeleteSessions(rec); err != nil {
		t.Fatalf("opencodeDeleteSessions: %v", err)
	}
	if len(execs) != 3 {
		t.Fatalf("execs = %v, want part+message+session deletes", execs)
	}
	for _, want := range []string{"DELETE FROM part", "DELETE FROM message", "DELETE FROM session"} {
		found := false
		for _, e := range execs {
			if strings.HasPrefix(e, want) && strings.Contains(e, "ses-aaa") {
				found = true
			}
		}
		if !found {
			t.Errorf("no %q scoped to ses-aaa in %v", want, execs)
		}
	}
}

func TestOpencodeDeleteSessionsEmptyIsNoop(t *testing.T) {
	if err := opencodeDeleteSessions(Record{Instance: "task-2"}); err != nil {
		t.Fatalf("empty: %v", err)
	}
}

func TestRemoveUnitsAndRegistry(t *testing.T) {
	dir := withEnvDir(t)
	if err := os.WriteFile(filepath.Join(dir, "task-9.env"), []byte("AGENTMUX_INSTANCE_NAME=task-9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// systemctl/launchctl point at the real host here; the test only
	// asserts the file effects (unit paths that don't exist are skipped
	// and the registry entry is removed). /etc/systemd/system is not
	// writable in test, so removal errors for present-but-unwritable
	// files would surface — missing files are the expected case.
	if err := removeUnits("task-9-definitely-not-a-real-instance"); err != nil {
		t.Fatalf("removeUnits missing units: %v", err)
	}
	if err := removeRegistry("task-9"); err != nil {
		t.Fatalf("removeRegistry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-9.env")); !os.IsNotExist(err) {
		t.Error("registry entry still present")
	}
}
