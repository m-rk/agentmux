package session

import (
	"os/exec"
	"strings"
)

// unitDisabled reports whether the instance's main unit has been disabled,
// which `control stop` does: such an instance stays down, so the nightly
// update must not compact it or start it again (AMUX-129).
var unitDisabled = func(serviceName string) bool {
	out, _ := exec.Command("systemctl", "is-enabled", serviceName).Output()
	return strings.TrimSpace(string(out)) == "disabled"
}
