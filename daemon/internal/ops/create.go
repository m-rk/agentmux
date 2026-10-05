package ops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/m-rk/agentmux/daemon/internal/allowfiles"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// CreateRequest starts a task session on this host: a Git worktree on Branch
// and an instance that works in it, configured like Template.
type CreateRequest struct {
	Template   string   // address of an existing instance on this host
	Instance   string   // name of the new instance
	Branch     string   // branch the worktree is on
	Base       string   // branch on origin a new branch starts from, fetched first; default origin/HEAD's target, else HEAD
	Worktree   string   // directory name under <repo>-worktrees; default Instance
	AllowFiles []string // absolute paths outside the worktree the agent may read and edit
}

// CreateResult is the new (or reused) session plus what Create decided.
type CreateResult struct {
	Session
	Branch string `json:"branch"`
	// Created is false when an instance with that name and workdir already
	// existed and was reused.
	Created bool `json:"created"`
	// Base and BaseCommit are the origin branch and the commit the new
	// worktree started from. Set only when the request had a Base and this
	// call made the branch from it.
	Base       string `json:"base,omitempty"`
	BaseCommit string `json:"base_commit,omitempty"`
}

// Daemon is the part of the daemon client that Create uses.
type Daemon interface {
	ListInstances(ctx context.Context) ([]*pb.Instance, error)
	CreateInstance(ctx context.Context, req *pb.CreateInstanceRequest) (*pb.CreateInstanceResponse, error)
	Close() error
}

// GitFunc runs git in dir and returns its trimmed stdout.
type GitFunc func(ctx context.Context, dir string, args ...string) (string, error)

func (e Env) daemon() (Daemon, error) {
	if e.Dial != nil {
		return e.Dial()
	}
	return e.dial()
}

func (e Env) git(ctx context.Context, dir string, args ...string) (string, error) {
	if e.Git != nil {
		return e.Git(ctx, dir, args...)
	}
	return runGit(ctx, dir, args...)
}

// runGit runs git as the current user, with the PATH fix-ups a service
// needs, and never prompts.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
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

// Create makes a worktree and a new instance on this host from a template
// instance; see docs/design/gateway.md (phase 4b). It doesn't wait for the
// session to be ready. Calling it again with the same arguments reuses what
// exists (Created false); a name or path that is taken for anything else is
// refused as invalid.
func (e Env) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	tmpl, err := parseLocal(req.Template)
	if err != nil {
		return CreateResult{}, err
	}
	if err := provision.ValidateInstanceName(req.Instance); err != nil || strings.HasPrefix(req.Instance, ".") {
		return CreateResult{}, Refuse(safesend.ReasonInvalid, "instance %q: want letters, numbers, dots, underscores and hyphens, not starting with a dot", req.Instance)
	}
	wtName := req.Worktree
	if wtName == "" {
		wtName = req.Instance
	}
	if err := provision.ValidateInstanceName(wtName); err != nil || strings.HasPrefix(wtName, ".") {
		return CreateResult{}, Refuse(safesend.ReasonInvalid, "worktree %q: want one directory name of letters, numbers, dots, underscores and hyphens, not starting with a dot", wtName)
	}
	if req.Branch == "" || strings.HasPrefix(req.Branch, "-") || strings.ContainsAny(req.Branch, "\x00\r\n") {
		return CreateResult{}, Refuse(safesend.ReasonInvalid, "branch %q is not a valid branch name", req.Branch)
	}
	req.Base = strings.TrimPrefix(req.Base, "origin/")
	if req.Base != "" && (strings.HasPrefix(req.Base, "-") || strings.ContainsAny(req.Base, "\x00\r\n")) {
		return CreateResult{}, Refuse(safesend.ReasonInvalid, "base %q is not a valid branch name", req.Base)
	}

	fields, err := session.ReadRegistry(tmpl.Instance)
	if err != nil {
		return CreateResult{}, Refuse(safesend.ReasonNotFound, "no instance %q on this host", tmpl.Instance)
	}
	tmplWorkdir := fields["AGENTMUX_WORKDIR"]
	if tmplWorkdir == "" {
		return CreateResult{}, Refuse(safesend.ReasonUnsupported, "template %s has no workdir", tmpl.Instance)
	}
	if runUser := fields["AGENTMUX_RUN_USER"]; runUser != "" {
		if cur, err := user.Current(); err != nil || cur.Username != runUser {
			return CreateResult{}, Refuse(safesend.ReasonUnsupported, "template %s runs as %s, not the gateway's user", tmpl.Instance, runUser)
		}
	}
	agent := fields["AGENTMUX_AGENT"]
	if agent == "" {
		agent = "claude-code" // claude-code registry entries predate AGENTMUX_AGENT
	}

	toplevel, err := e.git(ctx, tmplWorkdir, "rev-parse", "--show-toplevel")
	if err != nil || toplevel == "" {
		return CreateResult{}, Refuse(safesend.ReasonUnsupported, "template %s workdir %s is not in a Git checkout", tmpl.Instance, tmplWorkdir)
	}
	wtPath := filepath.Join(filepath.Dir(toplevel), filepath.Base(toplevel)+"-worktrees", wtName)

	branch, err := e.git(ctx, toplevel, "check-ref-format", "--branch", req.Branch)
	if err != nil || branch != req.Branch {
		return CreateResult{}, Refuse(safesend.ReasonInvalid, "branch %q is not a valid branch name", req.Branch)
	}
	allow, err := allowfiles.Validate(req.AllowFiles, wtPath)
	if err != nil {
		return CreateResult{}, Refuse(safesend.ReasonInvalid, "%v", err)
	}

	// Look at the instance first, so a name clash is refused before anything
	// is created on disk.
	d, err := e.daemon()
	if err != nil {
		return CreateResult{}, err
	}
	defer d.Close()
	instances, err := d.ListInstances(ctx)
	if err != nil {
		return CreateResult{}, err
	}
	var existing *pb.Instance
	for _, inst := range instances {
		if inst.Name == req.Instance {
			existing = inst
		}
	}
	if existing != nil && !samePath(existing.Workdir, wtPath) {
		return CreateResult{}, Refuse(safesend.ReasonInvalid, "instance %q already exists with workdir %s, not %s", req.Instance, existing.Workdir, wtPath)
	}

	if req.Base != "" {
		if b, err := e.git(ctx, toplevel, "check-ref-format", "--branch", req.Base); err != nil || b != req.Base {
			return CreateResult{}, Refuse(safesend.ReasonInvalid, "base %q is not a valid branch name", req.Base)
		}
	}
	baseCommit, err := e.ensureWorktree(ctx, toplevel, wtPath, req.Branch, req.Base)
	if err != nil {
		return CreateResult{}, err
	}

	created := existing == nil
	if created {
		resp, err := d.CreateInstance(ctx, &pb.CreateInstanceRequest{
			InstanceName:      req.Instance,
			Agent:             agent,
			Provider:          fields["AGENTMUX_PROVIDER"],
			Model:             fields["AGENTMUX_MODEL"],
			Workdir:           wtPath,
			RunUser:           fields["AGENTMUX_RUN_USER"],
			ProviderBaseUrl:   fields["AGENTMUX_PROVIDER_BASE_URL"],
			ProviderApiKeyEnv: fields["AGENTMUX_PROVIDER_API_KEY_ENV"],
			AmpDirs:           fields["AGENTMUX_AMP_DIRS"],
			AmpDiscoverDirs:   fields["AGENTMUX_AMP_DISCOVER_DIRS"] == "1",
			AmpUpdate:         fields["AGENTMUX_AMP_UPDATE"],
			AmpMode:           fields["AGENTMUX_AMP_MODE"],
			AllowFiles:        allow,
		})
		if err != nil {
			return CreateResult{}, err
		}
		if !resp.Ok {
			return CreateResult{}, Refuse(safesend.ReasonFailed, "creating instance %s: %s", req.Instance, resp.Message)
		}
		// An amp template authenticated by an op env-file passes that auth
		// to the new instance: copy the file (references only — it never
		// holds secret values, see session/openv.go) so the task instance
		// launches through `op run` exactly like its template instead of
		// falling back to the stored `amp login`. Non-amp templates have no
		// env-file concept, and a template without one leaves the new
		// instance exactly as before. Copying into place after
		// CreateInstance succeeds keeps a failed creation from leaving a
		// stray env-file behind for an instance that was never made.
		if agent == "amp" {
			if err := copyOpEnvFile(tmpl.Instance, req.Instance); err != nil {
				return CreateResult{}, err
			}
		}
		if instances, err = d.ListInstances(ctx); err != nil {
			return CreateResult{}, err
		}
		existing = nil
		for _, inst := range instances {
			if inst.Name == req.Instance {
				existing = inst
			}
		}
	}
	inst := existing
	if inst == nil { // not discovered yet
		inst = &pb.Instance{Name: req.Instance, Agent: agent, Provider: fields["AGENTMUX_PROVIDER"], Model: fields["AGENTMUX_MODEL"], Workdir: wtPath, Status: pb.Status_STATUS_DEAD}
	}
	sess := SessionFrom(tmpl.Host, inst)
	sess.Project = ProjectOf(inst.Name, inst.Workdir, ProjectKeys())
	res := CreateResult{Session: sess, Branch: req.Branch, Created: created}
	if baseCommit != "" {
		res.Base, res.BaseCommit = req.Base, baseCommit
	}
	return res, nil
}

// copyOpEnvFile copies the template's op env-file
// (~/.agentmux/env/<template>.env) to the new instance's own file, so an
// amp task instance inherits its template's 1Password-backed auth. The
// home is runas.CurrentUserHome — the same directory the instance's
// runner will read its env-file from at launch (see session.opEnvFilePath)
// — which is the run user's home because the gateway runs as the run user
// (see the matching check in Create).
//
// A template without an env-file is a no-op (nil), leaving the new instance
// exactly as before. The copy holds references (op://...), never secret
// values, and is written mode 600 like a hand-made env-file. It never
// overwrites an existing file: with a reused instance name Create reuses
// the instance, and an env-file already there belongs to whoever put it
// there — silently replacing it could revoke auth someone set up by hand.
//
// Removal is manual, by design: there is no instance-removal RPC (see
// docs/amp-secrets.md), so deleting the worktree's instance means removing
// its units/registrations by hand, and the env-file goes with them.
func copyOpEnvFile(template, instance string) error {
	dir := filepath.Join(runas.CurrentUserHome(), ".agentmux", "env")
	src := filepath.Join(dir, template+".env")
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return Refuse(safesend.ReasonFailed, "reading template op env-file %s: %v", src, err)
	}
	dst := filepath.Join(dir, instance+".env")
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return Refuse(safesend.ReasonFailed, "creating instance op env-file %s: %v", dst, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(dst) // don't leave a partial file a retry would keep
		return Refuse(safesend.ReasonFailed, "writing instance op env-file %s: %v", dst, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dst)
		return Refuse(safesend.ReasonFailed, "writing instance op env-file %s: %v", dst, err)
	}
	return nil
}

// ensureWorktree makes wtPath a worktree on branch, or accepts one that
// already is. It returns the commit a new branch was started from when base
// was given, else "".
func (e Env) ensureWorktree(ctx context.Context, repo, wtPath, branch, base string) (string, error) {
	if base == "" {
		// A fetch that fails (offline, no origin) just leaves the start stale.
		fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, _ = e.git(fctx, repo, "fetch", "origin")
		cancel()
	}

	wts, err := e.worktrees(ctx, repo)
	if err != nil {
		return "", Refuse(safesend.ReasonFailed, "%v", err)
	}
	if _, err := os.Lstat(wtPath); err == nil {
		for _, wt := range wts {
			if samePath(wt.path, wtPath) {
				if wt.branch == "refs/heads/"+branch {
					return "", nil
				}
				return "", Refuse(safesend.ReasonInvalid, "worktree %s exists on %s, not branch %s", wtPath, strings.TrimPrefix(wt.branch, "refs/heads/"), branch)
			}
		}
		return "", Refuse(safesend.ReasonInvalid, "%s already exists and is not a worktree of this repository", wtPath)
	}

	if _, err := e.git(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		for _, wt := range wts {
			if wt.branch == "refs/heads/"+branch {
				return "", Refuse(safesend.ReasonInvalid, "branch %s is already checked out at %s", branch, wt.path)
			}
		}
		if _, err := e.git(ctx, repo, "worktree", "add", wtPath, branch); err != nil {
			return "", Refuse(safesend.ReasonFailed, "%v", err)
		}
		return "", nil
	}

	start := ""
	if base != "" {
		// Start from the freshly fetched origin branch, never a local ref.
		tracking := "refs/remotes/origin/" + base
		fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err := e.git(fctx, repo, "fetch", "origin", "+refs/heads/"+base+":"+tracking)
		cancel()
		if err != nil {
			return "", Refuse(safesend.ReasonFailed, "fetching base branch %s from origin: %v", base, err)
		}
		if start, err = e.git(ctx, repo, "rev-parse", "--verify", "--quiet", tracking+"^{commit}"); err != nil || start == "" {
			return "", Refuse(safesend.ReasonFailed, "origin/%s is not a commit in the template's repository", base)
		}
	} else {
		start = "HEAD"
		if ref, err := e.git(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
			start = ref
		}
		if _, err := e.git(ctx, repo, "rev-parse", "--verify", "--quiet", start+"^{commit}"); err != nil {
			return "", Refuse(safesend.ReasonInvalid, "start point %q is not a commit in the template's repository", start)
		}
	}
	if _, err := e.git(ctx, repo, "worktree", "add", "-b", branch, wtPath, start); err != nil {
		return "", Refuse(safesend.ReasonFailed, "%v", err)
	}
	if base == "" {
		return "", nil
	}
	return start, nil
}

type worktree struct{ path, branch string }

// worktrees parses `git worktree list --porcelain`. branch is the full ref,
// empty for a detached or bare entry.
func (e Env) worktrees(ctx context.Context, repo string) ([]worktree, error) {
	out, err := e.git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var wts []worktree
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			wts = append(wts, worktree{path: strings.TrimPrefix(line, "worktree ")})
		case strings.HasPrefix(line, "branch ") && len(wts) > 0:
			wts[len(wts)-1].branch = strings.TrimPrefix(line, "branch ")
		}
	}
	return wts, nil
}

// samePath compares two paths after resolving symlinks where they exist.
func samePath(a, b string) bool {
	return resolvePath(a) == resolvePath(b)
}

func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}
