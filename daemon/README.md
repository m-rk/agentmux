# agentmux daemon + TUI

`agentmuxd` (per-host daemon) and `agentmux` (TUI client) for visualizing
and controlling agentmux instances. See
[`docs/design/daemon-tui.md`](../docs/design/daemon-tui.md) for the full
design and phased rollout plan.

Phase 1 (current): both binaries talk over a Unix socket on one host — no
networking yet.

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

## Run

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
