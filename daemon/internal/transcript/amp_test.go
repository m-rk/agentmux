package transcript

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const (
	ampT1 = "T-00000000-0000-4000-8000-000000000001"
	ampT2 = "T-00000000-0000-4000-8000-000000000002"
	ampT3 = "T-00000000-0000-4000-8000-000000000003"
	ampT4 = "T-00000000-0000-4000-8000-000000000004"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeAmp replaces ampExec with canned output and records the calls.
type fakeAmp struct {
	t       *testing.T
	calls   [][]string
	exports map[string]string // thread id -> fixture
}

func newFakeAmp(t *testing.T) *fakeAmp {
	f := &fakeAmp{t: t, exports: map[string]string{
		ampT1: "amp_export_t1.json", ampT2: "amp_export_t2.json", ampT3: "amp_export_t3.json",
	}}
	old := ampExec
	ampExec = func(_ context.Context, _ Source, args ...string) ([]byte, error) {
		f.calls = append(f.calls, args)
		switch {
		case reflect.DeepEqual(args, []string{"threads", "list", "--json"}):
			return fixture(t, "amp_list.json"), nil
		case len(args) == 3 && args[0] == "threads" && args[1] == "export":
			if name, ok := f.exports[args[2]]; ok {
				return fixture(t, name), nil
			}
		}
		return nil, errors.New("fake amp: unexpected " + strings.Join(args, " "))
	}
	t.Cleanup(func() { ampExec = old })
	return f
}

func (f *fakeAmp) exportCalls() int {
	n := 0
	for _, c := range f.calls {
		if c[1] == "export" {
			n++
		}
	}
	return n
}

func ampSrc(t *testing.T) Source {
	return Source{Agent: "amp", Workdir: "/work/proj", Home: t.TempDir(), AmpRunnerID: "runner-a"}
}

func TestAmpRegistered(t *testing.T) {
	if _, err := For("amp"); err != nil {
		t.Fatal(err)
	}
}

func TestAmpThreadsFiltersByRunnerAndTree(t *testing.T) {
	f := newFakeAmp(t)
	src := ampSrc(t)
	ts, err := ampReader{}.Threads(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	// T4 is outside the tree (and /work/project-two is not under /work/proj),
	// T3 is in the tree but belongs to another runner.
	if len(ts) != 2 || ts[0].ID != ampT2 || ts[1].ID != ampT1 {
		t.Fatalf("threads: %+v", ts)
	}
	if ts[0].Title != "sub dir" || ts[0].Messages != 2 || ts[0].Updated.IsZero() {
		t.Fatalf("fields: %+v", ts[0])
	}
	if f.exportCalls() != 3 {
		t.Fatalf("exports: %v", f.calls)
	}

	// The other runner sees T3 only, from the same cache.
	src.AmpRunnerID = "runner-b"
	ts, err = ampReader{}.Threads(context.Background(), src)
	if err != nil || len(ts) != 1 || ts[0].ID != ampT3 {
		t.Fatalf("runner-b: %+v %v", ts, err)
	}
	if f.exportCalls() != 3 {
		t.Fatalf("cache miss: %v", f.calls)
	}
}

func TestAmpThreadsCacheFile(t *testing.T) {
	newFakeAmp(t)
	src := ampSrc(t)
	if _, err := (ampReader{}).Threads(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(src.Home, ".cache/agentmux/amp-thread-runners.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cache mode %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(p))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("cache dir mode %v", di.Mode().Perm())
	}
	if m := loadAmpCache(src.Home); m[ampT3].Runner != "runner-b" || m[ampT1].Runner != "runner-a" {
		t.Errorf("cache: %+v", m)
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Errorf("temp files left behind: %v", ents)
	}
}

func ampMaxExportsForTest(n int) func() {
	old := ampMaxExports
	ampMaxExports = n
	return func() { ampMaxExports = old }
}

func TestAmpThreadsExportBound(t *testing.T) {
	f := newFakeAmp(t)
	src := ampSrc(t)
	old := ampMaxExportsForTest(2)
	defer old()
	ts, _ := ampReader{}.Threads(context.Background(), src)
	// Newest first: T2 then T3 are exported; T1 waits for the next call.
	if f.exportCalls() != 2 || len(ts) != 1 || ts[0].ID != ampT2 {
		t.Fatalf("threads=%+v calls=%v", ts, f.calls)
	}
}

func TestAmpReadMapsMessages(t *testing.T) {
	newFakeAmp(t)
	src := ampSrc(t)
	p, err := ampReader{}.Read(context.Background(), src, ampT1, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if p.Thread != ampT1 || p.Older != "" {
		t.Fatalf("page: %+v", p)
	}
	// user, assistant (text+2 calls), tool results, assistant "Done."; the
	// thinking-only message is dropped.
	if len(p.Messages) != 4 {
		t.Fatalf("%d messages: %+v", len(p.Messages), p.Messages)
	}
	for _, m := range p.Messages {
		if !m.Untrusted || m.Thread != ampT1 || m.Time.IsZero() {
			t.Errorf("message meta: %+v", m)
		}
		if strings.Contains(m.Text, "private reasoning") {
			t.Error("thinking leaked")
		}
	}
	u, a, r := p.Messages[0], p.Messages[1], p.Messages[2]
	if u.Role != RoleUser || strings.Contains(u.Text, "FAKEFAKE") || !strings.Contains(u.Text, "[redacted]") {
		t.Errorf("user not redacted: %q", u.Text)
	}
	wantCalls := []ToolCall{{Name: "shell_command", Summary: "ls -la"}, {Name: "Read", Summary: "/work/proj/a.txt"}}
	if a.Role != RoleAssistant || a.Text != "Running ls." || !reflect.DeepEqual(a.Tools, wantCalls) {
		t.Errorf("assistant: %+v", a)
	}
	wantRes := []ToolCall{{Name: "shell_command", Summary: "a.txt b.txt"}, {Name: "Read", Summary: "", Error: true}}
	if r.Role != RoleTool || !reflect.DeepEqual(r.Tools, wantRes) {
		t.Errorf("tool results: %+v", r.Tools)
	}
	if p.Messages[3].Text != "Done." {
		t.Errorf("last: %+v", p.Messages[3])
	}
}

func TestAmpReadPaging(t *testing.T) {
	newFakeAmp(t)
	src := ampSrc(t)
	p, err := ampReader{}.Read(context.Background(), src, ampT1, "", 3)
	if err != nil || len(p.Messages) != 3 || p.Older != "1" || p.Messages[2].Text != "Done." {
		t.Fatalf("newest: %+v %v", p, err)
	}
	p, err = ampReader{}.Read(context.Background(), src, ampT1, p.Older, 3)
	if err != nil || len(p.Messages) != 1 || p.Older != "" || p.Messages[0].Role != RoleUser {
		t.Fatalf("older: %+v %v", p, err)
	}
	if _, err = (ampReader{}).Read(context.Background(), src, ampT1, "99", 3); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor: %v", err)
	}
}

func TestAmpReadDefaultsToNewestThread(t *testing.T) {
	newFakeAmp(t)
	p, err := ampReader{}.Read(context.Background(), ampSrc(t), "", "", 10)
	if err != nil || p.Thread != ampT2 || len(p.Messages) != 2 || p.Messages[1].ID != "2" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestAmpReadForeignRunnerIsNoThread(t *testing.T) {
	newFakeAmp(t)
	src := ampSrc(t)
	if _, err := (ampReader{}).Read(context.Background(), src, ampT3, "", 10); !errors.Is(err, ErrNoThread) {
		t.Fatalf("got %v", err)
	}
	if _, err := AmpThreadState(context.Background(), src, ampT3); !errors.Is(err, ErrNoThread) {
		t.Fatalf("state: %v", err)
	}
}

func TestAmpReadRejectsBadIDWithoutRunningCLI(t *testing.T) {
	f := newFakeAmp(t)
	for _, id := range []string{"x", "T-", "T-zz", "--help", "T-0001; rm -rf /", "../T-1", "t-0001", "T-0001\n"} {
		if _, err := (ampReader{}).Read(context.Background(), ampSrc(t), id, "", 10); !errors.Is(err, ErrNoThread) {
			t.Errorf("%q: %v", id, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("CLI ran: %v", f.calls)
	}
}

func TestAmpReadCachesRunner(t *testing.T) {
	f := newFakeAmp(t)
	src := ampSrc(t)
	if _, err := (ampReader{}).Read(context.Background(), src, ampT1, "", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := (ampReader{}).Threads(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	// T1 came from Read's cache entry: only T2 and T3 needed exports in
	// Threads (plus the one from Read).
	if f.exportCalls() != 3 {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestAmpThreadState(t *testing.T) {
	newFakeAmp(t)
	src := ampSrc(t)
	if s, err := AmpThreadState(context.Background(), src, ampT1); err != nil || s != "idle" {
		t.Fatalf("%q %v", s, err)
	}
	if s, err := AmpThreadState(context.Background(), src, ""); err != nil || s != "streaming" {
		t.Fatalf("newest: %q %v", s, err)
	}
}

func TestAmpCommandPlain(t *testing.T) {
	cmd, err := ampCommand(context.Background(), Source{Home: t.TempDir()}, "threads", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cmd.Args, []string{"amp", "threads", "list", "--json"}) {
		t.Errorf("argv: %v", cmd.Args)
	}
	if cmd.Stdin == nil {
		t.Error("stdin not set to an empty reader")
	}
}

func TestAmpCommandWrapsInOp(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config/op"), 0o700); err != nil {
		t.Fatal(err)
	}
	const tok = "fake-service-token-123"
	if err := os.WriteFile(filepath.Join(home, ".config/op/service_account_token"), []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := Source{Home: home, AmpEnvFile: "/envs/x.env"}
	cmd, err := ampCommand(context.Background(), src, "threads", "export", ampT1)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"op", "run", "--env-file=/envs/x.env", "--", "/usr/bin/env", "-u", "OP_SERVICE_ACCOUNT_TOKEN", "amp", "threads", "export", ampT1}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("argv: %v", cmd.Args)
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, tok) {
			t.Error("token in argv")
		}
	}
	found := false
	for _, e := range cmd.Env {
		if e == "OP_SERVICE_ACCOUNT_TOKEN="+tok {
			found = true
		}
	}
	if !found {
		t.Error("token not in child env")
	}
	if cmd.Stdin == nil {
		t.Error("stdin not set")
	}
}

func TestAmpCommandTokenErrors(t *testing.T) {
	home := t.TempDir()
	src := Source{Home: home, AmpEnvFile: "/envs/x.env"}
	_, err := ampCommand(context.Background(), src, "threads", "list")
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, ".config/op/service_account_token")) {
		t.Fatalf("missing: %v", err)
	}
	os.MkdirAll(filepath.Join(home, ".config/op"), 0o700)
	os.WriteFile(filepath.Join(home, ".config/op/service_account_token"), []byte(" \n"), 0o600)
	if _, err = ampCommand(context.Background(), src, "threads", "list"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty: %v", err)
	}
}

// TestAmpExecStdinEmpty runs the real ampExec against a stand-in amp on PATH
// that reports how much stdin it can read.
func TestAmpExecStdinEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"args=$* stdin=$(cat | wc -c | tr -d ' ')\"\n"
	if err := os.WriteFile(filepath.Join(dir, "amp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := ampExec(context.Background(), Source{Home: t.TempDir()}, "threads", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "args=threads list --json stdin=0" {
		t.Fatalf("got %q", got)
	}
}
