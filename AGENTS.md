# AGENTS.md

Guidance for a coding agent (e.g. a Claude Code instance) either developing
this repo or *operating* a live agentmux deployment — managing instances on
a host where `agentmuxd` is running.

## Prefer the CLI over the TUI or raw tmux

`agentmux`'s default UI is an interactive TUI (`agentmux` with no args) built
for a human at a real terminal. An agent invoking commands through a
non-interactive shell tool has no TTY, so the TUI isn't usable, and
`agentmux new`/`agentmux rename`'s interactive forms aren't either — use
their non-interactive/scriptable forms instead. Every TUI action has a
headless CLI equivalent:

| TUI key | Headless CLI |
|---|---|
| `a` (attach) | no headless equivalent by design — see below |
| `n` (new instance) | `agentmux new -y ...` |
| `R` (rename) | `agentmux rename -instance NAME -tmux-name ... -display-name ...` |
| `r` / `s` / `x` (restart/stop/start) | `agentmux control -instance NAME -action restart\|stop\|start` |
| `D` (Discord setup) | `agentmux notify discord setup -y -webhook-url URL` |
| (list) | `agentmux list` (`-json` for scripts) |
| — | `agentmux view -instance NAME` — read-only pane snapshot |
| — | `agentmux send-keys -instance NAME KEY...` — type into a pane |
| — | `agentmux doctor -dry-run` — preview the host-wide health/recovery pass |

**Do not shell out to raw `tmux -L`/`-S ...` commands against agentmux-managed
sessions.** It's tempting — every instance's tmux session is easy to find by
guessing the `agentmux-<name>` socket convention — but that convention is an
implementation detail, not a stable contract, and reaching around agentmux
this way risks exactly the kind of divergence that caused a real incident:
an instance got renamed underneath agentmux's own registry, leaving a stray
duplicate `claude --resume` process attached to the same session transcript
as the still-live original. `agentmux view`/`agentmux send-keys` resolve the
correct socket/session the same way `agentmux control`/the TUI's attach do
(via the daemon's own `discovery.List()`), so they can't drift from what
agentmux itself considers the live session.

- **`agentmux view -instance NAME [-lines N]`** — headless, read-only
  snapshot of the instance's current tmux pane (add `-lines N` for trailing
  scrollback). Use this to check what state a session is actually in before
  deciding whether/how to act — e.g. whether it's mid-response (don't type
  into it), sitting at a stuck confirmation menu, or genuinely idle.
- **`agentmux send-keys -instance NAME KEY...`** — headless equivalent of
  typing into the pane. Trailing args are passed straight through as tmux
  `send-keys` arguments: literal text and/or key names (`Escape`, `Enter`,
  `C-c`, ...). Example: `agentmux send-keys -instance minecraft Escape`
  dismisses a stuck Claude Code Remote Control confirmation menu (see
  `daemon/internal/session/claudecode.go`'s `ensureClaudeRemoteControl` for
  the automated version of the same fix).
- Interactive `attach` deliberately has no headless equivalent — a real PTY
  session isn't something a non-interactive command should be driving
  unattended. If you need to hand a session to a human, tell them to run
  `agentmux`, select the instance, and press `a` in the TUI. `agentmux list
  -json` reports the host and tmux session but deliberately doesn't expose the
  daemon's resolved socket path as a public contract.

## Developing agentmux itself

### UX screenshots in pull requests

UX-affecting pull requests should include screenshots produced from mocked,
sanitised state rather than a live agentmux deployment. Use fixed synthetic
users, host names, paths, instance names, providers, and credential variable
names so no local or private details can reach a public artifact.

- Reuse the production UI builders instead of recreating the interface for a
  screenshot. For the instance wizard, run
  `./scripts/generate-ux-screenshots.sh` and verify the checked-in output with
  `./scripts/generate-ux-screenshots.sh -check`.
- Inspect every screenshot before publishing it. Do not upload captures from a
  live daemon, terminal, browser session, or machine-derived configuration.
- Run `gh auth status` before publishing and repair authentication with
  `gh auth login` if needed. Upload the sanitised screenshot files with
  `gh image --repo m-rk/agentmux ...`, then embed the returned
  `user-attachments` Markdown in the pull request body.
- When a pull request description contains multiple screenshots, arrange them
  in a Markdown table and give every screenshot a clear heading.
- When a pull request changes UX, include before and after screenshots in the
  description as a Markdown table with headings.
- Never print, persist, commit, or paste an authentication or browser-session
  token. The pull request should contain only the uploaded attachment URLs.

- `daemon/proto/agentmuxd.proto` is the source of truth for the daemon's
  gRPC API. After editing it, regenerate stubs (see `daemon/README.md`):
  ```sh
  protoc --go_out=internal/pb --go_opt=paths=source_relative \
    --go-grpc_out=internal/pb --go-grpc_opt=paths=source_relative \
    -I proto proto/agentmuxd.proto
  ```
  `protoc`/`protoc-gen-go`/`protoc-gen-go-grpc` may not be preinstalled;
  `dnf install protobuf-compiler` (or your distro's equivalent) plus
  `go install google.golang.org/protobuf/cmd/protoc-gen-go@latest` and
  `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest` (added
  to `$(go env GOPATH)/bin`, which needs to be on `PATH`) is enough — an old
  system `protoc` (even a 2017-era 3.5.0) is fine, only the version comment
  in the generated header differs.
- Build/test from `daemon/`: `go build ./...`, `go test ./...`.
- Deploying a locally built binary to this host:
  `go build -o agentmux ./cmd/agentmux && sudo ./agentmux daemon install &&
  sudo systemctl restart agentmuxd.service` — `daemon install` alone does
  *not* restart an already-running daemon, the explicit restart is required.
  Restarting `agentmuxd` is safe to do at will: it doesn't touch the
  independently-managed per-instance tmux sessions/systemd units, only the
  supervising daemon process itself (confirm with
  `systemctl show agentmuxd.service -p KillMode,Type` — expect
  `Type=simple`, `KillMode=control-group` — and `ps --ppid <daemon-pid>`
  showing no children, if in doubt).
- Kilo instances can isolate their data and state directories, but existing
  instances require an explicit, one-at-a-time migration. Follow
  [`docs/kilo-xdg-isolation.md`](docs/kilo-xdg-isolation.md); never create the
  readiness marker or restart a live instance until its isolated login and
  session history have been verified and the operator has approved the
  cutover.
