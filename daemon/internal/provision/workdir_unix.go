//go:build darwin || linux

package provision

import (
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// ensureWorkdirForUser creates workdir with the permissions of u rather than
// as the daemon. On Linux the daemon runs as root, so creating a caller-chosen
// path and chowning it afterwards would let the caller take ownership of an
// existing system directory. Letting the kernel enforce u's normal filesystem
// permissions avoids both that ownership change and privileged path creation.
func ensureWorkdirForUser(workdir string, u *user.User) error {
	cmd, err := workdirMkdirCommand(workdir, u)
	if err != nil {
		return err
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating workdir %s as %s: %w: %s", workdir, u.Username, err, out)
	}
	return nil
}

func workdirMkdirCommand(workdir string, u *user.User) (*exec.Cmd, error) {
	if !filepath.IsAbs(workdir) {
		return nil, fmt.Errorf("workdir must be an absolute path: %q", workdir)
	}

	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid UID %q for user %q: %w", u.Uid, u.Username, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid GID %q for user %q: %w", u.Gid, u.Username, err)
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

	cmd := exec.Command("/bin/mkdir", "-p", "-m", "0755", "--", workdir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid:    uint32(uid),
		Gid:    uint32(gid),
		Groups: groups,
	}}
	return cmd, nil
}
