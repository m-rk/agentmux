#!/bin/bash
# Installs one agentmux instance on Linux using systemd.
set -euo pipefail

usage() {
    cat <<'EOF'
Installs one agentmux instance on Linux using systemd.

Flags:
  --instance NAME              instance name and default tmux session
  --agent NAME                 agent CLI: zero or opencode
  --provider NAME              model provider: ollama
  --model MODEL                provider model id/tag
  --provider-base-url URL      provider OpenAI-compatible base URL
  --provider-wait-seconds SEC  seconds to wait for provider at start
  --tmux-session NAME          tmux session name
  --workdir PATH               working directory for this instance
  --run-user USER              user the session runs as
  --on-calendar EXPR           systemd OnCalendar expression for maintenance
  --plan                       print the install plan without writing files
  --help                       show usage
EOF
}

RAW_RUN_USER="${AGENTMUX_RUN_USER:-${SUDO_USER:-}}"
INSTANCE_NAME="${AGENTMUX_INSTANCE_NAME:-agentmux}"
AGENT="${AGENTMUX_AGENT:-zero}"
PROVIDER="${AGENTMUX_PROVIDER:-ollama}"
MODEL="${AGENTMUX_MODEL:-${AGENTMUX_OLLAMA_MODEL:-gpt-oss:20b-cloud}}"
PROVIDER_BASE_URL="${AGENTMUX_PROVIDER_BASE_URL:-${AGENTMUX_OLLAMA_BASE_URL:-}}"
PROVIDER_WAIT_SECONDS="${AGENTMUX_PROVIDER_WAIT_SECONDS:-${AGENTMUX_OLLAMA_WAIT_SECONDS:-60}}"
TMUX_SESSION_NAME="${AGENTMUX_TMUX_SESSION_NAME:-${AGENTMUX_SESSION_NAME:-}}"
WORKDIR="${AGENTMUX_WORKDIR:-}"
ON_CALENDAR="${AGENTMUX_ON_CALENDAR:-*-*-* 03:00:00 UTC}"
PLAN=0

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
        --run-user)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            RAW_RUN_USER="$2"
            shift 2
            ;;
        --on-calendar)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            ON_CALENDAR="$2"
            shift 2
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

if [ "$PLAN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
    echo "install.sh must be run with sudo (use --plan to preview without sudo)" >&2
    exit 1
fi

RUN_USER="$RAW_RUN_USER"
if [ -z "$RUN_USER" ]; then
    if [ "$PLAN" -eq 1 ]; then
        RUN_USER="$(id -un 2>/dev/null || true)"
    else
        echo "Could not determine a user to run as; set AGENTMUX_RUN_USER or --run-user explicitly" >&2
        exit 1
    fi
fi

RUN_HOME=""
if command -v getent >/dev/null 2>&1; then
    RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6 2>/dev/null || true)"
fi
if [ -z "$RUN_HOME" ]; then
    RUN_HOME="$(eval echo "~$RUN_USER" 2>/dev/null || printf '%s' "$HOME")"
fi

case "$PROVIDER" in
    ollama)
        PROVIDER_BASE_URL="${PROVIDER_BASE_URL:-http://localhost:11434/v1}"
        ;;
esac
TMUX_SESSION_NAME="${TMUX_SESSION_NAME:-$INSTANCE_NAME}"
WORKDIR="${WORKDIR:-$RUN_HOME/.agentmux/$INSTANCE_NAME}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVICE_NAME="agentmux-$INSTANCE_NAME.service"
UPDATE_SERVICE_NAME="agentmux-$INSTANCE_NAME-update.service"
TIMER_NAME="agentmux-$INSTANCE_NAME-update.timer"
ENV_DIR="/etc/agentmux"
ENV_FILE="$ENV_DIR/$INSTANCE_NAME.env"

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

validate_identifier "instance name" "$INSTANCE_NAME"
validate_identifier "tmux session name" "$TMUX_SESSION_NAME"
validate_supported

print_plan() {
    echo "agentmux Linux install plan:"
    echo "  instance      : $INSTANCE_NAME"
    echo "  agent         : $AGENT"
    echo "  provider      : $PROVIDER"
    echo "  model         : $MODEL"
    echo "  provider url  : $PROVIDER_BASE_URL"
    echo "  provider wait : ${PROVIDER_WAIT_SECONDS}s"
    echo "  tmux session  : $TMUX_SESSION_NAME"
    echo "  tmux socket   : agentmux-$INSTANCE_NAME"
    echo "  run as        : $RUN_USER"
    echo "  workdir       : $WORKDIR"
    echo "  update timer  : $ON_CALENDAR"
    echo "  service       : $SERVICE_NAME"
    echo "  repo dir      : $REPO_DIR"
}

if [ "$PLAN" -eq 1 ]; then
    print_plan
    exit 0
fi

if ! command -v "$AGENT" >/dev/null 2>&1; then
    echo "$AGENT is not installed or not on PATH" >&2
    exit 1
fi
if [ "$PROVIDER" = "ollama" ]; then
    if ! command -v ollama >/dev/null 2>&1; then
        echo "ollama is not installed. Install it first: curl -fsSL https://ollama.com/install.sh | sh" >&2
        exit 1
    fi
    if ! systemctl is-active --quiet ollama; then
        echo "ollama.service is not running. Start it first: sudo systemctl enable --now ollama" >&2
        exit 1
    fi
fi
if ! sudo -u "$RUN_USER" env "PATH=$RUN_HOME/.local/bin:$RUN_HOME/.npm-global/bin:/usr/local/bin:/usr/bin:/bin" HOME="$RUN_HOME" \
        bash -c "command -v '$AGENT'" >/dev/null 2>&1; then
    echo "$AGENT is not installed for $RUN_USER" >&2
    exit 1
fi

echo "Installing agentmux instance:"
print_plan

chmod +x "$REPO_DIR/rc-start.sh" "$REPO_DIR/rc-update.sh"
mkdir -p "$ENV_DIR" "$WORKDIR"
chown "$RUN_USER" "$WORKDIR"

cat > "$ENV_FILE" <<EOF
AGENTMUX_INSTANCE_NAME=$INSTANCE_NAME
AGENTMUX_AGENT=$AGENT
AGENTMUX_PROVIDER=$PROVIDER
AGENTMUX_MODEL=$MODEL
AGENTMUX_PROVIDER_BASE_URL=$PROVIDER_BASE_URL
AGENTMUX_PROVIDER_WAIT_SECONDS=$PROVIDER_WAIT_SECONDS
AGENTMUX_SESSION_NAME=$TMUX_SESSION_NAME
AGENTMUX_TMUX_SESSION_NAME=$TMUX_SESSION_NAME
AGENTMUX_WORKDIR=$WORKDIR
EOF

render() {
    sed \
        -e "s|@@INSTANCE_NAME@@|$INSTANCE_NAME|g" \
        -e "s|@@AGENT@@|$AGENT|g" \
        -e "s|@@PROVIDER@@|$PROVIDER|g" \
        -e "s|@@RUN_USER@@|$RUN_USER|g" \
        -e "s|@@ENV_FILE@@|$ENV_FILE|g" \
        -e "s|@@REPO_DIR@@|$REPO_DIR|g" \
        -e "s|@@TMUX_SESSION_NAME@@|$TMUX_SESSION_NAME|g" \
        -e "s|@@ON_CALENDAR@@|$ON_CALENDAR|g" \
        "$1" > "$2"
}

render "$REPO_DIR/agentmux.service.tmpl" "/etc/systemd/system/$SERVICE_NAME"
render "$REPO_DIR/agentmux-update.service.tmpl" "/etc/systemd/system/$UPDATE_SERVICE_NAME"
render "$REPO_DIR/agentmux-update.timer.tmpl" "/etc/systemd/system/$TIMER_NAME"

systemctl daemon-reload
systemctl enable --now "$SERVICE_NAME"
systemctl enable --now "$TIMER_NAME"

echo
echo "Done. Reattach with: sudo -u $RUN_USER tmux -L agentmux-$INSTANCE_NAME attach -t $TMUX_SESSION_NAME"
echo "Update logs: journalctl -u $UPDATE_SERVICE_NAME"
