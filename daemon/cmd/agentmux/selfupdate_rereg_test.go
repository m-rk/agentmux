//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
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

// Every agentmux LaunchAgent the cmd package renders starts the binary
// through the /bin/sh launcher, so a binary update leaves launchd's (and
// Background Task Management's) view of the job unchanged (AMUX-143).
func TestRenderedPlistsUseLauncher(t *testing.T) {
	paseo, err := renderPaseoUpdatePlist("/x/agentmux", "03:15", "/x/log")
	if err != nil {
		t.Fatal(err)
	}
	for name, plist := range map[string]string{
		"self-update": renderSelfUpdatePlist("/x/agentmux", selfUpdateHostConfig{}, "/x/log"),
		"gateway":     renderGatewayPlist("/x/agentmux", []string{"gateway", "run"}, "/x/log"),
		"paseo":       paseo,
	} {
		argv := launchagent.ProgramArguments([]byte(plist))
		if !launchagent.Launched(argv) || argv[3] != "/x/agentmux" {
			t.Errorf("%s ProgramArguments = %q, want the launcher then the binary", name, argv)
		}
	}
}

func TestPrepareSelfUpdatePlist(t *testing.T) {
	legacy := renderSelfUpdatePlistDirect("/x/agentmux")
	launched := renderSelfUpdatePlist("/x/agentmux", selfUpdateHostConfig{}, "/x/log")
	loadedAs := func(prog string, err error) launchagent.Launchctl {
		return func(args ...string) (string, error) {
			return "\tprogram = " + prog + "\n", err
		}
	}
	cases := []struct {
		name, onDisk string
		launchctl    launchagent.Launchctl
		wantPinned   bool
	}{
		{"old job: rewrite and re-register once", legacy, loadedAs("/x/agentmux", nil), true},
		{"file rewritten, job still loaded old way", launched, loadedAs("/x/agentmux", nil), true},
		{"on the launcher: nothing to do", launched, loadedAs("/bin/sh", nil), false},
		{"print failed: re-register to be safe", launched, loadedAs("", errors.New("exit 113")), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plist := filepath.Join(t.TempDir(), selfUpdateLabel+".plist")
			if err := os.WriteFile(plist, []byte(c.onDisk), 0o644); err != nil {
				t.Fatal(err)
			}
			pinned, err := prepareSelfUpdatePlist(plist, c.launchctl, "501")
			if err != nil || pinned != c.wantPinned {
				t.Fatalf("prepare = %v, %v; want %v", pinned, err, c.wantPinned)
			}
			got, _ := os.ReadFile(plist)
			if !launchagent.Launched(launchagent.ProgramArguments(got)) {
				t.Error("plist is not on the launcher")
			}
		})
	}
}

// renderSelfUpdatePlistDirect is the self-update plist as installed before
// AMUX-143: the binary first, no launcher.
func renderSelfUpdatePlistDirect(bin string) string {
	return strings.Replace(renderSelfUpdatePlist(bin, selfUpdateHostConfig{}, "/x/log"), launchagent.LauncherXML, "", 1)
}
