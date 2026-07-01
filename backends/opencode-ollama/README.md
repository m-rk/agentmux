# opencode + Ollama Cloud backend

A persistent [opencode](https://opencode.ai) session backed by
[Ollama Cloud](https://docs.ollama.com/cloud) models, following the same
shape as [`backends/claude-code`](../claude-code): `rc-start.sh` brings up a
tmux session, the install/uninstall scripts wire it into the host supervisor
(systemd on Linux, user LaunchAgents on macOS), and a timer keeps the
opencode CLI itself up to date.

## How auth works

opencode never sees an API key directly. Instead:

1. On Linux, `ollama serve` runs as its own system-wide systemd service
   (set up by ollama's own installer, not by agentmux). On macOS, the same
   local daemon can come from `brew services start ollama`, the Ollama app,
   or a separate `ollama serve` process. Either way, it exposes an
   OpenAI-compatible endpoint on `localhost:11434`.
2. That daemon holds the actual Ollama Cloud credential, established once
   via `ollama signin` — a per-machine device-pairing flow (prints a
   `https://ollama.com/connect?...` URL you open and approve in a browser
   logged into your account). The resulting credential lives with the
   daemon's state and survives restarts on its own.
3. opencode is pointed at that local endpoint via a custom provider in
   `opencode.json` (written automatically by `rc-start.sh` on first run):
   ```json
   {
     "model": "ollama/gpt-oss:20b-cloud",
     "provider": {
       "ollama": {
         "npm": "@ai-sdk/openai-compatible",
         "options": { "baseURL": "http://localhost:11434/v1" }
       }
     }
   }
   ```
   Any model tag ending in `cloud` (e.g. `glm-5.2:cloud`,
   `gpt-oss:120b-cloud`, `qwen3-coder:480b-cloud` — tag format varies by
   model, check with `ollama pull` first) is transparently offloaded to
   Ollama's cloud by the daemon — nothing runs on this box. `ollama
   list`/`ollama ps` stay empty; pulling one of these tags only fetches a
   small manifest, not weights.

   Not every cloud tag is included on every plan — `ollama run <tag>`
   returns `403 Forbidden: this model requires a subscription` for
   anything above your tier (e.g. GLM 5.x and DeepSeek V4 needed an
   upgrade past the base plan). Sanity-check a model with `ollama run
   <tag>` before pointing `AGENTMUX_OLLAMA_MODEL` at it.

This avoids opencode's own `/connect` TUI wizard entirely, which matters
because agentmux needs the whole flow to be scriptable — the only manual,
interactive step in the entire setup is the one-time `ollama signin`.

## Prerequisites (one-time, manual)

### macOS

Run as your macOS user:

```sh
brew install tmux ollama
brew services start ollama        # or use the Ollama app / ollama serve
ollama signin                     # open the printed URL, approve in browser
npm install -g opencode-ai
```

`install-macos.sh` checks that `tmux`, `ollama`, and `opencode` are on PATH,
and that the local Ollama daemon is reachable.

### Linux

Run as the user the session will run under (`AGENTMUX_RUN_USER`):

```sh
curl -fsSL https://ollama.com/install.sh | sh   # installs + starts ollama.service
ollama signin                                    # open the printed URL, approve in browser
npm install -g opencode-ai
```

`install.sh` checks for all three and refuses to proceed if any are
missing.

## Quickstart

### macOS

Run as your macOS user, not with `sudo`:

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/opencode-ollama
./install-macos.sh
```

When run from a terminal, the installer prompts for the tmux session name,
Ollama model, update time, and final confirmation. The generated default
tmux name is `<machine-slug>-opencode-YYYY-MM-DD`.

For unattended installs, pass flags instead:

```sh
./install-macos.sh \
  --tmux-session work-opencode \
  --ollama-model gpt-oss:20b-cloud \
  --update-time 03:00 \
  --yes
```

Use `./install-macos.sh --plan` to preview the LaunchAgents and settings
without writing files.

This writes:

- `~/Library/LaunchAgents/com.agentmux.opencode-ollama.plist`
- `~/Library/LaunchAgents/com.agentmux.opencode-ollama.update.plist`

The start LaunchAgent runs at login and then every five minutes by default.
It waits up to 60 seconds for Ollama to become reachable before starting
opencode. The update LaunchAgent runs at 03:00 local time by default. Logs
are written under `~/Library/Logs/agentmux`.

Useful overrides:

```sh
AGENTMUX_TMUX_SESSION_NAME="work-opencode" \
AGENTMUX_OLLAMA_MODEL="gpt-oss:20b-cloud" \
AGENTMUX_OLLAMA_WAIT_SECONDS=60 \
AGENTMUX_UPDATE_TIME=03:00 \
AGENTMUX_START_INTERVAL=300 \
./install-macos.sh --yes
```

Uninstall:

```sh
./uninstall-macos.sh
```

Uninstalling removes the LaunchAgents but leaves any running tmux session,
`~/.agentmux/opencode-ollama`, and Ollama alone.

### Linux systemd

```sh
git clone https://github.com/m-rk/agentmux.git
cd agentmux/backends/opencode-ollama
sudo AGENTMUX_SESSION_NAME="my-opencode" ./install.sh
```

Reattach with `tmux attach -t $AGENTMUX_SESSION_NAME`. Override the default
model with `AGENTMUX_OLLAMA_MODEL` (see the relevant installer header for
all env vars).

Uninstall with `sudo ./uninstall.sh` — leaves the tmux session, the ollama
installation, and its service alone.

## Open questions from the original roadmap, resolved

- **Remote access**: opencode has no equivalent to Claude Code's
  `--remote-control`. `opencode attach <url>` / `opencode serve` exist but
  would mean exposing a port beyond localhost, which is a separate
  security tradeoff this backend doesn't make by default. For now, remote
  access is SSH + `tmux attach`, same as any other tmux session — including
  from a phone. In practice, from an iPhone: an SSH client with decent TUI
  support (e.g. Termius) + `tmux attach -t $AGENTMUX_SESSION_NAME` as the
  host's startup command gets you straight into the running session on
  connect. Termius's key-management feature ("SSH ID", `sshid.io/<user>`)
  can publish your public key(s) for `curl ... >> authorized_keys`-style
  setup; if your device offers a Secure-Enclave/passkey-backed key
  alongside plain portable keys, the passkey one may fail per-host in ways
  that are hard to debug (no rejection is logged server-side because the
  client never gets far enough to offer it) — falling back to a plain
  ED25519/ECDSA/RSA key from the same picker is the quicker fix.
- **Update strategy**: turned out to be simpler than expected — Ollama
  Cloud model tags are resolved server-side on every request, so there's
  no local weight to "pull a new version of"; only the opencode CLI needs
  a version-diff-and-restart timer (`rc-update.sh`, mirroring the
  claude-code backend). The `ollama` daemon manages its own persistence
  outside agentmux, via its systemd unit on Linux or whichever macOS launch
  path you choose.
