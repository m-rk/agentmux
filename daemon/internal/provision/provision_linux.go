package provision

import (
	"fmt"
	"os"
	"os/user"
	"strings"
)

// resumeHomeDir resolves runUser's home directory — required on Linux,
// since the daemon runs as root and any instance's session could belong to
// a different user than whoever's asking.
func resumeHomeDir(runUser string) (string, error) {
	if runUser == "" {
		return "", fmt.Errorf("run_user is required")
	}
	u, err := user.Lookup(runUser)
	if err != nil {
		return "", fmt.Errorf("looking up user %q: %w", runUser, err)
	}
	return u.HomeDir, nil
}

// realUserCount mirrors install.sh's real_user_count(): counts /etc/passwd
// entries with UID >= 1000, excluding the UID 65534 "nobody" account. A
// var, not a plain func, so tests can stub it out instead of depending on
// the real machine's actual account count.
var realUserCount = func() int {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return 1
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		var uid int
		if _, err := fmt.Sscanf(fields[2], "%d", &uid); err != nil {
			continue
		}
		if uid >= 1000 && uid != 65534 {
			count++
		}
	}
	return count
}

// unitFileExists reports whether name's systemd unit is already on disk —
// used by guardAgentMismatch to catch a collision with an instance that
// predates the registry-based provisioner (e.g. one installed by
// backends/claude-code/install.sh or backends/agentmux/install.sh, which
// has no *.env registry file for guardAgentMismatch's own agent check to
// find, so that check alone would silently approve overwriting it). A var,
// not a plain func, so tests can stub it out instead of touching the real
// /etc/systemd/system.
var unitFileExists = func(name string) bool {
	_, err := os.Stat("/etc/systemd/system/agentmux-" + name + ".service")
	return err == nil
}
