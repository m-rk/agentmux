#!/bin/bash
# Starts (or leaves alone, if already running) the persistent agentmux tmux
# session for the opencode + Ollama Cloud backend. Idempotent — safe to
# re-run any time, including from the periodic update service.
set -uo pipefail

: "${AGENTMUX_SESSION_NAME:=agentmux-opencode}"
: "${AGENTMUX_WORKDIR:=$HOME/.agentmux/opencode-ollama}"
: "${AGENTMUX_OLLAMA_MODEL:=gpt-oss:20b-cloud}"
: "${AGENTMUX_OLLAMA_WAIT_SECONDS:=60}"

export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
mkdir -p "$AGENTMUX_WORKDIR"

wait_for_ollama() {
    local deadline=$((SECONDS + AGENTMUX_OLLAMA_WAIT_SECONDS))

    while ! ollama list >/dev/null 2>&1; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            echo "ollama is not reachable after ${AGENTMUX_OLLAMA_WAIT_SECONDS}s; start ollama and re-run this script" >&2
            return 1
        fi
        sleep 2
    done
}

wait_for_ollama || exit 1

# Runs in its own dedicated working directory (not $HOME) so it never
# collides with whatever interactive conversation is most recent there.
#
# Points opencode at the local ollama daemon's OpenAI-compatible endpoint
# rather than opencode's own /connect login flow: ollama serve already
# holds the Ollama Cloud credential (from `ollama signin`, see README) and
# transparently offloads any "-cloud" tagged model, so opencode itself
# never needs credentials of its own.
CONFIG_FILE="$AGENTMUX_WORKDIR/opencode.json"
if [ ! -f "$CONFIG_FILE" ]; then
    cat > "$CONFIG_FILE" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "model": "ollama/$AGENTMUX_OLLAMA_MODEL",
  "provider": {
    "ollama": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Ollama Cloud",
      "options": {
        "baseURL": "http://localhost:11434/v1"
      },
      "models": {
        "$AGENTMUX_OLLAMA_MODEL": {
          "name": "$AGENTMUX_OLLAMA_MODEL"
        }
      }
    }
  }
}
EOF
fi

if ! tmux has-session -t "$AGENTMUX_SESSION_NAME" 2>/dev/null; then
    tmux new-session -d -s "$AGENTMUX_SESSION_NAME" -c "$AGENTMUX_WORKDIR" "opencode"
fi
