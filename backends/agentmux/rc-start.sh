#!/bin/bash
# Starts (or leaves alone, if already running) one configured agentmux instance.
# An instance is an agent CLI + provider + model + workdir + tmux session.
set -uo pipefail

: "${AGENTMUX_INSTANCE_NAME:=agentmux}"
: "${AGENTMUX_AGENT:=zero}"
: "${AGENTMUX_PROVIDER:=ollama}"
: "${AGENTMUX_MODEL:=gpt-oss:20b-cloud}"
: "${AGENTMUX_TMUX_SESSION_NAME:=${AGENTMUX_SESSION_NAME:-$AGENTMUX_INSTANCE_NAME}}"
: "${AGENTMUX_WORKDIR:=$HOME/.agentmux/$AGENTMUX_INSTANCE_NAME}"
: "${AGENTMUX_PROVIDER_WAIT_SECONDS:=60}"

case "$AGENTMUX_PROVIDER" in
    ollama)
        : "${AGENTMUX_PROVIDER_BASE_URL:=http://localhost:11434/v1}"
        ;;
    *)
        : "${AGENTMUX_PROVIDER_BASE_URL:=}"
        ;;
esac

export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:${PATH:-}"
mkdir -p "$AGENTMUX_WORKDIR"

fail() {
    echo "$*" >&2
    exit 1
}

require_supported() {
    case "$AGENTMUX_AGENT:$AGENTMUX_PROVIDER" in
        zero:ollama | opencode:ollama) ;;
        *) fail "unsupported agent/provider combination: $AGENTMUX_AGENT/$AGENTMUX_PROVIDER" ;;
    esac
}

wait_for_provider() {
    local deadline

    case "$AGENTMUX_PROVIDER" in
        ollama)
            deadline=$((SECONDS + AGENTMUX_PROVIDER_WAIT_SECONDS))
            while ! ollama list >/dev/null 2>&1; do
                if [ "$SECONDS" -ge "$deadline" ]; then
                    fail "ollama is not reachable after ${AGENTMUX_PROVIDER_WAIT_SECONDS}s; start ollama and re-run this script"
                fi
                sleep 2
            done
            ;;
    esac
}

write_zero_config() {
    local config_dir="$AGENTMUX_WORKDIR/.zero"
    local config_file="$config_dir/config.json"
    local tmp_file="$config_file.tmp"

    mkdir -p "$config_dir"
    cat > "$tmp_file" <<EOF
{
  "activeProvider": "$AGENTMUX_PROVIDER",
  "providers": [
    {
      "name": "$AGENTMUX_PROVIDER",
      "provider_kind": "openai-compatible",
      "catalogID": "$AGENTMUX_PROVIDER",
      "baseURL": "$AGENTMUX_PROVIDER_BASE_URL",
      "apiFormat": "chat-completions",
      "model": "$AGENTMUX_MODEL"
    }
  ]
}
EOF
    mv "$tmp_file" "$config_file"

    (cd "$AGENTMUX_WORKDIR" && zero providers check "$AGENTMUX_PROVIDER" --connectivity >/dev/null)
}

write_opencode_config() {
    local config_file="$AGENTMUX_WORKDIR/opencode.json"
    local tmp_file="$config_file.tmp"
    local model_ref="$AGENTMUX_PROVIDER/$AGENTMUX_MODEL"

    cat > "$tmp_file" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "model": "$model_ref",
  "provider": {
    "$AGENTMUX_PROVIDER": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "$AGENTMUX_PROVIDER",
      "options": {
        "baseURL": "$AGENTMUX_PROVIDER_BASE_URL"
      },
      "models": {
        "$AGENTMUX_MODEL": {
          "name": "$AGENTMUX_MODEL"
        }
      }
    }
  }
}
EOF
    mv "$tmp_file" "$config_file"
}

configure_agent() {
    case "$AGENTMUX_AGENT" in
        zero) write_zero_config ;;
        opencode) write_opencode_config ;;
        *) fail "unsupported agent: $AGENTMUX_AGENT" ;;
    esac
}

launch_command() {
    case "$AGENTMUX_AGENT" in
        zero) printf '%s' "zero" ;;
        opencode) printf '%s' "opencode" ;;
        *) fail "unsupported agent: $AGENTMUX_AGENT" ;;
    esac
}

require_supported
wait_for_provider
configure_agent

if ! tmux has-session -t "$AGENTMUX_TMUX_SESSION_NAME" 2>/dev/null; then
    tmux new-session -d -s "$AGENTMUX_TMUX_SESSION_NAME" -c "$AGENTMUX_WORKDIR" "$(launch_command)"
fi
