// Package runas execs commands as a different user by dropping root's
// privileges via a Credential, rather than shelling out to su/sudo — used
// wherever a root-context process (the update timer, CreateInstance) needs
// to run the actual claude/tmux commands as an instance's target user.
package runas

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Command returns an *exec.Cmd for running name as runUser, with HOME/PATH
// set appropriately (systemd/launchd don't inherit a login shell's PATH,
// and dropping privileges via Credential doesn't change HOME/PATH itself).
//
// name is resolved against the target user's PATH explicitly, not just
// passed through: exec.Command's own lookup uses the *calling* process's
// $PATH (os.Getenv, not cmd.Env), so a plain exec.Command("claude", ...)
// run from a root daemon with a minimal PATH would silently fail to find a
// user-installed binary like claude even with cmd.Env set correctly.
func Command(runUser, name string, args ...string) *exec.Cmd {
	u, err := user.Lookup(runUser)
	if err != nil {
		return exec.Command(name, args...)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	path := u.HomeDir + "/.local/bin:" + u.HomeDir + "/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

	resolved := name
	if !filepath.IsAbs(name) {
		if found := lookPathIn(name, path); found != "" {
			resolved = found
		}
	}

	cmd := exec.Command(resolved, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	cmd.Env = append(os.Environ(), "HOME="+u.HomeDir, "PATH="+path)
	return cmd
}

// lookPathIn searches an arbitrary PATH string for an executable named
// name, since exec.LookPath only ever searches the calling process's own
// $PATH. Returns "" if not found.
func lookPathIn(name, pathEnv string) string {
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}
