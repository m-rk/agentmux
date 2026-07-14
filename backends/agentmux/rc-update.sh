#!/bin/bash
# Periodic maintenance for one configured agentmux instance.
set -uo pipefail

: "${AGENTMUX_INSTANCE_NAME:=agentmux}"
: "${AGENTMUX_AGENT:=zero}"
: "${AGENTMUX_PROVIDER:=ollama}"
: "${AGENTMUX_TMUX_SESSION_NAME:=${AGENTMUX_SESSION_NAME:-$AGENTMUX_INSTANCE_NAME}}"
: "${AGENTMUX_TMUX_SOCKET:=agentmux-$AGENTMUX_INSTANCE_NAME}"
: "${AGENTMUX_WORKDIR:=$HOME/.agentmux/$AGENTMUX_INSTANCE_NAME}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CURRENT_HOME="${HOME:-}"

if [ -n "$CURRENT_HOME" ]; then
    export PATH="$CURRENT_HOME/.local/bin:$CURRENT_HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
else
    export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
fi

log() { echo "[agentmux-$AGENTMUX_INSTANCE_NAME-update] $*"; }
timestamp() { date '+%Y-%m-%dT%H:%M:%S%z'; }

current_version() {
    case "$AGENTMUX_AGENT" in
        zero) zero --version 2>&1 ;;
        opencode) opencode --version 2>&1 ;;
        *) echo "unsupported agent: $AGENTMUX_AGENT"; return 1 ;;
    esac
}

update_agent() {
    case "$AGENTMUX_AGENT" in
        zero)
            zero update --check
            ;;
        opencode)
            opencode upgrade --method npm
            ;;
        *)
            log "unsupported agent: $AGENTMUX_AGENT"
            return 1
            ;;
    esac
}

log "starting at $(timestamp)"

BEFORE="$(current_version)"
log "current version: $BEFORE"

if ! update_agent; then
    log "$AGENTMUX_AGENT update/check failed; leaving existing session running untouched"
    exit 1
fi

AFTER="$(current_version)"
log "version after maintenance: $AFTER"

if [ "$BEFORE" = "$AFTER" ] && tmux -L "$AGENTMUX_TMUX_SOCKET" has-session -t "$AGENTMUX_TMUX_SESSION_NAME" 2>/dev/null; then
    log "no version change and $AGENTMUX_TMUX_SESSION_NAME session is already running"
    exit 0
fi

if [ "$BEFORE" != "$AFTER" ]; then
    log "restarting tmux session $AGENTMUX_TMUX_SESSION_NAME ($BEFORE -> $AFTER)"
    tmux -L "$AGENTMUX_TMUX_SOCKET" kill-session -t "$AGENTMUX_TMUX_SESSION_NAME" 2>/dev/null || true
else
    log "$AGENTMUX_TMUX_SESSION_NAME session is missing; starting it"
fi

"$SCRIPT_DIR/rc-start.sh"

sleep 5
if tmux -L "$AGENTMUX_TMUX_SOCKET" has-session -t "$AGENTMUX_TMUX_SESSION_NAME" 2>/dev/null; then
    log "$AGENTMUX_TMUX_SESSION_NAME session is up"
else
    log "ERROR: $AGENTMUX_TMUX_SESSION_NAME session did not come up"
    exit 1
fi
