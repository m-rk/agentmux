package session

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// withNpmGlobalLock serializes callers that share the same npm global
// prefix (keyed by home), so concurrent "npm install -g opencode-ai@latest"
// runs -- one per opencode instance, all triggered by the same nightly
// StartCalendarInterval/cron schedule -- don't race on the same
// ~/.npm-global/lib/node_modules/opencode-ai directory.
//
// Confirmed live on a macOS host: every local instance runs as the same
// user (LaunchAgents don't drop privilege the way the Linux hosts'
// per-instance runUser does), so all of that host's opencode instances
// share one npm-global prefix. Six of them refreshing at once stomped on
// each other's in-progress postinstall (its own node_modules and temp
// files getting rewritten out from under a sibling's still-running
// install), surfacing as ENOENT-from-spawn and exit-1 failures across
// every instance sharing that HOME. The OS releases the flock
// automatically if the holder dies, so a crash mid-update can't wedge
// every other instance's refresh behind a stale lock.
//
// owner chowns a freshly-created lock dir/file to that user when this
// process runs privileged (the Linux update path runs as root, dropping
// to the instance's run user only for the exec'd agent commands
// themselves -- see runas.Command's doc comment): otherwise MkdirAll
// would leave a root-owned .agentmux inside the run user's home the one
// time that directory doesn't already exist (normally it does, created
// user-owned by ensureWorkdirForUser during provisioning). Pass nil on
// macOS, where this process already runs as the target user.
func withNpmGlobalLock(home string, owner *user.User, fn func() error) error {
	lockDir := filepath.Join(home, ".agentmux")
	dirExisted := true
	if _, err := os.Stat(lockDir); os.IsNotExist(err) {
		dirExisted = false
	}
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return fmt.Errorf("preparing npm update lock dir: %w", err)
	}
	if owner != nil && !dirExisted {
		if err := chownTo(lockDir, owner); err != nil {
			return fmt.Errorf("setting owner of npm update lock dir: %w", err)
		}
	}

	lockPath := filepath.Join(lockDir, "npm-update.lock")
	fileExisted := true
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		fileExisted = false
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening npm update lock: %w", err)
	}
	defer f.Close()
	if owner != nil && !fileExisted {
		if err := chownTo(lockPath, owner); err != nil {
			return fmt.Errorf("setting owner of npm update lock file: %w", err)
		}
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("acquiring npm update lock: %w", err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

func chownTo(path string, owner *user.User) error {
	uid, err := strconv.Atoi(owner.Uid)
	if err != nil {
		return fmt.Errorf("invalid UID %q for user %q: %w", owner.Uid, owner.Username, err)
	}
	gid, err := strconv.Atoi(owner.Gid)
	if err != nil {
		return fmt.Errorf("invalid GID %q for user %q: %w", owner.Gid, owner.Username, err)
	}
	return os.Chown(path, uid, gid)
}
