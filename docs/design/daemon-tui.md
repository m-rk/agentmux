# Cross-device daemon + TUI

Status: phase 1 (localhost) and phase 2 (multi-host over TCP/Tailscale) implemented in `../../daemon`
Related: [`backends/agentmux`](../../backends/agentmux), [`backends/claude-code`](../../backends/claude-code)

## Problem

Today an agentmux instance is entirely single-host: a tmux session managed
by systemd (Linux) or a LaunchAgent (macOS), reattached locally with
`tmux attach`. There's no way to see, from one device, what instances are
running across all of a user's devices, or to attach/control a remote one
without first `ssh`-ing in.

Goal: run a daemon on every device that hosts agentmux instances, and a TUI
that can visualize and control sessions on any of those devices from any one
of them.

## Constraints / assumptions

- Devices reach each other over a Tailscale mesh (stable hostnames/IPs,
  no NAT traversal or relay needed).
- The TUI should support full control: list, attach (interactive), and
  lifecycle actions (start/stop/restart), not just read-only status.
- Built in Go: single static binary per platform, cross-compiles cleanly for
  the Linux/macOS split this repo already targets, and gRPC gives us unary +
  bidirectional streaming (needed for PTY attach) in one schema.

## Components

- **`agentmuxd`** — one per host. Runs as its own root-owned systemd unit
  (`agentmuxd.service`), separate from the per-instance units it manages.
  Root because it needs to call `systemctl {start,stop,restart}` on
  `agentmux-<instance>.service` units, which are themselves root-installed
  system units (see `backends/agentmux/agentmux.service.tmpl`). Scoping this
  down with a polkit rule instead of running fully as root is a reasonable
  later hardening step, not needed for v1 single-user boxes.
- **`agentmux`** — the TUI client. Run from any device; connects to one or
  more `agentmuxd` instances concurrently.

## Discovery & data model

`agentmuxd` does not maintain its own instance registry — it reads the
existing source of truth:

- Linux: `/etc/agentmux/*.env` (written by `backends/agentmux/install.sh`)
- macOS: the `com.agentmux.*` LaunchAgent plists

and cross-references with `tmux list-sessions` / `list-panes` for liveness.

Per instance it reports:

- name, agent, provider, model, workdir, tmux session name
- pid / uptime
- status: `running` / `idle` / `dead`, derived from tmux pane activity

Status heuristic (v1): idle-time based — no pane output change for N
seconds means `idle`. True "waiting for input" detection is agent-CLI
specific (each of zero/opencode/kilo/claude-code prompts differently) and is
deferred; the idle heuristic is a reasonable proxy and can be special-cased
per agent later without changing the wire protocol.

Note from implementation: tmux's own `#{pane_activity}` is only populated
when a window has `monitor-activity on`, which agentmux instances don't set
and shouldn't have to. Instead the daemon hashes `capture-pane` output on
every poll and tracks when that hash last changed itself
(`internal/discovery`'s `activityCache`).

tmux servers are also per-user, and — since a shared server means one
instance's systemd restart can cgroup-kill every other instance's session
along with it (hit in practice: an unattended-upgrades-triggered restart of
one instance silently took down two unrelated ones) — each instance now
runs its own tmux server, named `agentmux-<instance>` rather than the user's
default `default` socket. `agentmuxd` runs as root, so discovery globs every
per-user, per-instance socket (`/tmp/tmux-<uid>/agentmux-*`) directly rather
than talking to a single default server. Root can connect to any user's
tmux socket regardless of file ownership.

## Transport & auth

`agentmuxd` always binds a Unix socket (local use, phase 1) and can
optionally also bind a TCP address via `-listen` — point it at the host's
Tailscale IP:port to make it reachable from other devices. No embedded
`tsnet` node, no custom TLS/auth layer for v1 — access control is delegated
to the tailnet's ACLs (document restricting the agentmux port to the user's
own devices). This can be revisited if agentmux is ever shared across
users/tailnets.

## Protocol (`proto/agentmuxd.proto`)

gRPC service, four RPCs:

- `ListInstances() -> InstanceList` — unary snapshot, used on connect and
  on demand.
- `StreamEvents() -> stream InstanceEvent` — server-streaming; pushes status
  changes so the TUI updates reactively instead of polling every host.
- `Attach(instance) <-> stream PtyData` — bidirectional stream of PTY bytes
  plus resize events. Backed by `tmux attach-session -t <name>` spawned in a
  pty on the daemon side. This is what makes remote control feel native:
  the TUI becomes the terminal, no separate `ssh` + `tmux attach` hop.
  Note: the spawned `tmux` needs `TERM` set explicitly — under systemd (no
  controlling terminal) it's unset, and tmux refuses to attach without it.
- `Control(instance, action: start|stop|restart) -> ControlResult` — unary;
  shells to `systemctl <action> agentmux-<instance>` (Linux) or
  `launchctl kickstart` (macOS).

## TUI

Bubble Tea. Config at `~/.config/agentmux/hosts.yaml` lists known hosts —
each a `name` plus a dial `address` (`unix:///path/to.sock` for a local
daemon, `tcp://100.x.y.z:4287` for one over Tailscale). The TUI dials every
host concurrently and merges their `StreamEvents` feeds into one table,
tagged by host; if no `hosts.yaml` exists it falls back to a single local
host over `-socket`, matching phase 1 exactly. If one host's stream errors
(e.g. temporarily unreachable over Tailscale), that host's rows just sit
still and an inline `host: error (retrying)` line appears — the rest of the
table keeps updating; it retries on a fixed delay rather than tearing down
the whole TUI.

Current layout: a single flat table (HOST/NAME/AGENT/MODEL/STATUS/WORKDIR),
sorted by host then name. A host/instance tree with a separate detail pane
is a reasonable follow-up once there's more than a couple of hosts and
columns get cramped — not needed yet. Keys: `a` attach in place, `r`/`s`/`x`
restart/stop/start (each requires a `y` confirmation keypress before
firing), `q` quit. `l` scrollback/log view is not implemented yet.

## CLI: daemon self-install + instance wizard

`agentmuxd` and `agentmux` have since merged into one binary. Subcommands:

- `agentmux` (no args) — the TUI, unchanged.
- `agentmux daemon install|uninstall|status` — installs/removes/checks the
  background daemon on this host: a systemd unit (Linux, requires root) or
  a per-user LaunchAgent (macOS, no root — matches the instance-level
  provisioning's own no-root stance below). Pins a stable copy of the
  running binary (`/usr/local/bin/agentmux` on Linux,
  `~/.agentmux/bin/agentmux` on macOS) so the installed unit doesn't depend
  on wherever the binary was originally built/downloaded.
- `agentmux daemon run` — the daemon's foreground process (what `agentmuxd`
  used to be). What the installed unit execs.
- `agentmux new` — a `charmbracelet/huh` wizard that creates a real
  instance on any device from the same host list the TUI already dials,
  via a new `CreateInstance` RPC.
- `agentmux session run|update|stop --instance NAME` — hidden; the
  per-instance unit's ExecStart/ExecStop, replacing `rc-start.sh`/
  `rc-update.sh`.

### Registry file

Every instance — either OS — gets a `KEY=VALUE` file in the format
`discovery.go` already parses:

- Linux: `/etc/agentmux/<name>.env` (root-owned, as before).
- macOS: `~/.agentmux/registry/<name>.env` (new — macOS instances had no
  registry at all before this work, and so were invisible to
  `discovery`/the TUI entirely; a real gap this incidentally fixed).

`agentmux session run/update/stop` reads its own registry file by instance
name instead of relying on systemd's `EnvironmentFile=` or launchd's
`EnvironmentVariables` dict — both unit templates just pass `--instance
NAME` now, and the Go binary looks up its own config.

### Provisioning architecture

`internal/provision` (native Go port of `backends/*/install.sh` and
`install-macos.sh`) and `internal/session` (native Go port of
`rc-start.sh`/`rc-update.sh`) implement the full instance lifecycle with no
bash in the loop, for every agent/platform combination this repo supports
(`claude-code`, `zero`, `opencode`, `kilo` × Linux, macOS). Both packages split
three ways per agent:

- A shared file (`claudecode.go`, `agentmux.go`) for logic that's genuinely
  identical cross-platform: input validation, the login-check JSON-response
  shape, workspace-trust patching, provider/model defaults.
- `*_linux.go`: root-context specifics — drops privileges to the instance's
  `run_user` via `internal/runas` (a `Credential`-based `exec.Cmd`, not
  `su`/`sudo`), renders systemd units.
- `*_darwin.go`: current-user-context specifics — no privilege drop needed
  (a macOS instance always runs as whoever invoked `agentmux new`, matching
  `install-macos.sh`'s own "must not run as root" check), renders
  LaunchAgent plists.

This split exists because the alternative — copying the per-OS glue instead
of sharing it — is exactly what caused two of the real bugs found while
building this (see Status below): a PATH-resolution helper existed once,
correctly, for one exec call site, but a second, separately-written copy
for a different call site didn't get the same fix when the first one did.

`internal/runas` centralizes the PATH-resolution gotcha that caused those
bugs: `exec.Command`'s own binary lookup uses the *calling* process's
ambient `$PATH` (`os.Getenv`, not `cmd.Env`), not whatever's about to be
set on the child — so both `Command` (drops to another user via a
Credential) and `CurrentUserCommand` (same PATH fixup, no privilege drop)
resolve the binary explicitly before building the `exec.Cmd`, rather than
setting `cmd.Env` on an already-built one and hoping.

### Safety: cross-agent overwrite guard

The wizard's instance-name field doesn't react to the agent dropdown — it
always starts at the claude-code default regardless of which agent is
selected. `provision.Create` refuses to proceed if the requested name
already belongs to a different agent, checked two ways: `existingAgentFor`
(the registry, for instances this provisioner itself created) and
`unitFileExists` (the plist/unit file directly, for older instances
installed by the bash scripts, which predate the registry and so have no
`*.env` file for the first check to find). Re-provisioning the *same*
agent under the same name is still allowed — that's the bash installers'
own "safe to re-run, rewrites config" behavior.

### Resume picker

`ListResumableSessions` scans `~/.claude/projects/<slug>/*.jsonl` — Claude
Code's own undocumented project-directory naming, where every `/` and `.`
in the working directory path becomes `-` — for a given workdir, returning
session IDs newest-first. The wizard shows a picker instead of a free-text
session-ID field when claude-code is selected and an explicit workdir was
typed (a blank workdir means "use the provisioner's own default," which
the wizard can't predict for an arbitrary remote device, so resume falls
back to "fresh session" in that case). A lookup failure or empty result is
treated as "fresh session," not an error — resume is an enhancement, not
core to creating an instance.

### Compact-before-resume on every nightly update

Claude Code has no documented flag/setting to suppress its own "this
session is huge, resume from a summary?" interactive prompt — a session
left running long enough eventually needs a human sitting at it just to
answer that before it's usable again, which defeats the point for an
unattended instance. Since the nightly update cycle already exists, it now
prevents this instead of needing to work around it: every run — not just
one that finds a new Claude Code version — sends `/compact` to the live
session (waiting for the pane to go idle first, via the same content-hash
approach `discovery` uses for status, so it doesn't land mid-response) and
restarts, keeping the session small enough that a later `--resume`
shouldn't hit that threshold.

The restart needed one more fix to be safe: most instances never got an
explicit resume ID recorded in their registry (that field is only set once,
at creation time, only if the wizard's resume picker was used), so a naive
restart would silently launch fresh and lose history for almost every real
instance. The restart now resolves the actual current session ID via the
same `~/.claude/projects` scan `ListResumableSessions` uses, preferring
that over the registry's (usually empty) field, and persists it back —
self-correcting the gap going forward.

This is opt-out, not hardcoded: `AGENTMUX_COMPACT_ON_UPDATE` in the
registry (set via `CreateInstanceRequest.compact_on_update`, exposed as a
wizard field) controls it per instance — `"off"` falls back to the old
version-change-only restart, anything else (including unset, so every
already-migrated instance keeps today's behavior) keeps compacting nightly.

An instance that sat idle since its last nightly update has nothing new to
compact — its transcript's last message is already the compact-boundary
summary from that earlier run. Sending another `/compact` in that state is
a pure no-op (Claude Code refuses it outright: "Not enough messages to
compact."), which would otherwise burn the idle-wait/compact timeouts on a
prompt that was never going anywhere. `LastMessageIsCompactSummary` checks
the newest `~/.claude/projects` transcript's last line for
`isCompactSummary:true` before sending `/compact` at all, so this case
skips straight to resolving the resume ID.

### Renaming an instance

`RenameInstance` updates an existing instance's tmux session name and/or
(claude-code only) its Remote Control display name — wired into the TUI as
`R`. A tmux session name change applies live via `tmux rename-session`,
no restart needed. A display name change can't be applied live (it's baked
into the `claude --remote-control` argv at launch), so it goes through the
same restart path `Control`'s own restart action uses.

That restart is issued **by the daemon**, a separate always-running
process — not by hand-driving `systemctl stop && systemctl start` as two
shell commands from inside the instance being restarted. The latter is a
real trap, hit once while migrating this repo's own hosting instance onto
the new provisioner: `stop`'s side effect (killing the tmux session) also
kills the shell running the command, so the chained `start` never executes
— the unit is left stopped until something else notices and starts it by
hand. `RenameInstance`/`Control`'s restart doesn't have this problem
because the daemon process lives outside any instance's own tmux session
tree, even when the instance being restarted is the one hosting whoever's
driving the TUI.

### Kilo Code's remote relay

Claude Code's Remote Control (mobile/web app visibility into a running
session) is a launch flag, `--remote-control <display>`, baked straight
into the `claude` argv at session-create time. Kilo CLI (the
`@kilocode/cli` npm package, an opencode fork) has no launch-flag
equivalent for its own version of the same feature — the only way to turn
it on is the in-TUI `/remote` slash command. So `RunAgentmux` drives it the
same way the nightly compact does for claude-code: after starting a brand
new kilo session (never on the idempotent no-op path), it types `/remote`
into the running pane and confirms it.

Two timing traps showed up doing this against a real kilo process, both
found by testing against actual provisioned instances rather than a fake
binary:

- **Idle-stability detection is the wrong readiness signal for a cold
  boot.** The nightly-compact code already waits for a pane to stop
  changing before typing into it, but that assumes the pane is already
  showing real content. A tmux pane that exists but hasn't been painted to
  yet (node/kilo still starting, fetching its model list, indexing the
  workspace) is *also* unchanging — idle-stability can't tell "settled"
  apart from "hasn't started yet," so it fired on a not-yet-interactive
  pane and the keystrokes went nowhere. Fixed by polling for a
  known-only-once-ready piece of text (`kiloReadyMarker`, the empty-input
  placeholder) instead of content stability.
- **The command palette's fuzzy filter updates asynchronously.** Typing
  `/remote` and pressing Enter in the same `tmux send-keys` call raced the
  palette's own filter/selection update: Enter could arrive before
  `/remote` was actually the selected item, leaving the palette open with
  the text typed but nothing chosen. Fixed by sending the text and Enter
  as two separate `send-keys` calls with a short pause between them.

## Repo layout

`daemon/`, one Go module, one binary (`cmd/agentmux`):

```
daemon/
  proto/agentmuxd.proto
  cmd/agentmux/       # dispatcher: tui (default) / daemon / new / session
  internal/
    discovery/        # reads registry files, cross-references tmux
    daemonserver/      # gRPC service: ListInstances/StreamEvents/Attach/
                        # Control/CreateInstance/ListResumableSessions
    daemoninstall/     # installs the daemon as a systemd unit/LaunchAgent
    provision/         # native Go port of install.sh / install-macos.sh
    session/           # native Go port of rc-start.sh / rc-update.sh
    runas/             # exec-as-another-user / PATH-fixup helper
    tuiclient/
    hostsconfig/       # parses ~/.config/agentmux/hosts.yaml
```

The bash installers in `backends/` are unchanged and still fully
functional standalone — kept as a documented, working fallback during the
transition, not removed. `agentmux new`'s native Go provisioning is a
parallel implementation, not a wrapper around them.

## Phased rollout

1. **Localhost only.** `agentmuxd` and `agentmux` talk over a Unix socket
   on one machine. Proves out discovery, the attach PTY plumbing, and
   control actions without any networking involved.
2. **Multi-host.** Point `agentmuxd` at the Tailscale interface, add the
   `hosts.yaml` config, verify against a second device.
3. **Polish.** Live event streaming refinements, per-agent status
   heuristics, confirmation UX, log/scrollback view.
4. **CLI + daemon self-install.** Merge `agentmuxd`/`agentmux` into one
   binary; `agentmux daemon install/uninstall/status` replaces manual unit
   files.
5. **Instance wizard.** `CreateInstance` RPC + `agentmux new`; native Go
   port of every backend this repo supports (`claude-code`, `zero`,
   `opencode`) on both Linux and macOS — no bash in the instance lifecycle.
6. **Resume picker.** `ListResumableSessions`, wired into the wizard.
7. **Docs, migration.** This write-up; a migration story for hosts with
   pre-existing bash-installed instances is still open (see Known gaps).

## Status

**Phases 1–3** (transport/TUI): implemented and locally verified —
instance listing, live status, a read-only PTY attach, and multi-host merge
over both a Unix socket and a TCP loopback all work correctly against this
box's real instances. Not yet verified: `Control` (restart/stop/start)
against a real instance on Linux specifically (only the unknown-instance
error path — see phase 5's note below for why), and an actual cross-device
link over Tailscale (every "multi-host" test so far has been one daemon
dialed twice on the same machine, not two separate devices).

**Phases 4–6** (CLI, wizard, provisioning, resume): implemented and
live-tested on **both Linux and macOS**. Every agent × platform combination
this repo supports produces a real registry file, systemd unit/LaunchAgent,
and live tmux session via `CreateInstance`, correctly reported by
`discovery`; the cross-agent-overwrite guard was verified against real
production instances on both platforms (refused as designed, instance
files confirmed byte-identical afterward); `ListResumableSessions` was
verified against this repo's own real Claude Code session history. The
interactive wizard *form* itself (not just the RPCs it calls) has been
driven end-to-end through a real pty on both platforms. `Control`
restart/stop/start has been verified for real on macOS (via
`control_darwin.go`) but only against the unknown-instance error path on
Linux — deliberately, to avoid disrupting real live sessions on the box
this was developed on. Compact-before-resume was verified end-to-end
(manually invoked, confirmed the idle-wait, `/compact`, restart, and
discovered/persisted `--resume` ID all worked) against a throwaway
instance too small to have ever hit the actual "resume from summary"
prompt — the prevention mechanism is proven, not the specific failure
mode it's meant to prevent, since manufacturing a genuinely huge session
isn't practical for a test.

Two real bugs surfaced by live testing, both in `internal/session`'s
PATH-resolution helper (see "Provisioning architecture" above for why this
keeps happening and how `internal/runas` now centralizes the fix):
`$HOME` isn't reliably set for a bare `Type=oneshot` + `User=` systemd unit
(breaking `PATH` resolution for the spawned agent binary), and `tmux -S
<bare-name>` is a literal CWD-relative path, not the named socket in
`/tmp/tmux-<uid>/` that `-L <bare-name>` resolves to.

**Known gaps:**
- No real cross-device test (see phase 1–3 status above).
- `Control` not live-tested against a real instance on Linux.
- No migration story yet for a host with pre-existing bash-installed
  instances, or the very first ad hoc `systemd-run` transient daemon setup
  used before `agentmux daemon install` existed.
