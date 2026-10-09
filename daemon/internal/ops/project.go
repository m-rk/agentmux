package ops

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/m-rk/agentmux/daemon/internal/collab"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/pb"
	"github.com/m-rk/agentmux/daemon/internal/provision"
	"github.com/m-rk/agentmux/daemon/internal/runas"
	"github.com/m-rk/agentmux/daemon/internal/safesend"
	"github.com/m-rk/agentmux/daemon/internal/session"
)

// ProjectKeys returns the collab config's per-instance project overrides, or
// nil when there is no readable config.
func ProjectKeys() map[string]string {
	path := discordnotify.DefaultPath()
	if path == "" {
		return nil
	}
	cfg, err := discordnotify.Load(path)
	if err != nil {
		return nil
	}
	return cfg.Collaboration.ProjectKeys
}

// RunnerAddRequest creates another agent instance for the source instance's
// project, workdir and run user. DryRun reports the operation without writes.
type RunnerAddRequest struct {
	From, Agent, Name string
	DryRun            bool
}
type RunnerAddResult struct {
	Name    string   `json:"name"`
	Agent   string   `json:"agent"`
	Project string   `json:"project"`
	Workdir string   `json:"workdir"`
	Created bool     `json:"created"`
	DryRun  bool     `json:"dry_run,omitempty"`
	Plan    []string `json:"plan,omitempty"`
}

// AddRunner provisions an instance from an existing project's instance.
func (e Env) AddRunner(ctx context.Context, req RunnerAddRequest) (RunnerAddResult, error) {
	from, err := parseLocal(req.From)
	if err != nil {
		return RunnerAddResult{}, err
	}
	fields, err := session.ReadRegistry(from.Instance)
	if err != nil {
		return RunnerAddResult{}, Refuse(safesend.ReasonNotFound, "no instance %q on this host", from.Instance)
	}
	if req.Agent == "" {
		req.Agent = "codex"
	}
	if req.Agent != "codex" {
		return RunnerAddResult{}, Refuse(safesend.ReasonUnsupported, "runner add currently supports codex only")
	}
	name := req.Name
	if name == "" {
		name = filepath.Base(fields["AGENTMUX_WORKDIR"]) + "-codex"
	}
	if err := provision.ValidateInstanceName(name); err != nil || strings.HasPrefix(name, ".") {
		return RunnerAddResult{}, Refuse(safesend.ReasonInvalid, "invalid instance name %q", name)
	}
	project := ProjectOf(from.Instance, fields["AGENTMUX_WORKDIR"], ProjectKeys())
	if project == "" {
		return RunnerAddResult{}, Refuse(safesend.ReasonUnsupported, "source instance has no detectable project")
	}
	d, err := e.daemon()
	if err != nil {
		return RunnerAddResult{}, err
	}
	defer d.Close()
	instances, err := d.ListInstances(ctx)
	if err != nil {
		return RunnerAddResult{}, err
	}
	for _, inst := range instances {
		f, ferr := session.ReadRegistry(inst.Name)
		if ferr != nil {
			continue
		}
		if ProjectOf(inst.Name, f["AGENTMUX_WORKDIR"], ProjectKeys()) == project && f["AGENTMUX_AGENT"] == req.Agent {
			return RunnerAddResult{Name: inst.Name, Agent: req.Agent, Project: project, Workdir: f["AGENTMUX_WORKDIR"], Created: false, DryRun: req.DryRun, Plan: []string{"reuse existing " + inst.Name}}, nil
		}
	}
	runUser := fields["AGENTMUX_RUN_USER"]
	if runUser == "" {
		return RunnerAddResult{}, Refuse(safesend.ReasonUnsupported, "source instance has no run user")
	}
	workdir := fields["AGENTMUX_WORKDIR"]
	if workdir == "" {
		return RunnerAddResult{}, Refuse(safesend.ReasonUnsupported, "source instance has no workdir")
	}
	plan := []string{fmt.Sprintf("create %s (codex) in %s as %s", name, workdir, runUser)}
	dirs := session.CodexAddDirs(fields[session.CodexAddDirsKey])
	for _, d := range defaultCodexAddDirs() {
		dirs = appendUnique(dirs, d)
	}
	dirs = append(dirs, e.codexGitDirs(ctx, workdir, req.DryRun)...)
	if len(dirs) > 0 {
		plan = append(plan, fmt.Sprintf("grant %d configured and Git directories to Codex", len(dirs)))
	}
	if req.DryRun {
		return RunnerAddResult{Name: name, Agent: req.Agent, Project: project, Workdir: workdir, DryRun: true, Plan: plan}, nil
	}
	resp, err := d.CreateInstance(ctx, &pb.CreateInstanceRequest{InstanceName: name, Agent: "codex", Workdir: workdir, RunUser: runUser})
	if err != nil {
		return RunnerAddResult{}, err
	}
	if !resp.Ok {
		return RunnerAddResult{}, Refuse(safesend.ReasonFailed, "%s", resp.Message)
	}
	if len(dirs) > 0 {
		if err := session.SetRegistryField(name, session.CodexAddDirsKey, strings.Join(dirs, ",")); err != nil {
			return RunnerAddResult{}, err
		}
	}
	if err := session.SetRegistryField(name, "AGENTMUX_PROJECT", project); err != nil {
		return RunnerAddResult{}, fmt.Errorf("recording project for %s: %w", name, err)
	}
	return RunnerAddResult{Name: name, Agent: req.Agent, Project: project, Workdir: workdir, Created: true}, nil
}

// ProjectOf is the project of the instance name with the given workdir, as
// collab.DetectProject finds it: the collab project key for name (keys, from
// ProjectKeys), else the registry's AGENTMUX_PROJECT, else the workdir's Git
// origin. It runs at most one git call and returns "" on any error. Only
// meaningful on the instance's own host.
func ProjectOf(name, workdir string, keys map[string]string) string {
	override := keys[name]
	if override == "" {
		if fields, err := session.ReadRegistry(name); err == nil {
			override = fields["AGENTMUX_PROJECT"]
		}
	}
	if workdir == "" && override == "" {
		return ""
	}
	project, err := collab.DetectProject(workdir, override, runas.CurrentUserCommand)
	if err != nil {
		return ""
	}
	return project
}
