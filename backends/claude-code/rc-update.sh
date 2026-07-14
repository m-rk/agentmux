#!/bin/bash
# Nightly maintenance for an agentmux Claude Code session: updates the
# Claude Code CLI and restarts the tmux session if the version changed or
# the session isn't actually running any more (e.g. it crashed, or died as
# collateral from another instance's restart — see the -L note in
# rc-start.sh).
#
# On Linux this runs as root from the systemd *-update.service unit, then
# performs npm/claude work as AGENTMUX_RUN_USER. On macOS this runs as the
# target user from a LaunchAgent and restarts the tmux session directly.
set -uo pipefail

: "${AGENTMUX_INSTANCE_NAME:=claude-code}"
TMUX_SESSION_NAME="${AGENTMUX_TMUX_SESSION_NAME:-${AGENTMUX_SESSION_NAME:-agentmux}}"
TMUX_SOCKET="${AGENTMUX_TMUX_SOCKET:-agentmux-$AGENTMUX_INSTANCE_NAME}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CURRENT_HOME="${HOME:-}"

if [ -n "$CURRENT_HOME" ]; then
    export PATH="$CURRENT_HOME/.local/bin:$CURRENT_HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
else
    export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
fi
log() { echo "[agentmux-$AGENTMUX_INSTANCE_NAME-update] $*"; }
timestamp() { date '+%Y-%m-%dT%H:%M:%S%z'; }

if [ "$(uname -s)" = "Darwin" ]; then
    log "starting at $(timestamp)"

    BEFORE=$(claude --version 2>&1)
    log "current version: $BEFORE"

    if ! claude update; then
        log "claude update failed, leaving existing session running untouched"
        exit 1
    fi

    AFTER=$(claude --version 2>&1)
    log "version after update: $AFTER"

    if [ "$BEFORE" = "$AFTER" ] && tmux -L "$TMUX_SOCKET" has-session -t "$TMUX_SESSION_NAME" 2>/dev/null; then
        log "no version change and $TMUX_SESSION_NAME session is already running"
        exit 0
    fi

    if [ "$BEFORE" != "$AFTER" ]; then
        log "restarting tmux session $TMUX_SESSION_NAME ($BEFORE -> $AFTER)"
        tmux -L "$TMUX_SOCKET" kill-session -t "$TMUX_SESSION_NAME" 2>/dev/null || true
    else
        log "$TMUX_SESSION_NAME session is missing; starting it"
    fi
    "$SCRIPT_DIR/rc-start.sh"

    sleep 5
    if tmux -L "$TMUX_SOCKET" has-session -t "$TMUX_SESSION_NAME" 2>/dev/null; then
        log "$TMUX_SESSION_NAME session is up on $AFTER"
    else
        log "ERROR: $TMUX_SESSION_NAME session did not come up on $AFTER"
        exit 1
    fi

    exit 0
fi

RUN_USER="${AGENTMUX_RUN_USER:?AGENTMUX_RUN_USER must be set}"
SERVICE_NAME="${AGENTMUX_SERVICE_NAME:?AGENTMUX_SERVICE_NAME must be set}"
RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"
AS_USER=(sudo -u "$RUN_USER" env "PATH=$RUN_HOME/.local/bin:$RUN_HOME/.npm-global/bin:/usr/local/bin:/usr/bin:/bin" "HOME=$RUN_HOME")

log "starting at $(timestamp)"

BEFORE=$("${AS_USER[@]}" claude --version 2>&1)
log "current version: $BEFORE"

if ! "${AS_USER[@]}" claude update; then
    log "claude update failed, leaving existing session running untouched"
    exit 1
fi

AFTER=$("${AS_USER[@]}" claude --version 2>&1)
log "version after update: $AFTER"

if [ "$BEFORE" = "$AFTER" ] && "${AS_USER[@]}" tmux -L "$TMUX_SOCKET" has-session -t "$TMUX_SESSION_NAME" 2>/dev/null; then
    log "no version change and $TMUX_SESSION_NAME session is already running"
    exit 0
fi

if [ "$BEFORE" != "$AFTER" ]; then
    log "restarting $SERVICE_NAME ($BEFORE -> $AFTER)"
else
    log "$TMUX_SESSION_NAME session is missing; restarting $SERVICE_NAME to bring it back"
fi
systemctl restart "$SERVICE_NAME"

sleep 5
if "${AS_USER[@]}" tmux -L "$TMUX_SOCKET" has-session -t "$TMUX_SESSION_NAME" 2>/dev/null; then
    log "$TMUX_SESSION_NAME session is up on $AFTER"
else
    log "ERROR: $TMUX_SESSION_NAME session did not come up on $AFTER"
    exit 1
fi
