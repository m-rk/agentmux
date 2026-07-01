#!/bin/bash
# Starts (or leaves alone, if already running) the persistent agentmux tmux
# session for the Claude Code backend. Idempotent — safe to re-run any time,
# including from the periodic update service.
set -uo pipefail

: "${AGENTMUX_TMUX_SESSION_NAME:=${AGENTMUX_SESSION_NAME:-agentmux}}"
: "${AGENTMUX_DISPLAY_NAME:=${AGENTMUX_REMOTE_NAME:-$AGENTMUX_TMUX_SESSION_NAME}}"
: "${AGENTMUX_WORKDIR:=$HOME/.agentmux/claude-code}"

export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
mkdir -p "$AGENTMUX_WORKDIR"

shell_quote() {
    local value="$1"
    printf "'"
    printf '%s' "$value" | sed "s/'/'\\\\''/g"
    printf "'"
}

# Runs in its own dedicated working directory (not $HOME) so it never
# collides with whatever interactive conversation is most recent there.
if ! tmux has-session -t "$AGENTMUX_TMUX_SESSION_NAME" 2>/dev/null; then
    DISPLAY_ARG="$(shell_quote "$AGENTMUX_DISPLAY_NAME")"
    tmux new-session -d -s "$AGENTMUX_TMUX_SESSION_NAME" -c "$AGENTMUX_WORKDIR" \
        "exec claude --remote-control $DISPLAY_ARG"
fi
