package daemonserver

import (
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
