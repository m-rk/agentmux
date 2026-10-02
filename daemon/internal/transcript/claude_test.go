package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testWorkdir = "/work/proj.x"

// claudeFixture copies testdata/claude into a temp home with distinct mtimes
// (sess-notitle newest, then sess-main, then sess-empty).
func claudeFixture(t *testing.T) Source {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-work-proj-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"sess-empty", "sess-main", "sess-notitle"} {
		b, err := os.ReadFile(filepath.Join("testdata", "claude", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name+".jsonl")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(p, mt, mt)
	}
	return Source{Instance: "i", Agent: "claude-code", Workdir: testWorkdir, Home: home}
}

func TestClaudeThreads(t *testing.T) {
	src := claudeFixture(t)
	r, err := For("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	th, err := r.Threads(context.Background(), src)
	if err != nil || len(th) != 3 {
		t.Fatalf("threads: %v %v", th, err)
	}
	if th[0].ID != "sess-notitle" || th[1].ID != "sess-main" || th[2].ID != "sess-empty" {
		t.Fatalf("order: %+v", th)
	}
	if th[1].Title != "Fix the widget" || th[0].Title != "investigate the flaky build please" || th[2].Title != "" {
		t.Fatalf("titles: %+v", th)
	}
}

func TestClaudeReadMergesAndFilters(t *testing.T) {
	src := claudeFixture(t)
	p, err := claudeReader{}.Read(context.Background(), src, "sess-main", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range p.Messages {
		roles = append(roles, m.Role)
		if !m.Untrusted || m.Thread != "sess-main" {
			t.Errorf("message not marked untrusted/threaded: %+v", m)
		}
	}
	want := "user assistant tool assistant tool user"
	if got := strings.Join(roles, " "); got != want {
		t.Fatalf("roles %q, want %q", got, want)
	}
	a := p.Messages[1]
	if a.ID != "msg_1" || a.Text != "Looking at it." || len(a.Tools) != 1 || a.Tools[0].Name != "Bash" || a.Tools[0].Summary != "ls -la" {
		t.Fatalf("merged assistant: %+v", a)
	}
	if tr := p.Messages[2]; len(tr.Tools) != 1 || tr.Tools[0].Name != "Bash" || !tr.Tools[0].Error {
		t.Fatalf("tool result error: %+v", tr)
	}
	if tr := p.Messages[4]; tr.Tools[0].Name != "Read" || tr.Tools[0].Error {
		t.Fatalf("ok tool result: %+v", tr)
	}
	if p.Messages[3].Tools[0].Summary != "/work/proj/a.go" {
		t.Fatalf("summary: %+v", p.Messages[3].Tools)
	}
	if last := p.Messages[5]; last.Text != "thanks" {
		t.Fatalf("last: %+v", last)
	}
	for _, m := range p.Messages {
		for _, bad := range []string{"secret reasoning", "subagent chatter", "meta caveat", "FAKEKEY"} {
			if strings.Contains(m.Text, bad) {
				t.Errorf("message leaks %q: %+v", bad, m)
			}
		}
	}
	if !strings.Contains(p.Messages[0].Text, "[redacted]") {
		t.Fatalf("secret not redacted: %q", p.Messages[0].Text)
	}
}

func TestClaudeReadPaging(t *testing.T) {
	src := claudeFixture(t)
	r := claudeReader{}
	p, err := r.Read(context.Background(), src, "sess-main", "", 4)
	if err != nil || len(p.Messages) != 4 || p.Older == "" || p.Messages[3].Text != "thanks" {
		t.Fatalf("page 1: %+v %v", p, err)
	}
	p, err = r.Read(context.Background(), src, "sess-main", p.Older, 4)
	if err != nil || len(p.Messages) != 2 || p.Older != "" || p.Messages[0].Role != RoleUser {
		t.Fatalf("page 2: %+v %v", p, err)
	}
	if _, err := r.Read(context.Background(), src, "sess-main", "bogus", 4); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestClaudeReadDefaultsToNewestAndEmpty(t *testing.T) {
	src := claudeFixture(t)
	p, err := claudeReader{}.Read(context.Background(), src, "", "", 10)
	if err != nil || p.Thread != "sess-notitle" || len(p.Messages) != 1 {
		t.Fatalf("default thread: %+v %v", p, err)
	}
	p, err = claudeReader{}.Read(context.Background(), src, "sess-empty", "", 10)
	if err != nil || len(p.Messages) != 0 || p.Older != "" {
		t.Fatalf("empty: %+v %v", p, err)
	}
}

func TestClaudeUnknownAndTraversal(t *testing.T) {
	src := claudeFixture(t)
	for _, id := range []string{"nope", "../sess-main", "a/b", `a\b`, "..", "."} {
		if _, err := (claudeReader{}).Read(context.Background(), src, id, "", 10); !errors.Is(err, ErrNoThread) {
			t.Errorf("thread %q: got %v", id, err)
		}
	}
	none := Source{Workdir: "/elsewhere", Home: src.Home}
	if _, err := (claudeReader{}).Read(context.Background(), none, "", "", 10); !errors.Is(err, ErrNoThread) {
		t.Errorf("no threads: %v", err)
	}
	if th, err := (claudeReader{}).Threads(context.Background(), none); err != nil || len(th) != 0 {
		t.Errorf("no threads list: %v %v", th, err)
	}
}

func TestClaudeVeryLongLine(t *testing.T) {
	src := claudeFixture(t)
	p := filepath.Join(src.Home, ".claude", "projects", "-work-proj-x", "sess-long.jsonl")
	long := strings.Repeat("x", 5<<20)
	data := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}`+"\n"+
		`{"type":"user","uuid":"u2","message":{"role":"user","content":"after"}}`+"\n", long)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	page, err := claudeReader{}.Read(context.Background(), src, "sess-long", "", 10)
	if err != nil || len(page.Messages) != 2 || page.Messages[1].Text != "after" || len(page.Messages[0].Text) > MaxTextBytes {
		t.Fatalf("long line: %v %v", len(page.Messages), err)
	}
}
