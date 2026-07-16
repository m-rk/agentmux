package session

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// RunAgentmux is `agentmux session run --instance NAME` for the zero/
// opencode agents: writes the provider config file, waits for the
// provider to be reachable, then idempotently ensures the instance's tmux
// session is running the agent CLI, matching
// backends/agentmux/rc-start.sh. Runs as the instance's target user
// already (the unit's User= directive).
func RunAgentmux(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	agent := fields["AGENTMUX_AGENT"]
	provider := fields["AGENTMUX_PROVIDER"]
	model := fields["AGENTMUX_MODEL"]
	baseURL := fields["AGENTMUX_PROVIDER_BASE_URL"]
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)
	workdir := fields["AGENTMUX_WORKDIR"]
	waitSeconds := 60
	if s := fields["AGENTMUX_PROVIDER_WAIT_SECONDS"]; s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			waitSeconds = n
		}
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("creating workdir %s: %w", workdir, err)
	}

	if err := waitForProvider(provider, waitSeconds); err != nil {
		return err
	}
	if err := configureAgent(agent, provider, model, baseURL, workdir); err != nil {
		return err
	}

	if hasSession(socket, session) {
		return nil
	}

	launchCmd, err := launchCommand(agent)
	if err != nil {
		return err
	}
	cmd := withPath("tmux", "-L", socket, "new-session", "-d", "-s", session, "-c", workdir, launchCmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("starting tmux session %s: %w: %s", session, err, out)
	}
	return nil
}

// StopAgentmux is the instance unit's ExecStop.
func StopAgentmux(name string) error {
	fields, err := registry(name)
	if err != nil {
		return err
	}
	session := sessionNameOf(fields, name)
	socket := tmuxSocket(name)
	_ = withPath("tmux", "-L", socket, "kill-session", "-t", session).Run()
	return nil
}

// UpdateAgentmux is `agentmux session update --instance NAME` for zero/
// opencode: checks for a new version and restarts the session only if it
// changed or the session isn't running, matching
// backends/agentmux/rc-update.sh. Runs as root (it needs to call
// systemctl), dropping to the instance's run user only for the agent CLI
// calls themselves.
func UpdateAgentmux(name string) error {
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
	default:
		return fmt.Errorf("unsupported agent: %s", agent)
	}
}

func waitForProvider(provider string, waitSeconds int) error {
	if provider != "ollama" {
		return nil
	}
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for {
		if withPath("ollama", "list").Run() == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ollama is not reachable after %ds; start ollama and re-run", waitSeconds)
		}
		time.Sleep(2 * time.Second)
	}
}

func configureAgent(agent, provider, model, baseURL, workdir string) error {
	switch agent {
	case "zero":
		return writeZeroConfig(provider, model, baseURL, workdir)
	case "opencode":
		return writeOpencodeConfig(provider, model, baseURL, workdir)
	default:
		return fmt.Errorf("unsupported agent: %s", agent)
	}
}

func launchCommand(agent string) (string, error) {
	switch agent {
	case "zero", "opencode":
		return agent, nil
	default:
		return "", fmt.Errorf("unsupported agent: %s", agent)
	}
}

func writeZeroConfig(provider, model, baseURL, workdir string) error {
	configDir := filepath.Join(workdir, ".zero")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	doc := map[string]any{
		"activeProvider": provider,
		"providers": []map[string]any{
			{
				"name":          provider,
				"provider_kind": "openai-compatible",
				"catalogID":     provider,
				"baseURL":       baseURL,
				"apiFormat":     "chat-completions",
				"model":         model,
			},
		},
	}
	if err := writeJSONAtomic(filepath.Join(configDir, "config.json"), doc); err != nil {
		return err
	}
	cmd := withPath("zero", "providers", "check", provider, "--connectivity")
	cmd.Dir = workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zero providers check: %w: %s", err, out)
	}
	return nil
}

func writeOpencodeConfig(provider, model, baseURL, workdir string) error {
	doc := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   provider + "/" + model,
		"provider": map[string]any{
			provider: map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": provider,
				"options": map[string]any{
					"baseURL": baseURL,
				},
				"models": map[string]any{
					model: map[string]any{
						"name": model,
					},
				},
			},
		},
	}
	return writeJSONAtomic(filepath.Join(workdir, "opencode.json"), doc)
}

func writeJSONAtomic(path string, doc any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
