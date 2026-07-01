#!/bin/bash
# Removes the agentmux Claude Code macOS LaunchAgents. Does not kill a
# currently running tmux session or touch ~/.agentmux/claude-code.
set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
    echo "uninstall-macos.sh is only for macOS; use uninstall.sh on Linux" >&2
    exit 1
fi

if [ "$(id -u)" -eq 0 ]; then
    echo "uninstall-macos.sh should be run as your macOS user, not with sudo" >&2
    exit 1
fi

LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
START_LABEL="com.agentmux.claude-code"
UPDATE_LABEL="com.agentmux.claude-code.update"
START_PLIST="$LAUNCH_AGENTS_DIR/$START_LABEL.plist"
UPDATE_PLIST="$LAUNCH_AGENTS_DIR/$UPDATE_LABEL.plist"
DOMAIN="gui/$(id -u)"

launchctl bootout "$DOMAIN" "$UPDATE_PLIST" 2>/dev/null || true
launchctl bootout "$DOMAIN" "$START_PLIST" 2>/dev/null || true
rm -f "$UPDATE_PLIST" "$START_PLIST"

echo "Removed agentmux claude-code LaunchAgents. Tmux session (if running) was left alone."
