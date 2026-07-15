package daemoninstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const label = "com.agentmux.daemon"

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>daemon</string>
        <string>run</string>
        <string>-socket</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/agentmuxd.log</string>
    <key>StandardErrorPath</key>
    <string>%s/agentmuxd.err.log</string>
</dict>
</plist>
`

// agentmuxDir returns ~/.agentmux, the root for everything daemoninstall
// manages on macOS: bin/ (the pinned binary), run/ (the daemon's Unix
// socket), log/ (launchd's stdout/stderr redirection).
func agentmuxDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentmux"), nil
}

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// Install renders and loads a per-user LaunchAgent, pointing it at a stable
// copy of the current binary under ~/.agentmux/bin. Unlike Linux, this must
// NOT run as root: instances on macOS are user-level LaunchAgents (see
// backends/*/install-macos.sh, which explicitly refuse sudo), so agentmuxd
// itself stays in the same unprivileged, per-user world — no root needed
// anywhere in the macOS path.
func Install() error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("must not be run as root/sudo on macOS; run as your normal user")
	}

	dir, err := agentmuxDir()
	if err != nil {
		return err
	}
	for _, sub := range []string{"bin", "run", "log"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}

	bin := filepath.Join(dir, "bin", "agentmux")
	if err := installSelf(bin); err != nil {
		return fmt.Errorf("installing binary to %s: %w", bin, err)
	}

	sock := filepath.Join(dir, "run", "agentmuxd.sock")
	plist, err := plistPath()
	if err != nil {
		return err
	}
	logDir := filepath.Join(dir, "log")
	content := fmt.Sprintf(plistTemplate, label, bin, sock, logDir, logDir)
	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", plist, err)
	}

	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = runCmd("launchctl", "bootout", domain, plist) // ignore error: may not be loaded yet
	if err := runCmd("launchctl", "bootstrap", domain, plist); err != nil {
		return err
	}
	if err := runCmd("launchctl", "kickstart", "-k", domain+"/"+label); err != nil {
		return err
	}

	fmt.Printf("Installed and started %s (binary: %s, socket: %s)\n", label, bin, sock)
	return nil
}

func Uninstall() error {
	plist, err := plistPath()
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = runCmd("launchctl", "bootout", domain, plist)
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", plist, err)
	}
	fmt.Printf("Removed %s\n", label)
	return nil
}

func Status() (string, error) {
	plist, err := plistPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return fmt.Sprintf("not installed (no %s)", plist), nil
	}
	dir, _ := agentmuxDir()
	sock := filepath.Join(dir, "run", "agentmuxd.sock")
	domain := "gui/" + strconv.Itoa(os.Getuid())
	out := captureCmd("launchctl", "print", domain+"/"+label)
	return fmt.Sprintf("plist: %s\nsocket: %s\n\n%s", plist, sock, out), nil
}
