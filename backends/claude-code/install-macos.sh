#!/bin/bash
# Installs the agentmux Claude Code backend on macOS using user LaunchAgents.
#
# Configure via env vars (all optional):
#   AGENTMUX_SESSION_NAME    tmux session name / Remote Control display name (default: agentmux)
#   AGENTMUX_WORKDIR         working directory for the session (default: ~/.agentmux/claude-code)
#   AGENTMUX_UPDATE_HOUR     local-hour update schedule, 0-23 (default: 3)
#   AGENTMUX_UPDATE_MINUTE   local-minute update schedule, 0-59 (default: 0)
#   AGENTMUX_START_INTERVAL  seconds between idempotent start checks (default: 300)
#   AGENTMUX_PATH            launchd PATH seed; detected tool dirs are prepended
#
# Example:
#   AGENTMUX_SESSION_NAME="my-mac" AGENTMUX_UPDATE_HOUR=3 ./install-macos.sh
set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
    echo "install-macos.sh is only for macOS; use install.sh on Linux" >&2
    exit 1
fi

if [ "$(id -u)" -eq 0 ]; then
    echo "install-macos.sh should be run as your macOS user, not with sudo" >&2
    exit 1
fi

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SESSION_NAME="${AGENTMUX_SESSION_NAME:-agentmux}"
WORKDIR="${AGENTMUX_WORKDIR:-$HOME/.agentmux/claude-code}"
UPDATE_HOUR="${AGENTMUX_UPDATE_HOUR:-3}"
UPDATE_MINUTE="${AGENTMUX_UPDATE_MINUTE:-0}"
START_INTERVAL="${AGENTMUX_START_INTERVAL:-300}"
LAUNCHD_PATH="${AGENTMUX_PATH:-$HOME/.local/bin:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin}"
LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
LOG_DIR="$HOME/Library/Logs/agentmux"
START_LABEL="com.agentmux.claude-code"
UPDATE_LABEL="com.agentmux.claude-code.update"
START_PLIST="$LAUNCH_AGENTS_DIR/$START_LABEL.plist"
UPDATE_PLIST="$LAUNCH_AGENTS_DIR/$UPDATE_LABEL.plist"
DOMAIN="gui/$(id -u)"

export PATH="$LAUNCHD_PATH:${PATH:-}"

require_int_range() {
    local name="$1"
    local value="$2"
    local min="$3"
    local max="$4"

    if ! [[ "$value" =~ ^[0-9]+$ ]] || [ "$value" -lt "$min" ] || [ "$value" -gt "$max" ]; then
        echo "$name must be an integer from $min to $max" >&2
        exit 1
    fi
}

prepend_path_dir() {
    local dir="$1"

    case ":$LAUNCHD_PATH:" in
        *":$dir:"*) ;;
        *) LAUNCHD_PATH="$dir:$LAUNCHD_PATH" ;;
    esac
}

xml_escape() {
    local value="$1"
    value=${value//&/&amp;}
    value=${value//</&lt;}
    value=${value//>/&gt;}
    value=${value//\"/&quot;}
    value=${value//\'/&apos;}
    printf '%s' "$value"
}

sed_escape() {
    sed -e 's/[\/&|]/\\&/g'
}

template_value() {
    xml_escape "$1" | sed_escape
}

render() {
    local source="$1"
    local target="$2"

    sed \
        -e "s|@@SESSION_NAME@@|$(template_value "$SESSION_NAME")|g" \
        -e "s|@@WORKDIR@@|$(template_value "$WORKDIR")|g" \
        -e "s|@@HOME@@|$(template_value "$HOME")|g" \
        -e "s|@@PATH@@|$(template_value "$LAUNCHD_PATH")|g" \
        -e "s|@@LOG_DIR@@|$(template_value "$LOG_DIR")|g" \
        -e "s|@@REPO_DIR@@|$(template_value "$REPO_DIR")|g" \
        -e "s|@@UPDATE_HOUR@@|$UPDATE_HOUR|g" \
        -e "s|@@UPDATE_MINUTE@@|$UPDATE_MINUTE|g" \
        -e "s|@@START_INTERVAL@@|$START_INTERVAL|g" \
        "$source" > "$target"
}

require_int_range AGENTMUX_UPDATE_HOUR "$UPDATE_HOUR" 0 23
require_int_range AGENTMUX_UPDATE_MINUTE "$UPDATE_MINUTE" 0 59
require_int_range AGENTMUX_START_INTERVAL "$START_INTERVAL" 60 86400
UPDATE_HOUR=$((10#$UPDATE_HOUR))
UPDATE_MINUTE=$((10#$UPDATE_MINUTE))
START_INTERVAL=$((10#$START_INTERVAL))

for cmd in launchctl tmux claude; do
    if ! cmd_path=$(command -v "$cmd" 2>/dev/null); then
        echo "$cmd is not installed or not on PATH for launchd: $LAUNCHD_PATH" >&2
        exit 1
    fi
    prepend_path_dir "$(dirname "$cmd_path")"
done
export PATH="$LAUNCHD_PATH:${PATH:-}"

echo "Installing agentmux claude-code backend for macOS:"
echo "  session name : $SESSION_NAME"
echo "  update time  : $(printf '%02d:%02d' "$UPDATE_HOUR" "$UPDATE_MINUTE") local"
echo "  start check  : every ${START_INTERVAL}s"
echo "  repo dir     : $REPO_DIR"

chmod +x "$REPO_DIR/rc-start.sh" "$REPO_DIR/rc-update.sh"
mkdir -p "$LAUNCH_AGENTS_DIR" "$LOG_DIR"

launchctl bootout "$DOMAIN" "$UPDATE_PLIST" 2>/dev/null || true
launchctl bootout "$DOMAIN" "$START_PLIST" 2>/dev/null || true

render "$REPO_DIR/com.agentmux.claude-code.plist.tmpl" "$START_PLIST"
render "$REPO_DIR/com.agentmux.claude-code.update.plist.tmpl" "$UPDATE_PLIST"

launchctl bootstrap "$DOMAIN" "$START_PLIST"
launchctl bootstrap "$DOMAIN" "$UPDATE_PLIST"
launchctl kickstart -k "$DOMAIN/$START_LABEL" 2>/dev/null || true

echo
echo "Done. Reattach with: tmux attach -t $SESSION_NAME"
echo "First run may ask you to trust '$WORKDIR'; attach once and confirm it if prompted."
echo "Logs: tail -f '$LOG_DIR/claude-code.log' '$LOG_DIR/claude-code.err.log'"
echo "Status: launchctl print '$DOMAIN/$START_LABEL'"
