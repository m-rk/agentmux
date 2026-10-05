package retire

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/discovery"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// Env is everything Retire and GC need from the host. Production uses
// LiveEnv; tests substitute a fake. The split keeps Retire/GC pure flow
// over swappable effects: Inspect gathers, Apply changes, DeleteLeftovers
// deletes at gc time.
type Env interface {
	// Now is the clock (UTC); tests pin it.
	Now() time.Time
	// ReadRegistry returns the instance's registry fields.
	ReadRegistry(instance string) (map[string]string, error)
	// Home is the run user's home for the retired record.
	Home(instance string, fields map[string]string) string
	// Inspect gathers the session state retire acts on: threads,
	// worktree cleanliness, and branch containment.
	Inspect(ctx context.Context, instance string, fields map[string]string) (State, error)
	// Apply performs the retire and returns what it did.
	Apply(ctx context.Context, instance, agent string, fields map[string]string, st State) (RetireResult, error)
	// GCHome is the home whose retired records gc scans.
	GCHome() string
	// RetentionPath is the retention.yaml path gc reads.
	RetentionPath() string
	// DeleteLeftovers deletes one due record's threads/sessions.
	DeleteLeftovers(ctx context.Context, rec Record) (GCDeleted, error)
}

// State is what Inspect learned about one session.
type State struct {
	// Workdir is the instance workdir (the worktree).
	Workdir string
	// Branch is the worktree's branch.
	Branch string
	// Repo is the worktree's toplevel parent (git rev-parse --show-toplevel).
	Repo string
	// BranchOK reports Inspect verified the branch is safe to delete:
	// its tip is on origin/<default>, or every unique commit has an
	// upstream equivalent (squash merge, cherry-pick).
	BranchOK bool
	// BranchWhy says why the branch is kept, when Branch is set but
	// BranchOK is false.
	BranchWhy string
	// BranchUpstream is the origin ref the check ran against, e.g.
	// "origin/main".
	BranchUpstream string
	// AmpThreads are the instance's amp thread ids; amp only.
	AmpThreads []string
	// OpencodeSessions are the stored opencode session ids whose
	// directory is the workdir; opencode only.
	OpencodeSessions []string
}

// Plan lists what retire would do for agent, dry-run output.
func (s State) Plan(agent string) []string {
	var plan []string
	switch agent {
	case "amp":
		for _, thread := range s.AmpThreads {
			plan = append(plan, "archive amp thread "+thread)
		}
		plan = append(plan, "stop session", "remove units and registry entry")
	case "claude-code":
		plan = append(plan, "stop session (keep transcripts)", "remove units and registry entry")
	default: // opencode, kilo, zero
		plan = append(plan, "stop session", "remove units and registry entry")
		if agent == "opencode" && len(s.OpencodeSessions) > 0 {
			plan = append(plan, fmt.Sprintf("record %d stored opencode sessions for gc", len(s.OpencodeSessions)))
		}
	}
	if s.Workdir != "" {
		plan = append(plan, "remove worktree "+s.Workdir)
	}
	switch {
	case s.Branch == "":
	case s.BranchOK:
		plan = append(plan, "delete branch "+s.Branch+" ("+s.BranchUpstream+" contains it)")
	case s.BranchWhy != "":
		plan = append(plan, "keep branch "+s.Branch+": "+s.BranchWhy)
	default:
		plan = append(plan, "keep branch "+s.Branch)
	}
	return plan
}

// LiveEnv is the production Env: real registry, git, amp CLI, sqlite3,
// tmux, and unit files.
type LiveEnv struct{}

func (LiveEnv) Now() time.Time { return time.Now() }

func (LiveEnv) ReadRegistry(instance string) (map[string]string, error) {
	return session.ReadRegistry(instance)
}

func (LiveEnv) Home(instance string, fields map[string]string) string {
	if runUser := fields["AGENTMUX_RUN_USER"]; runUser != "" {
		if u, err := lookupUser(runUser); err == nil {
			return u.HomeDir
		}
	}
	return runas.CurrentUserHome()
}

func (LiveEnv) GCHome() string { return runas.CurrentUserHome() }

func (LiveEnv) RetentionPath() string {
	home := runas.CurrentUserHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "agentmux", "retention.yaml")
}

// Inspect gathers threads, worktree state, and branch safety. It refuses
// when the worktree is dirty. Branch safety never refuses here: Inspect
// records whether the branch is safe to delete (BranchOK) and why not
// (BranchWhy), so the dry run reports it truthfully and Apply refuses a
// real retire of unmerged work.
func (LiveEnv) Inspect(ctx context.Context, instance string, fields map[string]string) (State, error) {
	agent := fields["AGENTMUX_AGENT"]
	if agent == "" {
		agent = "claude-code"
	}
	workdir := fields["AGENTMUX_WORKDIR"]
	var st State
	if workdir == "" {
		return st, errorf(safesend.ReasonUnsupported, "%s has no workdir", instance)
	}
	st.Workdir = workdir
	repo, err := gitOut(ctx, workdir, "rev-parse", "--show-toplevel")
	if err != nil {
		return st, errorf(safesend.ReasonFailed, "worktree %s is not in a Git checkout: %v", workdir, err)
	}
	st.Repo = repo
	branch, err := gitOut(ctx, workdir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" || branch == "HEAD" {
		return st, errorf(safesend.ReasonFailed, "worktree %s is not on a branch", workdir)
	}
	st.Branch = branch
	if out, err := gitOut(ctx, workdir, "status", "--porcelain"); err != nil {
		return st, errorf(safesend.ReasonFailed, "checking worktree %s: %v", workdir, err)
	} else if strings.TrimSpace(out) != "" {
		return st, errorf(safesend.ReasonInvalid,
			"worktree %s has uncommitted changes; commit or stash them before retiring", workdir)
	}
	if ok, why, upstream := checkBranchSafe(ctx, st.Repo, st.Branch); !ok {
		st.BranchWhy = why
	} else {
		st.BranchOK = true
		st.BranchUpstream = upstream
	}
	switch agent {
	case "amp":
		threads, err := liveAmpThread(ctx, instance, fields)
		if err != nil {
			return st, err
		}
		st.AmpThreads = threads
	case "opencode":
		sessions, err := liveOpencodeSessions(ctx, instance, fields)
		if err != nil {
			return st, err
		}
		st.OpencodeSessions = sessions
	}
	return st, nil
}

// Apply performs the retire: archive or stop, remove units and registry,
// remove the worktree, delete the branch only when Inspect verified it is
// safe. A real retire of unmerged work is refused so the caller can raise
// an ask; rerun the dry run after merging.
// Inspect verified BranchOK against the template repo it runs in, but the
// delete runs from the main checkout, so Apply re-verifies there before
// deleting: a branch that moved on since Inspect must not be deleted.
func (e LiveEnv) Apply(ctx context.Context, instance, agent string, fields map[string]string, st State) (RetireResult, error) {
	var res RetireResult
	res.Workdir = st.Workdir
	switch agent {
	case "amp":
		if len(st.AmpThreads) == 0 {
			return res, errorf(safesend.ReasonNotFound, "no amp thread found for %s", instance)
		}
		for _, thread := range st.AmpThreads {
			if err := ampArchive(ctx, instance, fields, thread); err != nil {
				return res, err
			}
		}
		res.AmpThreads = st.AmpThreads
	case "opencode":
		res.OpencodeSessions = st.OpencodeSessions
	}
	if err := session.Stop(instance); err != nil {
		return res, errorf(safesend.ReasonFailed, "stopping %s: %v", instance, err)
	}
	if err := removeUnits(instance); err != nil {
		return res, err
	}
	if err := removeRegistry(instance); err != nil {
		return res, err
	}
	// The op env-file holds references (op://...), never secret values,
	// but it authenticated this instance alone — leaving it behind would
	// let a reused name silently inherit auth. Remove it with the rest.
	_ = os.Remove(opEnvFile(instance, fields))
	branchRes, err := deleteBranchWhenMerged(ctx, st)
	if err != nil {
		return res, err
	}
	res.Branch, res.BranchDeleted, res.BranchKept = st.Branch, branchRes.deleted, branchRes.kept
	if err := removeWorktree(ctx, st, branchRes.deleted); err != nil {
		return res, err
	}
	return res, nil
}

// DeleteLeftovers deletes one due record's threads or stored sessions.
// Claude-code records hold neither — its transcripts are never deleted —
// so deleting one is just dropping the record, which GC does after this
// returns.
func (LiveEnv) DeleteLeftovers(ctx context.Context, rec Record) (GCDeleted, error) {
	del := deletedOf(rec)
	switch rec.Agent {
	case "amp":
		for _, thread := range rec.AmpThreads {
			if err := ampDelete(ctx, rec.Instance, thread); err != nil {
				return GCDeleted{}, err
			}
		}
	case "opencode":
		if err := opencodeDeleteSessions(rec); err != nil {
			return GCDeleted{}, err
		}
	}
	return del, nil
}

// gitOut runs git in dir and returns trimmed stdout.
var gitOut = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := runas.CurrentUserCommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %v", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// branchDisposition is what deleteBranchWhenMerged decided.
type branchDisposition struct {
	deleted bool
	kept    string // why not, when not deleted
}

// defaultBranchName returns the template repo's default-branch short name
// ("main", "master", ...): the local symbolic ref of origin/HEAD, or the
// configured init.defaultBranch, or the first of main/master that exists.
// It never guesses blindly — an unknown default is an error.
func defaultBranchName(ctx context.Context, repo string) (string, error) {
	if out, err := gitOut(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimPrefix(strings.TrimSpace(out), "origin/"); name != "" && name != out {
			return name, nil
		}
	}
	if out, err := gitOut(ctx, repo, "config", "--get", "init.defaultBranch"); err == nil {
		if name := strings.TrimSpace(out); name != "" {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if runGitOK(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+name) == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("cannot determine the default branch (no origin/HEAD, no init.defaultBranch, no main or master)")
}

// fetchOrigin updates only the default branch from origin. A full fetch
// would touch every remote-tracking ref; retire needs one ref fresh.
// GIT_TERMINAL_PROMPT=0 is already in gitOut's environment, so a network
// outage fails fast instead of prompting.
func fetchOrigin(ctx context.Context, repo, def string) error {
	if _, err := gitOut(ctx, repo, "fetch", "origin", def); err != nil {
		return fmt.Errorf("fetching origin %s: %v", def, err)
	}
	return nil
}

// branchUpstreamEquivalent reports whether every commit unique to branch
// has an equivalent on upstream — the squash-merge and cherry-pick case:
// `git cherry upstream branch` marks equivalents with "-" and missing
// commits with "+". No "+" lines means nothing would be lost. Merge
// commits always show as "+" (cherry can't match them), so they fall
// through to the keep path with a reason naming them.
func branchUpstreamEquivalent(ctx context.Context, repo, upstream, branch string) (bool, error) {
	out, err := gitOut(ctx, repo, "cherry", upstream, branch)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "+") {
			return false, nil
		}
	}
	return true, nil
}

// checkBranchSafe verifies branch is safe to delete from repo: after
// fetching origin's default branch, either the tip is an ancestor of the
// fresh origin/<default>, or every unique commit has an upstream
// equivalent. It returns (ok, keep-reason, upstream-ref). "main" itself
// and empty names are never deletable. Any git failure keeps the branch
// with the failure as the reason — retire deletes only on positive proof.
func checkBranchSafe(ctx context.Context, repo, branch string) (ok bool, why, upstream string) {
	if branch == "" || branch == "main" || branch == "master" {
		return false, "not a task branch", ""
	}
	def, err := defaultBranchName(ctx, repo)
	if err != nil {
		return false, err.Error(), ""
	}
	upstream = "origin/" + def
	if err := fetchOrigin(ctx, repo, def); err != nil {
		return false, err.Error(), upstream
	}
	if err := runGitOK(ctx, repo, "merge-base", "--is-ancestor", branch, upstream); err == nil {
		return true, "", upstream
	}
	equiv, err := branchUpstreamEquivalent(ctx, repo, upstream, branch)
	if err != nil {
		return false, fmt.Sprintf("checking %s against %s: %v", branch, upstream, err), upstream
	}
	if equiv {
		return true, "", upstream
	}
	return false, fmt.Sprintf("branch %s has commits not on %s; merge it before retiring", branch, upstream), upstream
}

// deleteBranchWhenMerged decides the branch's fate at retire time. It
// deletes only when Inspect verified safety (BranchOK) and the check
// still holds now: re-fetch and re-verify, because the branch may have
// moved on since Inspect ran. Otherwise it keeps the branch and says why
// — the caller raises an ask instead of deleting unmerged work.
func deleteBranchWhenMerged(ctx context.Context, st State) (branchDisposition, error) {
	if st.Branch == "" || st.Branch == "main" || st.Branch == "master" {
		return branchDisposition{kept: "not a task branch"}, nil
	}
	if !st.BranchOK {
		why := st.BranchWhy
		if why == "" {
			why = fmt.Sprintf("branch %s was not verified safe to delete; merge it before retiring", st.Branch)
		}
		return branchDisposition{}, errorf(safesend.ReasonInvalid, "%s", why)
	}
	if ok, why, _ := checkBranchSafe(ctx, st.Repo, st.Branch); !ok {
		return branchDisposition{}, errorf(safesend.ReasonInvalid, "%s", why)
	}
	return branchDisposition{deleted: true}, nil
}

// removeWorktree removes the worktree; when the branch was deleted it also
// deletes the branch ref with -D (safety was verified twice already, and
// the worktree's own checkout can make -d's check unreliable). When the
// branch was kept the ref is left alone. The worktree goes first: with
// --force the remove succeeds even while the branch is checked out in
// it, and the branch delete follows from the main checkout.
func removeWorktree(ctx context.Context, st State, branchDeleted bool) error {
	if st.Workdir == "" {
		return nil
	}
	// Resolve the main checkout before removing the worktree: for a
	// linked worktree `rev-parse --show-toplevel` returns the worktree
	// itself, and the remove below deletes that directory, so -C it
	// afterwards would fail (confirmed live in the e2e test).
	main, merr := mainWorktree(ctx, st.Repo)
	if st.Branch != "" && st.Branch != "main" {
		if merr != nil {
			return errorf(safesend.ReasonFailed, "locating main checkout: %v", merr)
		}
	}
	if _, err := gitOut(ctx, st.Repo, "worktree", "remove", "--force", st.Workdir); err != nil {
		return errorf(safesend.ReasonFailed, "removing worktree %s: %v", st.Workdir, err)
	}
	if branchDeleted && st.Branch != "" && st.Branch != "main" && st.Branch != "master" {
		if _, err := gitOut(ctx, main, "branch", "-D", st.Branch); err != nil {
			return errorf(safesend.ReasonFailed, "deleting branch %s: %v", st.Branch, err)
		}
	}
	return nil
}

// mainWorktree returns the main checkout path: the first entry of
// `git worktree list --porcelain`, which git always prints first. dir may
// be any worktree of the repo (or the repo itself).
func mainWorktree(ctx context.Context, dir string) (string, error) {
	out, err := gitOut(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok && path != "" {
			return path, nil
		}
	}
	return "", fmt.Errorf("no worktrees listed")
}

func runGitOK(ctx context.Context, dir string, args ...string) error {
	_, err := gitOut(ctx, dir, args...)
	return err
}

// removeUnits disables and deletes every unit/timer/plist the provisioner
// may have installed for the instance, on either platform. Missing units
// are fine — an amp instance with self-updates off never had an update
// unit, and a macOS host has no systemd units at all.
func removeUnits(instance string) error {
	systemctl("disable", "--now", "agentmux-"+instance+".service")
	systemctl("disable", "--now", "agentmux-"+instance+"-update.service")
	systemctl("disable", "--now", "agentmux-"+instance+"-update.timer")
	systemctl("disable", "--now", "agentmux-"+instance+"-tick.service")
	systemctl("disable", "--now", "agentmux-"+instance+"-tick.timer")
	for _, unit := range []string{"agentmux-" + instance + ".service",
		"agentmux-" + instance + "-update.service",
		"agentmux-" + instance + "-update.timer",
		"agentmux-" + instance + "-tick.service",
		"agentmux-" + instance + "-tick.timer"} {
		if err := os.Remove(filepath.Join("/etc/systemd/system", unit)); err != nil && !os.IsNotExist(err) {
			return errorf(safesend.ReasonFailed, "removing unit %s: %v", unit, err)
		}
	}
	systemctl("daemon-reload")
	launchctlBootout("com.agentmux." + instance)
	launchctlBootout("com.agentmux." + instance + ".update")
	for _, label := range []string{"com.agentmux." + instance, "com.agentmux." + instance + ".update"} {
		home := runas.CurrentUserHome()
		if home == "" {
			continue
		}
		if err := os.Remove(filepath.Join(home, "Library", "LaunchAgents", label+".plist")); err != nil && !os.IsNotExist(err) {
			return errorf(safesend.ReasonFailed, "removing plist %s: %v", label, err)
		}
	}
	return nil
}

var systemctl = func(args ...string) {
	_ = exec.Command("systemctl", args...).Run()
}

var launchctlBootout = func(label string) {
	home := runas.CurrentUserHome()
	if home == "" {
		return
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	_ = exec.Command("launchctl", "bootout", "gui/"+uidOf(), plist).Run()
}

func uidOf() string {
	return fmt.Sprint(os.Getuid())
}

// removeRegistry deletes the instance's registry file, so discovery stops
// listing it.
func removeRegistry(instance string) error {
	path := filepath.Join(discovery.EnvDir, instance+".env")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return errorf(safesend.ReasonFailed, "removing registry for %s: %v", instance, err)
	}
	return nil
}

// opEnvFile is the instance's 1Password env-file, if any (see
// session/openv.go for the canonical path).
func opEnvFile(instance string, fields map[string]string) string {
	home := runas.CurrentUserHome()
	if runUser := fields["AGENTMUX_RUN_USER"]; runUser != "" {
		if u, err := lookupUser(runUser); err == nil {
			home = u.HomeDir
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".agentmux", "env", instance+".env")
}
