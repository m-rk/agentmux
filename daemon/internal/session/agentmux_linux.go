package session

import (
	"fmt"
	"os/exec"
)

// updateAgentmux runs as root (it needs to call systemctl), dropping to the
// instance's run user via runas only for the agent CLI calls themselves.
func updateAgentmux(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	runUser := fields["AGENTMUX_RUN_USER"]
	serviceName := fields["AGENTMUX_SERVICE_NAME"]
	agent := fields["AGENTMUX_AGENT"]
	if runUser == "" || serviceName == "" {
		return fmt.Errorf("registry for %s is missing AGENTMUX_RUN_USER/AGENTMUX_SERVICE_NAME", name)
	}
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)

	before, _ := agentVersion(runUser, agent)
	if err := updateAgent(runUser, agent); err != nil {
		return fmt.Errorf("%s update/check failed, leaving existing session running untouched: %w", agent, err)
	}
	after, _ := agentVersion(runUser, agent)

	if before == after && hasSessionAs(runUser, socket, session) {
		return nil // no version change, session already running
	}
	if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
		return fmt.Errorf("restarting %s: %w", serviceName, err)
	}
	return nil
}

func agentVersion(runUser, agent string) (string, error) {
	out, err := runAs(runUser, agent, "--version").CombinedOutput()
	return string(out), err
}

func updateAgent(runUser, agent string) error {
	switch agent {
	case "zero":
		return runAs(runUser, "zero", "update", "--check").Run()
	case "opencode":
		return runAs(runUser, "opencode", "upgrade", "--method", "npm").Run()
	case "kilo":
		return runAs(runUser, "kilo", "upgrade").Run()
	default:
		return fmt.Errorf("unsupported agent: %s", agent)
	}
}
