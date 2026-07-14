# Cross-device daemon + TUI

Status: phase 1 (localhost) implemented in `../../daemon`
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
specific (each of zero/opencode/claude-code prompts differently) and is
deferred; the idle heuristic is a reasonable proxy and can be special-cased
per agent later without changing the wire protocol.

Note from implementation: tmux's own `#{pane_activity}` is only populated
when a window has `monitor-activity on`, which agentmux instances don't set
and shouldn't have to. Instead the daemon hashes `capture-pane` output on
every poll and tracks when that hash last changed itself
(`internal/discovery`'s `activityCache`).

tmux servers are also per-user (socket under `/tmp/tmux-<uid>/default`), and
`agentmuxd` runs as root — so discovery globs every per-user socket on the
host rather than assuming a single default one. Root can connect to any
user's tmux socket regardless of file ownership.

## Transport & auth

`agentmuxd` binds a TCP port on the host's Tailscale interface. No embedded
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

Bubble Tea. Config at `~/.config/agentmux/hosts.yaml` listing known hosts
(Tailscale hostname + port); the TUI connects to all of them concurrently
and merges their `StreamEvents` feeds.

Layout: host/instance tree on the left, grouped by device; detail pane on
the right (status, model, uptime, tail of recent output). Keys: `a` attach
in place, `r`/`s`/`x` restart/stop/start, `l` scrollback/log view. Actions
that change state (restart/stop) should require a confirmation keypress
before firing.

## Repo layout

New `daemon/` directory, one Go module:

```
daemon/
  proto/agentmuxd.proto
  cmd/agentmuxd/
  cmd/agentmux/
  internal/
    discovery/   # reads env files / plists, cross-references tmux
    daemonserver/
    tuiclient/
```

The existing bash installers in `backends/` are unchanged in what they
provision; they gain one more step — install/enable `agentmuxd.service` —
once phase 2 lands.

## Phased rollout

1. **Localhost only.** `agentmuxd` and `agentmux` talk over a Unix socket
   on one machine. Proves out discovery, the attach PTY plumbing, and
   control actions without any networking involved.
2. **Multi-host.** Point `agentmuxd` at the Tailscale interface, add the
   `hosts.yaml` config, verify against a second device.
3. **Polish.** Live event streaming refinements, per-agent status
   heuristics, confirmation UX, log/scrollback view.

Phase 1 is implemented: `agentmuxd` and `agentmux` build and run against
this box's real `/etc/agentmux` instances over a Unix socket — instance
listing, live status via `StreamEvents`, and a read-only PTY attach have all
been smoke-tested against live sessions. `Control` (start/stop/restart) is
implemented but has only been exercised against an unknown-instance error
path so far, not a real restart, to avoid disrupting live sessions during
development. Phase 2 (Tailscale, multi-host) is not started.
