package session

import (
	"fmt"
	"os/exec"
	"os/user"
)

// updateAgentmux runs as root (it needs to call systemctl), dropping to the
// instance's run user via runas only for the agent CLI calls themselves.
func updateAgentmux(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	runUser := fields["AGENTMUX_RUN_USER"]
	serviceName := "agentmux-" + name + ".service"
	agent := fields["AGENTMUX_AGENT"]
	if runUser == "" {
		return fmt.Errorf("registry for %s is missing AGENTMUX_RUN_USER", name)
	}
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)
	agentEnv, err := agentUpdateEnv(name, runUser, agent)
	if err != nil {
		return err
	}

	before, _ := agentVersion(runUser, agent, agentEnv)
	if err := updateAgent(runUser, agent, agentEnv); err != nil {
		return fmt.Errorf("%s update/check failed, leaving existing session running untouched: %w", agent, err)
	}
	after, err := agentVersion(runUser, agent, agentEnv)
	if err != nil {
		return fmt.Errorf("%s reported success but is not runnable afterward, leaving existing session running untouched: %w", agent, err)
	}

	if before == after && hasSessionAs(runUser, socket, session) {
		return nil // no version change, session already running
	}
	if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
		return fmt.Errorf("restarting %s: %w", serviceName, err)
	}
	return nil
}

func agentUpdateEnv(name, runUser, agent string) ([]string, error) {
	if agent != "kilo" {
		return nil, nil
	}
	u, err := user.Lookup(runUser)
	if err != nil {
		return nil, fmt.Errorf("looking up run user %q for kilo isolation: %w", runUser, err)
	}
	env, err := kiloInstanceXDGEnvForHome(name, u.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("preparing isolated kilo data for %s: %w", name, err)
	}
	return env, nil
}

func agentVersion(runUser, agent string, env []string) (string, error) {
	cmd := runAs(runUser, agent, "--version")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func updateAgent(runUser, agent string, env []string) error {
	var cmd *exec.Cmd
	switch agent {
	case "zero":
		cmd = runAs(runUser, "zero", "update", "--check")
	case "opencode":
		// Not `opencode upgrade --method npm`: that shells out through the
		// currently-installed opencode binary itself, so a broken install
		// (confirmed live: npm's postinstall silently failed to link the
		// platform binary, once while disk was nearly full) can never
		// upgrade its way back to working — every future refresh just
		// repeats the same exec failure forever. Installing the npm
		// package directly needs nothing from the existing binary.
		//
		// Retried once on failure: postinstall fetches the platform-specific
		// binary package as its own separate npm registry call, and a single
		// transient network hiccup there is enough to fail the whole install
		// and leave the *shared* global binary as a broken stub — breaking
		// not just this instance's already-running session (which keeps
		// working untouched) but any brand-new opencode invocation anywhere
		// on the host, including ones this daemon doesn't manage (confirmed
		// live: Paseo failed to start a new session against exactly this
		// stub, moments after a nightly refresh failed here; re-running the
		// same install with no other change succeeded immediately).
		return runWithRetry(runUser, "npm", []string{"install", "-g", "opencode-ai@latest"}, env)
	case "kilo":
		cmd = runAs(runUser, "kilo", "upgrade")
	default:
		return fmt.Errorf("unsupported agent: %s", agent)
	}
	cmd.Env = append(cmd.Env, env...)
	return cmd.Run()
}

func runWithRetry(runUser, name string, args, env []string) error {
	cmd := runAs(runUser, name, args...)
	cmd.Env = append(cmd.Env, env...)
	if err := cmd.Run(); err == nil {
		return nil
	}
	cmd = runAs(runUser, name, args...)
	cmd.Env = append(cmd.Env, env...)
	return cmd.Run()
}
