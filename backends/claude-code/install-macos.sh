#!/bin/bash
# Installs the agentmux Claude Code backend on macOS using user LaunchAgents.
#
# Configure via flags or env vars. Flags win over env vars.
#
# Flags:
#   --tmux-session NAME     tmux session name
#   --display-name NAME     Claude display name
#   --workdir PATH          working directory for the session
#   --update-time HH:MM     local-time update schedule
#   --start-interval SEC    seconds between idempotent start checks
#   --path PATH             launchd PATH seed; detected tool dirs are prepended
#   --attach                attach to the tmux session after installing
#   --no-attach             do not attach after installing
#   --yes                   do not prompt before installing
#   --plan                  print the install plan without writing files
#   --help                  show usage
#
# Env aliases:
#   AGENTMUX_TMUX_SESSION_NAME, AGENTMUX_SESSION_NAME
#   AGENTMUX_DISPLAY_NAME, AGENTMUX_REMOTE_NAME
#   AGENTMUX_WORKDIR, AGENTMUX_UPDATE_TIME
#   AGENTMUX_UPDATE_HOUR, AGENTMUX_UPDATE_MINUTE
#   AGENTMUX_START_INTERVAL, AGENTMUX_PATH
#   AGENTMUX_ATTACH_AFTER_INSTALL
set -euo pipefail

usage() {
    cat <<'EOF'
Installs the agentmux Claude Code backend on macOS using user LaunchAgents.

Configure via flags or env vars. Flags win over env vars.

Flags:
  --tmux-session NAME     tmux session name
  --display-name NAME     Claude display name
  --workdir PATH          working directory for the session
  --update-time HH:MM     local-time update schedule
  --start-interval SEC    seconds between idempotent start checks
  --path PATH             launchd PATH seed; detected tool dirs are prepended
  --attach                attach to the tmux session after installing
  --no-attach             do not attach after installing
  --yes                   do not prompt before installing
  --plan                  print the install plan without writing files
  --help                  show usage

Env aliases:
  AGENTMUX_TMUX_SESSION_NAME, AGENTMUX_SESSION_NAME
  AGENTMUX_DISPLAY_NAME, AGENTMUX_REMOTE_NAME
  AGENTMUX_WORKDIR, AGENTMUX_UPDATE_TIME
  AGENTMUX_UPDATE_HOUR, AGENTMUX_UPDATE_MINUTE
  AGENTMUX_START_INTERVAL, AGENTMUX_PATH
  AGENTMUX_ATTACH_AFTER_INSTALL
EOF
}

if [ "$(uname -s)" != "Darwin" ]; then
    echo "install-macos.sh is only for macOS; use install.sh on Linux" >&2
    exit 1
fi

if [ "$(id -u)" -eq 0 ]; then
    echo "install-macos.sh should be run as your macOS user, not with sudo" >&2
    exit 1
fi

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
YES=0
PLAN=0
ATTACH_AFTER_INSTALL="${AGENTMUX_ATTACH_AFTER_INSTALL:-}"

machine_name() {
    local name=""

    if command -v scutil >/dev/null 2>&1; then
        name="$(scutil --get ComputerName 2>/dev/null || true)"
    fi
    if [ -z "$name" ]; then
        name="$(hostname -s 2>/dev/null || hostname 2>/dev/null || true)"
    fi
    if [ -z "$name" ]; then
        name="mac"
    fi

    printf '%s' "$name"
}

slugify() {
    printf '%s' "$1" |
        tr '[:upper:]' '[:lower:]' |
        sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//; s/-+/-/g'
}

default_tmux_session() {
    local base
    base="$(slugify "$(machine_name)")"
    if [ -z "$base" ]; then
        base="mac"
    fi

    printf '%s-claude-%s' "$base" "$(date +%Y-%m-%d)"
}

TMUX_SESSION_NAME="${AGENTMUX_TMUX_SESSION_NAME:-${AGENTMUX_SESSION_NAME:-$(default_tmux_session)}}"
DISPLAY_NAME="${AGENTMUX_DISPLAY_NAME:-${AGENTMUX_REMOTE_NAME:-$(machine_name) agentmux}}"
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

parse_update_time() {
    local value="$1"

    if [[ "$value" =~ ^([0-9]{1,2}):([0-9]{2})$ ]]; then
        UPDATE_HOUR="${BASH_REMATCH[1]}"
        UPDATE_MINUTE="${BASH_REMATCH[2]}"
    else
        echo "update time must be HH:MM, for example 03:00" >&2
        exit 1
    fi
}

if [ -n "${AGENTMUX_UPDATE_TIME:-}" ]; then
    parse_update_time "$AGENTMUX_UPDATE_TIME"
fi

while [ "$#" -gt 0 ]; do
    case "$1" in
        --tmux-session | --session-name)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            TMUX_SESSION_NAME="$2"
            shift 2
            ;;
        --display-name | --remote-name)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            DISPLAY_NAME="$2"
            shift 2
            ;;
        --workdir)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            WORKDIR="$2"
            shift 2
            ;;
        --update-time)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            parse_update_time "$2"
            shift 2
            ;;
        --start-interval)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            START_INTERVAL="$2"
            shift 2
            ;;
        --path)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            LAUNCHD_PATH="$2"
            shift 2
            ;;
        --attach)
            ATTACH_AFTER_INSTALL=1
            shift
            ;;
        --no-attach)
            ATTACH_AFTER_INSTALL=0
            shift
            ;;
        --yes | -y)
            YES=1
            shift
            ;;
        --plan)
            PLAN=1
            shift
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

validate_tmux_session() {
    if ! [[ "$TMUX_SESSION_NAME" =~ ^[A-Za-z0-9._-]+$ ]]; then
        echo "tmux session name must contain only letters, numbers, dots, underscores, and hyphens" >&2
        exit 1
    fi
}

validate_attach_after_install() {
    case "$ATTACH_AFTER_INSTALL" in
        0 | 1) ;;
        *)
            echo "AGENTMUX_ATTACH_AFTER_INSTALL must be 0 or 1" >&2
            exit 1
            ;;
    esac
}

prompt_value() {
    local label="$1"
    local default="$2"
    local value

    read -r -p "$label [$default]: " value
    printf '%s' "${value:-$default}"
}

confirm_install() {
    local answer

    read -r -p "Install LaunchAgents now? [Y/n]: " answer
    case "$answer" in
        "" | [Yy] | [Yy][Ee][Ss]) return 0 ;;
        *) return 1 ;;
    esac
}

confirm_attach() {
    local answer

    read -r -p "Attach to Claude Code now to finish first-run login/trust? [Y/n]: " answer
    case "$answer" in
        "" | [Yy] | [Yy][Ee][Ss]) return 0 ;;
        *) return 1 ;;
    esac
}

if [ "$PLAN" -eq 0 ] && [ "$YES" -eq 0 ] && [ -t 0 ]; then
    TMUX_SESSION_NAME="$(prompt_value "Tmux session name" "$TMUX_SESSION_NAME")"
    DISPLAY_NAME="$(prompt_value "Claude display name" "$DISPLAY_NAME")"
    parse_update_time "$(prompt_value "Update time" "$(printf '%02d:%02d' "$((10#$UPDATE_HOUR))" "$((10#$UPDATE_MINUTE))")")"

    if ! confirm_install; then
        echo "Cancelled."
        exit 0
    fi

    if [ -z "$ATTACH_AFTER_INSTALL" ]; then
        if confirm_attach; then
            ATTACH_AFTER_INSTALL=1
        else
            ATTACH_AFTER_INSTALL=0
        fi
    fi
fi

if [ -z "$ATTACH_AFTER_INSTALL" ]; then
    ATTACH_AFTER_INSTALL=0
fi

require_int_range AGENTMUX_UPDATE_HOUR "$UPDATE_HOUR" 0 23
require_int_range AGENTMUX_UPDATE_MINUTE "$UPDATE_MINUTE" 0 59
require_int_range AGENTMUX_START_INTERVAL "$START_INTERVAL" 60 86400
UPDATE_HOUR=$((10#$UPDATE_HOUR))
UPDATE_MINUTE=$((10#$UPDATE_MINUTE))
START_INTERVAL=$((10#$START_INTERVAL))
validate_tmux_session
validate_attach_after_install

prepend_path_dir() {
    local dir="$1"

    case ":$LAUNCHD_PATH:" in
        *":$dir:"*) ;;
        *) LAUNCHD_PATH="$dir:$LAUNCHD_PATH" ;;
    esac
}

MISSING_TOOLS=()
export PATH="$LAUNCHD_PATH:${PATH:-}"
for cmd in launchctl tmux claude; do
    if cmd_path=$(command -v "$cmd" 2>/dev/null); then
        prepend_path_dir "$(dirname "$cmd_path")"
    else
        MISSING_TOOLS+=("$cmd")
    fi
done
export PATH="$LAUNCHD_PATH:${PATH:-}"

print_plan() {
    echo "agentmux claude-code macOS install plan:"
    echo "  tmux session : $TMUX_SESSION_NAME"
    echo "  display name : $DISPLAY_NAME"
    echo "  workdir      : $WORKDIR"
    echo "  update time  : $(printf '%02d:%02d' "$UPDATE_HOUR" "$UPDATE_MINUTE") local"
    echo "  start check  : every ${START_INTERVAL}s"
    echo "  start label  : $START_LABEL"
    echo "  update label : $UPDATE_LABEL"
    echo "  start plist  : $START_PLIST"
    echo "  update plist : $UPDATE_PLIST"
    echo "  logs         : $LOG_DIR"
    echo "  attach after : $ATTACH_AFTER_INSTALL"
    if [ "${#MISSING_TOOLS[@]}" -gt 0 ]; then
        echo "  missing tools: ${MISSING_TOOLS[*]}"
    fi
}

if [ "$PLAN" -eq 1 ]; then
    print_plan
    exit 0
fi

if [ "${#MISSING_TOOLS[@]}" -gt 0 ]; then
    echo "missing required tools: ${MISSING_TOOLS[*]}" >&2
    echo "PATH checked for launchd: $LAUNCHD_PATH" >&2
    exit 1
fi

wait_for_tmux_session() {
    local attempts=40

    while [ "$attempts" -gt 0 ]; do
        if tmux has-session -t "$TMUX_SESSION_NAME" 2>/dev/null; then
            return 0
        fi
        attempts=$((attempts - 1))
        sleep 0.25
    done

    return 1
}

attach_tmux_session() {
    if [ ! -t 0 ]; then
        echo "Cannot attach because stdin is not a terminal." >&2
        return 1
    fi

    if ! wait_for_tmux_session; then
        echo "Cannot attach because tmux session '$TMUX_SESSION_NAME' is not running yet." >&2
        return 1
    fi

    if [ -n "${TMUX:-}" ]; then
        exec tmux switch-client -t "$TMUX_SESSION_NAME"
    fi

    exec tmux attach -t "$TMUX_SESSION_NAME"
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
        -e "s|@@SESSION_NAME@@|$(template_value "$TMUX_SESSION_NAME")|g" \
        -e "s|@@TMUX_SESSION_NAME@@|$(template_value "$TMUX_SESSION_NAME")|g" \
        -e "s|@@DISPLAY_NAME@@|$(template_value "$DISPLAY_NAME")|g" \
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

echo "Installing agentmux claude-code backend for macOS:"
echo "  tmux session : $TMUX_SESSION_NAME"
echo "  display name : $DISPLAY_NAME"
echo "  update time  : $(printf '%02d:%02d' "$UPDATE_HOUR" "$UPDATE_MINUTE") local"
echo "  start check  : every ${START_INTERVAL}s"
echo "  attach after : $ATTACH_AFTER_INSTALL"
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
echo "Done. Reattach with: tmux attach -t $TMUX_SESSION_NAME"
echo "First run may ask you to trust '$WORKDIR'; attach once and confirm it if prompted."
echo "Logs: tail -f '$LOG_DIR/claude-code.log' '$LOG_DIR/claude-code.err.log'"
echo "Status: launchctl print '$DOMAIN/$START_LABEL'"

if [ "$ATTACH_AFTER_INSTALL" -eq 1 ]; then
    echo
    echo "Attaching to Claude Code now. Detach later with: Ctrl-b then d"
    attach_tmux_session
fi
