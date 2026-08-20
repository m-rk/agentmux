package session

import (
	"fmt"
	"os/exec"
)

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
	agentEnv, err := agentUpdateEnv(name, agent)
	if err != nil {
		return err
	}

	before, _ := agentVersion(agent, agentEnv)
	if err := updateAgent(agent, agentEnv); err != nil {
		return fmt.Errorf("%s update/check failed, leaving existing session running untouched: %w", agent, err)
	}
	after, err := agentVersion(agent, agentEnv)
	if err != nil {
		return fmt.Errorf("%s reported success but is not runnable afterward, leaving existing session running untouched: %w", agent, err)
	}

	if before == after && hasSession(socket, session) {
		return nil // no version change, session already running
	}
	if err := StopAgentmux(name); err != nil {
		return fmt.Errorf("stopping %s before restart: %w", name, err)
	}
	return RunAgentmux(name)
}

func agentUpdateEnv(name, agent string) ([]string, error) {
	if agent != "kilo" {
		return nil, nil
	}
	env, err := kiloInstanceXDGEnv(name)
	if err != nil {
		return nil, fmt.Errorf("preparing isolated kilo data for %s: %w", name, err)
	}
	return env, nil
}

func agentVersion(agent string, env []string) (string, error) {
	cmd := withPath(agent, "--version")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func updateAgent(agent string, env []string) error {
	var cmd *exec.Cmd
	switch agent {
	case "zero":
		cmd = withPath("zero", "update", "--check")
	case "opencode":
		// Not `opencode upgrade --method npm`: that shells out through the
		// currently-installed opencode binary itself, so a broken install
		// can never upgrade its way back to working. Installing the npm
		// package directly needs nothing from the existing binary. See the
		// matching comment in agentmux_linux.go for the incident that
		// prompted this.
		cmd = withPath("npm", "install", "-g", "opencode-ai@latest")
	case "kilo":
		cmd = withPath("kilo", "upgrade")
	default:
		return fmt.Errorf("unsupported agent: %s", agent)
	}
	cmd.Env = append(cmd.Env, env...)
	return cmd.Run()
}
