// Package selfupdate is the pull-based updater for a Mac host (see
// AMUX-29): every 10 minutes a launchd job fetches origin/main for
// agentmux and mergentic, and installs up to the orchestrator's shipped
// commit when it moved. It never installs a commit the hub has not
// recorded as shipped, and only commits reachable from origin/main.
//
// Layout on the host (all under ~/.agentmux/self-update, created by
// `self-update install`): repos/agentmux and repos/mergentic are the
// fetch mirrors, shipped.json records the orchestrator's shipped commit
// per repo, versions.json records the installed commit per repo, and
// update.log carries the one-line-per-event log the hub reads with the
// log tail op.
//
// The shipped gate is a local file on purpose: the hub publishes the
// shipped commit through the gateway (the publish op writes it), and the
// Mac updater reads it with no network of its own beyond git fetch. The
// log tail op exposes the same updater's own log, so the orchestrator
// can confirm "laptop is on <sha>" or warn when it lags.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Repos are the checkouts the updater pulls. Keys are the repo names used
// in shipped.json, versions.json and the log lines (deployed <repo>@<sha>).
var Repos = []string{"agentmux", "mergentic"}

// BaseDir joins home with the updater's state root. Exported so the CLI,
// the launchd installer and the gateway log tail agree on one path.
func BaseDir(home string) string { return filepath.Join(home, ".agentmux", "self-update") }

// ReposDir is where the fetch mirrors live.
func ReposDir(home string) string { return filepath.Join(BaseDir(home), "repos") }

// ShippedPath is the ship gate the orchestrator publishes through the
// gateway: {"agentmux": "<sha>", "mergentic": "<sha>"}.
func ShippedPath(home string) string { return filepath.Join(BaseDir(home), "shipped.json") }

// VersionsPath records the installed commit per repo: same shape as
// shipped.json. The gateway versions op reads it, so the orchestrator
// can confirm which commit the Mac runs without touching the Mac.
func VersionsPath(home string) string { return filepath.Join(BaseDir(home), "versions.json") }

// LogPath is the updater's event log: one line per event, including the
// `deployed <repo>@<sha>` line on success. The gateway log tail op reads
// it, and the orchestrator's deploy watch greps it.
func LogPath(home string) string { return filepath.Join(BaseDir(home), "update.log") }

// LockPath is the lock file the updater holds while installing: a local
// session that is mid-deploy creates it (see `deploy begin`/`deploy end`)
// and the updater skips the run while it exists.
func LockPath(home string) string { return filepath.Join(BaseDir(home), "deploy.lock") }

// Dirs returns every directory `self-update install` creates.
func Dirs(home string) []string {
	return []string{BaseDir(home), ReposDir(home), filepath.Join(home, ".agentmux", "log")}
}

// shaRE matches a full 40-hex commit sha, the only form the updater and
// the gate accept. Short shas never appear in state files or the log.
var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidSHA reports whether s is a full 40-hex commit sha.
func ValidSHA(s string) bool { return shaRE.MatchString(s) }

// LoadCommits reads a {"repo": "<sha>"} file. A missing file is not an
// error: no shipped commit means nothing is cleared to install, and no
// versions file means nothing is installed yet. Unknown repos are kept
// so a future repo never loses the orchestrator's record; malformed
// entries (bad shas) are dropped.
func LoadCommits(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]string{}
	for repo, sha := range raw {
		if !KnownRepo(repo) || !ValidSHA(sha) {
			continue
		}
		out[repo] = sha
	}
	return out, nil
}

// SaveCommits writes commits as {"repo": "<sha>"} atomically (temp file,
// then rename), so a crash mid-write never leaves a half-written gate
// the updater would read as cleared-to-install.
func SaveCommits(path string, commits map[string]string) error {
	clean := map[string]string{}
	for repo, sha := range commits {
		if !KnownRepo(repo) || !ValidSHA(sha) {
			continue
		}
		clean[repo] = sha
	}
	data, err := json.MarshalIndent(clean, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// KnownRepo reports whether repo is one the updater pulls.
func KnownRepo(repo string) bool {
	for _, r := range Repos {
		if r == repo {
			return true
		}
	}
	return false
}

// WantInstall reports whether repo should move from installed to shipped:
// the gate names a commit, it differs from what is installed, and both
// are well formed. Anything else means stay: no gate (nothing cleared),
// already there, or a malformed gate that must never install.
func WantInstall(repo, installed, shipped string) bool {
	if !KnownRepo(repo) || shipped == "" || shipped == installed {
		return false
	}
	return ValidSHA(shipped)
}

// Locked reports whether a local session is mid-deploy: the lock file
// exists and is fresh (under an hour old). A stale lock is a crashed
// deploy's leftover, not a live one, so the updater proceeds past it.
func Locked(home string) bool {
	fi, err := os.Stat(LockPath(home))
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) < time.Hour
}

// LogEvent appends one timestamped line to the updater log, creating the
// directory on first use. Every install writes exactly one of these:
// `deployed <repo>@<sha> old=<old> new=<sha>` on success, or
// `failed <repo>@<sha>: <reason>` (plus `rolled back ...` when a bad
// binary went in and came back out). The hub greps update.log for the
// deployed line.
func LogEvent(home, event string) error {
	if err := os.MkdirAll(filepath.Dir(LogPath(home)), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(LogPath(home), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(event))
	return err
}

// DeployedLine is the success line the hub greps for.
func DeployedLine(repo, oldSHA, newSHA string) string {
	if oldSHA == "" {
		oldSHA = "none"
	}
	return fmt.Sprintf("deployed %s@%s old=%s new=%s", repo, newSHA, oldSHA, newSHA)
}

// FailedLine is the failure line the hub greps for.
func FailedLine(repo, sha, reason string) string {
	return fmt.Sprintf("failed %s@%s: %s", repo, sha, reason)
}
