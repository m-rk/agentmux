# agentmux

Agents that are remote controlled, persistent, redundant and self-maintained.

The idea: coding-agent CLIs (Claude Code, [opencode](https://opencode.ai) +
ollama, and whatever comes next) are most useful when there's always a live
session you can drop into from anywhere — not just while a terminal happens
to be open. agentmux keeps one running per backend, brings it back after a
reboot, and keeps the CLI itself up to date without you babysitting it.

Four properties every backend here aims for:

- **Persistence** — the session lives in `tmux`, detached, so SSH drops and
  network blips don't kill it.
- **Remote access** — reattach from anywhere (`tmux attach`, or a backend's
  own remote-control feature if it has one).
- **Self-maintenance** — a scheduled job updates the CLI and restarts the
  session only when the version actually changed, so it doesn't go stale.
- **Redundancy** — (roadmap) running more than one instance/host so a single
  box going down doesn't take out your only point of access.

## Backends

| Backend | Status |
|---|---|
| [`backends/claude-code`](backends/claude-code) | Working |
| [`backends/opencode-ollama`](backends/opencode-ollama) | Planned |

## Quickstart (Claude Code backend)

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/claude-code
sudo AGENTMUX_SESSION_NAME="my-session" \
     AGENTMUX_ON_CALENDAR="*-*-* 03:00:00 Australia/Perth" \
     ./install.sh
```

This sets up two systemd units (running as whichever user invoked `sudo`,
override with `AGENTMUX_RUN_USER`):

- `agentmux-claude-code.service` — starts (and restarts, on boot) a `tmux`
  session named `$AGENTMUX_SESSION_NAME` running `claude --remote-control`
  in `~/.agentmux/claude-code`.
- `agentmux-claude-code-update.timer` — nightly (default 3am UTC, override
  with `AGENTMUX_ON_CALENDAR`) checks for a new Claude Code version, and
  only restarts the session if one was installed.

Reattach any time with `tmux attach -t $AGENTMUX_SESSION_NAME`, or from the
Claude Code mobile app via Remote Control (the session shows up under
`$AGENTMUX_SESSION_NAME`).

To remove: `sudo ./uninstall.sh` (leaves any running tmux session alone).

See [`backends/claude-code`](backends/claude-code) for the scripts and unit
templates.

## Roadmap

- opencode + ollama backend
- Multi-instance / multi-host redundancy (systemd template units, one
  `install.sh` invocation per instance)
- Health-check / notification on failed updates instead of just journal logs
