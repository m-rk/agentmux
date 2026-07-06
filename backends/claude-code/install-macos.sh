#!/bin/bash
# Installs the agentmux Claude Code backend on macOS using user LaunchAgents.
#
# Configure via flags or env vars. Flags win over env vars.
#
# Flags:
#   --instance NAME         instance name (default: claude-code)
#   --tmux-session NAME     tmux session name
#   --display-name NAME     Claude display name
#   --workdir PATH          working directory for the session
#   --update-time HH:MM     local-time update schedule
#   --start-interval SEC    seconds between idempotent start checks
#   --path PATH             launchd PATH seed; detected tool dirs are prepended
#   --no-suffix             don't append " agentmux" to the display name
#   --attach                attach to the tmux session after installing
#   --no-attach             do not attach after installing
#   --yes                   do not prompt before installing
#   --plan                  print the install plan without writing files
#   --help                  show usage
#
# Env aliases:
#   AGENTMUX_INSTANCE_NAME (default: claude-code)
#   AGENTMUX_TMUX_SESSION_NAME, AGENTMUX_SESSION_NAME
#   AGENTMUX_DISPLAY_NAME, AGENTMUX_REMOTE_NAME
#   AGENTMUX_DISPLAY_SUFFIX (default: 1; set 0/false/no/off to disable)
#   AGENTMUX_WORKDIR, AGENTMUX_UPDATE_TIME
#   AGENTMUX_UPDATE_HOUR, AGENTMUX_UPDATE_MINUTE
#   AGENTMUX_START_INTERVAL, AGENTMUX_PATH
#   AGENTMUX_ATTACH_AFTER_INSTALL
#
# The instance name defaults to "claude-code" so a zero-flag install
# reproduces today's exact LaunchAgent labels, workdir, and log paths.
# Passing --instance NAME (a second, third, ... instance) installs
# side-by-side with its own labels, workdir, and default tmux
# session/display name derived from NAME instead.
#
# The display name defaults to "<user>:<host> 🤹 <workdir-basename>" when
# unset, which already self-identifies as an agentmux session, so no suffix
# is added to it. An explicit name (flag, env var, or typed at the prompt)
# still gets " agentmux" appended unless --no-suffix / AGENTMUX_DISPLAY_SUFFIX=0
# is given.
set -euo pipefail

usage() {
    cat <<'EOF'
Installs the agentmux Claude Code backend on macOS using user LaunchAgents.

Configure via flags or env vars. Flags win over env vars.

Flags:
  --instance NAME         instance name (default: claude-code)
  --tmux-session NAME     tmux session name
  --display-name NAME     Claude display name
  --workdir PATH          working directory for the session
  --update-time HH:MM     local-time update schedule
  --start-interval SEC    seconds between idempotent start checks
  --path PATH             launchd PATH seed; detected tool dirs are prepended
  --no-suffix             don't append " agentmux" to the display name
  --attach                attach to the tmux session after installing
  --no-attach             do not attach after installing
  --yes                   do not prompt before installing
  --plan                  print the install plan without writing files
  --help                  show usage

Env aliases:
  AGENTMUX_INSTANCE_NAME (default: claude-code)
  AGENTMUX_TMUX_SESSION_NAME, AGENTMUX_SESSION_NAME
  AGENTMUX_DISPLAY_NAME, AGENTMUX_REMOTE_NAME
  AGENTMUX_DISPLAY_SUFFIX (default: 1; set 0/false/no/off to disable)
  AGENTMUX_WORKDIR, AGENTMUX_UPDATE_TIME
  AGENTMUX_UPDATE_HOUR, AGENTMUX_UPDATE_MINUTE
  AGENTMUX_START_INTERVAL, AGENTMUX_PATH
  AGENTMUX_ATTACH_AFTER_INSTALL

A second (or third, ...) instance can be installed side by side with
--instance NAME (and typically --workdir), producing distinct LaunchAgent
labels, workdir, logs, and (by default) tmux session/display name.
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

    # LocalHostName is the mDNS/Bonjour name (e.g. "harley-mini", the
    # <name> in <name>.local) -- distinct from the free-text ComputerName
    # (e.g. "Harley Mini") shown in System Settings, which isn't a valid
    # hostname (can contain spaces) and needn't match it.
    if command -v scutil >/dev/null 2>&1; then
        name="$(scutil --get LocalHostName 2>/dev/null || true)"
    fi
    if [ -z "$name" ]; then
        name="$(hostname -s 2>/dev/null || hostname 2>/dev/null || true)"
    fi
    name="${name%.local}"
    if [ -z "$name" ]; then
        name="mac"
    fi

    printf '%s' "$name"
}

# Real (human) user accounts only -- excludes macOS system/service accounts,
# which all have UID < 500 by convention.
real_user_count() {
    command -v dscl >/dev/null 2>&1 || { echo 0; return; }
    dscl . -list /Users UniqueID 2>/dev/null | awk '{if ($2+0 >= 500) c++} END {print c+0}'
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

default_display_name() {
    local user_prefix=""
    if [ "$(real_user_count)" != "1" ]; then
        user_prefix="$(id -un):"
    fi
    printf '%s%s 🤹 %s' "$user_prefix" "$(machine_name)" "$(basename "$WORKDIR")"
}

apply_display_suffix() {
    local name="$1"
    if [ "$DISPLAY_SUFFIX_ENABLED" -eq 1 ] && [[ "$name" != *" agentmux" ]]; then
        printf '%s agentmux' "$name"
    else
        printf '%s' "$name"
    fi
}

DEFAULT_INSTANCE_NAME="claude-code"
INSTANCE_NAME="${AGENTMUX_INSTANCE_NAME:-$DEFAULT_INSTANCE_NAME}"
TMUX_SESSION_NAME="${AGENTMUX_TMUX_SESSION_NAME:-${AGENTMUX_SESSION_NAME:-}}"
RAW_DISPLAY_NAME="${AGENTMUX_DISPLAY_NAME:-${AGENTMUX_REMOTE_NAME:-}}"
DISPLAY_SUFFIX_ENABLED=1
case "${AGENTMUX_DISPLAY_SUFFIX:-1}" in
    0 | false | no | off) DISPLAY_SUFFIX_ENABLED=0 ;;
esac
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
        --tmux-session | --session-name)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            TMUX_SESSION_NAME="$2"
            shift 2
            ;;
        --display-name | --remote-name)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            RAW_DISPLAY_NAME="$2"
            shift 2
            ;;
        --no-suffix)
            DISPLAY_SUFFIX_ENABLED=0
            shift
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

if [ -z "$TMUX_SESSION_NAME" ]; then
    if [ "$INSTANCE_NAME" = "$DEFAULT_INSTANCE_NAME" ]; then
        TMUX_SESSION_NAME="$(default_tmux_session)"
    else
        TMUX_SESSION_NAME="$INSTANCE_NAME"
    fi
fi

WORKDIR="${WORKDIR:-$HOME/.agentmux/$INSTANCE_NAME}"
START_LABEL="com.agentmux.$INSTANCE_NAME"
UPDATE_LABEL="com.agentmux.$INSTANCE_NAME.update"
START_PLIST="$LAUNCH_AGENTS_DIR/$START_LABEL.plist"
UPDATE_PLIST="$LAUNCH_AGENTS_DIR/$UPDATE_LABEL.plist"

# Default display name is "<user>:<host> 🤹 <workdir-basename>" — already
# self-identifying (the 🤹 marks it as an agentmux session), so it's left
# unsuffixed unlike an explicit --display-name, which still gets " agentmux"
# appended (unless --no-suffix) to distinguish it from an unrelated name.
if [ -n "$RAW_DISPLAY_NAME" ]; then
    DISPLAY_NAME="$(apply_display_suffix "$RAW_DISPLAY_NAME")"
else
    DISPLAY_NAME="$(default_display_name)"
fi

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
    PROMPTED_DISPLAY_NAME="$(prompt_value "Claude display name" "$DISPLAY_NAME")"
    if [ "$PROMPTED_DISPLAY_NAME" != "$DISPLAY_NAME" ]; then
        DISPLAY_NAME="$(apply_display_suffix "$PROMPTED_DISPLAY_NAME")"
    fi
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
validate_identifier "instance name" "$INSTANCE_NAME"
validate_identifier "tmux session name" "$TMUX_SESSION_NAME"
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
    echo "  instance     : $INSTANCE_NAME"
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

find_claude_json() {
    # The real path is $HOME/.claude.json (confirmed directly, not
    # assumed). $HOME/.claude/.claude.json is checked as a fallback in
    # case some other Claude Code version/config uses that nesting.
    if [ -f "$HOME/.claude.json" ]; then
        printf '%s' "$HOME/.claude.json"
    elif [ -f "$HOME/.claude/.claude.json" ]; then
        printf '%s' "$HOME/.claude/.claude.json"
    fi
}

claude_is_logged_in() {
    local claude_json
    claude_json="$(find_claude_json)"
    if [ -n "$claude_json" ] && command -v python3 >/dev/null 2>&1; then
        if python3 -c "
import json, sys
try:
    with open(sys.argv[1]) as f:
        d = json.load(f)
    sys.exit(0 if d.get('oauthAccount') or d.get('userID') else 1)
except Exception:
    sys.exit(1)
" "$claude_json"; then
            return 0
        fi
    fi

    # Current Claude Code versions may keep credentials in the OS keychain
    # with no on-disk JSON file at all; `claude auth status` is the
    # authoritative check regardless of where credentials are stored.
    command -v claude >/dev/null 2>&1 || return 1
    command -v python3 >/dev/null 2>&1 || return 1
    local status_json
    status_json="$(claude auth status --json 2>/dev/null)" || return 1
    python3 -c "
import json, sys
try:
    d = json.loads(sys.argv[1])
    sys.exit(0 if d.get('loggedIn') else 1)
except Exception:
    sys.exit(1)
" "$status_json"
}

preaccept_workspace_trust() {
    local workdir="$1"
    local claude_json
    claude_json="$(find_claude_json)"

    [ -n "$claude_json" ] || return 0
    command -v python3 >/dev/null 2>&1 || return 0

    python3 - "$workdir" "$claude_json" <<'PYEOF'
import json, sys, os

workdir, path = sys.argv[1], sys.argv[2]
try:
    with open(path) as f:
        d = json.load(f)
    proj = d.setdefault('projects', {}).setdefault(workdir, {})
    if not proj.get('hasTrustDialogAccepted'):
        proj['hasTrustDialogAccepted'] = True
        tmp = path + '.tmp'
        with open(tmp, 'w') as f:
            json.dump(d, f, separators=(',', ':'))
        os.replace(tmp, path)
        print(f"  trust pre-accepted for {workdir}")
except Exception as e:
    sys.stderr.write(f"Warning: could not pre-accept workspace trust: {e}\n")
PYEOF
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
        -e "s|@@INSTANCE_NAME@@|$(template_value "$INSTANCE_NAME")|g" \
        -e "s|@@SESSION_NAME@@|$(template_value "$TMUX_SESSION_NAME")|g" \
        -e "s|@@TMUX_SESSION_NAME@@|$(template_value "$TMUX_SESSION_NAME")|g" \
        -e "s|@@DISPLAY_NAME@@|$(template_value "$DISPLAY_NAME")|g" \
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

if ! claude_is_logged_in; then
    echo "Claude Code does not appear to be logged in." >&2
    echo "Run 'claude' once in your terminal to complete login, then rerun this installer." >&2
    exit 1
fi

echo "Installing agentmux claude-code backend for macOS:"
echo "  instance     : $INSTANCE_NAME"
echo "  tmux session : $TMUX_SESSION_NAME"
echo "  display name : $DISPLAY_NAME"
echo "  update time  : $(printf '%02d:%02d' "$UPDATE_HOUR" "$UPDATE_MINUTE") local"
echo "  start check  : every ${START_INTERVAL}s"
echo "  attach after : $ATTACH_AFTER_INSTALL"
echo "  repo dir     : $REPO_DIR"

chmod +x "$REPO_DIR/rc-start.sh" "$REPO_DIR/rc-update.sh"
mkdir -p "$LAUNCH_AGENTS_DIR" "$LOG_DIR" "$WORKDIR"
preaccept_workspace_trust "$WORKDIR"

launchctl bootout "$DOMAIN" "$UPDATE_PLIST" 2>/dev/null || true
launchctl bootout "$DOMAIN" "$START_PLIST" 2>/dev/null || true

render "$REPO_DIR/claude-code.plist.tmpl" "$START_PLIST"
render "$REPO_DIR/claude-code.update.plist.tmpl" "$UPDATE_PLIST"

launchctl bootstrap "$DOMAIN" "$START_PLIST"
launchctl bootstrap "$DOMAIN" "$UPDATE_PLIST"
launchctl kickstart -k "$DOMAIN/$START_LABEL" 2>/dev/null || true

echo
echo "Done. Reattach with: tmux attach -t $TMUX_SESSION_NAME"
echo "Logs: tail -f '$LOG_DIR/$INSTANCE_NAME.log' '$LOG_DIR/$INSTANCE_NAME.err.log'"
echo "Status: launchctl print '$DOMAIN/$START_LABEL'"

if [ "$ATTACH_AFTER_INSTALL" -eq 1 ]; then
    echo
    echo "Attaching to Claude Code now. Detach later with: Ctrl-b then d"
    attach_tmux_session
fi
