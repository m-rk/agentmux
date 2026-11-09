package daemoninstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// After a binary swap every loaded agentmux job is re-registered, jobs that
// were stopped on purpose stay unloaded, and the jobs Install already handles
// and the one running us are left alone (AMUX-125).
func TestReloadLoadedAgentJobs(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{
		"com.agentmux.alpha.plist", "com.agentmux.alpha.update.plist",
		"com.agentmux.stopped.plist",
		"com.m-rk.agentmux.self-update.plist",
		"com.m-rk.agentmux.gateway.plist",
		"com.example.other.plist", "com.agentmux.notaplist.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded := map[string]bool{
		"com.agentmux.alpha": true, "com.agentmux.alpha.update": true,
		"com.m-rk.agentmux.self-update": true, "com.m-rk.agentmux.gateway": true,
		"com.example.other": true,
	}
	var calls []string
	old := launchctl
	launchctl = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "print" {
			if loaded[strings.TrimPrefix(args[1], "gui/501/")] {
				return "", nil
			}
			return "not found", os.ErrNotExist
		}
		return "", nil
	}
	defer func() { launchctl = old }()

	t.Setenv("XPC_SERVICE_NAME", "com.m-rk.agentmux.self-update")
	reloaded, err := reloadLoadedAgentJobs("gui/501", dir, map[string]bool{"com.m-rk.agentmux.gateway": true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(reloaded, ","), "com.agentmux.alpha,com.agentmux.alpha.update"; got != want {
		t.Errorf("reloaded = %s, want %s", got, want)
	}
	for _, c := range calls {
		for _, banned := range []string{"stopped", "gateway", "other", "self-update"} {
			if strings.HasPrefix(c, "bootstrap") && strings.Contains(c, banned) {
				t.Errorf("must not re-register %s: %s", banned, c)
			}
		}
	}
	plist := filepath.Join(dir, "com.agentmux.alpha.plist")
	if !contains(calls, "bootout gui/501 "+plist) || !contains(calls, "bootstrap gui/501 "+plist) {
		t.Errorf("alpha was not booted out and back in: %q", calls)
	}
}

func TestReloadLoadedAgentJobsReportsFailuresAndContinues(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"com.agentmux.a.plist", "com.agentmux.b.plist"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := launchctl
	launchctl = func(args ...string) (string, error) {
		if args[0] == "bootstrap" && strings.HasSuffix(args[2], "com.agentmux.a.plist") {
			return "Bootstrap failed: 5", os.ErrInvalid
		}
		return "", nil
	}
	defer func() { launchctl = old }()
	t.Setenv("XPC_SERVICE_NAME", "")
	reloaded, err := reloadLoadedAgentJobs("gui/501", dir, nil)
	if err == nil || !strings.Contains(err.Error(), "com.agentmux.a") {
		t.Fatalf("err = %v, want one naming com.agentmux.a", err)
	}
	if len(reloaded) != 1 || reloaded[0] != "com.agentmux.b" {
		t.Errorf("reloaded = %v, want [com.agentmux.b]", reloaded)
	}
}

func TestReloadLoadedAgentJobsNoLaunchAgentsDir(t *testing.T) {
	if r, err := reloadLoadedAgentJobs("gui/501", filepath.Join(t.TempDir(), "none"), nil); err != nil || len(r) != 0 {
		t.Errorf("missing dir = %v, %v; want nothing, nil", r, err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
