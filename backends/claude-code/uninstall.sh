#!/bin/bash
# Removes an agentmux Claude Code backend instance's systemd units. Does not
# kill a currently running tmux session or touch the instance's workdir.
set -euo pipefail

usage() {
    cat <<'EOF'
Removes an agentmux Claude Code backend instance's systemd units.

Flags:
  --instance NAME    instance name (default: $AGENTMUX_INSTANCE_NAME or claude-code)
  --help             show usage
EOF
}

if [ "$(id -u)" -ne 0 ]; then
    echo "uninstall.sh must be run with sudo" >&2
    exit 1
fi

INSTANCE_NAME="${AGENTMUX_INSTANCE_NAME:-claude-code}"

while [ "$#" -gt 0 ]; do
    case "$1" in
        --instance)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            INSTANCE_NAME="$2"
            shift 2
            ;;
        --help | -h)
            usage
            exit 0
            ;;
        *)
            echo "unknown option: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
done

if ! [[ "$INSTANCE_NAME" =~ ^[A-Za-z0-9._-]+$ ]]; then
    echo "instance name must contain only letters, numbers, dots, underscores, and hyphens" >&2
    exit 1
fi

SERVICE_NAME="agentmux-$INSTANCE_NAME.service"
UPDATE_SERVICE_NAME="agentmux-$INSTANCE_NAME-update.service"
TIMER_NAME="agentmux-$INSTANCE_NAME-update.timer"

systemctl disable --now "$TIMER_NAME" 2>/dev/null || true
systemctl disable --now "$SERVICE_NAME" 2>/dev/null || true
rm -f "/etc/systemd/system/$SERVICE_NAME"
rm -f "/etc/systemd/system/$UPDATE_SERVICE_NAME"
rm -f "/etc/systemd/system/$TIMER_NAME"
rm -f "/etc/agentmux/$INSTANCE_NAME.env"
systemctl daemon-reload

echo "Removed agentmux claude-code instance $INSTANCE_NAME systemd units. Tmux session (if running) was left alone."
