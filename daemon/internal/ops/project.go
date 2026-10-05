package ops

import (
	"github.com/m-rk/agentmux/daemon/internal/collab"
	"github.com/m-rk/agentmux/daemon/internal/discordnotify"
	"github.com/m-rk/agentmux/daemon/internal/runas"
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
