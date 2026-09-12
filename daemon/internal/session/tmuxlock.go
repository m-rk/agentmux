package session

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// withTmuxInputLock serializes every caller that types into name's tmux
// pane via send-keys: the nightly compact-on-update restart
// (compactAndResolveResume), the periodic Remote Control reconnect
// (ensureClaudeRemoteControl), and Discord collaboration delivery
// (syncCollaboration). Each only checks "does the pane look idle" as its
// own proxy for "safe to type" — not airtight, since there's a real gap
// between sending a keystroke sequence and Claude Code actually consuming
// and rendering a response to it. A second caller landing in that gap
// sees the same unchanged pane, also concludes it's safe, and its literal
// text lands concatenated onto the first caller's still-unsubmitted input
// line. Confirmed live: Discord collab digests arriving with a stray
// "/remote-control" or "/compact" fragment stitched onto a boundary with
// no separator, and multiple ticks' digests concatenated into one delivery
// because none of them ever got a lone Enter to itself.
//
// waitBlocking true waits indefinitely for the lock (used by the nightly
// update, which isn't time-boxed the way a periodic tick is); false tries
// non-blocking and returns ok=false immediately if another caller already
// holds it, matching the existing "pane's busy, skip this tick, the next
// one retries" handling already used elsewhere for a busy pane.
//
// owner chowns a freshly-created lock dir/file to that user when this
// process runs privileged (the Linux nightly update path runs as root,
// dropping to the instance's run user only for the exec'd tmux/claude
// commands — see runas.Command's doc comment): pass nil when this process
// already runs as the target user (macOS, and the periodic tick on
// Linux, which runs as the unit's own User=).
func withTmuxInputLock(home string, owner *user.User, name string, waitBlocking bool, fn func() error) (ok bool, err error) {
	lockDir := filepath.Join(home, ".agentmux", name)
	dirExisted := true
	if _, statErr := os.Stat(lockDir); os.IsNotExist(statErr) {
		dirExisted = false
	}
	if mkErr := os.MkdirAll(lockDir, 0o755); mkErr != nil {
		return false, fmt.Errorf("preparing tmux input lock dir for %s: %w", name, mkErr)
	}
	if owner != nil && !dirExisted {
		if chErr := chownTo(lockDir, owner); chErr != nil {
			return false, fmt.Errorf("setting owner of tmux input lock dir for %s: %w", name, chErr)
		}
	}

	lockPath := filepath.Join(lockDir, "tmux-input.lock")
	fileExisted := true
	if _, statErr := os.Stat(lockPath); os.IsNotExist(statErr) {
		fileExisted = false
	}
	f, openErr := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if openErr != nil {
		return false, fmt.Errorf("opening tmux input lock for %s: %w", name, openErr)
	}
	defer f.Close()
	if owner != nil && !fileExisted {
		if chErr := chownTo(lockPath, owner); chErr != nil {
			return false, fmt.Errorf("setting owner of tmux input lock file for %s: %w", name, chErr)
		}
	}

	mode := unix.LOCK_EX
	if !waitBlocking {
		mode |= unix.LOCK_NB
	}
	if flockErr := unix.Flock(int(f.Fd()), mode); flockErr != nil {
		if !waitBlocking && flockErr == unix.EWOULDBLOCK {
			return false, nil
		}
		return false, fmt.Errorf("acquiring tmux input lock for %s: %w", name, flockErr)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return true, fn()
}

// tmuxLockHomeAndOwner resolves the (home, owner) pair withTmuxInputLock
// needs for name's run user: runUser empty means this process already
// runs as the target user (macOS, and the Linux periodic tick, which runs
// as the unit's own User=), so os.UserHomeDir() with a nil owner is
// correct; otherwise (the Linux nightly update, which runs as root) it
// looks the run user up so the lock file lands in — and is owned by —
// their home, not root's.
func tmuxLockHomeAndOwner(runUser string) (string, *user.User, error) {
	if runUser == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, fmt.Errorf("resolving home for tmux input lock: %w", err)
		}
		return home, nil, nil
	}
	u, err := user.Lookup(runUser)
	if err != nil {
		return "", nil, fmt.Errorf("looking up run user %q for tmux input lock: %w", runUser, err)
	}
	return u.HomeDir, u, nil
}
