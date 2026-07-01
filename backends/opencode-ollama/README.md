# opencode + ollama backend (planned)

Not implemented yet. Will follow the same shape as `backends/claude-code`:
a `rc-start.sh` that brings up a persistent tmux session running
[opencode](https://opencode.ai) against a local ollama model, matching
`install.sh` / `uninstall.sh`, and systemd unit templates for boot
persistence and self-update.

Open questions to resolve before implementing:
- opencode doesn't have Claude Code's `--remote-control` equivalent yet, so
  remote access will likely mean SSH + tmux attach only, unless/until
  opencode ships something similar.
- Update strategy: pull a new ollama model tag vs. bump the opencode CLI
  itself vs. both — probably two independent timers.
