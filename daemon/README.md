# agentmux daemon + TUI

`agentmuxd` (per-host daemon) and `agentmux` (TUI client) for visualizing
and controlling agentmux instances. See
[`docs/design/daemon-tui.md`](../docs/design/daemon-tui.md) for the full
design and phased rollout plan.

Phase 1: both binaries talk over a Unix socket on one host — no networking
needed. Phase 2 (current): `agentmuxd` can also listen on a TCP address
(e.g. a Tailscale IP), and `agentmux` can connect to several hosts at once
via `~/.config/agentmux/hosts.yaml`.

## Build

```sh
go build -o agentmuxd ./cmd/agentmuxd
go build -o agentmux ./cmd/agentmux
```

Regenerate the protobuf/gRPC stubs after editing `proto/agentmuxd.proto`:

```sh
protoc --go_out=internal/pb --go_opt=paths=source_relative \
  --go-grpc_out=internal/pb --go-grpc_opt=paths=source_relative \
  -I proto proto/agentmuxd.proto
```

## Run: single host (phase 1)

`agentmuxd` reads instance state from `/etc/agentmux/*.env` (written by
`backends/agentmux/install.sh` and `backends/claude-code/install.sh`) and
needs root to call `systemctl` for lifecycle control:

```sh
sudo mkdir -p /run/agentmux
sudo ./agentmuxd -socket /run/agentmux/agentmuxd.sock
```

Then, from the same host:

```sh
./agentmux -socket /run/agentmux/agentmuxd.sock
```

Keys: `↑`/`↓` navigate, `a` attach (detach with `ctrl-\`), `r`/`s`/`x`
restart/stop/start (asks for `y` confirmation), `q` quit.

## Run: multiple hosts over Tailscale (phase 2)

On each host you want to control remotely, also bind a TCP listener on its
Tailscale IP (find it with `tailscale ip -4`):

```sh
sudo ./agentmuxd -socket /run/agentmux/agentmuxd.sock -listen 100.x.y.z:4287
```

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
