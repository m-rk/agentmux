package daemonserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

func TestLinuxStartRestartsActiveButDeadOneshot(t *testing.T) {
	dead := discovery.Instance{Name: "one", Status: discovery.StatusDead}
	if got := linuxSystemdAction(dead, "start"); got != "restart" {
		t.Fatalf("dead start action = %q, want restart", got)
	}
	live := discovery.Instance{Name: "one", Status: discovery.StatusIdle}
	if got := linuxSystemdAction(live, "start"); got != "start" {
		t.Fatalf("live start action = %q, want start", got)
	}
}

// A stopped instance must stay stopped: stop disables the unit and the two
// timers that would start it again, and start turns them back on first
// (AMUX-129).
func TestLinuxStopDisablesAndStartEnables(t *testing.T) {
	var calls []string
	old := systemctlRun
	systemctlRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	defer func() { systemctlRun = old }()
	inst := discovery.Instance{Name: "one", Status: discovery.StatusIdle}

	if ok, msg := applyControl(context.Background(), inst, "stop"); !ok {
		t.Fatalf("stop: %s", msg)
	}
	want := []string{
		"stop agentmux-one.service",
		"disable --now agentmux-one-update.timer",
		"disable --now agentmux-one-tick.timer",
		"disable agentmux-one.service",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("stop calls = %q, want %q", calls, want)
	}

	calls = nil
	if ok, msg := applyControl(context.Background(), inst, "start"); !ok {
		t.Fatalf("start: %s", msg)
	}
	want = []string{
		"enable agentmux-one.service",
		"enable --now agentmux-one-update.timer",
		"enable --now agentmux-one-tick.timer",
		"start agentmux-one.service",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("start calls = %q, want %q", calls, want)
	}
}

func TestLinuxStopReportsDisableFailure(t *testing.T) {
	old := systemctlRun
	systemctlRun = func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "disable" && args[1] == "agentmux-one.service" {
			return []byte("boom"), errors.New("exit 1")
		}
		return nil, nil
	}
	defer func() { systemctlRun = old }()
	ok, msg := applyControl(context.Background(), discovery.Instance{Name: "one"}, "stop")
	if ok || !strings.Contains(msg, "may come back") {
		t.Fatalf("stop with failed disable = %v, %q; want a failure that says it may come back", ok, msg)
	}
}
