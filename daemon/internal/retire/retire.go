// Package retire ends a finished task session and, later, deletes what's
// left of it. `agentmux sessions retire <addr>` runs after a task is done
// and merged: it archives the amp thread (or stops the local session),
// removes the instance's units and registry entry, removes its worktree,
// and deletes its branch only when origin's default branch provably
// contains every commit (directly, or as a squash/cherry-pick
// equivalent). `agentmux gc` runs daily:
// for records retired longer ago than retention.yaml allows, it deletes
// the archived amp threads and stored opencode sessions. Claude Code
// transcripts are never deleted — only the registration and units go,
// at retire time.
//
// Only task-* instances created by `sessions create` are ever touched.
// Long-lived agents (mergentic, orchestrator, *-amp templates, …) are
// refused. Retire refuses when the worktree has uncommitted changes or a
// detached HEAD holds commits no branch points at; unmerged branches are
// kept with their reason and reported, never a refusal — the dry run and
// the real retire agree. Only -require-merged restores the old
// refuse-the-whole-retire behavior for unmerged branches.
//
// The privileged half (stop, units, registry) runs through the daemon,
// which owns those paths; git and worktree operations run as the
// instance's run user, never as root. Every external effect (amp CLI,
// sqlite3, systemctl/launchctl, git, tmux) goes through a package-level
// var so tests substitute fakes; production assigns the real runners in
// rm.go's init-adjacent vars.
package retire

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/retention"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
)

// TaskPrefix is the only instance-name prefix retire and gc ever touch.
// `sessions create` names task sessions task-* (see docs/gateway.md), and
// long-lived agents never carry that prefix.
const TaskPrefix = "task-"

// Record is one retired session: what gc may delete, and when. Stored as
// ~/.local/state/agentmux/retired/<instance>.json under the run user's
// home (see state.go's sibling ampRunStateDir in session/amprun.go).
type Record struct {
	// Instance is the retired instance name (task-*).
	Instance string `json:"instance"`
	// Agent is claude-code, opencode, kilo, zero, or amp.
	Agent string `json:"agent"`
	// RetiredAt is when retire ran (UTC); gc counts retention from here.
	RetiredAt time.Time `json:"retired_at"`
	// AmpThreads are the amp thread ids to delete at gc time; amp only.
	AmpThreads []string `json:"amp_threads,omitempty"`
	// OpencodeSessions are the stored opencode session ids to delete at
	// gc time; opencode only. Empty means "every session whose directory
	// is the workdir" is resolved fresh at gc time from the database —
	// the list is recorded when known at retire time.
	OpencodeSessions []string `json:"opencode_sessions,omitempty"`
	// Workdir is the removed worktree path, kept for the report.
	Workdir string `json:"workdir,omitempty"`
	// Branch is the deleted branch, kept for the report.
	Branch string `json:"branch,omitempty"`
	// RunUser is the instance's run user (AGENTMUX_RUN_USER): gc-time
	// deletions run as this user. Records written before the field
	// existed fall back to the current user.
	RunUser string `json:"run_user,omitempty"`
}

// RetireResult is what retire did, for the CLI and the agent log.
type RetireResult struct {
	Instance string `json:"instance"`
	Agent    string `json:"agent"`
	// AmpThreads are the archived amp thread ids; amp only.
	AmpThreads []string `json:"amp_threads,omitempty"`
	// OpencodeSessions lists the stored sessions gc will delete; opencode only.
	OpencodeSessions []string `json:"opencode_sessions,omitempty"`
	// Branch is the worktree's branch (the first fate); BranchDeleted
	// reports at least one branch was deleted because origin contained
	// it, and BranchKept explains the first kept branch. Branches holds
	// every branch's own verdict.
	Branch        string       `json:"branch,omitempty"`
	BranchDeleted bool         `json:"branch_deleted,omitempty"`
	BranchKept    string       `json:"branch_kept,omitempty"`
	Branches      []BranchFate `json:"branches,omitempty"`
	Workdir       string       `json:"workdir,omitempty"`
	// WorktreeKept says why the worktree stays, when it does.
	WorktreeKept string `json:"worktree_kept,omitempty"`
	RetiredAt    string `json:"retired_at,omitempty"`
	// DryRun is set when nothing was changed.
	DryRun bool `json:"dry_run,omitempty"`
	// Plan lists what would happen, dry-run only.
	Plan []string `json:"plan,omitempty"`
}

// GCResult is what gc did, for the CLI and the agent log.
type GCResult struct {
	// RetentionDays is the retention that applied.
	RetentionDays int `json:"retention_days"`
	// Deleted lists retired instances whose leftovers were deleted.
	Deleted []GCDeleted `json:"deleted,omitempty"`
	// Kept lists retired instances still inside retention.
	Kept []GCKept `json:"kept,omitempty"`
	// DryRun is set when nothing was changed.
	DryRun bool `json:"dry_run,omitempty"`
}

// GCDeleted is one gc deletion.
type GCDeleted struct {
	Instance string `json:"instance"`
	Agent    string `json:"agent"`
	// AmpThreads / OpencodeSessions are what was deleted.
	AmpThreads       []string `json:"amp_threads,omitempty"`
	OpencodeSessions []string `json:"opencode_sessions,omitempty"`
	RetiredAt        string   `json:"retired_at,omitempty"`
}

// GCKept is one retired instance still inside retention.
type GCKept struct {
	Instance  string `json:"instance"`
	RetiredAt string `json:"retired_at,omitempty"`
	DeleteAt  string `json:"delete_at,omitempty"`
}

// CheckResult is one safety check retire runs before changing anything.
type CheckResult struct {
	OK     bool
	Reason safesend.Reason
	Detail string
}

// errorf builds a refusal with a stable reason.
func errorf(r safesend.Reason, format string, a ...any) error {
	return &retireError{Reason: r, Detail: fmt.Sprintf(format, a...)}
}

type retireError struct {
	Reason safesend.Reason
	Detail string
}

func (e *retireError) Error() string { return string(e.Reason) + ": " + e.Detail }

// ReasonOf turns any retire error into its stable reason; unknown errors
// are ReasonFailed.
func ReasonOf(err error) safesend.Reason {
	if e, ok := err.(*retireError); ok {
		return e.Reason
	}
	return safesend.ReasonFailed
}

// DetailOf is the human-readable part of a retire error.
func DetailOf(err error) string {
	if e, ok := err.(*retireError); ok {
		return e.Detail
	}
	return err.Error()
}

// guardTask refuses anything that isn't a task-* instance.
func guardTask(instance string) error {
	if !strings.HasPrefix(instance, TaskPrefix) {
		return errorf(safesend.ReasonForbidden,
			"not a task session: %q does not start with %q; retire only touches task-* agents created by `sessions create`",
			instance, TaskPrefix)
	}
	return nil
}

// stateDir returns ~/.local/state/agentmux/retired for home.
func stateDir(home string) string {
	return filepath.Join(home, ".local", "state", "agentmux", "retired")
}

// recordPath is the record file for one instance.
func recordPath(home, instance string) string {
	return filepath.Join(stateDir(home), instance+".json")
}

// loadRecord reads one retired-session record; os.IsNotExist means never
// retired (or already gc'd).
func loadRecord(home, instance string) (Record, error) {
	var rec Record
	data, err := os.ReadFile(recordPath(home, instance))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("parsing retired record for %s: %w", instance, err)
	}
	return rec, nil
}

// saveRecord writes one retired-session record atomically (temp file and
// rename, 0600 in a 0700 directory), like the amp thread-runner cache.
func saveRecord(home string, rec Record) error {
	if home == "" {
		return fmt.Errorf("no home directory for retired record")
	}
	dir := stateDir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".retired-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(f.Name(), 0o600) != nil || os.Rename(f.Name(), recordPath(home, rec.Instance)) != nil {
		os.Remove(f.Name())
		return fmt.Errorf("writing retired record for %s", rec.Instance)
	}
	return nil
}

// saveRecordAs writes one retired-session record like saveRecord, then
// hands ownership to runUser when the caller is root: `sessions retire`
// under sudo must not leave root-owned files in the run user's state
// dir — the same root-owns-user-files shape as the git issue, one
// directory over (confirmed live: a sudo retire left retired/ itself
// root-owned, blocking every later unprivileged record write).
func saveRecordAs(home, runUser string, rec Record) error {
	if err := saveRecord(home, rec); err != nil {
		return err
	}
	if os.Geteuid() != 0 || runUser == "" {
		return nil
	}
	u, err := lookupUser(runUser)
	if err != nil {
		return nil // record is written; ownership healing is best-effort
	}
	uid, gid, uerr := atoiUIDGID(u)
	if uerr != nil {
		return nil
	}
	// The directory first: saveRecord's MkdirAll may have created it
	// root-owned, and an older sudo retire may have left it that way.
	_ = os.Chown(stateDir(home), uid, gid)
	_ = os.Chown(recordPath(home, rec.Instance), uid, gid)
	return nil
}

// atoiUIDGID parses a looked-up user's numeric ids for chown.
func atoiUIDGID(u *user.User) (uid, gid int, err error) {
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, err
	}
	if gid, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// listRecords returns every retired-session record under home.
func listRecords(home string) ([]Record, error) {
	entries, err := os.ReadDir(stateDir(home))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, err := loadRecord(home, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// due reports whether rec is older than retentionDays.
func due(rec Record, retentionDays int, now time.Time) bool {
	return now.Sub(rec.RetiredAt) >= time.Duration(retentionDays)*24*time.Hour
}

// retentionDays loads the host retention, defaulting on any problem the
// same way ampconfig does: a broken host file is caller-visible only when
// it would matter, and gc always has a sane number to enforce.
func retentionDays(path string) int {
	cfg, err := retention.Load(path)
	if err != nil {
		return retention.DefaultDays
	}
	return cfg.RetentionDays
}

// Retire ends one finished task session. See the package doc comment for
// the per-agent behavior. ctx bounds the whole operation. A real retire
// retires the agent and worktree and keeps unmerged branches with their
// reason — the dry run already said so. Only RequireMerged refuses the
// whole retire for unmerged branches, so the caller can raise an ask.
func Retire(ctx context.Context, env Env, instance string, opts Options) (RetireResult, error) {
	if err := guardTask(instance); err != nil {
		return RetireResult{}, err
	}
	fields, err := env.ReadRegistry(instance)
	if err != nil {
		return RetireResult{}, errorf(safesend.ReasonNotFound, "no instance %q on this host", instance)
	}
	agent := fields["AGENTMUX_AGENT"]
	if agent == "" {
		agent = "claude-code" // registry entries predate AGENTMUX_AGENT
	}
	workdir := fields["AGENTMUX_WORKDIR"]
	home := env.Home(instance, fields)

	st, err := env.Inspect(ctx, instance, fields)
	if err != nil {
		return RetireResult{}, err
	}
	plan := st.Plan(agent)
	if opts.DryRun {
		return RetireResult{Instance: instance, Agent: agent, Workdir: workdir,
			AmpThreads: st.AmpThreads, OpencodeSessions: st.OpencodeSessions,
			Branch: st.Branch, Branches: inspectFates(st),
			DryRun: true, Plan: plan}, nil
	}
	if opts.RequireMerged {
		for _, f := range st.Branches {
			if !f.Deleted {
				why := f.Kept
				if why == "" {
					why = fmt.Sprintf("branch %s was not verified safe to delete; merge it before retiring", f.Branch)
				}
				return RetireResult{}, errorf(safesend.ReasonInvalid, "%s", why)
			}
		}
	}
	res, err := env.Apply(ctx, instance, agent, fields, st)
	if err != nil {
		return RetireResult{}, err
	}
	res.Instance, res.Agent = instance, agent
	now := env.Now().UTC()
	res.RetiredAt = now.Format(time.RFC3339)
	if err := saveRecordAs(home, fields["AGENTMUX_RUN_USER"], Record{
		Instance: instance, Agent: agent, RetiredAt: now,
		AmpThreads: st.AmpThreads, OpencodeSessions: st.OpencodeSessions,
		Workdir: workdir, Branch: branchName(res),
		RunUser: fields["AGENTMUX_RUN_USER"],
	}); err != nil {
		return RetireResult{}, err
	}
	return res, nil
}

// inspectFates copies Inspect's per-branch verdicts into the dry-run
// result: verified-safe branches read deleted (they would be),
// otherwise kept with their reason — the same lines the plan reports.
func inspectFates(st State) []BranchFate {
	fates := make([]BranchFate, 0, len(st.Branches))
	return append(fates, st.Branches...)
}

func branchName(res RetireResult) string {
	if res.BranchDeleted {
		return res.Branch
	}
	return ""
}

// GC deletes the leftovers of retired sessions older than the host
// retention. ctx bounds the whole operation.
func GC(ctx context.Context, env Env, dryRun bool, now time.Time) (GCResult, error) {
	days := retentionDays(env.RetentionPath())
	res := GCResult{RetentionDays: days, DryRun: dryRun}
	recs, err := listRecords(env.GCHome())
	if err != nil {
		return GCResult{}, err
	}
	for _, rec := range recs {
		if err := guardTask(rec.Instance); err != nil {
			continue // never touch a record that isn't task-* shaped
		}
		if !due(rec, days, now) {
			res.Kept = append(res.Kept, GCKept{Instance: rec.Instance,
				RetiredAt: rec.RetiredAt.Format(time.RFC3339),
				DeleteAt:  rec.RetiredAt.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)})
			continue
		}
		if dryRun {
			res.Deleted = append(res.Deleted, deletedOf(rec))
			continue
		}
		del, err := env.DeleteLeftovers(ctx, rec)
		if err != nil {
			return GCResult{}, err
		}
		if err := os.Remove(recordPath(env.GCHome(), rec.Instance)); err != nil && !os.IsNotExist(err) {
			return GCResult{}, err
		}
		res.Deleted = append(res.Deleted, del)
	}
	return res, nil
}

func deletedOf(rec Record) GCDeleted {
	return GCDeleted{Instance: rec.Instance, Agent: rec.Agent,
		AmpThreads: rec.AmpThreads, OpencodeSessions: rec.OpencodeSessions,
		RetiredAt: rec.RetiredAt.Format(time.RFC3339)}
}
