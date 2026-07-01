#!/bin/bash
# Installs the agentmux Claude Code backend on this host: a persistent tmux
# session with Claude Code's Remote Control enabled, kept alive across
# reboots by systemd, plus a nightly timer that updates the CLI and restarts
# the session only when the version actually changes.
#
# Must be run with sudo. Configure via env vars (all optional):
#   AGENTMUX_SESSION_NAME  tmux session name / Remote Control display name (default: agentmux)
#   AGENTMUX_RUN_USER      user the session runs as (default: $SUDO_USER)
#   AGENTMUX_ON_CALENDAR   systemd OnCalendar expression for the update timer
#                          (default: "*-*-* 03:00:00 UTC")
#
# Example:
#   sudo AGENTMUX_SESSION_NAME="my-server" AGENTMUX_ON_CALENDAR="*-*-* 03:00:00 Australia/Perth" ./install.sh
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must be run with sudo" >&2
    exit 1
fi

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_USER="${AGENTMUX_RUN_USER:-${SUDO_USER:-}}"
if [ -z "$RUN_USER" ]; then
    echo "Could not determine a user to run as; set AGENTMUX_RUN_USER explicitly" >&2
    exit 1
fi
SESSION_NAME="${AGENTMUX_SESSION_NAME:-agentmux}"
ON_CALENDAR="${AGENTMUX_ON_CALENDAR:-*-*-* 03:00:00 UTC}"
SERVICE_NAME="agentmux-claude-code.service"
ENV_DIR="/etc/agentmux"
ENV_FILE="$ENV_DIR/claude-code.env"

echo "Installing agentmux claude-code backend:"
echo "  session name : $SESSION_NAME"
echo "  run as       : $RUN_USER"
echo "  update timer : $ON_CALENDAR"
echo "  repo dir     : $REPO_DIR"

chmod +x "$REPO_DIR/rc-start.sh" "$REPO_DIR/rc-update.sh"

mkdir -p "$ENV_DIR"
cat > "$ENV_FILE" <<EOF
AGENTMUX_SESSION_NAME=$SESSION_NAME
AGENTMUX_RUN_USER=$RUN_USER
AGENTMUX_SERVICE_NAME=$SERVICE_NAME
EOF

render() {
    sed \
        -e "s|@@SESSION_NAME@@|$SESSION_NAME|g" \
        -e "s|@@RUN_USER@@|$RUN_USER|g" \
        -e "s|@@ENV_FILE@@|$ENV_FILE|g" \
        -e "s|@@REPO_DIR@@|$REPO_DIR|g" \
        -e "s|@@ON_CALENDAR@@|$ON_CALENDAR|g" \
        "$1" > "$2"
}

render "$REPO_DIR/agentmux-claude-code.service.tmpl" "/etc/systemd/system/agentmux-claude-code.service"
render "$REPO_DIR/agentmux-claude-code-update.service.tmpl" "/etc/systemd/system/agentmux-claude-code-update.service"
render "$REPO_DIR/agentmux-claude-code-update.timer.tmpl" "/etc/systemd/system/agentmux-claude-code-update.timer"

systemctl daemon-reload
systemctl enable --now agentmux-claude-code.service
systemctl enable --now agentmux-claude-code-update.timer

echo
echo "Done. Reattach with: sudo -u $RUN_USER tmux attach -t $SESSION_NAME"
echo "Update logs: journalctl -u agentmux-claude-code-update.service"
