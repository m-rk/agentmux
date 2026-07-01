# Claude Code backend

A persistent Claude Code session with Remote Control enabled. `rc-start.sh`
creates a detached `tmux` session running `claude --remote-control` in
`~/.agentmux/claude-code`; `rc-update.sh` updates the CLI and restarts the
session only when the version changed.

## macOS

Run as your macOS user, not with `sudo`:

```sh
cd backends/claude-code
./install-macos.sh
```

When run from a terminal, the installer prompts for the tmux session name,
Claude display name, update time, final confirmation, and whether to attach
to the tmux session immediately. The generated default tmux name is
`<machine-slug>-claude-YYYY-MM-DD`; the generated default display name is
`<machine-name> agentmux`.

For unattended installs, pass flags instead:

```sh
./install-macos.sh \
  --tmux-session work-claude \
  --display-name "Work Claude" \
  --update-time 03:00 \
  --yes
```

Add `--attach` to attach immediately after installing, which is useful on
first run so you can complete Claude Code login and trust prompts before
leaving the session detached. Use `--no-attach` to make that explicit in
scripts.

Use `./install-macos.sh --plan` to preview the LaunchAgents and settings
without writing files.

This writes:

- `~/Library/LaunchAgents/com.agentmux.claude-code.plist`
- `~/Library/LaunchAgents/com.agentmux.claude-code.update.plist`

The start LaunchAgent runs at login and then every five minutes by default.
The update LaunchAgent runs at 03:00 local time by default. Logs are written
under `~/Library/Logs/agentmux`.

On first launch, Claude Code may stop at login or at its workspace trust
prompt for the dedicated workdir. If you let the installer attach after
installing, complete those prompts there. Otherwise attach once with
`tmux attach -t <tmux-session-name>` and finish login/trust for
`~/.agentmux/claude-code`; later restarts use that same workdir.

Useful overrides:

```sh
AGENTMUX_TMUX_SESSION_NAME="work-claude" \
AGENTMUX_DISPLAY_NAME="Work Claude" \
AGENTMUX_WORKDIR="$HOME/.agentmux/claude-code" \
AGENTMUX_UPDATE_TIME=03:00 \
AGENTMUX_START_INTERVAL=300 \
./install-macos.sh --yes
```

Reattach:

```sh
tmux attach -t "$AGENTMUX_TMUX_SESSION_NAME"
```

Uninstall:

```sh
./uninstall-macos.sh
```

Uninstalling removes the LaunchAgents but leaves any running tmux session
and `~/.agentmux/claude-code` alone.

## Linux systemd

Run with `sudo`:

```sh
cd backends/claude-code
sudo AGENTMUX_SESSION_NAME="my-session" \
     AGENTMUX_ON_CALENDAR="*-*-* 03:00:00 Australia/Perth" \
     ./install.sh
```

This writes:

- `/etc/systemd/system/agentmux-claude-code.service`
- `/etc/systemd/system/agentmux-claude-code-update.service`
- `/etc/systemd/system/agentmux-claude-code-update.timer`
- `/etc/agentmux/claude-code.env`

Useful overrides:

```sh
AGENTMUX_SESSION_NAME="my-session"   # tmux session / Remote Control display name
AGENTMUX_RUN_USER="runner"           # defaults to SUDO_USER
AGENTMUX_ON_CALENDAR="*-*-* 03:00:00 UTC"
```

Uninstall:

```sh
sudo ./uninstall.sh
```

Uninstalling removes the systemd units and env file but leaves any running
tmux session and `~/.agentmux/claude-code` alone.
