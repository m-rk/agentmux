# agentmux daemon + TUI

`agentmux` is a single binary: the TUI by default, plus subcommands to
install its own background daemon and to create new instances. See
[`docs/design/daemon-tui.md`](../docs/design/daemon-tui.md) for the full
design and phased rollout plan.

Phase 1: TUI + daemon talk over a Unix socket on one host — no networking
needed. Phase 2: the daemon can also listen on a TCP address (e.g. a
Tailscale IP), and the TUI can connect to several hosts at once via
`~/.config/agentmux/hosts.yaml`. `agentmux new` — a wizard that creates a
real instance (registry file + systemd unit/LaunchAgent + tmux session) on
any configured device — is native Go end to end (no bash) for every
agent/platform combination this repo supports: `claude-code`, `zero`, and
`opencode`, on both Linux and macOS.

## Build

```sh
go build -o agentmux ./cmd/agentmux
```

There's only one binary now — `agentmuxd` was folded into `agentmux daemon
run`.

Regenerate the protobuf/gRPC stubs after editing `proto/agentmuxd.proto`:

```sh
protoc --go_out=internal/pb --go_opt=paths=source_relative \
  --go-grpc_out=internal/pb --go-grpc_opt=paths=source_relative \
  -I proto proto/agentmuxd.proto
```

## Install the daemon

```sh
sudo ./agentmux daemon install     # Linux: root required, installs a systemd unit
./agentmux daemon install          # macOS: do NOT use sudo, installs a per-user LaunchAgent
```

This copies the running binary to a stable path (`/usr/local/bin/agentmux`
on Linux, `~/.agentmux/bin/agentmux` on macOS), installs the unit/plist
pointing at `agentmux daemon run`, and starts it. `agentmux daemon status`
/ `agentmux daemon uninstall` check/remove it. Re-running `install` after
rebuilding the binary does *not* restart an already-running daemon (systemd
`enable --now` is a no-op if it's already active) — `sudo systemctl restart
agentmuxd` (Linux) or `launchctl kickstart -k gui/$(id -u)/com.agentmux.daemon`
(macOS) to pick up a new build.

Then, from the same host:

```sh
./agentmux
```

Keys: `↑`/`↓` navigate, `a` attach (detach with `ctrl-\`), `n` create a new
instance, `r`/`s`/`x` restart/stop/start (asks for `y` confirmation), `q`
quit.

## Create a new instance

```sh
./agentmux new
```

Prompts for device (any host from `hosts.yaml`, or `local`), agent
(`claude-code`, `zero`, or `opencode`), instance name, run-as user (Linux
only — a macOS instance always runs as whoever ran the wizard), workdir,
and provider/model (zero/opencode only). Calls the target device's daemon
over the same connection the TUI uses — creating on a remote device just
means picking it from the same list. If `claude-code` is selected and an
explicit workdir was given, it looks up resumable sessions for that workdir
(via `~/.claude/projects`) and offers a picker instead of asking for a
session ID by hand.

Picking an instance name that's already in use by a *different* agent is
refused rather than silently overwritten — this also catches the case of
an instance installed by the older `backends/*/install.sh` scripts, which
predate the registry file this wizard reads/writes.

## Multiple hosts over Tailscale

On each host you want to control remotely, also bind a TCP listener on its
Tailscale IP (find it with `tailscale ip -4`):

```sh
sudo ./agentmux daemon run -listen 100.x.y.z:4287
```

(or pass `-listen` via the systemd unit's `ExecStart` if installed with
`daemon install` — there's no flag for it yet, so edit
`/etc/systemd/system/agentmuxd.service` and `daemon-reload` for now.)

There's no TLS or auth on that TCP listener — it relies entirely on the
tailnet's ACLs to keep it reachable only by your own devices. Restrict the
port to your devices in your Tailscale ACL policy before exposing it.

On the device you run the TUI from, list every host you want to see in
`~/.config/agentmux/hosts.yaml`:

```yaml
hosts:
  - name: laptop
    address: "unix:///run/agentmux/agentmuxd.sock"
  - name: homelab
    address: "tcp://100.x.y.z:4287"
```

Then just run `./agentmux` (no `-socket` needed once `hosts.yaml` exists —
it's only used as the phase-1 fallback when the file is missing). The TUI
dials every host concurrently and merges them into one table tagged by
host. If a host is unreachable, its row shows an inline error and the TUI
keeps retrying in the background rather than blocking the rest of the
table.
