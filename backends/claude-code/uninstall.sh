#!/bin/bash
# Removes the agentmux Claude Code backend's systemd units. Does not kill a
# currently running tmux session or touch ~/.agentmux/claude-code.
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "uninstall.sh must be run with sudo" >&2
    exit 1
fi

systemctl disable --now agentmux-claude-code-update.timer 2>/dev/null || true
systemctl disable --now agentmux-claude-code.service 2>/dev/null || true
rm -f /etc/systemd/system/agentmux-claude-code.service
rm -f /etc/systemd/system/agentmux-claude-code-update.service
rm -f /etc/systemd/system/agentmux-claude-code-update.timer
rm -f /etc/agentmux/claude-code.env
systemctl daemon-reload

echo "Removed agentmux-claude-code systemd units. Tmux session (if running) was left alone."
