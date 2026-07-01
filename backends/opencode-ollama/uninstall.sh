#!/bin/bash
# Removes the agentmux opencode-ollama backend's systemd units. Does not
# kill a currently running tmux session, touch ~/.agentmux/opencode-ollama,
# or touch the system-wide ollama installation/service.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "uninstall.sh must be run with sudo" >&2
    exit 1
fi

systemctl disable --now agentmux-opencode-ollama-update.timer 2>/dev/null || true
systemctl disable --now agentmux-opencode-ollama.service 2>/dev/null || true
rm -f /etc/systemd/system/agentmux-opencode-ollama.service
rm -f /etc/systemd/system/agentmux-opencode-ollama-update.service
rm -f /etc/systemd/system/agentmux-opencode-ollama-update.timer
rm -f /etc/agentmux/opencode-ollama.env
systemctl daemon-reload

echo "Removed agentmux-opencode-ollama systemd units. Tmux session (if running) and ollama were left alone."
