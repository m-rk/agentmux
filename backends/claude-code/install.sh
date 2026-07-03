#!/bin/bash
# Installs the agentmux Claude Code backend on this host: a persistent tmux
# session with Claude Code's Remote Control enabled, kept alive across
# reboots by systemd, plus a nightly timer that updates the CLI and restarts
# the session only when the version actually changes.
#
# Must be run with sudo (except --help/--plan). Configure via flags or env
# vars; flags win over env vars.
#
# Flags:
#   --session-name NAME    tmux session name (also: --tmux-session)
#   --display-name NAME    Remote Control display name (also: --remote-name)
#   --no-suffix            don't append " agentmux" to the display name
#   --run-user USER        user the session runs as
#   --on-calendar EXPR     systemd OnCalendar expression for the update timer
#   --plan                 print the install plan without writing files
#   --help                 show usage
#
# Env aliases:
#   AGENTMUX_TMUX_SESSION_NAME, AGENTMUX_SESSION_NAME
#   AGENTMUX_DISPLAY_NAME, AGENTMUX_REMOTE_NAME
#   AGENTMUX_DISPLAY_SUFFIX (default: 1; set 0/false/no/off to disable)
#   AGENTMUX_RUN_USER (default: $SUDO_USER)
#   AGENTMUX_ON_CALENDAR (default: "*-*-* 03:00:00 UTC")
#
# The display name defaults to "<machine name> agentmux" when unset, and
# gets " agentmux" appended to any explicit name too unless --no-suffix /
# AGENTMUX_DISPLAY_SUFFIX=0 is given (matching install-macos.sh's default).
#
# Example:
#   sudo ./install.sh --session-name my-server --on-calendar "*-*-* 03:00:00 Australia/Perth"
#
# Re-running is safe and rewrites the units/env file with current values —
# the env file is regenerated each time, not merged — but it does not
# restart an already-running session, so follow up with
# `systemctl restart agentmux-claude-code.service` to apply changes.
set -euo pipefail

usage() {
    cat <<'EOF'
Installs the agentmux Claude Code backend on this host via systemd.

Must be run with sudo (except --help/--plan). Configure via flags or env
vars; flags win over env vars.

Flags:
  --session-name NAME    tmux session name (also: --tmux-session)
  --display-name NAME    Remote Control display name (also: --remote-name)
  --no-suffix            don't append " agentmux" to the display name
  --run-user USER        user the session runs as
  --on-calendar EXPR     systemd OnCalendar expression for the update timer
  --plan                 print the install plan without writing files
  --help                 show usage

Env aliases:
  AGENTMUX_TMUX_SESSION_NAME, AGENTMUX_SESSION_NAME
  AGENTMUX_DISPLAY_NAME, AGENTMUX_REMOTE_NAME
  AGENTMUX_DISPLAY_SUFFIX (default: 1; set 0/false/no/off to disable)
  AGENTMUX_RUN_USER (default: $SUDO_USER)
  AGENTMUX_ON_CALENDAR (default: "*-*-* 03:00:00 UTC")
EOF
}

RAW_SESSION_NAME="${AGENTMUX_TMUX_SESSION_NAME:-${AGENTMUX_SESSION_NAME:-}}"
RAW_DISPLAY_NAME="${AGENTMUX_DISPLAY_NAME:-${AGENTMUX_REMOTE_NAME:-}}"
DISPLAY_SUFFIX_ENABLED=1
case "${AGENTMUX_DISPLAY_SUFFIX:-1}" in
    0 | false | no | off) DISPLAY_SUFFIX_ENABLED=0 ;;
esac
RUN_USER="${AGENTMUX_RUN_USER:-${SUDO_USER:-}}"
ON_CALENDAR="${AGENTMUX_ON_CALENDAR:-*-*-* 03:00:00 UTC}"
PLAN=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --session-name | --tmux-session)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            RAW_SESSION_NAME="$2"
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
        --run-user)
            [ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 1; }
            RUN_USER="$2"
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

SESSION_NAME="${RAW_SESSION_NAME:-agentmux}"

machine_name() {
    local name=""
    name="$(hostname -s 2>/dev/null || hostname 2>/dev/null || true)"
    if [ -z "$name" ]; then
        name="linux"
    fi
    printf '%s' "$name"
}

apply_display_suffix() {
    local name="$1"
    if [ "$DISPLAY_SUFFIX_ENABLED" -eq 1 ] && [[ "$name" != *" agentmux" ]]; then
        printf '%s agentmux' "$name"
    else
        printf '%s' "$name"
    fi
}

DISPLAY_NAME="$(apply_display_suffix "${RAW_DISPLAY_NAME:-$(machine_name)}")"

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVICE_NAME="agentmux-claude-code.service"
ENV_DIR="/etc/agentmux"
ENV_FILE="$ENV_DIR/claude-code.env"

print_plan() {
    echo "  session name : $SESSION_NAME"
    echo "  display name : $DISPLAY_NAME"
    echo "  run as       : ${RUN_USER:-<unset>}"
    echo "  update timer : $ON_CALENDAR"
    echo "  repo dir     : $REPO_DIR"
}

if [ "$PLAN" -eq 1 ]; then
    echo "agentmux claude-code install plan:"
    print_plan
    exit 0
fi

if [ -z "$RUN_USER" ]; then
    echo "Could not determine a user to run as; set AGENTMUX_RUN_USER or --run-user explicitly" >&2
    exit 1
fi

USER_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6 2>/dev/null || eval echo "~$RUN_USER")"
CLAUDE_JSON="$USER_HOME/.claude/.claude.json"
WORKDIR="${AGENTMUX_WORKDIR:-$USER_HOME/.agentmux/claude-code}"

claude_is_logged_in() {
    [ -f "$CLAUDE_JSON" ] || return 1
    command -v python3 >/dev/null 2>&1 || return 1
    python3 -c "
import json, sys
try:
    with open(sys.argv[1]) as f:
        d = json.load(f)
    sys.exit(0 if d.get('oauthAccount') or d.get('userID') else 1)
except Exception:
    sys.exit(1)
" "$CLAUDE_JSON"
}

preaccept_workspace_trust() {
    local workdir="$1"
    [ -f "$CLAUDE_JSON" ] || return 0
    command -v python3 >/dev/null 2>&1 || return 0

    # Run as RUN_USER so file writes keep correct ownership
    su -s /bin/bash "$RUN_USER" -c "python3 - '$workdir' '$CLAUDE_JSON'" <<'PYEOF'
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

if ! claude_is_logged_in; then
    echo "Claude Code does not appear to be logged in for user '$RUN_USER'." >&2
    echo "Run 'claude' once as $RUN_USER to complete login, then rerun this installer." >&2
    exit 1
fi

echo "Installing agentmux claude-code backend:"
print_plan

chmod +x "$REPO_DIR/rc-start.sh" "$REPO_DIR/rc-update.sh"

mkdir -p "$ENV_DIR" "$WORKDIR"
preaccept_workspace_trust "$WORKDIR"
cat > "$ENV_FILE" <<EOF
AGENTMUX_SESSION_NAME=$SESSION_NAME
AGENTMUX_DISPLAY_NAME=$DISPLAY_NAME
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
