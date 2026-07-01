#!/bin/bash
# Installs the agentmux opencode + Ollama Cloud backend on this host: a
# persistent tmux session running opencode against Ollama Cloud, kept alive
# across reboots by systemd, plus a nightly timer that updates the opencode
# CLI and restarts the session only when the version actually changes.
#
# Prerequisites (not installed by this script — see README.md):
#   - ollama installed and its systemd service running
#   - `ollama signin` has been run once, as AGENTMUX_RUN_USER, to link this
#     machine to an Ollama Cloud account
#   - opencode-ai installed globally via npm, as AGENTMUX_RUN_USER
#
# Must be run with sudo. Configure via env vars (all optional):
#   AGENTMUX_SESSION_NAME  tmux session name (default: agentmux-opencode)
#   AGENTMUX_RUN_USER      user the session runs as (default: $SUDO_USER)
#   AGENTMUX_ON_CALENDAR   systemd OnCalendar expression for the update timer
#                          (default: "*-*-* 03:00:00 UTC")
#   AGENTMUX_OLLAMA_MODEL  ollama cloud model tag to default to, e.g.
#                          "gpt-oss:120b-cloud" or "glm-4.7:cloud"
#                          (default: glm-5.2:cloud) — note not every tag is
#                          included on every Ollama Cloud plan; a 403 from
#                          `ollama run <tag>` means it needs a plan upgrade
#
# Example:
#   sudo AGENTMUX_SESSION_NAME="my-server-opencode" ./install.sh
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
RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"
SESSION_NAME="${AGENTMUX_SESSION_NAME:-agentmux-opencode}"
ON_CALENDAR="${AGENTMUX_ON_CALENDAR:-*-*-* 03:00:00 UTC}"
OLLAMA_MODEL="${AGENTMUX_OLLAMA_MODEL:-glm-5.2:cloud}"
SERVICE_NAME="agentmux-opencode-ollama.service"
ENV_DIR="/etc/agentmux"
ENV_FILE="$ENV_DIR/opencode-ollama.env"

if ! command -v ollama >/dev/null 2>&1; then
    echo "ollama is not installed. Install it first: curl -fsSL https://ollama.com/install.sh | sh" >&2
    exit 1
fi
if ! systemctl is-active --quiet ollama; then
    echo "ollama.service is not running. Start it first: sudo systemctl enable --now ollama" >&2
    exit 1
fi
if ! sudo -u "$RUN_USER" env "PATH=$RUN_HOME/.npm-global/bin:/usr/bin:/bin" HOME="$RUN_HOME" \
        bash -c 'command -v opencode' >/dev/null 2>&1; then
    echo "opencode is not installed for $RUN_USER. Install it first: npm install -g opencode-ai" >&2
    exit 1
fi

echo "Installing agentmux opencode-ollama backend:"
echo "  session name : $SESSION_NAME"
echo "  run as       : $RUN_USER"
echo "  update timer : $ON_CALENDAR"
echo "  ollama model : $OLLAMA_MODEL"
echo "  repo dir     : $REPO_DIR"

chmod +x "$REPO_DIR/rc-start.sh" "$REPO_DIR/rc-update.sh"

mkdir -p "$ENV_DIR"
cat > "$ENV_FILE" <<EOF
AGENTMUX_SESSION_NAME=$SESSION_NAME
AGENTMUX_RUN_USER=$RUN_USER
AGENTMUX_SERVICE_NAME=$SERVICE_NAME
AGENTMUX_OLLAMA_MODEL=$OLLAMA_MODEL
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

render "$REPO_DIR/agentmux-opencode-ollama.service.tmpl" "/etc/systemd/system/agentmux-opencode-ollama.service"
render "$REPO_DIR/agentmux-opencode-ollama-update.service.tmpl" "/etc/systemd/system/agentmux-opencode-ollama-update.service"
render "$REPO_DIR/agentmux-opencode-ollama-update.timer.tmpl" "/etc/systemd/system/agentmux-opencode-ollama-update.timer"

systemctl daemon-reload
systemctl enable --now agentmux-opencode-ollama.service
systemctl enable --now agentmux-opencode-ollama-update.timer

echo
echo "Done. Reattach with: sudo -u $RUN_USER tmux attach -t $SESSION_NAME"
echo "Update logs: journalctl -u agentmux-opencode-ollama-update.service"
