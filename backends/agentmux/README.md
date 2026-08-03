# agentmux configurable backend

This backend installs one named agentmux instance: an agent CLI, a model
provider, a model, a workdir, a tmux session, and host supervisor wiring.

Provider adapters included today:

| Agent CLI | Provider | Notes |
|---|---|---|
| `zero` | `ollama` | Writes `.zero/config.json` in the instance workdir. |
| `opencode` | `ollama` | Writes `opencode.json` in the instance workdir. |
| `kilo` | `ollama` | Writes `kilo.json` in the instance workdir (Kilo CLI is an opencode fork sharing its config schema). |

The backend itself is not Ollama-specific. Agent CLI, provider, provider base
URL, model, and provider readiness are separate settings. Ollama is the adapter
implemented and exercised today, so the examples below use it as one concrete,
known-good configuration. New provider adapters can use the same backend shape
without creating another provider-specific directory.

These manual scripts provide the basic persistent-process setup. Kilo's native
daemon provisioner goes further by discovering and resuming its latest workdir
session and enabling the remote relay; the scripts currently launch a plain
`kilo` session and don't claim that parity.

## Prerequisites

Install `tmux`, the agent CLI you want to run, and whatever runtime,
credentials, or network access your selected provider requires.

For the included Ollama example:

```sh
brew install tmux ollama
brew services start ollama
ollama signin

npm install -g @gitlawb/zero      # for --agent zero
npm install -g opencode-ai        # for --agent opencode
npm install -g @kilocode/cli      # for --agent kilo
```

On Linux, this example needs Ollama installed with its systemd service and
`ollama signin` run as the user the instance will run under.

## macOS

```sh
cd backends/agentmux
./install-macos.sh \
  --instance work-zero \
  --agent zero \
  --provider ollama \
  --model gpt-oss:20b-cloud \
  --yes
```

This writes:

- `~/Library/LaunchAgents/com.agentmux.work-zero.plist`
- `~/Library/LaunchAgents/com.agentmux.work-zero.update.plist`

Use `--plan` to preview without writing files. Remove an instance with:

```sh
./uninstall-macos.sh --instance work-zero
```

## Linux systemd

```sh
cd backends/agentmux
sudo ./install.sh \
  --instance work-zero \
  --agent zero \
  --provider ollama \
  --model gpt-oss:20b-cloud
```

This writes instance-specific units such as:

- `agentmux-work-zero.service`
- `agentmux-work-zero-update.service`
- `agentmux-work-zero-update.timer`

Remove an instance with:

```sh
sudo ./uninstall.sh --instance work-zero
```

## Multiple Instances

Install another instance by changing `--instance`, `--agent`, `--model`, and
optionally `--workdir` / `--tmux-session`:

```sh
./install-macos.sh \
  --instance work-opencode \
  --agent opencode \
  --provider ollama \
  --model gpt-oss:20b-cloud \
  --yes
```

Each instance gets its own workdir, tmux session, LaunchAgent/systemd names,
logs, and generated agent config — and, like the Claude Code backend, its
own tmux server (`tmux -L agentmux-<instance>`) so no instance's restart
can collaterally kill another's session. Reattach with `tmux -L
agentmux-<instance> attach -t <session>`. See
[the Claude Code backend's upgrade note](../claude-code/README.md#upgrading-an-existing-multi-instance-host)
if migrating a host with instances installed before this change.
