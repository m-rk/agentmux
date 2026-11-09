//go:build !windows

package main

import (
	"strings"
	"testing"
)

func TestSelfUpdateReregisterScript(t *testing.T) {
	got := selfUpdateReregisterScript("501", "/Users/a b/Library/LaunchAgents/"+selfUpdateLabel+".plist")
	for _, want := range []string{
		"sleep 5;",
		"launchctl bootout gui/501 '/Users/a b/Library/LaunchAgents/" + selfUpdateLabel + ".plist';",
		"launchctl bootstrap gui/501 '/Users/a b/Library/LaunchAgents/" + selfUpdateLabel + ".plist'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("script %q is missing %q", got, want)
		}
	}
	if q := shellQuote("it's"); q != `'it'\''s'` {
		t.Errorf("shellQuote = %s", q)
	}
}

func TestShouldReregisterSelfUpdate(t *testing.T) {
	cases := []struct {
		goos, svc string
		want      bool
	}{
		{"darwin", selfUpdateLabel, true},
		{"darwin", "", false},                   // run by hand
		{"darwin", "com.agentmux.other", false}, // some other job
		{"linux", selfUpdateLabel, false},
	}
	for _, c := range cases {
		if got := shouldReregisterSelfUpdate(c.goos, c.svc); got != c.want {
			t.Errorf("%s %q = %v, want %v", c.goos, c.svc, got, c.want)
		}
	}
}
