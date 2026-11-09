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
	want := "3 launchd jobs failed to start and are not running: com.agentmux.beta (exit 78), com.agentmux.beta.update (exit 78), com.m-rk.agentmux.self-update (exit -9)"
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := StuckJobWarnings(ParseLaunchctlList("PID\tStatus\tLabel\n10\t0\tcom.agentmux.a\n")); got != nil {
		t.Errorf("healthy host warned: %v", got)
	}
}
