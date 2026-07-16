package provision

import (
	"fmt"
	"os"
	"strings"
)

// realUserCount mirrors install.sh's real_user_count(): counts /etc/passwd
// entries with UID >= 1000, excluding the UID 65534 "nobody" account.
func realUserCount() int {
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
// find, so that check alone would silently approve overwriting it).
func unitFileExists(name string) bool {
	_, err := os.Stat("/etc/systemd/system/agentmux-" + name + ".service")
	return err == nil
}
