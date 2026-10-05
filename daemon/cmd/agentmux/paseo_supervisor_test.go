package main

import (
	"strings"
	"testing"
)

func TestIsPaseoDaemonCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"/Users/x/.npm-global/bin/paseo", "daemon", "run", "--home", "/Users/x/.paseo"}, true},
		{[]string{"/usr/bin/env", "paseo", "daemon", "start", "--foreground"}, true},
		{[]string{"/Users/x/.agentmux/bin/agentmux", "paseo", "update"}, false},
		{[]string{"/Users/x/.agentmux/bin/agentmux", "session", "run", "--instance", "paseo-test"}, false},
		{nil, false},
	} {
		if got := isPaseoDaemonCommand(tc.args); got != tc.want {
			t.Errorf("isPaseoDaemonCommand(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestSystemdExecStart(t *testing.T) {
	unit := "# /etc/systemd/system/paseo-daemon.service\n[Service]\nExecStart=\nExecStart=-/usr/bin/paseo daemon start --foreground\n"
	got := strings.Join(systemdExecStart(unit), " ")
	if got != "/usr/bin/paseo daemon start --foreground" {
		t.Fatalf("got %q", got)
	}
}

func TestLaunchdFailure(t *testing.T) {
	crashLoop := "com.paseo.daemon = {\n\tstate = spawn scheduled\n\truns = 11557\n\tlast exit code = 1\n\tendpoints = {\n\t\tstate = active\n\t}\n}"
	if got := launchdFailure(crashLoop); got != "not running, last exit code 1" {
		t.Fatalf("crash loop: got %q", got)
	}
	for _, healthy := range []string{
		"\tstate = running\n\tlast exit code = 1\n",
		"\tstate = waiting\n\tlast exit code = 0\n",
		"\tstate = waiting\n\tlast exit code = (never exited)\n",
	} {
		if got := launchdFailure(healthy); got != "" {
			t.Errorf("launchdFailure(%q) = %q, want none", healthy, got)
		}
	}
}

func TestSupervisorProblem(t *testing.T) {
	var none *paseoSupervisor
	if none.problem() != "" {
		t.Fatal("nil supervisor reported a problem")
	}
	old := &paseoSupervisor{Kind: "LaunchAgent", Name: "com.paseo.daemon", Args: []string{"paseo", "daemon", "start", "--foreground"}}
	if !strings.Contains(old.problem(), "paseo daemon run") {
		t.Fatalf("got %q", old.problem())
	}
	ok := &paseoSupervisor{Kind: "LaunchAgent", Name: "com.paseo.daemon", Args: []string{"paseo", "daemon", "run"}}
	if ok.problem() != "" {
		t.Fatalf("got %q", ok.problem())
	}
	failing := &paseoSupervisor{Kind: "systemd unit", Name: paseoUnit, Args: []string{"paseo", "daemon", "run"}, Failing: "activating/auto-restart"}
	if !strings.Contains(failing.problem(), "is failing") {
		t.Fatalf("got %q", failing.problem())
	}
}
