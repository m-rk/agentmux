#!/bin/bash
# Nightly maintenance for an agentmux opencode session: updates the opencode
# CLI and, only if the version actually changed, restarts the running tmux
# session so it picks up the new binary.
#
# On Linux this runs as root from the systemd *-update.service unit, then
# performs npm/opencode work as AGENTMUX_RUN_USER. On macOS this runs as the
# target user from a LaunchAgent and restarts the tmux session directly.
#
# Ollama Cloud models are resolved by tag on every request (there's nothing
# to "pull" for a newer version — see README), and the ollama daemon itself
# persists and restarts on its own via the systemd unit its installer sets
# up, so neither is this script's concern.
set -uo pipefail

SESSION_NAME="${AGENTMUX_SESSION_NAME:-agentmux-opencode}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CURRENT_HOME="${HOME:-}"

if [ -n "$CURRENT_HOME" ]; then
    export PATH="$CURRENT_HOME/.local/bin:$CURRENT_HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
else
    export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
fi
log() { echo "[agentmux-opencode-ollama-update] $*"; }
timestamp() { date '+%Y-%m-%dT%H:%M:%S%z'; }

if [ "$(uname -s)" = "Darwin" ]; then
    log "starting at $(timestamp)"

    BEFORE=$(opencode --version 2>&1)
    log "current version: $BEFORE"

    if ! opencode upgrade --method npm; then
        log "opencode upgrade failed, leaving existing session running untouched"
        exit 1
    fi

    AFTER=$(opencode --version 2>&1)
    log "version after update: $AFTER"

    if [ "$BEFORE" = "$AFTER" ]; then
        log "no version change, nothing to restart"
        exit 0
    fi

    if ! ollama list >/dev/null 2>&1; then
        log "ollama is not reachable, leaving existing session running untouched"
        exit 1
    fi

    log "restarting tmux session $SESSION_NAME ($BEFORE -> $AFTER)"
    tmux kill-session -t "$SESSION_NAME" 2>/dev/null || true
    "$SCRIPT_DIR/rc-start.sh"

    sleep 5
    if tmux has-session -t "$SESSION_NAME" 2>/dev/null; then
        log "$SESSION_NAME session is back up on $AFTER"
    else
        log "ERROR: $SESSION_NAME session did not come back up after updating to $AFTER"
        exit 1
    fi

    exit 0
fi

RUN_USER="${AGENTMUX_RUN_USER:?AGENTMUX_RUN_USER must be set}"
SERVICE_NAME="${AGENTMUX_SERVICE_NAME:?AGENTMUX_SERVICE_NAME must be set}"
RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"
AS_USER=(sudo -u "$RUN_USER" env "PATH=$RUN_HOME/.local/bin:$RUN_HOME/.npm-global/bin:/usr/local/bin:/usr/bin:/bin" "HOME=$RUN_HOME")

log "starting at $(timestamp)"

BEFORE=$("${AS_USER[@]}" opencode --version 2>&1)
log "current version: $BEFORE"

if ! "${AS_USER[@]}" opencode upgrade --method npm; then
    log "opencode upgrade failed, leaving existing session running untouched"
    exit 1
fi

AFTER=$("${AS_USER[@]}" opencode --version 2>&1)
log "version after update: $AFTER"

if [ "$BEFORE" = "$AFTER" ]; then
    log "no version change, nothing to restart"
    exit 0
fi

log "restarting $SERVICE_NAME ($BEFORE -> $AFTER)"
systemctl restart "$SERVICE_NAME"

sleep 5
if "${AS_USER[@]}" tmux has-session -t "$SESSION_NAME" 2>/dev/null; then
    log "$SESSION_NAME session is back up on $AFTER"
else
    log "ERROR: $SESSION_NAME session did not come back up after updating to $AFTER"
    exit 1
fi
