#!/bin/bash
# Starts (or leaves alone, if already running) the persistent agentmux tmux
# session for the Claude Code backend. Idempotent — safe to re-run any time,
# including from the periodic update service.
set -uo pipefail

: "${AGENTMUX_SESSION_NAME:=agentmux}"
: "${AGENTMUX_WORKDIR:=$HOME/.agentmux/claude-code}"

export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
mkdir -p "$AGENTMUX_WORKDIR"

# Runs in its own dedicated working directory (not $HOME) so it never
# collides with whatever interactive conversation is most recent there.
if ! tmux has-session -t "$AGENTMUX_SESSION_NAME" 2>/dev/null; then
    tmux new-session -d -s "$AGENTMUX_SESSION_NAME" -c "$AGENTMUX_WORKDIR" \
        "claude --remote-control \"$AGENTMUX_SESSION_NAME\""
fi
