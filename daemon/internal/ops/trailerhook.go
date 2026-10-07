package ops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// trailerHookName is the commit-msg hook installTrailerHook writes into the
// template repo's hooks dir. The name is agentmux-specific so it never
// collides with a hook the repo already had: when core.hooksPath points at
// the hooks dir, git runs <hooks>/commit-msg only, so the install writes
// that entry point too — preserving any pre-existing commit-msg as
// commit-msg.local and chaining to it first. Deleting trailerHookName plus
// the worktree's core.hooksPath restores stock behaviour.
const trailerHookName = "agentmux-commit-msg"

// trailerHookScript is the commit-msg hook: it strips the agent/AI trailer
// lines MERG-7's attribution rule forbids outside go1com repos
// (Amp-Thread-ID, Co-Authored-By, Claude-Session, Generated-With and
// "Generated with ..." footers) and logs one line to stderr when it
// removed anything. Pure POSIX sh plus sed/awk, so it runs wherever git
// does. It never fails the commit: a hook exit of 0 keeps the commit
// flowing even when the message file is missing or unwritable.
const trailerHookScript = `#!/bin/sh
# agentmux-commit-msg: strip agent/AI trailer lines from the commit message.
# Managed by agentmux (AMUX-64): task worktrees for repos outside go1com get
# this hook at sessions-create time so commits there carry no Amp-Thread-ID,
# Co-Authored-By, Claude-Session or Generated-with trailers.
# Safe to delete: without it, agent trailers land in the commit again and
# only the ship-time scan catches them.
set -u
MSG="${1:-}"
[ -n "$MSG" ] && [ -f "$MSG" ] || exit 0
# Chain the repo's own hook first: with core.hooksPath set, git runs only
# this dir's commit-msg, so the preserved copy lives beside it.
HERE="$(dirname "$0")"
if [ -x "$HERE/commit-msg.local" ]; then
  "$HERE/commit-msg.local" "$@" || exit $?
fi
BEFORE=$(wc -c < "$MSG")
TMP="${MSG}.agentmux-strip.$$"
trap 'rm -f "$TMP" "${TMP}.t"' EXIT INT TERM
if ! sed -E \
  -e '/^[[:space:]]*[Aa][Mm][Pp]-[Tt][Hh][Rr][Ee][Aa][Dd]-[Ii][Dd][[:space:]]*:/d' \
  -e '/^[[:space:]]*[Cc][Oo]-[Aa][Uu][Tt][Hh][Oo][Rr][Ee][Dd]-[Bb][Yy][[:space:]]*:/d' \
  -e '/^[[:space:]]*[Cc][Ll][Aa][Uu][Dd][Ee]-[Ss][Ee][Ss][Ss][Ii][Oo][Nn][[:space:]]*:/d' \
  -e '/^[[:space:]]*[Gg][Ee][Nn][Ee][Rr][Aa][Tt][Ee][Dd]-[Ww][Ii][Tt][Hh][[:space:]]*:/d' \
  -e '/^[[:space:]]*[Gg][Ee][Nn][Ee][Rr][Aa][Tt][Ee][Dd][[:space:]][Ww][Ii][Tt][Hh][[:space:]]/d' \
  "$MSG" > "$TMP"; then
  exit 0
fi
if [ -n "$(tail -c 1 "$MSG" 2>/dev/null)" ]; then
  : # no trailing newline: keep the file byte-identical below
else
  printf '\n' >> "$TMP"
fi
awk '/^$/{n++; next} {for(i=0;i<n;i++)print ""; n=0; print} END{}' n=0 "$TMP" > "${TMP}.t" && mv "${TMP}.t" "$TMP"
if cmp -s "$MSG" "$TMP"; then
  exit 0
fi
if ! cat "$TMP" > "$MSG"; then
  exit 0
fi
REMOVED=$((BEFORE - $(wc -c < "$MSG")))
[ "$REMOVED" -lt 0 ] && REMOVED=0
echo "agentmux commit-msg hook: stripped agent trailers (${REMOVED} bytes)" >&2
exit 0
`

// installTrailerHook installs the AMUX-64 commit-msg hook for the task
// worktree at wtPath in repo. Repos whose origin lives under the go1com
// org keep stock behaviour (nil): trailers are allowed there. Everywhere
// else the hook goes in the worktree's own git dir (<common>/worktrees/
// <name>/hooks) and the worktree gets a worktree-scoped core.hooksPath
// at it, so only that worktree's commits are filtered — the main
// checkout and every other worktree keep whatever hooks they had, and
// nothing is ever written to the shared hooks dir.
//
// The install is idempotent: re-running it rewrites the same hook file
// and re-sets the same worktree-scoped key. A pre-existing commit-msg
// hook in the hooks dir is preserved as commit-msg.local and chained
// first, so a hand-installed hook keeps working. A dry run installs
// nothing; callers skip the call. runUser is the template's run user
// ("": none) for the root-ownership chown (see AMUX-23).
func (e Env) installTrailerHook(ctx context.Context, repo, wtPath, runUser string) error {
	if org, err := e.projectOrgOfOrigin(ctx, repo); err == nil && org == go1comOrg {
		return nil
	} else if err != nil {
		// No origin, or an origin that cannot be parsed (a local path,
		// say): forbid trailers rather than allow them.
		_ = err
	}
	commonDir, err := e.git(ctx, wtPath, "rev-parse", "--absolute-git-dir")
	if err != nil || commonDir == "" {
		// Fall back to the git-common-dir: for a linked worktree the
		// absolute git dir is the per-worktree dir, which carries the
		// worktree-scoped config; the common dir is the template repo's
		// .git, whose hooks dir the worktree would otherwise read.
		// Either way the hook lands in a dir this worktree's
		// core.hooksPath will point at.
		if commonDir, err = e.git(ctx, wtPath, "rev-parse", "--git-common-dir"); err != nil || commonDir == "" {
			return Refuse(safesend.ReasonFailed, "locating the hooks dir for %s: %v", wtPath, err)
		}
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(wtPath, commonDir)
		}
	}
	hooksDir := filepath.Join(commonDir, "hooks")
	hookPath := filepath.Join(hooksDir, "commit-msg")
	// A pre-existing entry point chains through commit-msg.local in the
	// same dir the worktree's hooksPath points at. When the repo already
	// had its own commit-msg in the common hooks dir, preserve it here
	// too, so the worktree keeps running it.
	commonHookPath := ""
	if abs, err := e.git(ctx, wtPath, "rev-parse", "--git-common-dir"); err == nil && abs != "" {
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(wtPath, abs)
		}
		if abs != commonDir {
			commonHookPath = filepath.Join(abs, "hooks", "commit-msg")
		}
	}
	localPath := filepath.Join(hooksDir, "commit-msg.local")

	// Preserve a pre-existing commit-msg the worktree would otherwise run:
	// one already in this hooks dir, or (when it differs) the common
	// hooks dir's entry point. Move it aside once to commit-msg.local, so
	// the agentmux entry point chains to it first. A rerun finds our entry
	// point in place and leaves .local alone.
	needsPreserve := false
	if data, err := os.ReadFile(hookPath); err == nil {
		if !strings.Contains(string(data), "agentmux-commit-msg") {
			needsPreserve = true
		}
	} else if !os.IsNotExist(err) {
		return Refuse(safesend.ReasonFailed, "reading %s: %v", hookPath, err)
	}
	if !needsPreserve && commonHookPath != "" {
		if _, err := os.Stat(localPath); os.IsNotExist(err) {
			if _, err := os.Stat(commonHookPath); err == nil {
				if data, err := os.ReadFile(commonHookPath); err == nil && !strings.Contains(string(data), "agentmux-commit-msg") {
					if err := os.MkdirAll(hooksDir, 0o755); err != nil {
						return Refuse(safesend.ReasonFailed, "creating the hooks dir %s: %v", hooksDir, err)
					}
					if err := os.WriteFile(localPath, data, 0o755); err != nil {
						return Refuse(safesend.ReasonFailed, "preserving the existing commit-msg hook: %v", err)
					}
				}
			}
		}
	}
	if needsPreserve {
		if err := os.Rename(hookPath, localPath); err != nil {
			return Refuse(safesend.ReasonFailed, "preserving the existing commit-msg hook: %v", err)
		}
	}

	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return Refuse(safesend.ReasonFailed, "creating the hooks dir %s: %v", hooksDir, err)
	}
	if err := os.WriteFile(hookPath, []byte(trailerHookScript), 0o755); err != nil {
		return Refuse(safesend.ReasonFailed, "writing the commit-msg hook %s: %v", hookPath, err)
	}
	// The hook file is written by direct file I/O, not through the
	// asUser git runner, so a root create chowns it to the run user
	// (AMUX-23); a non-root create already owns it.
	chownHookToRunUser(runUser, hookPath, localPath)

	// Scope core.hooksPath to this worktree only: `git config --worktree`
	// writes <commonDir>/worktrees/<name>/config.worktree, leaving the
	// main checkout and every other worktree untouched. That needs
	// extensions.worktreeConfig in the main config first (once per repo).
	if _, err := e.git(ctx, repo, "config", "extensions.worktreeConfig", "true"); err != nil {
		return Refuse(safesend.ReasonFailed, "enabling worktreeConfig: %v", err)
	}
	if _, err := e.git(ctx, wtPath, "config", "--worktree", "core.hooksPath", hooksDir); err != nil {
		return Refuse(safesend.ReasonFailed, "scoping core.hooksPath to %s: %v", wtPath, err)
	}
	return nil
}

// chownHookToRunUser chowns freshly written hook files to runUser when this
// process runs as root for one: Create's git already runs as the run user
// (see asUser), but the hook file itself is written by direct file I/O,
// so a root create would leave it root-owned and the agent could not
// execute it. Best-effort: a failed lookup or chown never fails the
// create. Callers without a run user pass "".
func chownHookToRunUser(runUser string, paths ...string) {
	if os.Geteuid() != 0 || runUser == "" {
		return
	}
	uid, gid, err := atoiUIDGID(runUser)
	if err != nil {
		return
	}
	for _, p := range paths {
		_ = os.Chown(p, uid, gid)
	}
}

// atoiUIDGID parses a looked-up user's numeric ids for chown.
func atoiUIDGID(username string) (uid, gid int, err error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, 0, err
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, err
	}
	if gid, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// runGitAsRunUser runs git args in dir as the run user, for callers that
// need one git invocation outside Env's own git runner.
func runGitAsRunUser(ctx context.Context, runUser, dir string, args ...string) (string, error) {
	var cmd *exec.Cmd
	if runUser == "" {
		cmd = runas.CurrentUserCommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	} else {
		cmd = runas.CommandContext(ctx, runUser, "git", append([]string{"-C", dir}, args...)...)
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}
