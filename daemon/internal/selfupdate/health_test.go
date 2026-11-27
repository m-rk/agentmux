package selfupdate

import (
	"strings"
	"testing"
	"time"
)

func TestLagWarnings(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	shipped := map[string]string{"agentmux": "bbbbbbbbbbbb", "mergentic": "cccccccc"}
	installed := map[string]string{"agentmux": "aaaaaaaaaaaa", "mergentic": "cccccccc"}

	if got := LagWarnings(now, shipped, installed, now.Add(-29*time.Minute)); got != nil {
		t.Errorf("29 minutes behind warned: %v", got)
	}
	got := LagWarnings(now, shipped, installed, now.Add(-31*time.Minute))
	if len(got) != 1 || !strings.Contains(got[0], "agentmux installed aaaaaaa lags shipped bbbbbbb for 31 minutes") {
		t.Errorf("31 minutes behind = %v, want one agentmux warning", got)
	}
	if got := LagWarnings(now, shipped, shipped, now.Add(-5*time.Hour)); got != nil {
		t.Errorf("up to date warned: %v", got)
	}
	if got := LagWarnings(now, map[string]string{}, installed, now.Add(-5*time.Hour)); got != nil {
		t.Errorf("never shipped warned: %v", got)
	}
	if got := LagWarnings(now, shipped, installed, time.Time{}); got != nil {
		t.Errorf("unknown ship time warned: %v", got)
	}
	if got := LagWarnings(now, shipped, map[string]string{}, now.Add(-2*time.Hour)); len(got) != 2 || !strings.Contains(got[0], "(none)") {
		t.Errorf("nothing installed = %v, want both repos flagged", got)
	}
}

func TestStuckJobWarnings(t *testing.T) {
	out := "PID\tStatus\tLabel\n" +
		"-\t0\tcom.apple.thing\n" +
		"412\t0\tcom.agentmux.alpha\n" +
		"-\t78\tcom.agentmux.beta\n" +
		"-\t78\tcom.agentmux.beta.update\n" +
		"-\t0\tcom.agentmux.idle\n" +
		"-\t1\tcom.example.other\n" +
		"-\t-9\tcom.m-rk.agentmux.self-update\n" +
		"junk line\n"
	jobs := ParseLaunchctlList(out)
	if len(jobs) != 7 {
		t.Fatalf("parsed %d jobs, want 7: %+v", len(jobs), jobs)
	}
	got := StuckJobWarnings(jobs)
	want := "3 launchd jobs last exited non-zero and are not running: com.agentmux.beta (exit 78), com.agentmux.beta.update (exit 78), com.m-rk.agentmux.self-update (exit -9)"
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := StuckJobWarnings(ParseLaunchctlList("PID\tStatus\tLabel\n10\t0\tcom.agentmux.a\n")); got != nil {
		t.Errorf("healthy host warned: %v", got)
	}
}

// A failed scheduled job says why, from the last line of its stderr log
// (AMUX-145: the gc job's warning gave only "exit 1").
func TestStuckJobWarningsQuoteLastError(t *testing.T) {
	jobs := []Job{{Label: "com.m-rk.agentmux.gc", Status: 1,
		LastErr: LastLine([]byte("collected on local\ngc on hub: forbidden: gc is not permitted\n\n"))}}
	want := "1 launchd jobs last exited non-zero and are not running: com.m-rk.agentmux.gc (exit 1: gc on hub: forbidden: gc is not permitted)"
	if got := StuckJobWarnings(jobs); len(got) != 1 || got[0] != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLastLine(t *testing.T) {
	if got := LastLine(nil); got != "" {
		t.Errorf("empty log = %q", got)
	}
	long := strings.Repeat("x", 500)
	got := LastLine([]byte("first\n" + long + "\n"))
	if len([]rune(got)) != maxLastErr+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("long line not cut: %d runes", len([]rune(got)))
	}
}
