package session

import "fmt"

// updateAgentmux runs as the instance's own user already (macOS
// LaunchAgents are per-user; no privilege drop needed) and restarts by
// calling StopAgentmux/RunAgentmux directly — see claudecode_darwin.go's
// updateClaudeCode for why re-kickstarting the LaunchAgent instead
// wouldn't work (RunAgentmux is intentionally idempotent, a no-op against
// the still-running stale session).
func updateAgentmux(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	agent := fields["AGENTMUX_AGENT"]
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)

	before, _ := agentVersion(agent)
	if err := updateAgent(agent); err != nil {
		return fmt.Errorf("%s update/check failed, leaving existing session running untouched: %w", agent, err)
	}
	after, _ := agentVersion(agent)

	if before == after && hasSession(socket, session) {
		return nil // no version change, session already running
	}
	if err := StopAgentmux(name); err != nil {
		return fmt.Errorf("stopping %s before restart: %w", name, err)
	}
	return RunAgentmux(name)
}

func agentVersion(agent string) (string, error) {
	out, err := withPath(agent, "--version").CombinedOutput()
	return string(out), err
}

func updateAgent(agent string) error {
	switch agent {
	case "zero":
		return withPath("zero", "update", "--check").Run()
	case "opencode":
		return withPath("opencode", "upgrade", "--method", "npm").Run()
	case "kilo":
		return withPath("kilo", "upgrade").Run()
	default:
		return fmt.Errorf("unsupported agent: %s", agent)
	}
}
