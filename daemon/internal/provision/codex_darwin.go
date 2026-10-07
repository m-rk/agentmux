package provision

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/allowfiles"
	"github.com/m-rk/agentmux/daemon/internal/runas"
)

// createCodex provisions a codex instance on macOS: registry entry plus the
// same RunAtLoad+StartInterval LaunchAgent amp uses (no update agent: codex
// has nothing to update or restart, see session.UpdateCodex).
func createCodex(opts Options) (string, error) {
	name := opts.InstanceName
	if name == "" {
		name = defaultInstanceName(opts.Agent, opts.Workdir)
	}
	if err := validateIdentifier("instance name", name); err != nil {
		return "", err
	}
	if err := rejectUnsupportedCodexOptions(opts); err != nil {
		return "", err
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolving current user: %w", err)
	}
	workdir := opts.Workdir
	if workdir == "" {
		workdir = filepath.Join(u.HomeDir, ".agentmux", name)
	}
	hostName, err := resolveHostName(opts.HostName)
	if err != nil {
		return "", err
	}
	if err := checkAgentInstalledCurrentUser("codex"); err != nil {
		return "", err
	}
	if problem := codexLoginProblemVia(runas.CurrentUserCommand("codex", "login", "status")); problem != "" {
		return "", fmt.Errorf("%s; run 'codex login', then retry", problem)
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return "", fmt.Errorf("creating workdir %s: %w", workdir, err)
	}
	label := "com.agentmux." + name
	_, alreadyExisted := existingAgentFor(name)
	allowFiles, err := prepareAllowFiles(opts, workdir)
	if err != nil {
		return "", err
	}
	regPath, err := writeRegistry(name, []kv{
		{"AGENTMUX_INSTANCE_NAME", name},
		{"AGENTMUX_AGENT", "codex"},
		{"AGENTMUX_MODEL", opts.Model},
		{"AGENTMUX_SESSION_NAME", name},
		{"AGENTMUX_TMUX_SESSION_NAME", name},
		{"AGENTMUX_HOST_NAME", hostName},
		{"AGENTMUX_WORKDIR", workdir},
		{allowfiles.RegistryKey, allowFiles},
		{"AGENTMUX_RUN_USER", u.Username},
		{"AGENTMUX_SERVICE_NAME", label},
	})
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	launchAgentsDir := filepath.Join(u.HomeDir, "Library", "LaunchAgents")
	logDir := filepath.Join(u.HomeDir, "Library", "Logs", "agentmux")
	for _, dir := range []string{launchAgentsDir, logDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	plistPath := filepath.Join(launchAgentsDir, label+".plist")
	plist := fmt.Sprintf(ampPlistTemplate, label, self, name, defaultAgentmuxStartInterval, logDir)
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return "", err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, plistPath).Run()
	if err := exec.Command("launchctl", "bootstrap", domain, plistPath).Run(); err != nil {
		return "", fmt.Errorf("bootstrapping %s: %w", label, err)
	}
	if err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+label).Run(); err != nil {
		return "", fmt.Errorf("starting %s: %w", label, err)
	}
	verb := "Created"
	if alreadyExisted {
		verb = "Updated"
	}
	return fmt.Sprintf("%s instance %q (registry: %s). Runs are headless: use `agentmux sessions run`", verb, name, regPath), nil
}
