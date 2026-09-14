//go:build linux

package provision

import (
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
)

// chownRegistryForUser makes name's registry file owned by u so the
// instance's own tick service — which runs as u, not root, on Linux — can
// self-correct fields (AGENTMUX_LAST_CONFIG_HASH, AGENTMUX_RESUME, the
// claude-code auth/trust-dialog notify flags — see SetRegistryField's
// callers) without needing privilege elevation it doesn't have. Confirmed
// live as the root cause of a fleet-wide incident: every opencode/kilo/zero
// tick started failing with EACCES the moment a build shipped
// configureAgentIfChanged's self-correcting write, because writeRegistry
// leaves the file root-owned (the provisioner runs as root) while the tick
// that needs to update it runs as the run user.
//
// Unlike ensureWorkdirForUser's exec-as-user approach, a direct os.Chown
// here is safe: the path is always <name>.env inside discovery.EnvDir, a
// location entirely owned by agentmux's own provisioning, never a
// caller-chosen existing path.
func chownRegistryForUser(name string, u *user.User) error {
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("invalid UID %q for user %q: %w", u.Uid, u.Username, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("invalid GID %q for user %q: %w", u.Gid, u.Username, err)
	}
	path := filepath.Join(discovery.EnvDir, name+".env")
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("chowning registry %s to %s: %w", path, u.Username, err)
	}
	return nil
}

// SelfHealRegistryOwnership fixes registry files left root-owned by
// provisioning that predates chownRegistryForUser, so hosts that already
// existed before this fix shipped don't stay broken until each instance
// happens to be re-provisioned. Called once at `agentmux daemon run`
// startup, which is root on Linux; a no-op when not root, since a
// non-root daemon couldn't chown anything here anyway. Best-effort per
// file — a lookup or chown failure for one instance is logged and does not
// stop the others or fail daemon startup.
func SelfHealRegistryOwnership() {
	if os.Geteuid() != 0 {
		return
	}
	entries, err := os.ReadDir(discovery.EnvDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".env") {
			continue
		}
		path := filepath.Join(discovery.EnvDir, e.Name())
		name := strings.TrimSuffix(e.Name(), ".env")

		runUser := readRegistryRunUser(path)
		if runUser == "" {
			continue
		}
		u, err := user.Lookup(runUser)
		if err != nil {
			log.Printf("self-heal registry ownership: looking up run user %q for %s: %v", runUser, name, err)
			continue
		}

		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(path); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid {
				continue // already correctly owned
			}
		}

		if err := chownRegistryForUser(name, u); err != nil {
			log.Printf("self-heal registry ownership: %v", err)
		}
	}
}

// readRegistryRunUser reads just the AGENTMUX_RUN_USER field from a
// registry file, tolerating a read error by returning "" (same
// fail-quiet-and-skip contract as the rest of self-heal).
func readRegistryRunUser(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == "AGENTMUX_RUN_USER" {
			return strings.TrimSpace(val)
		}
	}
	return ""
}
