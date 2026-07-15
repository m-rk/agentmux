package provision

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/m-rk/agentmux/daemon/internal/runas"
)

const defaultClaudeCodeInstance = "claude-code"

const claudeCodeUnitTemplate = `[Unit]
Description=Persistent agentmux Claude Code session (%[1]s / %[2]s)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
User=%[3]s
ExecStart=%[4]s session run --instance %[1]s
ExecStop=%[4]s session stop --instance %[1]s
TimeoutStartSec=30

[Install]
WantedBy=multi-user.target
`

const claudeCodeUpdateUnitTemplate = `[Unit]
Description=Update Claude Code CLI and restart the %[1]s / %[2]s session
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=%[3]s session update --instance %[1]s
`

const claudeCodeTimerTemplate = `[Unit]
Description=Periodic Claude Code update for %[1]s / %[2]s

[Timer]
OnCalendar=%[3]s
Persistent=true
RandomizedDelaySec=120

[Install]
WantedBy=timers.target
`

const defaultOnCalendar = "*-*-* 03:00:00 UTC"

// createClaudeCode is the native Go port of
// backends/claude-code/install.sh's Linux path: validate, resolve
// defaults, check login, pre-accept workspace trust, write the registry
// file, and install+enable the systemd unit/timer.
func createClaudeCode(opts Options) (string, error) {
	name := opts.InstanceName
	if name == "" {
		name = defaultClaudeCodeInstance
	}
	if err := validateIdentifier("instance name", name); err != nil {
		return "", err
	}
	if opts.ResumeSessionID != "" {
		if err := validateIdentifier("resume session ID", opts.ResumeSessionID); err != nil {
			return "", err
		}
	}

	sessionName := name
	if name == defaultClaudeCodeInstance {
		sessionName = "agentmux"
	}
	if err := validateIdentifier("tmux session name", sessionName); err != nil {
		return "", err
	}

	runUser := opts.RunUser
	if runUser == "" {
		return "", fmt.Errorf("run_user is required")
	}
	u, err := user.Lookup(runUser)
	if err != nil {
		return "", fmt.Errorf("looking up user %q: %w", runUser, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	workdir := opts.Workdir
	if workdir == "" {
		workdir = filepath.Join(u.HomeDir, ".agentmux", name)
	}

	claudeJSON := filepath.Join(u.HomeDir, ".claude.json")
	if !fileExists(claudeJSON) {
		if alt := filepath.Join(u.HomeDir, ".claude", ".claude.json"); fileExists(alt) {
			claudeJSON = alt
		}
	}

	displayName := displayNameFor(runUser, workdir)

	if !claudeLoggedIn(runUser) {
		return "", fmt.Errorf("Claude Code does not appear to be logged in for user %q; run 'claude' once as %s to log in, then retry", runUser, runUser)
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return "", fmt.Errorf("creating workdir %s: %w", workdir, err)
	}
	if err := os.Chown(workdir, uid, gid); err != nil {
		return "", fmt.Errorf("chown workdir: %w", err)
	}

	if err := preacceptWorkspaceTrust(claudeJSON, workdir, uid, gid); err != nil {
		fmt.Printf("warning: could not pre-accept workspace trust: %v\n", err)
	}

	serviceName := "agentmux-" + name + ".service"
	updateServiceName := "agentmux-" + name + "-update.service"
	timerName := "agentmux-" + name + "-update.timer"

	regPath, err := writeRegistry(name, []kv{
		{"AGENTMUX_INSTANCE_NAME", name},
		{"AGENTMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_TMUX_SESSION_NAME", sessionName},
		{"AGENTMUX_DISPLAY_NAME", displayName},
		{"AGENTMUX_RUN_USER", runUser},
		{"AGENTMUX_SERVICE_NAME", serviceName},
		{"AGENTMUX_WORKDIR", workdir},
		{"AGENTMUX_RESUME", opts.ResumeSessionID},
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

	if err := installClaudeCodeUnits(name, sessionName, runUser, self, serviceName, updateServiceName, timerName); err != nil {
		return "", err
	}

	return fmt.Sprintf("Created instance %q (registry: %s). Reattach with: sudo -u %s tmux -L agentmux-%s attach -t %s",
		name, regPath, runUser, name, sessionName), nil
}

func claudeLoggedIn(runUser string) bool {
	out, err := runas.Command(runUser, "claude", "auth", "status", "--json").Output()
	if err != nil {
		return false
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return false
	}
	return status.LoggedIn
}

// preacceptWorkspaceTrust patches claudeJSON's
// projects[workdir].hasTrustDialogAccepted = true, natively via
// encoding/json instead of install.sh's inline Python, then restores
// ownership to uid/gid since this process writes it as root.
func preacceptWorkspaceTrust(claudeJSON, workdir string, uid, gid int) error {
	if !fileExists(claudeJSON) {
		return nil
	}
	data, err := os.ReadFile(claudeJSON)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	projects, _ := doc["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		doc["projects"] = projects
	}
	proj, _ := projects[workdir].(map[string]any)
	if proj == nil {
		proj = map[string]any{}
		projects[workdir] = proj
	}
	if accepted, _ := proj["hasTrustDialogAccepted"].(bool); accepted {
		return nil
	}
	proj["hasTrustDialogAccepted"] = true

	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	tmp := claudeJSON + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Chown(tmp, uid, gid); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, claudeJSON)
}

func installClaudeCodeUnits(name, sessionName, runUser, binPath, serviceName, updateServiceName, timerName string) error {
	unit := fmt.Sprintf(claudeCodeUnitTemplate, name, sessionName, runUser, binPath)
	updateUnit := fmt.Sprintf(claudeCodeUpdateUnitTemplate, name, sessionName, binPath)
	timer := fmt.Sprintf(claudeCodeTimerTemplate, name, sessionName, defaultOnCalendar)

	if err := os.WriteFile("/etc/systemd/system/"+serviceName, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+updateServiceName, []byte(updateUnit), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/"+timerName, []byte(timer), 0o644); err != nil {
		return err
	}

	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", serviceName); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", timerName); err != nil {
		return err
	}
	return nil
}

func runSystemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
	}
	return nil
}
