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
- **Redundancy** — running more than one backend side by side on the same
  box (different CLIs, different model providers) so an outage or degraded
  provider doesn't take out your only agent, and gives you a choice of
  agent/model for the task at hand.

## Backends

| Backend | Linux | macOS |
|---|---|---|
| [`backends/claude-code`](backends/claude-code) | systemd | LaunchAgents |
| [`backends/opencode-ollama`](backends/opencode-ollama) | systemd | LaunchAgents |

## Quickstart (Claude Code backend)

### macOS

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/claude-code
./install-macos.sh
```

When run from a terminal, the installer prompts for the tmux session name,
Claude display name, update time, and final confirmation. The
default tmux name is `<machine-slug>-claude-YYYY-MM-DD`; the default display
name is `<machine-name> agentmux`. For unattended installs, pass flags
instead:

```sh
./install-macos.sh \
  --tmux-session work-claude \
  --display-name "Work Claude" \
  --update-time 03:00 \
  --yes
```

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
first launch, Claude Code may ask you to trust the dedicated workdir; attach
once and confirm it if prompted.

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

## Quickstart (opencode + Ollama Cloud backend)

### macOS

```sh
# one-time, manual:
brew install tmux ollama
brew services start ollama
ollama signin
npm install -g opencode-ai

git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/opencode-ollama
./install-macos.sh
```

When run from a terminal, the installer prompts for the tmux session name,
Ollama model, update time, and final confirmation. The default tmux name is
`<machine-slug>-opencode-YYYY-MM-DD`. For unattended installs, pass flags
instead:

```sh
./install-macos.sh \
  --tmux-session work-opencode \
  --ollama-model gpt-oss:20b-cloud \
  --update-time 03:00 \
  --yes
```

Use `./install-macos.sh --plan` to preview the LaunchAgents and settings
without writing files. The macOS backend assumes the local Ollama daemon is
already reachable. You can use `brew services start ollama`, the Ollama app,
or a separate `ollama serve` process; agentmux does not own that daemon on
macOS.

To remove the LaunchAgents: `./uninstall-macos.sh` (leaves any running tmux
session and Ollama alone).

### Linux systemd

```sh
# one-time, manual (see backends/opencode-ollama/README.md for why):
curl -fsSL https://ollama.com/install.sh | sh
ollama signin
npm install -g opencode-ai

git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/opencode-ollama
sudo AGENTMUX_SESSION_NAME="my-opencode" ./install.sh
```

Reattach with `tmux attach -t $AGENTMUX_SESSION_NAME`. Unlike the Claude
Code backend there's no remote-control feature yet, so this is SSH +
`tmux attach` only. See
[`backends/opencode-ollama`](backends/opencode-ollama) for details, including
how Ollama Cloud auth flows through to opencode without opencode ever
holding an API key.

## Roadmap

- More backends (Codex CLI, Gemini CLI, whatever comes next) — each one
  running side by side adds to the redundancy/variety this repo is going
  for
- Health-check / notification on failed updates instead of just journal logs
