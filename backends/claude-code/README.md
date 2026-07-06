# Claude Code backend

A persistent Claude Code session with Remote Control enabled. `rc-start.sh`
creates a detached `tmux` session running `claude --remote-control` in
`~/.agentmux/claude-code`; `rc-update.sh` updates the CLI and restarts the
session only when the version changed.

Each install is a named **instance** (`--instance NAME` /
`AGENTMUX_INSTANCE_NAME`, default `claude-code`), so a second, third, ...
instance can run side by side on the same machine with its own workdir,
tmux session, LaunchAgent/systemd names, and logs. See
[Multiple Instances](#multiple-instances) below. The default instance name
(`claude-code`) reproduces every identifier documented in this file exactly,
so existing installs are unaffected.

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
`🤹 <user>:<host> <workdir-basename>` (e.g. `🤹 mark:Harley Mini claude-code`)
— already self-identifying as an agentmux session, so no suffix is added to
it. `" agentmux"` is still appended to any custom display name (flag, env
var, or typed at the prompt) unless you pass `--no-suffix`.

For unattended installs, pass flags instead:

```sh
./install-macos.sh \
  --tmux-session work-claude \
  --display-name "Work Claude" \
  --update-time 03:00 \
  --yes
```

Add `--instance NAME` (default: `claude-code`) to install a second instance
side by side — see [Multiple Instances](#multiple-instances).

This installs with display name `Work Claude agentmux`; add `--no-suffix`
if you want exactly `Work Claude` with no suffix.

Add `--attach` to attach immediately after installing. Use `--no-attach` to
make that explicit in scripts.

Use `./install-macos.sh --plan` to preview the LaunchAgents and settings
without writing files.

The installer requires Claude Code to already be logged in — it checks via
`claude auth status` (falling back to the legacy `~/.claude/.claude.json`
file for older Claude Code versions) and exits early with a helpful message
if neither shows a logged-in session. It also pre-accepts the workspace trust
for the dedicated workdir (`~/.agentmux/claude-code` by default) so the
session starts fully unattended.

This writes:

- `~/Library/LaunchAgents/com.agentmux.claude-code.plist`
- `~/Library/LaunchAgents/com.agentmux.claude-code.update.plist`

The start LaunchAgent runs at login and then every five minutes by default.
The update LaunchAgent runs at 03:00 local time by default. Logs are written
under `~/Library/Logs/agentmux`.

Useful overrides:

```sh
AGENTMUX_INSTANCE_NAME="claude-code" \
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
and `~/.agentmux/claude-code` alone. Pass `--instance NAME` to remove a
non-default instance's LaunchAgents instead.

## Linux systemd

Run with `sudo`. Configure via flags or env vars — flags win over env vars,
mirroring `install-macos.sh`:

```sh
cd backends/claude-code
sudo ./install.sh \
  --session-name my-session \
  --on-calendar "*-*-* 03:00:00 Australia/Perth"
```

The default display name is `🤹 <user>:<host> <workdir-basename>` — already
self-identifying as an agentmux session, so no suffix is added to it.
`" agentmux"` is still appended to any custom `--display-name`/
`AGENTMUX_DISPLAY_NAME` too, unless you pass `--no-suffix`. Use
`./install.sh --plan` (no `sudo` required) to preview the resolved values
without writing anything.

This writes:

- `/etc/systemd/system/agentmux-claude-code.service`
- `/etc/systemd/system/agentmux-claude-code-update.service`
- `/etc/systemd/system/agentmux-claude-code-update.timer`
- `/etc/agentmux/claude-code.env`

Flags (`./install.sh --help` for the full list):

```
--instance NAME       instance name (default: claude-code)
--session-name NAME   tmux session name (also: --tmux-session)
--display-name NAME   Remote Control display name (also: --remote-name)
--no-suffix           don't append " agentmux" to the display name
--run-user USER       user the session runs as (default: $SUDO_USER)
--on-calendar EXPR    systemd OnCalendar expression for the update timer
--plan                print the install plan without writing files
```

Add `--instance NAME` to install a second instance side by side — see
[Multiple Instances](#multiple-instances).

Equivalent env vars, for automation:

```sh
AGENTMUX_INSTANCE_NAME="claude-code"
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
tmux session and `~/.agentmux/claude-code` alone. Pass `--instance NAME` to
remove a non-default instance's units instead.

## Multiple Instances

Install a second instance by giving it a different `--instance` and
`--workdir` (and, if you want tmux/display defaults other than the
instance name, `--tmux-session`/`--display-name`):

```sh
# macOS
./install-macos.sh \
  --instance pointpost \
  --workdir "$HOME/projects/pointpost" \
  --yes

# Linux (no --workdir flag; use the env var instead)
sudo AGENTMUX_WORKDIR="$HOME/projects/pointpost" ./install.sh \
  --instance pointpost
```

This creates `com.agentmux.pointpost[.update]` LaunchAgents (or
`agentmux-pointpost[.service|-update.service|-update.timer]` on Linux),
a dedicated workdir, a tmux session named `pointpost` by default, and a
Remote Control display name of `🤹 <user>:<host> pointpost` by default —
each distinct from the default `claude-code` instance so both can run side
by side without colliding. Remove it with `./uninstall-macos.sh --instance
pointpost` or `sudo ./uninstall.sh --instance pointpost`.
