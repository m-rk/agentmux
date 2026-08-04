// Package runas execs commands as a different user by dropping root's
// privileges via a Credential, rather than shelling out to su/sudo — used
// wherever a root-context process (the update timer, CreateInstance) needs
// to run the actual claude/tmux commands as an instance's target user.
package runas

import (
	"context"
	"fmt"
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
	return command(nil, runUser, name, args...)
}

// CommandContext is Command with cancellation support. It is useful for
// bounded one-shot jobs such as the session doctor, where a provider CLI
// must not be allowed to keep a systemd/launchd job alive indefinitely.
func CommandContext(ctx context.Context, runUser, name string, args ...string) *exec.Cmd {
	return command(ctx, runUser, name, args...)
}

func command(ctx context.Context, runUser, name string, args ...string) *exec.Cmd {
	u, err := user.Lookup(runUser)
	if err != nil {
		return failedCommand(ctx, name, args, fmt.Errorf("looking up run user %q: %w", runUser, err))
	}
	path := pathFor(u)

	resolved := name
	if !filepath.IsAbs(name) {
		resolved = lookPathIn(name, path)
		if resolved == "" {
			return failedCommand(ctx, name, args, fmt.Errorf("%q not found in %s's PATH", name, runUser))
		}
	}

	credential, err := credentialFor(u)
	if err != nil {
		return failedCommand(ctx, resolved, args, err)
	}
	cmd := execCommand(ctx, resolved, args...)
	if credential != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	}
	cmd.Env = append(os.Environ(), "HOME="+u.HomeDir, "PATH="+path)
	return cmd
}

// credentialFor returns nil when the process already runs as u. Reapplying a
// Credential in that case makes Go call setgroups during fork/exec, which a
// normal unprivileged process cannot do and surfaces as a misleading EPERM.
// A different target user is permitted only from root and receives the full
// supplementary-group set; every lookup/parse failure is returned rather than
// accidentally leaving the child privileged.
func credentialFor(u *user.User) (*syscall.Credential, error) {
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid UID %q for user %q: %w", u.Uid, u.Username, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid GID %q for user %q: %w", u.Gid, u.Username, err)
	}
	if uint64(os.Geteuid()) == uid {
		return nil, nil
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("cannot run as user %q (uid %d) from euid %d", u.Username, uid, os.Geteuid())
	}

	groupIDs, err := u.GroupIds()
	if err != nil {
		return nil, fmt.Errorf("resolving groups for user %q: %w", u.Username, err)
	}
	groups := make([]uint32, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		parsed, err := strconv.ParseUint(groupID, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid supplementary GID %q for user %q: %w", groupID, u.Username, err)
		}
		groups = append(groups, uint32(parsed))
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}, nil
}

func failedCommand(ctx context.Context, name string, args []string, err error) *exec.Cmd {
	cmd := execCommand(ctx, name, args...)
	cmd.Err = err
	return cmd
}

// CurrentUserCommand returns an *exec.Cmd for name/args, running as the
// calling process's own user with PATH (and HOME) fixed up to include
// common per-user tool install locations (notably Homebrew's /opt/homebrew/
// bin, where tmux lives on Apple Silicon). Every exec.Command("tmux", ...)
// call site needs this, not just the ones that already had it: systemd/
// launchd give a daemon a minimal ambient PATH, and exec.Command's lookup
// uses that ambient $PATH (os.Getenv, not cmd.Env) rather than whatever
// PATH ends up in cmd.Env — so a bare exec.Command("tmux", ...) silently
// fails to find a Homebrew-installed tmux even though the tmux server socket
// it's trying to reach is right there. Callers that need extra env vars
// (e.g. TERM for an interactive attach) should append to cmd.Env after
// calling this, not replace it.
func CurrentUserCommand(name string, args ...string) *exec.Cmd {
	return currentUserCommand(nil, name, args...)
}

// CurrentUserCommandContext is CurrentUserCommand with cancellation support.
func CurrentUserCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return currentUserCommand(ctx, name, args...)
}

func currentUserCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	home, path := currentUserPath()

	resolved := name
	if !filepath.IsAbs(name) {
		if found := lookPathIn(name, path); found != "" {
			resolved = found
		}
	}

	cmd := execCommand(ctx, resolved, args...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+path)
	return cmd
}

func execCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if ctx == nil {
		return exec.Command(name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// CurrentUserLookPath searches the calling process's own fixed-up PATH (the
// same construction CurrentUserCommand uses) for an executable named name,
// without running anything — for preflight "is this even installed" checks
// where actually executing the binary isn't a fair thing to demand (it may
// need network/provider connectivity just to start up).
func CurrentUserLookPath(name string) (string, error) {
	_, path := currentUserPath()
	if found := lookPathIn(name, path); found != "" {
		return found, nil
	}
	return "", fmt.Errorf("%q not found in PATH", name)
}

func currentUserPath() (home, path string) {
	home = os.Getenv("HOME")
	if home == "" {
		if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	}
	path = home + "/.local/bin:" + home + "/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:" + os.Getenv("PATH")
	return home, path
}

// LookPath searches runUser's PATH (the same construction Command uses)
// for an executable named name, without running anything — for preflight
// "is this even installed" checks where actually executing the binary
// (e.g. via --version) isn't a fair thing to demand (it may need network/
// provider connectivity just to start up).
func LookPath(runUser, name string) (string, error) {
	u, err := user.Lookup(runUser)
	if err != nil {
		return "", fmt.Errorf("looking up user %q: %w", runUser, err)
	}
	if found := lookPathIn(name, pathFor(u)); found != "" {
		return found, nil
	}
	return "", fmt.Errorf("%q not found in %s's PATH", name, runUser)
}

func pathFor(u *user.User) string {
	return u.HomeDir + "/.local/bin:" + u.HomeDir + "/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
}

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
