#!/bin/bash
# Lightweight regression checks for the shell-based agentmux backend.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_ROOT="${TMPDIR:-/tmp}/agentmux-smoke.$$"
FAKE_HOME="$TMP_ROOT/home"
FAKE_BIN="$FAKE_HOME/.local/bin"

cleanup() {
    rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

mkdir -p "$FAKE_BIN"

pass() {
    printf 'ok - %s\n' "$1"
}

fail() {
    printf 'not ok - %s\n' "$1" >&2
    exit 1
}

assert_contains() {
    local haystack="$1"
    local needle="$2"
    local label="$3"

    case "$haystack" in
        *"$needle"*) pass "$label" ;;
        *) fail "$label: expected to find '$needle'" ;;
    esac
}

assert_file_contains() {
    local file="$1"
    local needle="$2"
    local label="$3"

    if grep -Fq "$needle" "$file"; then
        pass "$label"
    else
        fail "$label: expected $file to contain '$needle'"
    fi
}

assert_fails() {
    local label="$1"
    shift

    if "$@" >/dev/null 2>&1; then
        fail "$label: command unexpectedly succeeded"
    fi
    pass "$label"
}

write_fake_tools() {
    cat > "$FAKE_BIN/ollama" <<'SH'
#!/bin/bash
case "$1" in
    list) exit 0 ;;
    run) printf 'ok\n'; exit 0 ;;
    *) exit 0 ;;
esac
SH

    cat > "$FAKE_BIN/zero" <<'SH'
#!/bin/bash
if [ "${1:-}" = "--version" ]; then
    printf 'zero 0.1.0\n'
    exit 0
fi
if [ "${1:-}" = "providers" ] && [ "${2:-}" = "check" ]; then
    exit 0
fi
if [ "${1:-}" = "update" ] && [ "${2:-}" = "--check" ]; then
    printf 'zero is current\n'
    exit 0
fi
if [ "${1:-}" = "exec" ]; then
    printf 'ok\n'
    exit 0
fi
exit 0
SH

    cat > "$FAKE_BIN/opencode" <<'SH'
#!/bin/bash
if [ "${1:-}" = "--version" ]; then
    printf 'opencode 0.0.0\n'
    exit 0
fi
if [ "${1:-}" = "upgrade" ]; then
    printf 'already current\n'
    exit 0
fi
exit 0
SH

    cat > "$FAKE_BIN/kilo" <<'SH'
#!/bin/bash
if [ "${1:-}" = "--version" ]; then
    printf 'kilo 0.0.0\n'
    exit 0
fi
if [ "${1:-}" = "upgrade" ]; then
    printf 'already current\n'
    exit 0
fi
exit 0
SH

    cat > "$FAKE_BIN/tmux" <<'SH'
#!/bin/bash
if [ "${1:-}" = "-L" ]; then
    shift 2
fi
case "$1" in
    has-session)
        exit 1
        ;;
    new-session)
        printf '%s\n' "$*" >> "${AGENTMUX_TEST_TMUX_LOG:?}"
        exit 0
        ;;
    kill-session)
        exit 0
        ;;
    *)
        exit 0
        ;;
esac
SH

    chmod +x "$FAKE_BIN/ollama" "$FAKE_BIN/zero" "$FAKE_BIN/opencode" "$FAKE_BIN/kilo" "$FAKE_BIN/tmux"
}

check_shell_syntax() {
    bash -n "$ROOT"/backends/agentmux/*.sh "$ROOT"/backends/claude-code/*.sh
    pass "shell syntax"
}

check_plan_modes() {
    local linux_plan

    linux_plan="$("$ROOT/backends/agentmux/install.sh" --plan \
        --instance work-zero \
        --agent zero \
        --provider ollama \
        --model gpt-oss:20b-cloud)"
    assert_contains "$linux_plan" "service       : agentmux-work-zero.service" "linux plan uses instance service"
    assert_contains "$linux_plan" "workdir       :" "linux plan prints workdir"
    assert_contains "$linux_plan" "provider url  : http://localhost:11434/v1" "linux plan defaults ollama base URL"

    assert_fails "unsupported provider rejected" \
        "$ROOT/backends/agentmux/install.sh" --plan --agent zero --provider anthropic

    if [ "$(uname -s)" = "Darwin" ]; then
        local mac_plan

        mac_plan="$("$ROOT/backends/agentmux/install-macos.sh" --plan \
            --instance work-zero \
            --agent zero \
            --provider ollama \
            --model gpt-oss:20b-cloud)"
        assert_contains "$mac_plan" "start label   : com.agentmux.work-zero" "macOS plan uses instance start label"
        assert_contains "$mac_plan" "update label  : com.agentmux.work-zero.update" "macOS plan uses instance update label"
    else
        pass "macOS plan skipped on non-macOS"
    fi
}

check_zero_config_rendering() {
    local workdir="$TMP_ROOT/zero-work"
    local tmux_log="$TMP_ROOT/zero-tmux.log"

    AGENTMUX_TEST_TMUX_LOG="$tmux_log" \
    HOME="$FAKE_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    AGENTMUX_INSTANCE_NAME="test-zero" \
    AGENTMUX_AGENT="zero" \
    AGENTMUX_PROVIDER="ollama" \
    AGENTMUX_MODEL="gpt-oss:20b-cloud" \
    AGENTMUX_TMUX_SESSION_NAME="test-zero" \
    AGENTMUX_WORKDIR="$workdir" \
    AGENTMUX_PROVIDER_WAIT_SECONDS=1 \
        bash "$ROOT/backends/agentmux/rc-start.sh"

    assert_file_contains "$workdir/.zero/config.json" '"activeProvider": "ollama"' "zero config active provider"
    assert_file_contains "$workdir/.zero/config.json" '"model": "gpt-oss:20b-cloud"' "zero config model"
    assert_file_contains "$tmux_log" "zero" "zero launch command"
}

check_opencode_config_rendering() {
    local workdir="$TMP_ROOT/opencode-work"
    local tmux_log="$TMP_ROOT/opencode-tmux.log"

    AGENTMUX_TEST_TMUX_LOG="$tmux_log" \
    HOME="$FAKE_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    AGENTMUX_INSTANCE_NAME="test-opencode" \
    AGENTMUX_AGENT="opencode" \
    AGENTMUX_PROVIDER="ollama" \
    AGENTMUX_MODEL="gpt-oss:20b-cloud" \
    AGENTMUX_TMUX_SESSION_NAME="test-opencode" \
    AGENTMUX_WORKDIR="$workdir" \
    AGENTMUX_PROVIDER_WAIT_SECONDS=1 \
        bash "$ROOT/backends/agentmux/rc-start.sh"

    assert_file_contains "$workdir/opencode.json" '"model": "ollama/gpt-oss:20b-cloud"' "opencode config model"
    assert_file_contains "$workdir/opencode.json" '"baseURL": "http://localhost:11434/v1"' "opencode config base URL"
    assert_file_contains "$tmux_log" "opencode" "opencode launch command"
}

check_kilo_config_rendering() {
    local workdir="$TMP_ROOT/kilo-work"
    local tmux_log="$TMP_ROOT/kilo-tmux.log"

    AGENTMUX_TEST_TMUX_LOG="$tmux_log" \
    HOME="$FAKE_HOME" \
    PATH="$FAKE_BIN:$PATH" \
    AGENTMUX_INSTANCE_NAME="test-kilo" \
    AGENTMUX_AGENT="kilo" \
    AGENTMUX_PROVIDER="ollama" \
    AGENTMUX_MODEL="gpt-oss:20b-cloud" \
    AGENTMUX_TMUX_SESSION_NAME="test-kilo" \
    AGENTMUX_WORKDIR="$workdir" \
    AGENTMUX_PROVIDER_WAIT_SECONDS=1 \
        bash "$ROOT/backends/agentmux/rc-start.sh"

    assert_file_contains "$workdir/kilo.json" '"model": "ollama/gpt-oss:20b-cloud"' "kilo config model"
    assert_file_contains "$workdir/kilo.json" '"baseURL": "http://localhost:11434/v1"' "kilo config base URL"
    assert_file_contains "$tmux_log" "kilo" "kilo launch command"
}

check_live_ollama() {
    if [ "${AGENTMUX_LIVE_OLLAMA:-0}" != "1" ]; then
        pass "live ollama smoke skipped"
        return
    fi

    command -v ollama >/dev/null 2>&1 || fail "live ollama smoke requires ollama"
    command -v zero >/dev/null 2>&1 || fail "live ollama smoke requires zero"
    command -v tmux >/dev/null 2>&1 || fail "live ollama smoke requires tmux"

    local workdir="$TMP_ROOT/live-zero-work"
    local session="agentmux-live-smoke-$$"

    AGENTMUX_INSTANCE_NAME="$session" \
    AGENTMUX_AGENT="zero" \
    AGENTMUX_PROVIDER="ollama" \
    AGENTMUX_MODEL="${AGENTMUX_LIVE_MODEL:-gpt-oss:20b-cloud}" \
    AGENTMUX_TMUX_SESSION_NAME="$session" \
    AGENTMUX_WORKDIR="$workdir" \
    AGENTMUX_PROVIDER_WAIT_SECONDS=5 \
        bash "$ROOT/backends/agentmux/rc-start.sh"

    local output="$TMP_ROOT/live-zero-output"

    if ! (cd "$workdir" && zero exec --model "${AGENTMUX_LIVE_MODEL:-gpt-oss:20b-cloud}" "reply with exactly: ok" > "$output"); then
        tmux -L "agentmux-$session" kill-session -t "$session" 2>/dev/null || true
        fail "live zero exec"
    fi
    assert_file_contains "$output" "ok" "live zero exec"
    tmux -L "agentmux-$session" kill-session -t "$session" 2>/dev/null || true
}

check_live_opencode() {
    if [ "${AGENTMUX_LIVE_OPENCODE:-0}" != "1" ]; then
        pass "live opencode smoke skipped"
        return
    fi

    command -v ollama >/dev/null 2>&1 || fail "live opencode smoke requires ollama"
    command -v opencode >/dev/null 2>&1 || fail "live opencode smoke requires opencode"
    command -v tmux >/dev/null 2>&1 || fail "live opencode smoke requires tmux"

    local workdir="$TMP_ROOT/live-opencode-work"
    local session="agentmux-live-opencode-$$"
    local model="${AGENTMUX_LIVE_MODEL:-gpt-oss:20b-cloud}"
    local output="$TMP_ROOT/live-opencode-output"

    AGENTMUX_INSTANCE_NAME="$session" \
    AGENTMUX_AGENT="opencode" \
    AGENTMUX_PROVIDER="ollama" \
    AGENTMUX_MODEL="$model" \
    AGENTMUX_TMUX_SESSION_NAME="$session" \
    AGENTMUX_WORKDIR="$workdir" \
    AGENTMUX_PROVIDER_WAIT_SECONDS=5 \
        bash "$ROOT/backends/agentmux/rc-start.sh"

    if ! (cd "$workdir" && opencode run --model "ollama/$model" "reply with exactly: ok" > "$output"); then
        tmux -L "agentmux-$session" kill-session -t "$session" 2>/dev/null || true
        fail "live opencode run"
    fi
    assert_file_contains "$output" "ok" "live opencode run"
    tmux -L "agentmux-$session" kill-session -t "$session" 2>/dev/null || true
}

write_fake_tools
check_shell_syntax
check_plan_modes
check_zero_config_rendering
check_opencode_config_rendering
check_kilo_config_rendering
check_live_ollama
check_live_opencode
