package provision

import (
	"os/exec"
	"strconv"
	"strings"
)

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
