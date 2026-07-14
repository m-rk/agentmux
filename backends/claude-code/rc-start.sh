#!/bin/bash
# Starts (or leaves alone, if already running) the persistent agentmux tmux
# session for the Claude Code backend. Idempotent — safe to re-run any time,
# including from the periodic update service.
set -uo pipefail

: "${AGENTMUX_INSTANCE_NAME:=claude-code}"
: "${AGENTMUX_TMUX_SESSION_NAME:=${AGENTMUX_SESSION_NAME:-agentmux}}"
: "${AGENTMUX_TMUX_SOCKET:=agentmux-$AGENTMUX_INSTANCE_NAME}"
: "${AGENTMUX_DISPLAY_NAME:=${AGENTMUX_REMOTE_NAME:-$AGENTMUX_TMUX_SESSION_NAME}}"
: "${AGENTMUX_WORKDIR:=$HOME/.agentmux/$AGENTMUX_INSTANCE_NAME}"
: "${AGENTMUX_RESUME:=}"

export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
mkdir -p "$AGENTMUX_WORKDIR"

shell_quote() {
    local value="$1"
    printf "'"
    printf '%s' "$value" | sed "s/'/'\\\\''/g"
    printf "'"
}

# Each instance gets its own tmux server (-L), not the user's default one.
# Otherwise every instance's session ends up as a child of whichever tmux
# server happened to start first, all sharing that instance's systemd unit's
# cgroup — so stopping/restarting *that one* unit (its own update timer, an
# admin, or a package upgrade's needrestart) SIGKILLs the whole shared server
# via KillMode=control-group, taking every other instance's session down
# with it, silently, with only the owning unit's ExecStart around to bring
# its own session back.
if ! tmux -L "$AGENTMUX_TMUX_SOCKET" has-session -t "$AGENTMUX_TMUX_SESSION_NAME" 2>/dev/null; then
    DISPLAY_ARG="$(shell_quote "$AGENTMUX_DISPLAY_NAME")"
    RESUME_ARG=""
    if [ -n "$AGENTMUX_RESUME" ]; then
        RESUME_ARG="--resume $(shell_quote "$AGENTMUX_RESUME")"
    fi
    tmux -L "$AGENTMUX_TMUX_SOCKET" new-session -d -s "$AGENTMUX_TMUX_SESSION_NAME" -c "$AGENTMUX_WORKDIR" \
        "exec claude --remote-control $DISPLAY_ARG $RESUME_ARG"
fi
