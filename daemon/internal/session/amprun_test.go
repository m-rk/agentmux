package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "run.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAmpRunArgs(t *testing.T) {
	// Flags before -x, prompt last: -x eats the next argument as its
	// message even when it names a flag (confirmed live against the amp
	// CLI 2026-10-05).
	got := AmpRunArgs("do it", "high", "", "")
	want := []string{"--stream-json", "-m", "high", "-x", "do it"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("start: %q want %q", got, want)
	}
	got = AmpRunArgs("again", "", "T-1", "")
	want = []string{"threads", "continue", "T-1", "--stream-json", "-x", "again"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("continue: %q want %q", got, want)
	}
	got = AmpRunArgs("do it", "", "", "")
	if len(got) != 3 || got[0] != "--stream-json" {
		t.Fatalf("no mode: %q", got)
	}
	// A title names a new thread and leaves it unarchived so it stays
	// findable and renamable; a continue ignores it.
	got = AmpRunArgs("do it", "", "", "AMUX-17 do the thing")
	want = []string{"--stream-json", "--title", "AMUX-17 do the thing", "--no-archive-after-execute", "-x", "do it"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("title: %q want %q", got, want)
	}
	if got := AmpRunArgs("again", "", "T-1", "AMUX-17 do the thing"); strings.Contains(strings.Join(got, " "), "--title") {
		t.Fatalf("continue takes no title: %q", got)
	}
}

// TestCleanAmpTitle trims, caps length, and rejects control bytes: the
// title rides argv through a sh double-fork, so a newline could smuggle
// a second command.
func TestCleanAmpTitle(t *testing.T) {
	if got, err := CleanAmpTitle("  AMUX-17 do the thing  "); err != nil || got != "AMUX-17 do the thing" {
		t.Fatalf("trim: %q %v", got, err)
	}
	for _, raw := range []string{"", "   "} {
		if got, err := CleanAmpTitle(raw); err != nil || got != "" {
			t.Fatalf("empty %q: %q %v", raw, got, err)
		}
	}
	if _, err := CleanAmpTitle(strings.Repeat("x", 300)); err == nil {
		t.Fatal("overlong title accepted")
	}
	for _, raw := range []string{"a\nb", "a\rb", "a\x00b"} {
		if _, err := CleanAmpTitle(raw); err == nil {
			t.Fatalf("control title %q accepted", raw)
		}
	}
}

// TestScanAmpInit reads the thread id out of the stream log, skips
// non-init lines, and tells "not yet" from "exited without init".
func TestScanAmpInit(t *testing.T) {
	p := writeLog(t,
		`{"type":"assistant","message":{"role":"assistant"}}`,
		`not json at all`,
		`{"type":"system","subtype":"init","session_id":"T-abc"}`,
	)
	if id, done, err := scanAmpInit(p); err != nil || done || id != "T-abc" {
		t.Fatalf("init: %q %v %v", id, done, err)
	}
	pending := writeLog(t, `{"type":"assistant"}`)
	if id, done, err := scanAmpInit(pending); err != nil || done || id != "" {
		t.Fatalf("pending: %q %v %v", id, done, err)
	}
	if _, err := os.Create(pending + ".done"); err != nil {
		t.Fatal(err)
	}
	if id, done, err := scanAmpInit(pending); err != nil || !done || id != "" {
		t.Fatalf("failed: %q %v %v", id, done, err)
	}
	if id, done, err := scanAmpInit(filepath.Join(t.TempDir(), "missing.jsonl")); err != nil || done || id != "" {
		t.Fatalf("missing: %q %v %v", id, done, err)
	}
}

// TestStartAmpRunReturnsOnInit fakes the spawn: the "child" writes an init
// record to the log, and startAmpRun returns its thread id.
func TestStartAmpRunReturnsOnInit(t *testing.T) {
	old := ampStartNew
	ampStartNew = func(_ context.Context, envFile string, argv []string, workdir, logPath string) (*os.Process, error) {
		if envFile != "" || argv[0] != "-x" || workdir != "/work/proj" {
			t.Fatalf("spawn: env %q argv %q in %q", envFile, argv, workdir)
		}
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			t.Fatal(err)
		}
		init, _ := json.Marshal(ampStreamInit{Type: "system", Subtype: "init", SessionID: "T-run"})
		if err := os.WriteFile(logPath, append(init, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return fakeRunProcess()
	}
	t.Cleanup(func() { ampStartNew = old })
	id, err := StartAmpRun(context.Background(), "", []string{"-x", "hi"}, "/work/proj", filepath.Join(t.TempDir(), "run.jsonl"))
	if err != nil || id != "T-run" {
		t.Fatalf("run: %q %v", id, err)
	}
}

// TestStartAmpRunFailedLaunch surfaces the child's stderr tail when it
// exits before printing init (e.g. amp rejecting the mode).
func TestStartAmpRunFailedLaunch(t *testing.T) {
	old := ampStartNew
	ampStartNew = func(_ context.Context, _ string, _ []string, _, logPath string) (*os.Process, error) {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, []byte("Error: Unexpected error inside Amp CLI.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Create(logPath + ".done"); err != nil {
			t.Fatal(err)
		}
		return fakeRunProcess()
	}
	t.Cleanup(func() { ampStartNew = old })
	_, err := StartAmpRun(context.Background(), "", []string{"-x"}, "/w", filepath.Join(t.TempDir(), "run.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "Unexpected error") {
		t.Fatalf("failed launch: %v", err)
	}
}

// fakeRunProcess stands in for the detached middle child: already
// released, so Release is a no-op. Uses the test binary itself (which
// always exists) rather than a fixed /bin path.
func fakeRunProcess() (*os.Process, error) {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return nil, err
	}
	// Release detaches; on our own pid it fails, which startAmpRun
	// ignores — the point is only that the value is non-nil.
	_ = p.Release()
	// Re-find so the returned process was never released.
	return os.FindProcess(os.Getpid())
}

// TestCheckAmpModeValidation rejects hostile modes without spawning.
func TestCheckAmpModeValidation(t *testing.T) {
	if err := checkAmpMode(context.Background(), "", ""); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if err := checkAmpMode(context.Background(), "", strings.Repeat("x", 300)); err == nil {
		t.Fatal("overlong mode accepted")
	}
	if err := checkAmpMode(context.Background(), "", "a\nb"); err == nil {
		t.Fatal("newline mode accepted")
	}
}

// TestAmpRunStateOf covers the log states: running mid-turn, done on a
// success result, failed on an error result or a launch-failure sentinel,
// and running when the log doesn't exist yet.
func TestAmpRunStateOf(t *testing.T) {
	init, _ := json.Marshal(ampStreamInit{Type: "system", Subtype: "init", SessionID: "T-s"})
	mk := func(lines ...string) string {
		p := filepath.Join(t.TempDir(), "run.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "did it"})
	errRes, _ := json.Marshal(map[string]any{"type": "result", "subtype": "error", "is_error": true, "result": "boom"})

	if st := AmpRunStateOf(filepath.Join(t.TempDir(), "missing.jsonl")); st.State != "running" {
		t.Fatalf("missing: %+v", st)
	}
	if st := AmpRunStateOf(mk(string(init))); st.State != "running" || st.ThreadID != "T-s" {
		t.Fatalf("mid-turn: %+v", st)
	}
	if st := AmpRunStateOf(mk(string(init), string(ok))); st.State != "done" || st.ThreadID != "T-s" {
		t.Fatalf("done: %+v", st)
	}
	st := AmpRunStateOf(mk(string(init), string(errRes)))
	if st.State != "failed" || !strings.Contains(st.Reason, "boom") {
		t.Fatalf("error result: %+v", st)
	}
	p := mk("Error: Unexpected error inside Amp CLI.")
	if _, err := os.Create(p + ".done"); err != nil {
		t.Fatal(err)
	}
	if st := AmpRunStateOf(p); st.State != "failed" || !strings.Contains(st.Reason, "Unexpected error") {
		t.Fatalf("launch failure: %+v", st)
	}
}
