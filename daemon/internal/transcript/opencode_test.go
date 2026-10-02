package transcript

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSqlite answers the reader's queries from canned JSON, keyed on a
// substring of the SQL, and records every statement it was given.
type fakeSqlite struct {
	answers map[string]string
	sqls    []string
}

func (f *fakeSqlite) run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) != 4 || args[0] != "-readonly" || args[1] != "-json" {
		return nil, errors.New("unexpected sqlite3 args")
	}
	f.sqls = append(f.sqls, args[3])
	for key, out := range f.answers {
		if strings.Contains(args[3], key) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func opencodeFixture(t *testing.T, answers map[string]string) (*opencodeReader, *fakeSqlite, Source) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeSqlite{answers: answers}
	return &opencodeReader{run: f.run}, f, Source{Agent: "opencode", Workdir: "/work/it's", Home: home}
}

var opencodeAnswers = map[string]string{
	"FROM session WHERE directory": `[{"id":"ses_b","title":"Newer one","time_updated":1700000200000,"messages":3},
		{"id":"ses_a","title":"Older  one","time_updated":1700000100000,"messages":0}]`,
	"FROM session WHERE id = 'ses_b'": `[{"id":"ses_b"}]`,
	"FROM session WHERE id = 'ses_e'": `[{"id":"ses_e"}]`,
	"FROM message WHERE session_id = 'ses_b'": `[
		{"id":"m1","time_created":1700000110000,"role":"user"},
		{"id":"m2","time_created":1700000120000,"role":"assistant"},
		{"id":"m3","time_created":1700000130000,"role":"assistant"},
		{"id":"m4","time_created":1700000140000,"role":"assistant"}]`,
	"FROM part WHERE session_id = 'ses_b'": `[
		{"message_id":"m1","type":"text","text":"deploy with token=FAKEsecretvalue99"},
		{"message_id":"m2","type":"text","text":"Running it."},
		{"message_id":"m2","type":"tool","tool":"bash","status":"error","input":"{\"command\":\"make\",\"workdir\":\"/x\"}"},
		{"message_id":"m3","type":"tool","tool":"read","status":"completed","input":"{\"filePath\":\"/work/a.go\"}"},
		{"message_id":"m4","type":"text","text":"Done."},
		{"message_id":"orphan","type":"text","text":"ignored"}]`,
}

func TestOpencodeThreads(t *testing.T) {
	r, f, src := opencodeFixture(t, opencodeAnswers)
	th, err := r.Threads(context.Background(), src)
	if err != nil || len(th) != 2 {
		t.Fatalf("threads: %v %v", th, err)
	}
	if th[0].ID != "ses_b" || th[0].Messages != 3 || th[1].Title != "Older one" || th[0].Updated.UnixMilli() != 1700000200000 {
		t.Fatalf("threads: %+v", th)
	}
	// Directory filter is applied in SQL with the workdir quoted.
	if !strings.Contains(f.sqls[0], `directory = '/work/it''s'`) {
		t.Fatalf("sql: %s", f.sqls[0])
	}
}

func TestOpencodeRead(t *testing.T) {
	r, _, src := opencodeFixture(t, opencodeAnswers)
	p, err := r.Read(context.Background(), src, "ses_b", "", 10)
	if err != nil || len(p.Messages) != 4 {
		t.Fatalf("read: %+v %v", p, err)
	}
	m := p.Messages
	if m[0].Role != RoleUser || !strings.Contains(m[0].Text, "[redacted]") || strings.Contains(m[0].Text, "FAKEsecret") {
		t.Fatalf("user/redaction: %+v", m[0])
	}
	if m[1].Text != "Running it." || m[1].Tools[0].Name != "bash" || m[1].Tools[0].Summary != "make" || !m[1].Tools[0].Error {
		t.Fatalf("assistant tool error: %+v", m[1])
	}
	if m[2].Tools[0].Summary != "/work/a.go" || m[2].Tools[0].Error {
		t.Fatalf("tool ok: %+v", m[2])
	}
	for _, x := range m {
		if !x.Untrusted || x.Thread != "ses_b" {
			t.Errorf("not untrusted/threaded: %+v", x)
		}
	}
	// Paging across pages.
	p, _ = r.Read(context.Background(), src, "ses_b", "", 3)
	if len(p.Messages) != 3 || p.Older != "1" {
		t.Fatalf("page 1: %+v", p)
	}
	p, _ = r.Read(context.Background(), src, "ses_b", p.Older, 3)
	if len(p.Messages) != 1 || p.Messages[0].ID != "m1" || p.Older != "" {
		t.Fatalf("page 2: %+v", p)
	}
	// Default thread is the newest.
	if p, err = r.Read(context.Background(), src, "", "", 10); err != nil || p.Thread != "ses_b" {
		t.Fatalf("default thread: %+v %v", p, err)
	}
}

func TestOpencodeUnknownAndEmpty(t *testing.T) {
	r, f, src := opencodeFixture(t, opencodeAnswers)
	// A session in another directory yields no row from the filtered lookup.
	if _, err := r.Read(context.Background(), src, "ses_other'; DROP TABLE x;--", "", 10); !errors.Is(err, ErrNoThread) {
		t.Fatalf("unknown: %v", err)
	}
	last := f.sqls[len(f.sqls)-1]
	if !strings.Contains(last, `'ses_other''; DROP TABLE x;--'`) || !strings.Contains(last, `directory = '/work/it''s'`) {
		t.Fatalf("sql not quoted/filtered: %s", last)
	}
	p, err := r.Read(context.Background(), src, "ses_e", "", 10)
	if err != nil || len(p.Messages) != 0 || p.Older != "" {
		t.Fatalf("empty session: %+v %v", p, err)
	}
	// No sessions at all.
	r2, _, src2 := opencodeFixture(t, map[string]string{})
	if _, err := r2.Read(context.Background(), src2, "", "", 10); !errors.Is(err, ErrNoThread) {
		t.Fatalf("no sessions: %v", err)
	}
}

func TestOpencodeMissingDB(t *testing.T) {
	r := &opencodeReader{run: func(context.Context, ...string) ([]byte, error) { t.Fatal("ran sqlite"); return nil, nil }}
	_, err := r.Threads(context.Background(), Source{Home: t.TempDir(), Workdir: "/w"})
	if !errors.Is(err, ErrNoThread) {
		t.Fatalf("missing db: %v", err)
	}
}
