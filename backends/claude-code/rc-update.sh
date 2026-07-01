#!/bin/bash
# Nightly maintenance for an agentmux Claude Code session: updates the
# Claude Code CLI and, only if the version actually changed, restarts the
# session's systemd service so the running tmux session picks up the new
# binary. Runs as root (invoked by the *-update.service unit) but does the
# npm/claude work as AGENTMUX_RUN_USER so that user's global npm prefix stays
# owned by them.
set -uo pipefail

RUN_USER="${AGENTMUX_RUN_USER:?AGENTMUX_RUN_USER must be set}"
SERVICE_NAME="${AGENTMUX_SERVICE_NAME:?AGENTMUX_SERVICE_NAME must be set}"
SESSION_NAME="${AGENTMUX_SESSION_NAME:-agentmux}"
RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"

AS_USER=(sudo -u "$RUN_USER" env "PATH=$RUN_HOME/.npm-global/bin:/usr/bin:/bin" "HOME=$RUN_HOME")
log() { echo "[agentmux-claude-code-update] $*"; }

log "starting at $(date -Is)"

BEFORE=$("${AS_USER[@]}" claude --version 2>&1)
log "current version: $BEFORE"

if ! "${AS_USER[@]}" claude update; then
    log "claude update failed, leaving existing session running untouched"
    exit 1
fi

AFTER=$("${AS_USER[@]}" claude --version 2>&1)
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
