# agentmux

Agents that are remote controlled, persistent, redundant and self-maintained.

The idea: coding-agent CLIs (Claude Code, [opencode](https://opencode.ai),
[Zero](https://github.com/Gitlawb/zero), and whatever comes next) are most
useful when there's always a live
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
- **Redundancy** — running more than one backend side by side on the same
  box (different CLIs, different model providers) so an outage or degraded
  provider doesn't take out your only agent, and gives you a choice of
  agent/model for the task at hand.

## Backends

| Backend | Linux | macOS |
|---|---|---|
| [`backends/agentmux`](backends/agentmux) | systemd | LaunchAgents |
| [`backends/claude-code`](backends/claude-code) | systemd | LaunchAgents |

`backends/agentmux` is the direction of travel: one named instance combines an
agent CLI, a model provider, a model, a workdir, and host supervisor wiring.
Provider-specific backends like `opencode-ollama` and `zero-ollama` are avoided
so new agents/providers/models can be mixed without cloning whole directories.

## Quickstart (configurable backend)

### macOS

```sh
# one-time, manual:
brew install tmux ollama
brew services start ollama
ollama signin
npm install -g @gitlawb/zero

git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/agentmux
./install-macos.sh \
  --instance work-zero \
  --agent zero \
  --provider ollama \
  --model gpt-oss:20b-cloud \
  --yes
```

This creates `com.agentmux.work-zero` and
`com.agentmux.work-zero.update` LaunchAgents, plus a dedicated workdir at
`~/.agentmux/work-zero`. Reattach with:

```sh
tmux attach -t work-zero
```

Use another instance name, agent, model, or workdir to run multiple agentmux
instances side by side on the same machine.

### Linux systemd

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/agentmux
sudo ./install.sh \
  --instance work-zero \
  --agent zero \
  --provider ollama \
  --model gpt-oss:20b-cloud
```

See [`backends/agentmux`](backends/agentmux) for supported agent/provider
combinations and all install flags.

## Tests

Run the lightweight regression harness:

```sh
tests/smoke.sh
```

By default it uses fake local tools for provider/agent checks, so it does not
need a running model provider. To include a real Ollama + Zero generation smoke:

```sh
AGENTMUX_LIVE_OLLAMA=1 tests/smoke.sh
```

To include a real Ollama + opencode generation smoke:

```sh
AGENTMUX_LIVE_OPENCODE=1 tests/smoke.sh
```

## Quickstart (Claude Code backend)

### macOS

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/claude-code
./install-macos.sh
```

When run from a terminal, the installer prompts for the tmux session name,
Claude display name, update time, final confirmation, and whether to attach
to the tmux session immediately. The default tmux name is
`<machine-slug>-claude-YYYY-MM-DD`; the default display name is
`<user>:<host> 🤹 <workdir-basename>`. For unattended installs, pass flags
instead:

```sh
./install-macos.sh \
  --tmux-session work-claude \
  --display-name "Work Claude" \
  --update-time 03:00 \
  --yes
```

Add `--attach` to attach immediately after installing, which is useful on
first run so you can complete Claude Code login and trust prompts before
leaving the session detached.

Use `./install-macos.sh --plan` to preview the LaunchAgents and settings
without writing files. A normal install creates two user LaunchAgents,
without `sudo`:

- `com.agentmux.claude-code` runs `rc-start.sh` at login and every five
  minutes by default, creating the tmux session if it is missing.
- `com.agentmux.claude-code.update` runs nightly at 03:00 local time by
  default, updates Claude Code, and restarts the tmux session only when the
  version changed.

Logs go to `~/Library/Logs/agentmux`. Reattach with the configured tmux
session name, or from the Claude Code mobile app via Remote Control. On
first launch, Claude Code may ask you to log in or trust the dedicated
workdir; let the installer attach after installing, or attach once and
complete those prompts if needed.

Pass `--instance NAME` (default: `claude-code`) to install a second, third,
... instance side by side, each with its own workdir, tmux session, and
LaunchAgent/systemd names — see
[`backends/claude-code`](backends/claude-code#multiple-instances).

To remove the LaunchAgents: `./uninstall-macos.sh` (leaves any running tmux
session alone).

### Linux systemd

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/claude-code
sudo AGENTMUX_SESSION_NAME="my-session" \
     AGENTMUX_ON_CALENDAR="*-*-* 03:00:00 Australia/Perth" \
     ./install.sh
```

(`install.sh` also accepts flags, e.g. `--session-name`/`--on-calendar`, and
defaults the Remote Control display name to `<user>:<host> 🤹 <workdir-basename>`
— see [`backends/claude-code`](backends/claude-code) for the full list.)

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

See [`backends/claude-code`](backends/claude-code) for the scripts,
LaunchAgent templates, and systemd unit templates.

## Daemon + TUI

[`daemon/`](daemon) has an in-progress `agentmuxd` (per-host daemon) +
`agentmux` (TUI client) pair for visualizing and controlling instances —
today on one host over a Unix socket, with cross-device visualization over
Tailscale planned next. See
[`docs/design/daemon-tui.md`](docs/design/daemon-tui.md) for the design and
[`daemon/README.md`](daemon/README.md) to build and run it.

## Roadmap

- More backends (Codex CLI, Gemini CLI, whatever comes next) — each one
  running side by side adds to the redundancy/variety this repo is going
  for
- Health-check / notification on failed updates instead of just journal logs
- Cross-device visualization/control via the `daemon/` TUI (see above)
