package ops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// GitTopLevelFor resolves the repository top level for branch's checkout:
// workdir when given (or the current directory when empty), via git
// rev-parse --show-toplevel. It mirrors ops.Create's template check that
// the workdir sits in a Git checkout.
func GitTopLevelFor(branch, workdir string) (string, error) {
	dir := workdir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolving the worktree for branch %s: %v", branch, err)
		}
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("%s is not in a Git checkout", dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// forbiddenTrailerRE matches one agent/AI trailer line in a commit message:
// amp's Amp-Thread-ID / Co-authored-by, Claude's Co-Authored-By /
// Claude-Session, and "Generated with ..." footers (MERG-7's attribution
// rule). It matches whole lines only, so a subject that merely mentions a
// trailer name is not flagged. Case-insensitive across the name variants
// runners actually emit.
var forbiddenTrailerRE = regexp.MustCompile(`(?im)^[[:space:]]*(amp-thread-id|co-authored-by|claude-session|generated-with)[[:space:]]*:.*$|(?im)^[[:space:]]*generated[[:space:]]+with[[:space:]].*$`)

// ForbiddenTrailerNames lists the agent/AI trailer names CheckBranchTrailers
// and `agentmux ship-check` refuse outside go1com repos, in plain words.
const ForbiddenTrailerNames = "Amp-Thread-ID, Co-Authored-By, Claude-Session, Generated-With, Generated with"

// messageHasTrailer reports whether a commit message carries an agent/AI
// trailer line.
func messageHasTrailer(message string) bool {
	return forbiddenTrailerRE.MatchString(message)
}

// go1comOrg is the GitHub org whose repos allow AI trailers (MERG-7's
// attribution opt-in); every other repo forbids them.
const go1comOrg = "go1com"

// projectOrgOfOrigin resolves repo's origin URL to "host/org/repo" and
// reports its org. A local-path origin (the test fixture's bare repo) has
// no org: it returns "" with an error, which forbids trailers (never
// allows them) everywhere the result is used.
func (e Env) projectOrgOfOrigin(ctx context.Context, repo string) (string, error) {
	out, err := e.git(ctx, repo, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(out) == "" {
		return "", err
	}
	project, err := normalizeGitOrigin(strings.TrimSpace(out))
	if err != nil {
		return "", err
	}
	parts := strings.Split(project, "/")
	if len(parts) < 3 {
		return "", nil
	}
	return parts[1], nil
}

// normalizeGitOrigin normalizes a git origin URL to "host/org/repo",
// mirroring collab.NormalizeGitRemote's rules without importing collab.
func normalizeGitOrigin(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", fmt.Errorf("git origin is empty")
	}
	if filepath.IsAbs(remote) || strings.HasPrefix(remote, "./") || strings.HasPrefix(remote, "../") || strings.HasPrefix(remote, "file://") {
		return "", fmt.Errorf("local git origin %q has no org", remote)
	}
	host, repoPath := "", ""
	if strings.Contains(remote, "://") {
		rest := remote[strings.Index(remote, "://")+3:]
		i := strings.IndexAny(rest, "/")
		if i < 0 {
			return "", fmt.Errorf("unsupported git origin %q", remote)
		}
		host, repoPath = rest[:i], rest[i+1:]
	} else if at := strings.LastIndex(remote, "@"); at >= 0 {
		afterUser := remote[at+1:]
		colon := strings.Index(afterUser, ":")
		if colon <= 0 {
			return "", fmt.Errorf("unsupported git origin %q", remote)
		}
		host, repoPath = afterUser[:colon], afterUser[colon+1:]
	} else {
		return "", fmt.Errorf("unsupported git origin %q", remote)
	}
	host = strings.ToLower(host)
	repoPath = strings.Trim(strings.TrimSuffix(repoPath, ".git"), "/")
	if repoPath == "" {
		return "", fmt.Errorf("unsupported git origin %q", remote)
	}
	return host + "/" + strings.ToLower(repoPath), nil
}

// TrailerFinding names one branch commit carrying agent/AI trailers.
type TrailerFinding struct {
	Hash    string `json:"hash"`
	Subject string `json:"subject"`
}

// CheckBranchTrailers lists the commits on branch, past baseBranch, whose
// messages carry agent/AI trailers, oldest first. baseBranch defaults to
// the origin default branch (origin/HEAD's target, else origin/main) when
// empty. It returns nil without looking at any commit when the repo's
// origin lives under the go1com org, where trailers are allowed. A repo
// with no origin, or a local-path origin with no org, is scanned: the
// org check only ever skips go1com, it never skips the scan for lack of
// an org.
//
// The base is fetched first so a stale remote-tracking ref cannot hide a
// trailer that already merged — the same fetch discipline as Create.
func (e Env) CheckBranchTrailers(ctx context.Context, repo, branch, baseBranch string) ([]TrailerFinding, error) {
	if org, err := e.projectOrgOfOrigin(ctx, repo); err == nil && org == go1comOrg {
		return nil, nil
	}
	if baseBranch == "" {
		if ref, err := e.git(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && strings.TrimSpace(ref) != "" {
			baseBranch = strings.TrimSpace(ref)
		} else {
			baseBranch = "origin/main"
		}
	} else {
		baseBranch = strings.TrimPrefix(baseBranch, "origin/")
		baseBranch = "origin/" + baseBranch
	}
	// Fetch first so a stale remote-tracking ref cannot hide a trailer
	// that already merged — the same fetch discipline as Create. A plain
	// `git fetch origin` updates the configured refspecs (including the
	// branch when it is pushed); failures refuse rather than scan stale
	// refs.
	if _, err := e.git(ctx, repo, "fetch", "origin"); err != nil {
		return nil, Refuse(safesend.ReasonFailed, "fetching origin: %v", err)
	}
	// Resolve the branch the same way git would for the log: prefer the
	// remote-tracking ref when the branch is pushed (so the scan covers
	// what ship would merge), else the local branch.
	branchRef := branch
	if _, err := e.git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+strings.TrimPrefix(branch, "origin/")); err == nil {
		branchRef = "origin/" + strings.TrimPrefix(branch, "origin/")
	}
	out, err := e.git(ctx, repo, "log", "--format=%H%x00%s%x00%B%x00%x1e", baseBranch+".."+branchRef)
	if err != nil {
		return nil, Refuse(safesend.ReasonFailed, "listing commits on %s past %s: %v", branch, baseBranch, err)
	}
	var bad []TrailerFinding
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimPrefix(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		parts := strings.SplitN(rec, "\x00", 3)
		if len(parts) < 3 || strings.TrimSpace(parts[0]) == "" {
			continue
		}
		if messageHasTrailer(parts[2]) {
			bad = append(bad, TrailerFinding{Hash: strings.TrimSpace(parts[0]), Subject: strings.TrimSpace(parts[1])})
		}
	}
	return bad, nil
}

// privacyFinding is one file whose uncommitted changes (or untracked
// contents) look like they leak a private hostname or personal path.
type privacyFinding = string

// ScanWorktreePrivacy scans the worktree's uncommitted changes (staged and
// unstaged) plus untracked file contents for private hostnames and
// personal paths, returning one human-readable finding per offending
// file, capped at maxFindings (default 20). It is advisory: callers
// report the findings without refusing. It looks for:
//   - absolute home paths other than the generic /home/alice and
//     /Users/alice placeholders
//   - Tailscale-style hostnames (<name>.tailXXXX.ts.net) and .local/.lan
//     host references that look like real machine names
//
// Lockfiles and vendored output are skipped: paths there are build
// artifacts, not leaks.
func (e Env) ScanWorktreePrivacy(ctx context.Context, wtPath string, maxFindings int) []privacyFinding {
	if maxFindings <= 0 {
		maxFindings = 20
	}
	type fileLines struct {
		path  string
		lines []string
	}
	var candidates []fileLines
	addDiff := func(args ...string) {
		out, err := e.git(ctx, wtPath, args...)
		if err != nil || out == "" {
			return
		}
		cur := ""
		var lines []string
		flush := func() {
			if cur != "" {
				candidates = append(candidates, fileLines{cur, lines})
				cur, lines = "", nil
			}
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "+++ b/") {
				flush()
				cur = strings.TrimPrefix(l, "+++ b/")
				continue
			}
			if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") && cur != "" {
				lines = append(lines, l[1:])
			}
		}
		flush()
	}
	addDiff("diff", "--no-color", "--unified=0", "--", ".")
	addDiff("diff", "--no-color", "--unified=0", "--cached", "--", ".")

	if out, err := e.git(ctx, wtPath, "status", "--porcelain", "--untracked-files=all", "--", "."); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if !strings.HasPrefix(l, "?? ") {
				continue
			}
			rel := strings.TrimPrefix(l, "?? ")
			data, rerr := os.ReadFile(filepath.Join(wtPath, rel))
			if rerr != nil {
				continue
			}
			lines := strings.Split(string(data), "\n")
			if len(lines) > 500 {
				lines = lines[:500]
			}
			candidates = append(candidates, fileLines{rel, lines})
		}
	}

	privatePathRE := regexp.MustCompile(`/((home|Users)/[^/\s]+/)`)
	tsHostRE := regexp.MustCompile(`[a-zA-Z0-9-]+(\.tail[a-zA-Z0-9]+\.ts\.net|\.local|\.lan)\b`)
	// hookFileRE matches the hook filenames this tooling manages
	// (commit-msg, commit-msg.local): tool vocabulary in code and
	// comments, not leaks. A line whose only hostname-shaped token is
	// one of these is skipped.
	hookFileRE := regexp.MustCompile(`commit-msg(\.local)?\b`)
	var findings []privacyFinding
	for _, c := range candidates {
		if len(findings) >= maxFindings {
			break
		}
		if strings.HasSuffix(c.path, ".lock") || strings.Contains(c.path, "package-lock.json") || strings.Contains(c.path, "node_modules/") {
			continue
		}
		hit := ""
		for _, l := range c.lines {
			if m := privatePathRE.FindString(l); m != "" && !strings.Contains(l, "/home/alice/") && !strings.Contains(l, "/Users/alice/") {
				hit = "personal path " + m
				break
			}
			if m := tsHostRE.FindString(l); m != "" && !hookFileRE.MatchString(l) {
				hit = "private hostname " + m
				break
			}
		}
		if hit != "" {
			findings = append(findings, c.path+": "+hit)
		}
	}
	return findings
}
