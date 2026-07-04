#!/bin/bash
# Removes one agentmux macOS LaunchAgent instance. Does not kill a currently
# running tmux session or delete the instance workdir.
set -euo pipefail

usage() {
    cat <<'EOF'
Removes one agentmux macOS LaunchAgent instance.

Flags:
  --instance NAME    instance name (default: $AGENTMUX_INSTANCE_NAME or agentmux)
  --help            show usage
EOF
}

if [ "$(uname -s)" != "Darwin" ]; then
    echo "uninstall-macos.sh is only for macOS; use uninstall.sh on Linux" >&2
    exit 1
fi

if [ "$(id -u)" -eq 0 ]; then
    echo "uninstall-macos.sh should be run as your macOS user, not with sudo" >&2
    exit 1
fi

INSTANCE_NAME="${AGENTMUX_INSTANCE_NAME:-agentmux}"

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

LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
START_LABEL="com.agentmux.$INSTANCE_NAME"
UPDATE_LABEL="com.agentmux.$INSTANCE_NAME.update"
START_PLIST="$LAUNCH_AGENTS_DIR/$START_LABEL.plist"
UPDATE_PLIST="$LAUNCH_AGENTS_DIR/$UPDATE_LABEL.plist"
DOMAIN="gui/$(id -u)"

launchctl bootout "$DOMAIN" "$UPDATE_PLIST" 2>/dev/null || true
launchctl bootout "$DOMAIN" "$START_PLIST" 2>/dev/null || true
rm -f "$UPDATE_PLIST" "$START_PLIST"

echo "Removed agentmux instance $INSTANCE_NAME LaunchAgents. Tmux session and workdir were left alone."
