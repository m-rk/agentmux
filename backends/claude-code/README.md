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
`<machine-name> agentmux`, and `" agentmux"` is appended to any custom
display name too (flag, env var, or typed at the prompt) unless you pass
`--no-suffix`.

For unattended installs, pass flags instead:

```sh
./install-macos.sh \
  --tmux-session work-claude \
  --display-name "Work Claude" \
  --update-time 03:00 \
  --yes
```

This installs with display name `Work Claude agentmux`; add `--no-suffix`
if you want exactly `Work Claude` with no suffix.

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

Run with `sudo`. Configure via flags or env vars — flags win over env vars,
mirroring `install-macos.sh`:

```sh
cd backends/claude-code
sudo ./install.sh \
  --session-name my-session \
  --on-calendar "*-*-* 03:00:00 Australia/Perth"
```

The default display name is `<machine-name> agentmux`, and `" agentmux"` is
appended to any custom `--display-name`/`AGENTMUX_DISPLAY_NAME` too, unless
you pass `--no-suffix`. Use `./install.sh --plan` (no `sudo` required) to
preview the resolved values without writing anything.

This writes:

- `/etc/systemd/system/agentmux-claude-code.service`
- `/etc/systemd/system/agentmux-claude-code-update.service`
- `/etc/systemd/system/agentmux-claude-code-update.timer`
- `/etc/agentmux/claude-code.env`

Flags (`./install.sh --help` for the full list):

```
--session-name NAME   tmux session name (also: --tmux-session)
--display-name NAME   Remote Control display name (also: --remote-name)
--no-suffix           don't append " agentmux" to the display name
--run-user USER       user the session runs as (default: $SUDO_USER)
--on-calendar EXPR    systemd OnCalendar expression for the update timer
--plan                print the install plan without writing files
```

Equivalent env vars, for automation:

```sh
AGENTMUX_SESSION_NAME="my-session"       # also: AGENTMUX_TMUX_SESSION_NAME
AGENTMUX_DISPLAY_NAME="My Session"       # also: AGENTMUX_REMOTE_NAME
AGENTMUX_DISPLAY_SUFFIX=0                # 0/false/no/off disables the suffix
AGENTMUX_RUN_USER="runner"               # defaults to SUDO_USER
AGENTMUX_ON_CALENDAR="*-*-* 03:00:00 UTC"
```

`install.sh` is safe to re-run to change these values, but note it only
rewrites the systemd units and env file — it does not restart an already
running session, so `sudo systemctl restart agentmux-claude-code.service`
afterwards to pick up the change.

### SELinux (RHEL, CentOS, Oracle Linux, Fedora)

On SELinux-enforcing hosts, systemd's `init_t` service context cannot
directly execute a script labeled `user_home_t` (like `rc-start.sh`, sitting
under a user's home directory) or a binary labeled `screen_exec_t` (like
`tmux`). Both show up as the service failing with `status=203/EXEC` and an
AVC `denied { execute }` in `ausearch -m avc`. `install.sh` avoids this by
routing `ExecStart`/`ExecStop` through `/bin/bash` rather than exec'ing
those paths directly, so no extra steps should be needed — this note is
here in case a similar denial ever resurfaces from a local edit.

Uninstall:

```sh
sudo ./uninstall.sh
```

Uninstalling removes the systemd units and env file but leaves any running
tmux session and `~/.agentmux/claude-code` alone.
