package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// resumeHomeDir always resolves to the current user on macOS — an
// instance always runs as whoever invoked `agentmux new`, so runUser
// (meaningful only on Linux, where the daemon runs as root) is ignored.
func resumeHomeDir(_ string) (string, error) {
	return os.UserHomeDir()
}

// unitFileExists reports whether name's LaunchAgent plist is already on
// disk — used by guardAgentMismatch to catch a collision with an instance
// that predates the registry-based provisioner (e.g. one installed by
// backends/claude-code/install-macos.sh, which has no *.env registry file
// for guardAgentMismatch's own agent check to find, so that check alone
// would silently approve overwriting it).
func unitFileExists(name string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.agentmux."+name+".plist"))
	return err == nil
}

// realUserCount mirrors install-macos.sh's real_user_count(): counts real
// (human) macOS user accounts via dscl, excluding system/service accounts
// (UID < 500 by convention) that "getent passwd"'s /etc/passwd-based Linux
// check would never see on macOS anyway (Directory Services, not a flat
// file, is the actual source of truth here).
func realUserCount() int {
	out, err := exec.Command("dscl", ".", "-list", "/Users", "UniqueID").Output()
	if err != nil {
		return 1
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if uid, err := strconv.Atoi(fields[1]); err == nil && uid >= 500 {
			count++
		}
	}
	return count
}
