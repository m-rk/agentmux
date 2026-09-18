//go:build linux

package provision

import (
	"fmt"
	"strings"
	"testing"
)

// The instance main units are Type=oneshot services whose ExecStart leaves a
// long-lived tmux server behind in the unit's cgroup. At stop/restart time
// systemd's default KillMode=control-group would SIGTERM every process in
// that cgroup — including the tmux server itself and any unrelated process
// adopted into it. Confirmed live 2026-09-17: restarting
// agentmux-minecraft.service SIGTERMed the PaperMC game server's tmux
// session (adopted into that unit's cgroup via an Amp-driven shell), whose
// JVM died with a jline pty Input/output error. Every template below must
// therefore carry KillMode=process, so only systemd's SIGTERM to the (by
// then already-exited) main process applies and the cgroup's other members
// are left alone — ExecStop's own pinned `tmux -L <socket> kill-session`
// remains the only thing that stops the instance's session.
func TestInstanceUnitTemplatesUseKillModeProcess(t *testing.T) {
	templates := map[string]string{
		"claudeCodeUnitTemplate": fmt.Sprintf(claudeCodeUnitTemplate, "probe", "probe-session", "probeuser", "/usr/local/bin/agentmux"),
		"agentmuxUnitTemplate":   fmt.Sprintf(agentmuxUnitTemplate, "probe", "zero", "ollama", "probeuser", "/usr/local/bin/agentmux"),
		"ampUnitTemplate":        fmt.Sprintf(ampUnitTemplate, "probe", "probe", "probeuser", "/usr/local/bin/agentmux"),
	}
	for name, unit := range templates {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(unit, "\nKillMode=process\n") {
				t.Errorf("%s must set KillMode=process so stop/restart cannot SIGTERM the cgroup's tmux server or adopted processes; got:\n%s", name, unit)
			}
			if strings.Contains(unit, "KillMode=control-group") {
				t.Errorf("%s must not use KillMode=control-group; got:\n%s", name, unit)
			}
		})
	}
}
