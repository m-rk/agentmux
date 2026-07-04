#!/bin/bash
# Installs one agentmux instance on macOS using user LaunchAgents.
set -euo pipefail

usage() {
    cat <<'EOF'
Installs one agentmux instance on macOS using user LaunchAgents.

An instance is an agent CLI + provider + model + workdir + tmux session.

Flags:
  --instance NAME              instance name and default tmux session
  --agent NAME                 agent CLI: zero or opencode
  --provider NAME              model provider: ollama
  --model MODEL                provider model id/tag
  --provider-base-url URL      provider OpenAI-compatible base URL
  --provider-wait-seconds SEC  seconds to wait for provider at start
  --tmux-session NAME          tmux session name
  --workdir PATH               working directory for this instance
  --update-time HH:MM          local-time update schedule
  --start-interval SEC         seconds between idempotent start checks
  --path PATH                  launchd PATH seed; detected tool dirs are prepended
  --yes                        do not prompt before installing
  --plan                       print the install plan without writing files
  --help                       show usage

Env aliases:
  AGENTMUX_INSTANCE_NAME, AGENTMUX_AGENT, AGENTMUX_PROVIDER, AGENTMUX_MODEL
  AGENTMUX_PROVIDER_BASE_URL, AGENTMUX_PROVIDER_WAIT_SECONDS
  AGENTMUX_TMUX_SESSION_NAME, AGENTMUX_SESSION_NAME
  AGENTMUX_WORKDIR, AGENTMUX_UPDATE_TIME
  AGENTMUX_UPDATE_HOUR, AGENTMUX_UPDATE_MINUTE
  AGENTMUX_START_INTERVAL, AGENTMUX_PATH
  AGENTMUX_OLLAMA_MODEL and AGENTMUX_OLLAMA_BASE_URL are compatibility aliases.
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

DEFAULT_INSTANCE="$(slugify "$(machine_name)")-agentmux"
INSTANCE_NAME="${AGENTMUX_INSTANCE_NAME:-$DEFAULT_INSTANCE}"
AGENT="${AGENTMUX_AGENT:-zero}"
PROVIDER="${AGENTMUX_PROVIDER:-ollama}"
MODEL="${AGENTMUX_MODEL:-${AGENTMUX_OLLAMA_MODEL:-gpt-oss:20b-cloud}}"
PROVIDER_BASE_URL="${AGENTMUX_PROVIDER_BASE_URL:-${AGENTMUX_OLLAMA_BASE_URL:-}}"
PROVIDER_WAIT_SECONDS="${AGENTMUX_PROVIDER_WAIT_SECONDS:-${AGENTMUX_OLLAMA_WAIT_SECONDS:-60}}"
TMUX_SESSION_NAME="${AGENTMUX_TMUX_SESSION_NAME:-${AGENTMUX_SESSION_NAME:-}}"
WORKDIR="${AGENTMUX_WORKDIR:-}"
UPDATE_HOUR="${AGENTMUX_UPDATE_HOUR:-3}"
UPDATE_MINUTE="${AGENTMUX_UPDATE_MINUTE:-0}"
START_INTERVAL="${AGENTMUX_START_INTERVAL:-300}"
LAUNCHD_PATH="${AGENTMUX_PATH:-$HOME/.local/bin:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin}"
LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"
LOG_DIR="$HOME/Library/Logs/agentmux"
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
        --instance)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            INSTANCE_NAME="$2"
            shift 2
            ;;
        --agent)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            AGENT="$2"
            shift 2
            ;;
        --provider)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            PROVIDER="$2"
            shift 2
            ;;
        --model)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            MODEL="$2"
            shift 2
            ;;
        --provider-base-url)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            PROVIDER_BASE_URL="$2"
            shift 2
            ;;
        --provider-wait-seconds)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            PROVIDER_WAIT_SECONDS="$2"
            shift 2
            ;;
        --tmux-session | --session-name)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            TMUX_SESSION_NAME="$2"
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

validate_identifier() {
    local label="$1"
    local value="$2"

    if ! [[ "$value" =~ ^[A-Za-z0-9._-]+$ ]]; then
        echo "$label must contain only letters, numbers, dots, underscores, and hyphens" >&2
        exit 1
    fi
}

validate_supported() {
    case "$AGENT:$PROVIDER" in
        zero:ollama | opencode:ollama) ;;
        *) echo "unsupported agent/provider combination: $AGENT/$PROVIDER" >&2; exit 1 ;;
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

if [ "$PLAN" -eq 0 ] && [ "$YES" -eq 0 ] && [ -t 0 ]; then
    INSTANCE_NAME="$(prompt_value "Instance name" "$INSTANCE_NAME")"
    AGENT="$(prompt_value "Agent" "$AGENT")"
    PROVIDER="$(prompt_value "Provider" "$PROVIDER")"
    MODEL="$(prompt_value "Model" "$MODEL")"
    TMUX_SESSION_NAME="${TMUX_SESSION_NAME:-$INSTANCE_NAME}"
    WORKDIR="${WORKDIR:-$HOME/.agentmux/$INSTANCE_NAME}"
    parse_update_time "$(prompt_value "Update time" "$(printf '%02d:%02d' "$((10#$UPDATE_HOUR))" "$((10#$UPDATE_MINUTE))")")"

    if ! confirm_install; then
        echo "Cancelled."
        exit 0
    fi
fi

case "$PROVIDER" in
    ollama)
        PROVIDER_BASE_URL="${PROVIDER_BASE_URL:-http://localhost:11434/v1}"
        ;;
esac
TMUX_SESSION_NAME="${TMUX_SESSION_NAME:-$INSTANCE_NAME}"
WORKDIR="${WORKDIR:-$HOME/.agentmux/$INSTANCE_NAME}"
START_LABEL="com.agentmux.$INSTANCE_NAME"
UPDATE_LABEL="com.agentmux.$INSTANCE_NAME.update"
START_PLIST="$LAUNCH_AGENTS_DIR/$START_LABEL.plist"
UPDATE_PLIST="$LAUNCH_AGENTS_DIR/$UPDATE_LABEL.plist"

require_int_range AGENTMUX_UPDATE_HOUR "$UPDATE_HOUR" 0 23
require_int_range AGENTMUX_UPDATE_MINUTE "$UPDATE_MINUTE" 0 59
require_int_range AGENTMUX_START_INTERVAL "$START_INTERVAL" 60 86400
require_int_range AGENTMUX_PROVIDER_WAIT_SECONDS "$PROVIDER_WAIT_SECONDS" 1 600
UPDATE_HOUR=$((10#$UPDATE_HOUR))
UPDATE_MINUTE=$((10#$UPDATE_MINUTE))
START_INTERVAL=$((10#$START_INTERVAL))
PROVIDER_WAIT_SECONDS=$((10#$PROVIDER_WAIT_SECONDS))
validate_identifier "instance name" "$INSTANCE_NAME"
validate_identifier "tmux session name" "$TMUX_SESSION_NAME"
validate_supported

prepend_path_dir() {
    local dir="$1"

    case ":$LAUNCHD_PATH:" in
        *":$dir:"*) ;;
        *) LAUNCHD_PATH="$dir:$LAUNCHD_PATH" ;;
    esac
}

MISSING_TOOLS=()
export PATH="$LAUNCHD_PATH:${PATH:-}"
for cmd in launchctl tmux "$AGENT"; do
    if cmd_path=$(command -v "$cmd" 2>/dev/null); then
        prepend_path_dir "$(dirname "$cmd_path")"
    else
        MISSING_TOOLS+=("$cmd")
    fi
done
if [ "$PROVIDER" = "ollama" ]; then
    if cmd_path=$(command -v ollama 2>/dev/null); then
        prepend_path_dir "$(dirname "$cmd_path")"
    else
        MISSING_TOOLS+=(ollama)
    fi
fi
export PATH="$LAUNCHD_PATH:${PATH:-}"

PROVIDER_REACHABLE=0
if [ "$PROVIDER" = "ollama" ] && command -v ollama >/dev/null 2>&1 && ollama list >/dev/null 2>&1; then
    PROVIDER_REACHABLE=1
fi

print_plan() {
    echo "agentmux macOS install plan:"
    echo "  instance      : $INSTANCE_NAME"
    echo "  agent         : $AGENT"
    echo "  provider      : $PROVIDER"
    echo "  model         : $MODEL"
    echo "  provider url  : $PROVIDER_BASE_URL"
    echo "  tmux session  : $TMUX_SESSION_NAME"
    echo "  workdir       : $WORKDIR"
    echo "  update time   : $(printf '%02d:%02d' "$UPDATE_HOUR" "$UPDATE_MINUTE") local"
    echo "  start check   : every ${START_INTERVAL}s"
    echo "  provider wait : ${PROVIDER_WAIT_SECONDS}s"
    echo "  start label   : $START_LABEL"
    echo "  update label  : $UPDATE_LABEL"
    echo "  logs          : $LOG_DIR"
    if [ "${#MISSING_TOOLS[@]}" -gt 0 ]; then
        echo "  missing tools : ${MISSING_TOOLS[*]}"
    fi
    if [ "$PROVIDER_REACHABLE" -eq 0 ]; then
        echo "  provider      : not reachable"
    else
        echo "  provider      : reachable"
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

if [ "$PROVIDER_REACHABLE" -eq 0 ]; then
    echo "$PROVIDER is installed but not reachable. Start it first, then run this installer again." >&2
    exit 1
fi

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
        -e "s|@@INSTANCE_NAME@@|$(template_value "$INSTANCE_NAME")|g" \
        -e "s|@@AGENT@@|$(template_value "$AGENT")|g" \
        -e "s|@@PROVIDER@@|$(template_value "$PROVIDER")|g" \
        -e "s|@@MODEL@@|$(template_value "$MODEL")|g" \
        -e "s|@@PROVIDER_BASE_URL@@|$(template_value "$PROVIDER_BASE_URL")|g" \
        -e "s|@@PROVIDER_WAIT_SECONDS@@|$PROVIDER_WAIT_SECONDS|g" \
        -e "s|@@TMUX_SESSION_NAME@@|$(template_value "$TMUX_SESSION_NAME")|g" \
        -e "s|@@WORKDIR@@|$(template_value "$WORKDIR")|g" \
        -e "s|@@START_LABEL@@|$(template_value "$START_LABEL")|g" \
        -e "s|@@UPDATE_LABEL@@|$(template_value "$UPDATE_LABEL")|g" \
        -e "s|@@HOME@@|$(template_value "$HOME")|g" \
        -e "s|@@PATH@@|$(template_value "$LAUNCHD_PATH")|g" \
        -e "s|@@LOG_DIR@@|$(template_value "$LOG_DIR")|g" \
        -e "s|@@REPO_DIR@@|$(template_value "$REPO_DIR")|g" \
        -e "s|@@UPDATE_HOUR@@|$UPDATE_HOUR|g" \
        -e "s|@@UPDATE_MINUTE@@|$UPDATE_MINUTE|g" \
        -e "s|@@START_INTERVAL@@|$START_INTERVAL|g" \
        "$source" > "$target"
}

echo "Installing agentmux instance for macOS:"
print_plan

chmod +x "$REPO_DIR/rc-start.sh" "$REPO_DIR/rc-update.sh"
mkdir -p "$LAUNCH_AGENTS_DIR" "$LOG_DIR" "$WORKDIR"

launchctl bootout "$DOMAIN" "$UPDATE_PLIST" 2>/dev/null || true
launchctl bootout "$DOMAIN" "$START_PLIST" 2>/dev/null || true

render "$REPO_DIR/agentmux.plist.tmpl" "$START_PLIST"
render "$REPO_DIR/agentmux.update.plist.tmpl" "$UPDATE_PLIST"

launchctl bootstrap "$DOMAIN" "$START_PLIST"
launchctl bootstrap "$DOMAIN" "$UPDATE_PLIST"
launchctl kickstart -k "$DOMAIN/$START_LABEL" 2>/dev/null || true

echo
echo "Done. Reattach with: tmux attach -t $TMUX_SESSION_NAME"
echo "Logs: tail -f '$LOG_DIR/$INSTANCE_NAME.log' '$LOG_DIR/$INSTANCE_NAME.err.log'"
echo "Status: launchctl print '$DOMAIN/$START_LABEL'"
