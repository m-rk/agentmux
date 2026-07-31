# agentmux configurable backend

This backend installs one named agentmux instance: an agent CLI, a model
provider, a model, a workdir, a tmux session, and host supervisor wiring.

Supported combinations today:

| Agent CLI | Provider | Notes |
|---|---|---|
| `zero` | `ollama` | Writes `.zero/config.json` in the instance workdir. |
| `opencode` | `ollama` | Writes `opencode.json` in the instance workdir. |
| `kilo` | `ollama` | Writes `kilo.json` in the instance workdir (Kilo CLI is an opencode fork sharing its config schema). |

The public shape is intentionally generic now, even while the provider matrix is
small, so new provider adapters can be added without creating another
provider-specific backend directory.

## Prerequisites

Install the agent CLI you want to run, plus `tmux` and the provider runtime.
For Ollama Cloud:

```sh
brew install tmux ollama
brew services start ollama
ollama signin

npm install -g @gitlawb/zero      # for --agent zero
npm install -g opencode-ai        # for --agent opencode
npm install -g @kilocode/cli      # for --agent kilo
```

On Linux, install Ollama with its systemd service and run `ollama signin` as
the user the instance will run under.

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
