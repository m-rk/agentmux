package provision

import (
	"fmt"
	"testing"

	"github.com/m-rk/agentmux/daemon/internal/launchagent"
)

// Instance and instance-update jobs start the binary through the launcher,
// so an agentmux update leaves their launchd registrations alone (AMUX-143).
func TestInstancePlistsUseLauncher(t *testing.T) {
	for name, plist := range map[string]string{
		"agentmux":        fmt.Sprintf(agentmuxPlistTemplate, "com.agentmux.a", "/tmp/agentmux", "a", 60, "/tmp/log"),
		"agentmux update": fmt.Sprintf(agentmuxUpdatePlistTemplate, "com.agentmux.a.update", "/tmp/agentmux", "a", 3, 0, "/tmp/log"),
		"claude":          fmt.Sprintf(claudeCodePlistTemplate, "com.agentmux.c", "/tmp/agentmux", "c", 60, "/tmp/log"),
		"claude update":   fmt.Sprintf(claudeCodeUpdatePlistTemplate, "com.agentmux.c.update", "/tmp/agentmux", "c", 3, 0, "/tmp/log"),
		"amp":             fmt.Sprintf(ampPlistTemplate, "com.agentmux.p", "/tmp/agentmux", "p", 60, "/tmp/log"),
	} {
		argv := launchagent.ProgramArguments([]byte(plist))
		if !launchagent.Launched(argv) || argv[3] != "/tmp/agentmux" {
			t.Errorf("%s ProgramArguments = %q", name, argv)
		}
	}
}
