package retire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
	"github.com/m-rk/agentmux/daemon/internal/transcript"
)

// Options tunes Retire; see the Retire function.
type Options struct {
	// DryRun lists what would go without changing anything.
	DryRun bool
	// RequireMerged refuses the retire when any branch isn't provably
	// merged instead of retiring with the branch kept: the strict mode
	// for callers that want the old refuse-and-ask behavior.
	RequireMerged bool
}

// Env is everything Retire and GC need from the host. Production uses
// DaemonEnv (privileged half through the daemon, git half as the run
// user); LiveEnv runs the managed half directly (macOS, where the caller
// already owns those paths, and tests); other tests substitute a fake.
// The split keeps Retire/GC pure flow over swappable effects: Inspect
// gathers, Apply changes, DeleteLeftovers deletes at gc time.
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
	// RemoveManaged stops the session and removes its units and
	// registry entry; the privileged half retire never runs as root
	// for. It returns a human-readable summary for the report.
	RemoveManaged(ctx context.Context, instance string) (string, error)
	// GCHome is the home whose retired records gc scans.
	GCHome() string
	// RetentionPath is the retention.yaml path gc reads.
	RetentionPath() string
	// DeleteLeftovers deletes one due record's threads/sessions.
	DeleteLeftovers(ctx context.Context, rec Record) (GCDeleted, error)
	// SweepDelete permanently deletes one swept junk thread whose
	// retention has expired (see the ampsweep package). The default
	// (nil) leaves swept records to the sweep's own gc; production
	// Envs wire the amp CLI delete.
	SweepDelete(ctx context.Context, thread string) error
	// SweptGone reports whether a swept thread is already gone from
	// the account (an export failure means archived-gone, so the
	// record drops instead of blocking the gc forever).
	SweptGone(ctx context.Context, thread string) (bool, error)
}

// BranchFate is what retire decided about one branch: deleted because
// origin provably contains it, or kept with the reason.
type BranchFate struct {
	Branch  string `json:"branch"`
	Deleted bool   `json:"deleted,omitempty"`
	Kept    string `json:"kept,omitempty"`
	// Upstream is the origin ref the check ran against, e.g.
	// "origin/main"; set when the check ran.
	Upstream string `json:"upstream,omitempty"`
}

// State is what Inspect learned about one session.
type State struct {
	// Workdir is the instance workdir (the worktree).
	Workdir string
	// Branch is the worktree's branch; empty when detached or on a
	// non-task branch (main/master), where there is nothing to decide.
	Branch string
	// Repo is the worktree's toplevel parent (git rev-parse --show-toplevel).
	Repo string
	// Branches is every branch retire will delete-or-keep: the
	// worktree's branch first, then the registry's recorded branch and
	// any task-family branches, each with its own verdict.
	Branches []BranchFate
	// Detached reports the worktree is on no branch.
	Detached bool
	// HeadSHA is the detached HEAD commit, short form for the report.
	HeadSHA string
	// HeadOnOrigin reports the detached HEAD is on origin's default
	// branch: nothing unique would be lost removing the worktree.
	HeadOnOrigin bool
	// WorktreeKept says why the worktree stays, when it does (dirty, or
	// detached with commits not on origin).
	WorktreeKept string
	// AmpThreads are the instance's amp thread ids; amp only.
	AmpThreads []string
	// OpencodeSessions are the stored opencode session ids whose
	// directory is the workdir; opencode only.
	OpencodeSessions []string
}

// branchLine renders one branch fate as a plan/report line.
func branchLine(f BranchFate) string {
	if f.Deleted {
		if f.Upstream != "" {
			return "delete branch " + f.Branch + " (" + f.Upstream + " contains it)"
		}
		return "delete branch " + f.Branch
	}
	if f.Kept != "" {
		return "keep branch " + f.Branch + ": " + f.Kept
	}
	return "keep branch " + f.Branch
}

// Plan lists what retire would do for agent, dry-run output.
func (s State) Plan(agent string) []string {
	var plan []string
	switch agent {
	case "amp":
		for _, thread := range s.AmpThreads {
			plan = append(plan, "archive amp thread "+thread)
		}
		plan = append(plan, "stop in-flight amp runs", "stop session", "remove units and registry entry")
	case "codex":
		plan = append(plan, "stop in-flight codex runs", "stop session (keep codex rollout files)", "remove units and registry entry")
	case "claude-code":
		plan = append(plan, "stop session (keep transcripts)", "remove units and registry entry")
	default: // opencode, kilo, zero
		plan = append(plan, "stop session", "remove units and registry entry")
		if agent == "opencode" && len(s.OpencodeSessions) > 0 {
			plan = append(plan, fmt.Sprintf("record %d stored opencode sessions for gc", len(s.OpencodeSessions)))
		}
	}
	if s.WorktreeKept != "" {
		plan = append(plan, "keep worktree "+s.Workdir+": "+s.WorktreeKept)
	} else if s.Workdir != "" {
		plan = append(plan, "remove worktree "+s.Workdir)
	}
	if s.Detached {
		plan = append(plan, "detached HEAD at "+s.HeadSHA)
	}
	for _, f := range s.Branches {
		plan = append(plan, branchLine(f))
	}
	return plan
}

// ManagedStopper ends a session's managed half: stop, remove units and
// registry. The daemon implements it (privileged); the ops layer adapts
// its daemon client to this interface.
type ManagedStopper interface {
	StopRemove(ctx context.Context, instance string) (string, error)
}

// ManagedRefusal wraps a managed-half refusal message as an error the
// ops layer returns when the daemon reports ok=false.
func ManagedRefusal(message string) error {
	return errorf(safesend.ReasonFailed, "%s", message)
}

// DaemonEnv is the production Env: the privileged half (stop, units,
// registry) goes through the daemon via ManagedStopper, so `sessions
// retire` works unprivileged; everything else runs here as the run user,
// never as root.
type DaemonEnv struct {
	Daemon ManagedStopper
}

func (DaemonEnv) Now() time.Time { return time.Now() }

func (DaemonEnv) ReadRegistry(instance string) (map[string]string, error) {
	return session.ReadRegistry(instance)
}

func (DaemonEnv) Home(instance string, fields map[string]string) string {
	return runUserHome(fields)
}

func (e DaemonEnv) RemoveManaged(ctx context.Context, instance string) (string, error) {
	if e.Daemon == nil {
		return "", errorf(safesend.ReasonFailed, "no daemon client for the managed half of retiring %s", instance)
	}
	return e.Daemon.StopRemove(ctx, instance)
}

// DeleteLeftovers deletes one due record's threads or stored sessions;
// see LiveEnv.DeleteLeftovers — gc behavior is identical either way.
func (DaemonEnv) DeleteLeftovers(ctx context.Context, rec Record) (GCDeleted, error) {
	return deleteLeftovers(ctx, rec)
}

// SweepDelete permanently deletes one swept junk thread whose retention
// has expired: `amp threads delete`, local and server-side.
func (DaemonEnv) SweepDelete(ctx context.Context, thread string) error {
	return sweepDelete(ctx, thread)
}

// SweptGone reports whether a swept thread is already gone from the
// account: an export failure means archived-gone (a visible thread
// exports fine), so the record drops instead of blocking the gc.
func (DaemonEnv) SweptGone(ctx context.Context, thread string) (bool, error) {
	return sweptGone(ctx, thread)
}

// DeleteLeftovers deletes one due record's threads or stored sessions.
// Claude-code records hold neither — its transcripts are never deleted —
// so deleting one is just dropping the record, which GC does after this
// returns.
func (LiveEnv) DeleteLeftovers(ctx context.Context, rec Record) (GCDeleted, error) {
	return deleteLeftovers(ctx, rec)
}

// SweepDelete permanently deletes one swept junk thread; see
// DaemonEnv.SweepDelete — gc behavior is identical either way.
func (LiveEnv) SweepDelete(ctx context.Context, thread string) error {
	return sweepDelete(ctx, thread)
}

// SweptGone reports whether a swept thread is already gone; see
// DaemonEnv.SweptGone.
func (LiveEnv) SweptGone(ctx context.Context, thread string) (bool, error) {
	return sweptGone(ctx, thread)
}

// deleteLeftovers is the shared gc-time deletion both Envs run.
func deleteLeftovers(ctx context.Context, rec Record) (GCDeleted, error) {
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

// sweptDirName is the state dir under the run user's home holding one
// JSON record per swept junk thread, mirroring retired records. The
// sweep writes them (see the ampsweep package); gc reads them here.
const sweptDirName = ".local/state/agentmux/swept"

// sweptRecord is one swept thread: the archived thread id, when the
// sweep archived it (UTC — gc counts retention from here), and the
// reason, kept for the report.
type sweptRecord struct {
	Thread     string    `json:"thread"`
	Title      string    `json:"title,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	ArchivedAt time.Time `json:"archived_at"`
}

// sweepDelete permanently deletes one swept junk thread whose retention
// has expired: `amp threads delete`, the same deletion retire's gc runs
// for archived worker threads (see ampDelete).
func sweepDelete(ctx context.Context, thread string) error {
	if !transcript.ValidAmpThreadID(thread) {
		return errorf(safesend.ReasonInvalid, "%q is not an amp thread id", thread)
	}
	src := transcript.Source{Agent: "amp"}
	if u, err := user.Current(); err == nil {
		src.Home = u.HomeDir
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := ampArchiveRun(ctx, src, "threads", "delete", thread); err != nil {
		return errorf(safesend.ReasonFailed, "deleting swept thread %s: %v", thread, ampErr(err))
	}
	return nil
}

// sweptGone reports whether a swept thread is already gone from the
// account: an export failure means archived-gone (a visible thread
// exports fine), so the record drops instead of blocking the gc
// forever. A cancelled context is not "gone" — it aborts the gc.
func sweptGone(ctx context.Context, thread string) (bool, error) {
	src := transcript.Source{Agent: "amp"}
	if u, err := user.Current(); err == nil {
		src.Home = u.HomeDir
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := ampArchiveRun(ctx, src, "threads", "export", thread); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return true, nil
	}
	return false, nil
}

// listSwept returns every swept-thread record under home. A missing dir
// is nil, not an error; an unreadable record is skipped, never fatal.
func listSwept(home string) ([]sweptRecord, error) {
	dir := filepath.Join(home, sweptDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []sweptRecord
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec sweptRecord
		if err := json.Unmarshal(data, &rec); err != nil || rec.Thread == "" {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// removeSwept drops one swept record; a missing record is not an error.
func removeSwept(home, thread string) error {
	err := os.Remove(filepath.Join(home, sweptDirName, thread+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (DaemonEnv) GCHome() string { return runas.CurrentUserHome() }

func (DaemonEnv) RetentionPath() string {
	home := runas.CurrentUserHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "agentmux", "retention.yaml")
}

// runUserHome resolves the registry's AGENTMUX_RUN_USER home, falling
// back to the current user (macOS, where every instance runs as its own
// user and the field is unset).
func runUserHome(fields map[string]string) string {
	if runUser := fields["AGENTMUX_RUN_USER"]; runUser != "" {
		if u, err := lookupUser(runUser); err == nil {
			return u.HomeDir
		}
	}
	return runas.CurrentUserHome()
}

// LiveEnv runs the managed half directly instead of through the daemon:
// macOS, where the caller already owns those paths, and tests. On Linux
// it needs the caller's privilege (root) for units and registry.
type LiveEnv struct{}

func (LiveEnv) Now() time.Time { return time.Now() }

func (LiveEnv) ReadRegistry(instance string) (map[string]string, error) {
	return session.ReadRegistry(instance)
}

func (LiveEnv) Home(instance string, fields map[string]string) string {
	return runUserHome(fields)
}

func (LiveEnv) RemoveManaged(_ context.Context, instance string) (string, error) {
	if err := session.StopManaged(instance); err != nil {
		// Same advisory-stop semantics as the daemon side: a
		// stopped-but-registered instance reads dead either way.
		return fmt.Sprintf("stopped session with a warning (%v), removed units and registry entry", err), removeManagedDirect(instance)
	}
	return "stopped session, removed units and registry entry", removeManagedDirect(instance)
}

func removeManagedDirect(instance string) error {
	if err := session.RemoveUnits(instance); err != nil {
		return err
	}
	return session.RemoveRegistry(instance)
}

func (LiveEnv) GCHome() string { return runas.CurrentUserHome() }

func (LiveEnv) RetentionPath() string {
	home := runas.CurrentUserHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "agentmux", "retention.yaml")
}

// Inspect gathers threads, worktree state, and per-branch safety. It
// refuses when the worktree is dirty or detached with commits not on
// origin — either way removing the worktree would lose work. Branch
// safety never refuses here: Inspect records each branch's fate, so the
// dry run reports it truthfully and a real retire keeps unmerged
// branches instead of refusing the whole retire.
func (LiveEnv) Inspect(ctx context.Context, instance string, fields map[string]string) (State, error) {
	return inspect(ctx, instance, fields)
}

// Inspect gathers threads, worktree state, and per-branch safety; see
// LiveEnv.Inspect. Shared so both Envs inspect identically.
func (e DaemonEnv) Inspect(ctx context.Context, instance string, fields map[string]string) (State, error) {
	return inspect(ctx, instance, fields)
}

func inspect(ctx context.Context, instance string, fields map[string]string) (State, error) {
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
	git := gitRunner(fields["AGENTMUX_RUN_USER"])
	repo, err := git(ctx, workdir, "rev-parse", "--show-toplevel")
	if err != nil {
		return st, errorf(safesend.ReasonFailed, "worktree %s is not in a Git checkout: %v", workdir, err)
	}
	st.Repo = repo
	if out, err := git(ctx, workdir, "status", "--porcelain"); err != nil {
		return st, errorf(safesend.ReasonFailed, "checking worktree %s: %v", workdir, err)
	} else if strings.TrimSpace(out) != "" {
		return st, errorf(safesend.ReasonInvalid,
			"worktree %s has uncommitted changes; commit or stash them before retiring", workdir)
	}
	branch, err := git(ctx, workdir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" {
		return st, errorf(safesend.ReasonFailed, "reading worktree %s branch: %v", workdir, err)
	}
	if branch == "HEAD" {
		// Detached (MERG-20): keep the worktree when HEAD holds commits
		// not on origin, else retire it with no branch to decide.
		sha, serr := git(ctx, workdir, "rev-parse", "--short", "HEAD")
		if serr != nil {
			return st, errorf(safesend.ReasonFailed, "reading worktree %s HEAD: %v", workdir, serr)
		}
		st.Detached, st.HeadSHA = true, sha
		if onOrigin, _ := headOnOrigin(ctx, git, repo); onOrigin {
			st.HeadOnOrigin = true
		} else {
			st.WorktreeKept = fmt.Sprintf("detached HEAD %s has commits not on origin; attach a branch or merge it before retiring", sha)
			return st, errorf(safesend.ReasonInvalid, "worktree %s: %s", workdir, st.WorktreeKept)
		}
	} else {
		st.Branch = branch
	}
	// Branches checked out in another live worktree are never
	// delete-or-keep candidates: deleting one would pull the branch out
	// from under that worktree (git refuses it anyway), so they are
	// kept with the path named.
	checkedOut := worktreeBranches(ctx, git, st.Repo)
	for _, b := range branchCandidates(ctx, git, instance, fields, st) {
		if other, ok := checkedOut[shortRef(b)]; ok && !sameGitPath(other, st.Workdir) {
			st.Branches = append(st.Branches, BranchFate{Branch: b,
				Kept: fmt.Sprintf("checked out at %s; retire that worktree first", other)})
			continue
		}
		ok, why, upstream := checkBranchSafe(ctx, git, st.Repo, b)
		f := BranchFate{Branch: b, Deleted: ok, Upstream: upstream}
		if !ok {
			f.Kept = why
		}
		st.Branches = append(st.Branches, f)
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

// Apply performs the retire: archive or record threads, end the managed
// half through the daemon, then delete-or-keep each branch and remove
// the worktree when nothing would be lost. Unmerged branches are kept
// with their reason, never a refusal: the dry run already said so.
func (e DaemonEnv) Apply(ctx context.Context, instance, agent string, fields map[string]string, st State) (RetireResult, error) {
	return apply(ctx, e.RemoveManaged, instance, agent, fields, st)
}

// Apply performs the retire; see DaemonEnv.Apply. Shared so both Envs
// apply identically.
func (e LiveEnv) Apply(ctx context.Context, instance, agent string, fields map[string]string, st State) (RetireResult, error) {
	return apply(ctx, e.RemoveManaged, instance, agent, fields, st)
}

func apply(ctx context.Context, removeManaged func(context.Context, string) (string, error), instance, agent string, fields map[string]string, st State) (RetireResult, error) {
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
		// Kill in-flight runs before the worktree goes: a detached
		// `amp -x` keeps working in the deleted directory otherwise
		// (see session.StopAmpRuns). Best-effort — a missing process
		// is not an error — and scoped to this instance's stamped
		// identity, so other instances' runs are untouched.
		session.StopAmpRuns(instance, st.Workdir)
	case "codex":
		// Stop in-flight `codex exec` children before the worktree goes;
		// the branch and codex's own rollout files are kept.
		session.StopCodexRuns(instance, st.Workdir)
	case "opencode":
		res.OpencodeSessions = st.OpencodeSessions
	}
	// Re-check cleanliness at apply time: the worktree may have gained
	// uncommitted changes since Inspect ran.
	git := gitRunner(fields["AGENTMUX_RUN_USER"])
	if out, err := git(ctx, st.Workdir, "status", "--porcelain"); err != nil {
		return res, errorf(safesend.ReasonFailed, "checking worktree %s: %v", st.Workdir, err)
	} else if strings.TrimSpace(out) != "" {
		return res, errorf(safesend.ReasonInvalid,
			"worktree %s has uncommitted changes; commit or stash them before retiring", st.Workdir)
	}
	// The managed half runs before the git half: when it fails (most
	// often units or registry the caller can't remove directly), the
	// retire stops before any branch or worktree is touched — only the
	// thread archiving above has happened, and the error names the
	// half to repair.
	managedMsg, merr := removeManaged(ctx, instance)
	_ = managedMsg
	if merr != nil {
		return res, errorf(safesend.ReasonFailed, "ending %s: %v", instance, merr)
	}
	// The op env-file holds references (op://...), never secret values,
	// but it authenticated this instance alone — leaving it behind would
	// let a reused name silently inherit auth. Remove it with the rest.
	_ = os.Remove(opEnvFile(instance, fields))
	// Verify every branch now (a branch that moved on since Inspect must
	// not be deleted), resolve the main checkout while the worktree
	// still exists, remove the worktree, then delete the verified
	// refs: the worktree goes first because git refuses to delete a
	// branch checked out in a live worktree.
	verified, verr := verifyBranches(ctx, git, st)
	if verr != nil {
		return res, verr
	}
	var main string
	if len(verified) > 0 {
		m, merr := mainWorktree(ctx, git, st.Repo)
		if merr != nil {
			return res, errorf(safesend.ReasonFailed, "locating main checkout: %v", merr)
		}
		main = m
	}
	if werr := removeWorktree(ctx, git, st); werr != nil {
		return res, werr
	}
	fates, derr := deleteVerifiedBranches(ctx, git, st, main, verified)
	if derr != nil {
		return res, derr
	}
	res.Branches = fates
	if len(fates) > 0 {
		res.Branch = fates[0].Branch
		for _, f := range fates {
			if f.Deleted {
				res.BranchDeleted = true
			} else if f.Kept != "" && res.BranchKept == "" {
				res.BranchKept = f.Kept
			}
		}
		if !res.BranchDeleted && res.BranchKept == "" {
			res.BranchKept = "kept"
		}
	}
	if st.WorktreeKept != "" {
		res.WorktreeKept = st.WorktreeKept
	}
	return res, nil
}

// gitRunnerFunc runs git in dir as runUser and returns trimmed stdout.
type gitRunnerFunc func(ctx context.Context, dir string, args ...string) (string, error)

// gitRunner returns the git runner for runUser: same-user direct, root
// dropping to the run user via runas, anything else a clear refusal.
// Git and worktree operations must never run as root: a root git rewrote
// the template repo's config and packed-refs root-owned (confirmed live
// on a Linux host), breaking every later unprivileged fetch.
func gitRunner(runUser string) gitRunnerFunc {
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		var cmd *exec.Cmd
		if runUser == "" {
			cmd = runas.CurrentUserCommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		} else {
			cmd = runas.CommandContext(ctx, runUser, "git", append([]string{"-C", dir}, args...)...)
		}
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
}

// branchCandidates lists the branches retire will delete-or-keep: the
// worktree's branch first (when it is a task branch — main/master have
// no fate), then the registry's recorded branch, then any other local
// branch in the same task family. The family covers the AMUX-12 shape:
// the worktree sat on fix/AMUX-12-base-branch while the task branch
// task/AMUX-12-… lived on separately — both get a verdict.
func branchCandidates(ctx context.Context, git gitRunnerFunc, instance string, fields map[string]string, st State) []string {
	var out []string
	seen := map[string]bool{}
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b == "" || b == "main" || b == "master" || seen[b] {
			return
		}
		seen[b] = true
		out = append(out, b)
	}
	if st.Branch != "" && !st.Detached {
		add(st.Branch)
	}
	if recorded := strings.TrimSpace(fields["AGENTMUX_BRANCH"]); recorded != "" {
		add(recorded)
	}
	for _, key := range taskKeys(instance, fields, st) {
		for _, b := range listTaskBranches(ctx, git, st.Repo, key) {
			add(b)
		}
	}
	return out
}

// taskKeys derives task-family keys from the instance name, the worktree
// branch, and the recorded branch: the LETTERS-DIGITS task id each names
// (AMUX-23 in task/AMUX-23-…, amux-23 in task-agentmux-amux-23), matched
// case-insensitively so instance slugs find their branches.
func taskKeys(instance string, fields map[string]string, st State) []string {
	var keys []string
	seen := map[string]bool{}
	add := func(key string) {
		key = strings.ToUpper(strings.TrimSpace(key))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		keys = append(keys, key)
	}
	for _, s := range []string{instance, st.Branch, fields["AGENTMUX_BRANCH"]} {
		add(taskKey(s))
	}
	return keys
}

// taskKey extracts the trailing LETTERS-DIGITS id from a branch or
// instance name, upper-cased: task/AMUX-23-… → AMUX-23,
// task-agentmux-amux-23 → AMUX-23, fix/AMUX-12-base-branch → AMUX-12.
// Empty when none fits.
func taskKey(s string) string {
	s = strings.TrimSuffix(s, "/")
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '/' || r == '-' || r == '_' || r == '.'
	})
	for i := len(parts) - 1; i >= 0; i-- {
		if digits := trailingDigits(parts[i]); digits != "" && i > 0 && isLetters(parts[i-1]) {
			return strings.ToUpper(parts[i-1]) + "-" + digits
		}
	}
	return ""
}

func trailingDigits(s string) string {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return ""
	}
	return s[i:]
}

func isLetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

// shortRef trims a branch short name to its refs/heads form for
// worktree-list comparison.
func shortRef(b string) string {
	return "refs/heads/" + strings.TrimPrefix(b, "refs/heads/")
}

// sameGitPath compares two worktree paths after cleaning.
func sameGitPath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// worktreeBranches maps each checked-out branch ref to its worktree
// path, from `git worktree list --porcelain`. Detached and bare entries
// carry no branch line and are skipped.
func worktreeBranches(ctx context.Context, git gitRunnerFunc, repo string) map[string]string {
	out, err := git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	m := map[string]string{}
	var cur string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch ") && cur != "":
			m[strings.TrimPrefix(line, "branch ")] = cur
		}
	}
	return m
}

// listTaskBranches returns local branches in the task family
// (task/<KEY>-*, case-insensitive): the branches the task may have used
// beyond the worktree's own checkout.
func listTaskBranches(ctx context.Context, git gitRunnerFunc, repo, key string) []string {
	if repo == "" || key == "" {
		return nil
	}
	out, err := git(ctx, repo, "branch", "--format=%(refname:short)", "--list", "task/*")
	if err != nil {
		return nil
	}
	prefix := strings.ToUpper(key) + "-"
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		b := strings.TrimSpace(line)
		short := strings.ToUpper(strings.TrimPrefix(b, "task/"))
		if b == "" || !strings.HasPrefix(short, prefix) {
			continue
		}
		branches = append(branches, b)
	}
	sort.Strings(branches)
	return branches
}

// headOnOrigin reports whether the detached HEAD is on origin's default
// branch: after fetching it, the commit is an ancestor of the fresh
// origin/<default>. Any failure keeps the worktree (false, with the
// upstream when known) — retire removes a detached worktree only on
// positive proof nothing unique would go.
func headOnOrigin(ctx context.Context, git gitRunnerFunc, repo string) (bool, string) {
	def, err := defaultBranchName(ctx, git, repo)
	if err != nil {
		return false, ""
	}
	upstream := "origin/" + def
	if err := fetchOrigin(ctx, git, repo, def); err != nil {
		return false, upstream
	}
	if err := runGitOK(ctx, git, repo, "merge-base", "--is-ancestor", "HEAD", upstream); err == nil {
		return true, upstream
	}
	return false, upstream
}

// defaultBranchName returns the template repo's default-branch short name
// ("main", "master", ...): the local symbolic ref of origin/HEAD, or the
// configured init.defaultBranch, or the first of main/master that exists.
// It never guesses blindly — an unknown default is an error.
func defaultBranchName(ctx context.Context, git gitRunnerFunc, repo string) (string, error) {
	if out, err := git(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimPrefix(strings.TrimSpace(out), "origin/"); name != "" && name != out {
			return name, nil
		}
	}
	if out, err := git(ctx, repo, "config", "--get", "init.defaultBranch"); err == nil {
		if name := strings.TrimSpace(out); name != "" {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if runGitOK(ctx, git, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+name) == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("cannot determine the default branch (no origin/HEAD, no init.defaultBranch, no main or master)")
}

// fetchOrigin updates only the default branch from origin. A full fetch
// would touch every remote-tracking ref; retire needs one ref fresh.
// GIT_TERMINAL_PROMPT=0 is already in the runner's environment, so a network
// outage fails fast instead of prompting.
func fetchOrigin(ctx context.Context, git gitRunnerFunc, repo, def string) error {
	if _, err := git(ctx, repo, "fetch", "origin", def); err != nil {
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
func branchUpstreamEquivalent(ctx context.Context, git gitRunnerFunc, repo, upstream, branch string) (bool, error) {
	out, err := git(ctx, repo, "cherry", upstream, branch)
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
func checkBranchSafe(ctx context.Context, git gitRunnerFunc, repo, branch string) (ok bool, why, upstream string) {
	if branch == "" || branch == "main" || branch == "master" {
		return false, "not a task branch", ""
	}
	def, err := defaultBranchName(ctx, git, repo)
	if err != nil {
		return false, err.Error(), ""
	}
	upstream = "origin/" + def
	if err := fetchOrigin(ctx, git, repo, def); err != nil {
		return false, err.Error(), upstream
	}
	if err := runGitOK(ctx, git, repo, "merge-base", "--is-ancestor", branch, upstream); err == nil {
		return true, "", upstream
	}
	equiv, err := branchUpstreamEquivalent(ctx, git, repo, upstream, branch)
	if err != nil {
		return false, fmt.Sprintf("checking %s against %s: %v", branch, upstream, err), upstream
	}
	if equiv {
		return true, "", upstream
	}
	return false, fmt.Sprintf("branch %s has commits not on %s; merge it before retiring", branch, upstream), upstream
}

// verifyBranches re-verifies every candidate at apply time: a branch
// that moved on since Inspect must not be deleted. It returns the
// branches verified safe; the rest keep their reason.
func verifyBranches(ctx context.Context, git gitRunnerFunc, st State) ([]string, error) {
	var verified []string
	for _, cand := range st.Branches {
		if cand.Kept != "" {
			continue
		}
		ok, _, _ := checkBranchSafe(ctx, git, st.Repo, cand.Branch)
		if ok {
			verified = append(verified, cand.Branch)
		}
	}
	return verified, nil
}

// deleteVerifiedBranches deletes the verified refs from the main
// checkout and builds the final fates: verified-and-deleted, or kept
// with the Inspect reason (plus a branch that moved on since Inspect,
// re-checked above). It runs after the worktree is gone — git refuses
// to delete a branch checked out in a live worktree, and the remove
// used --force precisely so this step can follow from the main
// checkout. main was resolved before the worktree was removed, since
// resolving it afterwards would fail (confirmed live in the e2e test).
func deleteVerifiedBranches(ctx context.Context, git gitRunnerFunc, st State, main string, verified []string) ([]BranchFate, error) {
	safe := map[string]bool{}
	for _, b := range verified {
		safe[b] = true
	}
	fates := make([]BranchFate, 0, len(st.Branches))
	for _, cand := range st.Branches {
		f := BranchFate{Branch: cand.Branch, Upstream: cand.Upstream}
		if !safe[cand.Branch] {
			f.Kept = cand.Kept
			if f.Kept == "" {
				f.Kept = fmt.Sprintf("branch %s moved on since the safety check; merge it before retiring", cand.Branch)
			}
			fates = append(fates, f)
			continue
		}
		if _, err := git(ctx, main, "branch", "-D", cand.Branch); err != nil {
			return nil, errorf(safesend.ReasonFailed, "deleting branch %s: %v", cand.Branch, err)
		}
		f.Deleted = true
		fates = append(fates, f)
	}
	return fates, nil
}

// removeWorktree removes the worktree when nothing would be lost: it is
// clean (re-checked at apply time) and every commit in it is on a kept
// branch or on origin — deleted branches were safe by construction, kept
// branches keep their refs, and a detached HEAD was verified on origin.
// Otherwise the worktree stays with its reason, and the retire still
// completes.
func removeWorktree(ctx context.Context, git gitRunnerFunc, st State) error {
	if st.Workdir == "" || st.WorktreeKept != "" {
		return nil
	}
	if _, err := git(ctx, st.Repo, "worktree", "remove", "--force", st.Workdir); err != nil {
		return errorf(safesend.ReasonFailed, "removing worktree %s: %v", st.Workdir, err)
	}
	return nil
}

// mainWorktree returns the main checkout path: the first entry of
// `git worktree list --porcelain`, which git always prints first. dir may
// be any worktree of the repo (or the repo itself).
func mainWorktree(ctx context.Context, git gitRunnerFunc, dir string) (string, error) {
	out, err := git(ctx, dir, "worktree", "list", "--porcelain")
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

func runGitOK(ctx context.Context, git gitRunnerFunc, dir string, args ...string) error {
	_, err := git(ctx, dir, args...)
	return err
}

// opEnvFile is the instance's 1Password env-file, if any (see
// session/openv.go for the canonical path).
func opEnvFile(instance string, fields map[string]string) string {
	home := runUserHome(fields)
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".agentmux", "env", instance+".env")
}
