#!/bin/bash
# Periodic maintenance for one configured agentmux instance.
#
# On Linux this runs as root from the systemd *-update.service unit (no
# User= there, since it needs to call systemctl), so the actual agent CLI
# and tmux session — which belong to AGENTMUX_RUN_USER — are driven through
# sudo -u below. On macOS this already runs as the target user from a
# LaunchAgent, so no user-switching is needed there.
set -uo pipefail

: "${AGENTMUX_INSTANCE_NAME:=agentmux}"
: "${AGENTMUX_AGENT:=zero}"
: "${AGENTMUX_PROVIDER:=ollama}"
: "${AGENTMUX_TMUX_SESSION_NAME:=${AGENTMUX_SESSION_NAME:-$AGENTMUX_INSTANCE_NAME}}"
: "${AGENTMUX_TMUX_SOCKET:=agentmux-$AGENTMUX_INSTANCE_NAME}"
: "${AGENTMUX_WORKDIR:=$HOME/.agentmux/$AGENTMUX_INSTANCE_NAME}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "$(id -u)" -eq 0 ]; then
    RUN_USER="${AGENTMUX_RUN_USER:?AGENTMUX_RUN_USER must be set when running as root}"
    SERVICE_NAME="${AGENTMUX_SERVICE_NAME:?AGENTMUX_SERVICE_NAME must be set when running as root}"
    RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"
    AS_USER=(sudo -u "$RUN_USER" env "PATH=$RUN_HOME/.local/bin:$RUN_HOME/.npm-global/bin:/usr/local/bin:/usr/bin:/bin" "HOME=$RUN_HOME")
else
    AS_USER=()
fi

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
        zero) "${AS_USER[@]}" zero --version 2>&1 ;;
        opencode) "${AS_USER[@]}" opencode --version 2>&1 ;;
        *) echo "unsupported agent: $AGENTMUX_AGENT"; return 1 ;;
    esac
}

update_agent() {
    case "$AGENTMUX_AGENT" in
        zero)
            "${AS_USER[@]}" zero update --check
            ;;
        opencode)
            "${AS_USER[@]}" opencode upgrade --method npm
            ;;
        *)
            log "unsupported agent: $AGENTMUX_AGENT"
            return 1
            ;;
    esac
}

session_alive() {
    "${AS_USER[@]}" tmux -L "$AGENTMUX_TMUX_SOCKET" has-session -t "$AGENTMUX_TMUX_SESSION_NAME" 2>/dev/null
}

restart_instance() {
    if [ "$(id -u)" -eq 0 ]; then
        systemctl restart "$SERVICE_NAME"
    else
        "$SCRIPT_DIR/rc-start.sh"
    fi
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

if [ "$BEFORE" = "$AFTER" ] && session_alive; then
    log "no version change and $AGENTMUX_TMUX_SESSION_NAME session is already running"
    exit 0
fi

if [ "$BEFORE" != "$AFTER" ]; then
    log "restarting $AGENTMUX_TMUX_SESSION_NAME ($BEFORE -> $AFTER)"
else
    log "$AGENTMUX_TMUX_SESSION_NAME session is missing; restarting to bring it back"
fi
restart_instance

sleep 5
if session_alive; then
    log "$AGENTMUX_TMUX_SESSION_NAME session is up"
else
    log "ERROR: $AGENTMUX_TMUX_SESSION_NAME session did not come up"
    exit 1
fi
