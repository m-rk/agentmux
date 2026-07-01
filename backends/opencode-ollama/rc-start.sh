#!/bin/bash
# Starts (or leaves alone, if already running) the persistent agentmux tmux
# session for the opencode + Ollama Cloud backend. Idempotent — safe to
# re-run any time, including from the periodic update service.
set -uo pipefail

: "${AGENTMUX_SESSION_NAME:=agentmux-opencode}"
: "${AGENTMUX_WORKDIR:=$HOME/.agentmux/opencode-ollama}"
: "${AGENTMUX_OLLAMA_MODEL:=glm-5.2:cloud}"

export PATH="$HOME/.npm-global/bin:$PATH"
mkdir -p "$AGENTMUX_WORKDIR"

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
