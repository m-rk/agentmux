# agentmux

Agents that are remote controlled, persistent, redundant and self-maintained.

The idea: coding-agent CLIs (Claude Code, [Kilo](https://kilo.ai),
[opencode](https://opencode.ai), [Zero](https://github.com/Gitlawb/zero), and
whatever comes next) are most useful when there's always a live session you
can drop into from anywhere — not just while a terminal happens to be open.
agentmux keeps one running per instance, brings it back after a reboot, and
keeps the CLI itself up to date without you babysitting it.

## agentmux CLI

`agentmux` is one Go binary: a background daemon, a TUI to see and control
every instance across every machine you run it on, and a wizard for creating
new ones. This is the recommended way to run agentmux — no bash installers to
run by hand. It orchestrates `tmux` and the agent CLIs already installed on
the host; it doesn't bundle those tools itself.

<p align="center">
  <img src="docs/design/img/tui-list.png" alt="agentmux TUI: list of instances across hosts" width="49%">
  <img src="docs/design/img/tui-wizard-claude.svg" alt="agentmux new: Claude instance creation wizard" width="49%">
</p>

The wizard preview is generated from its real form code with fixed synthetic
data. See [deterministic UX screenshots](docs/design/ux-screenshots.md) to
regenerate every state or add another one.

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/daemon
go build -o agentmux ./cmd/agentmux

sudo ./agentmux daemon install   # Linux: daemon + doctor systemd timer
./agentmux daemon install        # macOS: daemon + doctor LaunchAgent, no sudo

./agentmux new                   # agent-specific wizard: device, agent, relevant settings
./agentmux                       # TUI: attach, rename, restart, create — across every host
```

Building currently requires Go 1.26.5. Each host also needs `tmux`, the agent
CLI you plan to run, and whatever runtime, credentials, or network access your
selected model provider requires. agentmux checks those prerequisites but
leaves their installation and sign-in to you. The provider adapter included
for `zero`, `opencode`, and `kilo` today is Ollama; provider and model remain
separate parts of an instance rather than defining the backend itself.

- **One binary, no installer scripts** — `agentmux new` provisions
  `claude-code`, `zero`, `opencode`, and `kilo` instances end to end (registry
  file, systemd unit/LaunchAgent, tmux session) on Linux or macOS.
  `agentmux new -y ...` does the same non-interactively, for scripting.
- **Multi-host** — list other machines in `~/.config/agentmux/hosts.yaml`
  (e.g. reachable over Tailscale) and the TUI dials all of them at once,
  merged into one table.
- **Rename deliberately** — `agentmux rename` (or `R` in the TUI) changes a
  tmux session name live. Changing a Claude Code Remote Control display name
  requires a restart; see [Known limitations](#known-limitations) if keeping
  one exact transcript is important.
- **Headless view/send-keys** — `agentmux view -instance NAME` prints a
  read-only snapshot of an instance's tmux pane, and
  `agentmux send-keys -instance NAME KEY...` types into it (literal text
  and/or key names like `Escape`, `Enter`, `C-c`) — both without opening an
  interactive Attach session. This is the headless counterpart to attaching
  just to look or type a command; see [AGENTS.md](AGENTS.md) for why this
  matters for a coding agent driving another agentmux instance.
- **Resume lookup** — `agentmux resume-list` shows what Claude Code sessions
  are resumable for a workdir; the wizard offers the same as a picker.
- **Per-instance Kilo state** — prepared Kilo instances can keep their SQLite
  session database and process state under their own private agentmux
  directory while continuing to share global config and cache. Existing
  instances stay on their legacy shared paths until their migration is marked
  ready, so installing a new binary cannot silently log them out. See the
  [Kilo isolation runbook](docs/kilo-xdg-isolation.md).
- **Custom providers** — point a zero/opencode/kilo instance at any
  OpenAI-compatible endpoint, not just the built-in Ollama default, via
  `-provider`/`-provider-base-url` (and, for Kilo, `-provider-api-key-env`).
  Re-running `new -y` against an existing instance updates its
  provider/model in place. See
  [Custom providers](docs/custom-providers.md).
- **Compact-before-resume** — by default, nightly Claude Code maintenance
  compacts and restarts the session so a long-running unattended session
  doesn't get stuck behind Claude Code's own huge-session prompt. If the
  transcript already ends at a compact boundary, agentmux skips the redundant
  `/compact`. This is configurable per instance.
- **A doctor after refresh** — at 03:30, shortly after the default 03:00
  refresh, one host-wide doctor checks each service, tmux process, session
  identity, refresh result, pane readiness, and backend remote-control state.
  Healthy runs stop there. Problems escalate to Claude for a bounded repair
  plan, which agentmux validates and verifies before reporting the result.
- **Discord notifications (early)** — Discord is agentmux's outbound channel
  for anything it or its managed sessions need to tell you. `agentmux notify
  discord setup` stores a webhook for the current OS user; setup is also
  available from the wizard and the TUI's `D` key. Built-in messages now cover
  notable doctor findings and repairs, plus Linux Claude Code warnings
  around 48 hours before a refresh token expires and again when it does. See
  [Known limitations](#known-limitations) for the current scope.

### Session doctor

`agentmux daemon install` also installs one daily doctor for the host: a
systemd timer on Linux and a per-user LaunchAgent on macOS. It runs at 03:30
(Australia/Perth on Linux, local time on macOS), giving the default 03:00
per-instance refresh half an hour to finish. Choose another post-refresh time
when installing with `agentmux daemon install -doctor-time HH:MM`. Run the
same pass whenever you want with:

```sh
agentmux doctor -dry-run   # preview locally; no repair or Discord post
agentmux doctor            # diagnose, recover safely, and verify
```

The first stage is deterministic and backend-aware: it checks the service
manager, tmux/process identity, the latest refresh result, whether the pane is
interactive, and Claude/Kilo remote-control indicators where applicable. If
those checks are healthy, no model is called and no pane content leaves the
host.

When the first stage finds trouble, Claude Code is the escalation agent by
default and uses the existing Claude login for the session owner; `-model` can
pin a model when that is useful. On Linux, agentmux drops privileges before
launching Claude and normally chooses the owner of the first Claude Code
instance. `-run-user USER` makes that explicit on an unusual multi-user host.

Claude gets no tools and never types into a session itself. It receives only
the affected sessions' structured findings and capped pane snapshots, then
returns JSON for agentmux to validate. A dead session may be started; Escape
may be sent only when the visible pane advertises an Escape action; an idle
session may be restarted only with a verbatim pane excerpt as evidence. A
running session cannot be restarted, and no lifecycle repair runs while the
daily refresh is still active. agentmux refreshes the affected session's state
immediately before acting, then probes again afterward rather than treating a
successful command as proof of recovery. Pane text is still session content,
so use an account you trust with that small excerpt.

No Discord message is sent for an uneventful pass. Repairs, meaningful
observations, escalation failures, and inspection failures go to the webhook
configured for the same OS user with `agentmux notify discord setup`. Each
message includes the before/after state, including successful auto-recovery.
An unchanged unresolved incident is debounced; recovery or a changed/new
incident produces a fresh message. If the same repair leaves exactly the same
problem behind twice, later attempts are suppressed until the session state
changes, and that suppression is reported once rather than causing silent
daily restart churn.

## Trust model

agentmux is currently intended for a single user, or for a host and tailnet
where every client is trusted as an administrator. The API is not an
authentication boundary yet:

- The Linux daemon runs as root so it can manage systemd units. Native Linux
  provisioning creates an absolute workdir *as the requested run user*; it
  never chowns an existing caller-chosen path or creates one with root's
  filesystem permissions.
- The local Unix socket is currently mode `0666`, and the optional TCP listener
  has no authentication or TLS. Anyone who can reach either endpoint can list,
  create, control, view, attach to, and type into instances. Don't expose the
  TCP listener beyond tightly restricted, fully trusted devices.

Authentication and tighter local socket permissions are active hardening work,
not properties the README quietly assumes already exist.

See [`daemon/README.md`](daemon/README.md) to build and run it, and
[`docs/design/daemon-tui.md`](docs/design/daemon-tui.md) for the full design.

## Four properties

Every backend here aims for:

- **Persistence** — the session lives in `tmux`, detached, so SSH drops and
  network blips don't kill it.
- **Remote access** — reattach from anywhere (`tmux attach`, the `agentmux`
  TUI, or a backend's own remote-control feature if it has one).
- **Self-maintenance** — a scheduled job updates the CLI and restarts the
  session according to that backend's maintenance policy, so it doesn't go
  stale.
- **Redundancy** — running more than one backend side by side on the same
  box (different CLIs, different model providers) so an outage or degraded
  provider doesn't take out your only agent, and gives you a choice of
  agent/model for the task at hand.

## Manual install (no daemon)

`agentmux new` creates instances through a running agentmux daemon. If you
only need local instances and don't want the daemon or TUI, the installer
scripts below provide the same basic host-supervisor shape directly. They are
not exact feature equivalents: in particular, the native daemon path has
Kilo session resume and remote-relay setup that the manual scripts do not.

| Installer | Agent CLIs | Included provider adapter | Linux | macOS |
|---|---|---|---|---|
| [`backends/agentmux`](backends/agentmux) | `zero`, `opencode`, `kilo` | Ollama (today) | systemd | LaunchAgents |
| [`backends/claude-code`](backends/claude-code) | Claude Code | Managed by Claude Code | systemd | LaunchAgents |

`backends/agentmux` is the more general of the two: one named instance
combines an agent CLI, a model provider, a model, a workdir, and host
supervisor wiring, so new agents/providers/models can be mixed without
cloning whole directories. `backends/claude-code` is a dedicated installer
predating that generalization, kept for its Remote Control-specific
defaults. The worked commands below use Ollama because it is the provider
adapter currently included in the repository, not because the configurable
backend is inherently tied to it.

### Quickstart example: Zero + Ollama

#### macOS

```sh
# one-time, manual:
brew install tmux ollama
brew services start ollama
ollama signin
npm install -g @gitlawb/zero

git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/agentmux
./install-macos.sh \
  --instance work-zero \
  --agent zero \
  --provider ollama \
  --model gpt-oss:20b-cloud \
  --yes
```

This creates `com.agentmux.work-zero` and
`com.agentmux.work-zero.update` LaunchAgents, plus a dedicated workdir at
`~/.agentmux/work-zero`. Reattach with:

```sh
tmux -L agentmux-work-zero attach -t work-zero
```

Use another instance name, agent, model, or workdir to run multiple agentmux
instances side by side on the same machine.

#### Linux systemd

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/agentmux
sudo ./install.sh \
  --instance work-zero \
  --agent zero \
  --provider ollama \
  --model gpt-oss:20b-cloud
```

See [`backends/agentmux`](backends/agentmux) for supported agent/provider
combinations and all install flags.

### Quickstart (Claude Code backend)

#### macOS

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/claude-code
./install-macos.sh
```

When run from a terminal, the installer prompts for the tmux session name,
Claude display name, update time, final confirmation, and whether to attach
to the tmux session immediately. The default tmux name is
`<machine-slug>-claude-YYYY-MM-DD`; the default display name is
`<user>:<host> 🤹 <workdir-basename>`. For unattended installs, pass flags
instead:

```sh
./install-macos.sh \
  --tmux-session work-claude \
  --display-name "Work Claude" \
  --update-time 03:00 \
  --yes
```

Claude Code must already be authenticated: run `claude` once and complete
login before installing. The installer verifies authentication and
pre-accepts workspace trust for the configured workdir. Add `--attach` to
enter the tmux session immediately after installing.

Use `./install-macos.sh --plan` to preview the LaunchAgents and settings
without writing files. A normal install creates two user LaunchAgents,
without `sudo`:

- `com.agentmux.claude-code` runs `rc-start.sh` at login and every five
  minutes by default, creating the tmux session if it is missing.
- `com.agentmux.claude-code.update` runs nightly at 03:00 local time by
  default, updates Claude Code, and restarts the tmux session only when the
  version changed.

Logs go to `~/Library/Logs/agentmux`. Reattach with
`tmux -L agentmux-<instance> attach -t <tmux-session>`, or from the Claude
Code mobile app via Remote Control.

Pass `--instance NAME` (default: `claude-code`) to install a second, third,
... instance side by side, each with its own workdir, tmux session, and
LaunchAgent/systemd names — see
[`backends/claude-code`](backends/claude-code#multiple-instances).

To remove the LaunchAgents: `./uninstall-macos.sh` (leaves any running tmux
session alone).

#### Linux systemd

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/claude-code
sudo AGENTMUX_SESSION_NAME="my-session" \
     AGENTMUX_ON_CALENDAR="*-*-* 03:00:00 Australia/Perth" \
     ./install.sh
```

(`install.sh` also accepts flags, e.g. `--session-name`/`--on-calendar`, and
defaults the Remote Control display name to `<user>:<host> 🤹 <workdir-basename>`
— see [`backends/claude-code`](backends/claude-code) for the full list.)

This sets up two systemd units (running as whichever user invoked `sudo`,
override with `AGENTMUX_RUN_USER`):

- `agentmux-claude-code.service` — starts (and restarts, on boot) a `tmux`
  session named `$AGENTMUX_SESSION_NAME` running `claude --remote-control`
  in `~/.agentmux/claude-code`.
- `agentmux-claude-code-update.timer` — nightly (default 03:00 in the
  configured `Australia/Perth` timezone, override with
  `AGENTMUX_ON_CALENDAR`) checks for a new Claude Code version, and only
  restarts the session if one was installed.

Reattach any time with
`tmux -L agentmux-<instance> attach -t <tmux-session>`, or from the Claude
Code mobile app via Remote Control, where it appears under the configured
display name.

To remove: `sudo ./uninstall.sh` (leaves any running tmux session alone).

See [`backends/claude-code`](backends/claude-code) for the scripts,
LaunchAgent templates, and systemd unit templates.

## Tests

Run the Go checks for the daemon and CLI:

```sh
cd daemon
go test ./...
go vet ./...
```

Then run the shell regression harness from the repository root:

```sh
tests/smoke.sh
```

By default it uses fake local tools for provider/agent checks, so it does not
need a running model provider. To include a real Ollama + Zero generation smoke:

```sh
AGENTMUX_LIVE_OLLAMA=1 tests/smoke.sh
```

To include a real Ollama + opencode generation smoke:

```sh
AGENTMUX_LIVE_OPENCODE=1 tests/smoke.sh
```

## Known limitations

- **Restart identity is still being tightened.** A normal Claude Code control
  restart uses the resume ID saved in the instance registry; an instance
  created without one may start a fresh transcript. Nightly maintenance
  resolves and saves the newest workdir session for later restarts, but a
  Remote Control display-name rename can happen before that and also restarts.
  For valuable existing context, check `agentmux resume-list` and create the
  instance with an explicit `-resume` ID rather than assuming any restart will
  infer the exact transcript you meant.
- **Discord setup is local to one user on one host.** Discord is the general
  outbound communication channel for agentmux and its managed sessions; token
  expiry is simply the first event wired into it. The webhook URL is a bearer
  credential stored under that user's config directory, so configure it
  separately for the run user on each host and protect the file. The doctor
  reports on both Linux and macOS. Token-expiry warnings remain
  Linux-only because Claude Code keeps macOS credentials in Keychain, and the
  daemon reports them as unsupported instead of guessing at a Keychain item.
- **The manual Kilo backend is basic.** It writes `kilo.json`, launches the
  CLI, and maintains the process, but it doesn't yet mirror the native daemon's
  session discovery/resume, remote-relay setup, or migration-gated XDG data
  and state isolation.
- **Kilo's remote relay may allow only one connected CLI session per account
  — unconfirmed in practice.** Kilo's client treats close code `4409` as a
  permanent conflict:
  [`remote-ws.ts`](https://github.com/Kilo-Org/kilocode/blob/main/packages/opencode/src/kilo-sessions/remote-ws.ts)
  and
  [`remote-ws.test.ts`](https://github.com/Kilo-Org/kilocode/blob/main/packages/opencode/test/kilocode/sessions/remote-ws.test.ts)
  exercises that case, which suggests a one-connection limit. It hasn't
  reproduced in several concurrent long-running local instances, so the exact
  server-side rule remains unclear. Native Kilo sessions avoid re-toggling a
  relay that's already connected, which is a reasonable precaution either way.

## Roadmap

- More backends (Codex CLI, Gemini CLI, whatever comes next) — each one
  running side by side adds to the redundancy/variety this repo is going
  for
- Add richer refresh diagnostics to the doctor beyond service exit state
- Authentication for the daemon API, tighter local socket permissions, and
  TLS for TCP instead of relying solely on tailnet ACLs
