// Package daemoninstall installs, removes, and checks the status of the
// agentmuxd background daemon on this host: a systemd unit on Linux, a
// per-user LaunchAgent on macOS. Install/Uninstall/Status are implemented
// per-OS in install_linux.go / install_darwin.go; this file holds the
// platform-agnostic helpers both share.
package daemoninstall

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// installSelf copies the currently running executable to dst (atomically,
// via a temp file + rename), unless it's already running from there. Used
// so `agentmux daemon install` pins a stable binary path for the installed
// unit/plist to exec, instead of pointing at wherever the invoking binary
// happened to be built/downloaded.
func installSelf(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if self == dst {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func captureCmd(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out))
}
