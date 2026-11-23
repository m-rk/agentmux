package session

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeRunLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "run.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	stopThreadID = "00000000-0000-4000-8000-0000000000d1"
	ampStopID    = "T-11111111-1111-4111-8111-111111111111"
)

func TestCodexRunStateReadsStop(t *testing.T) {
	live := os.Getpid()
	start := `{"type":"agentmux.start","run":"r1","pid":` + itoa(live) + `}`
	thread := `{"type":"thread.started","thread_id":"` + stopThreadID + `"}`
	stop := `{"type":"agentmux.stop","run":"r1","by":"mark","at":"2026-11-23T12:00:00Z"}`
	cases := map[string]struct {
		lines []string
		want  string
	}{
		"stop pending exit":    {[]string{start, thread, `{"type":"turn.started"}`, stop}, "running"},
		"stopped":              {[]string{start, thread, `{"type":"turn.started"}`, stop, `{"type":"agentmux.exit","run":"r1","code":143}`}, "stopped"},
		"completed before":     {[]string{start, thread, `{"type":"turn.completed"}`, stop, `{"type":"agentmux.exit","run":"r1","code":0}`}, "done"},
		"older run's stop":     {[]string{start, thread, `{"type":"agentmux.stop","run":"r0"}`, `{"type":"agentmux.exit","run":"r1","code":1}`}, "failed"},
		"continued after stop": {[]string{start, thread, stop, `{"type":"agentmux.exit","run":"r1","code":143}`, strings.Replace(start, "r1", "r2", 1), thread}, "running"},
	}
	for name, tc := range cases {
		st := CodexRunStateOf(writeRunLog(t, tc.lines...))
		if st.State != tc.want {
			t.Errorf("%s: %+v, want %s", name, st, tc.want)
		}
		if tc.want == "stopped" && (st.StoppedBy != "mark" || st.StoppedAt != "2026-11-23T12:00:00Z") {
			t.Errorf("%s: %+v lacks who and when", name, st)
		}
	}
	// A stop whose launcher is gone reads stopped without an exit record.
	dead := `{"type":"agentmux.start","run":"r1","pid":2147483647}`
	if st := CodexRunStateOf(writeRunLog(t, dead, thread, stop)); st.State != "stopped" {
		t.Errorf("dead launcher: %+v", st)
	}
}

func TestAmpRunStateReadsStop(t *testing.T) {
	init := `{"type":"system","subtype":"init","session_id":"` + ampStopID + `"}`
	start := `{"type":"agentmux.start","at":"2026-11-23T11:00:00Z","pid":1}`
	stop := `{"type":"agentmux.stop","by":"orchestrator","at":"2026-11-23T12:00:00Z"}`
	exit := `{"type":"agentmux.exit","at":"2026-11-23T12:00:01Z","code":143}`
	success := `{"type":"result","subtype":"success","result":"ok"}`
	cases := map[string]struct {
		lines []string
		want  string
	}{
		"stop pending exit":    {[]string{start, init, stop}, "running"},
		"stopped":              {[]string{start, init, stop, exit}, "stopped"},
		"done before stop":     {[]string{start, init, success, stop, exit}, "done"},
		"continued after stop": {[]string{start, init, stop, exit, start, init}, "running"},
	}
	for name, tc := range cases {
		st := AmpRunStateOf(writeRunLog(t, tc.lines...))
		if st.State != tc.want {
			t.Errorf("%s: %+v, want %s", name, st, tc.want)
		}
		if tc.want == "stopped" && (st.StoppedBy != "orchestrator" || st.ThreadID != ampStopID) {
			t.Errorf("%s: %+v", name, st)
		}
	}
	// The launcher's .done sentinel for a non-zero exit does not turn a stop
	// into a failure.
	p := writeRunLog(t, start, init, stop, exit)
	if err := os.WriteFile(p+".done", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := AmpRunStateOf(p); st.State != "stopped" {
		t.Errorf("with .done: %+v", st)
	}
}

// With no launcher left, the stop writes the exit record itself so the run
// never reads as busy.
func TestStopCodexRunWithoutLauncherWritesExit(t *testing.T) {
	old := runStopGrace
	runStopGrace = 0
	t.Cleanup(func() { runStopGrace = old })
	// A start record without a pid: there is no launcher to wait for.
	p := writeRunLog(t, `{"type":"agentmux.start","run":"r1","pid":0}`, `{"type":"thread.started","thread_id":"`+stopThreadID+`"}`, `{"type":"turn.started"}`)
	st, stopped, err := StopCodexRun(context.Background(), "nobody-"+itoa(os.Getpid()), "/nonexistent", p, "mark")
	if err != nil || !stopped || st.State != "stopped" {
		t.Fatalf("stop = %+v, %v, %v", st, stopped, err)
	}
	if data, _ := os.ReadFile(p); !strings.Contains(string(data), `"type":"agentmux.exit"`) {
		t.Fatalf("no exit record:\n%s", data)
	}
	// Not running: left alone.
	if _, stopped, err := StopCodexRun(context.Background(), "x", "", p, "mark"); stopped || err != nil {
		t.Fatalf("second stop: %v %v", stopped, err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
