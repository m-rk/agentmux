#!/usr/bin/env bash
# Run the project's approved full Go test suite with per-worktree scratch space.
# Keep the command fixed: this script is the narrow entry point that may be
# approved to open local test listeners by a task-session sandbox.
set -u

if (( $# != 0 )); then
  echo "usage: scripts/codex-test.sh" >&2
  exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P) || exit 2
worktree=$(git -C "$repo_root" rev-parse --show-toplevel 2>/dev/null) || {
  echo "codex-test: not running from a Git worktree" >&2
  exit 2
}
worktree=$(cd "$worktree" && pwd -P)
daemon_dir="$repo_root/daemon"
state_dir="$worktree/.agentmux-codex-test"
cache_dir="$state_dir/go-cache"
tmp_dir="$state_dir/tmp"
mkdir -p "$cache_dir" "$tmp_dir" || exit 2
chmod 700 "$state_dir" "$cache_dir" "$tmp_dir" || exit 2

start=$(date +%s)
log=$(mktemp "$tmp_dir/output.XXXXXX") || exit 2
cleanup() { rm -f "$log"; }
trap cleanup EXIT HUP INT TERM

set +e
(cd "$daemon_dir" && nice -n 10 env GOCACHE="$cache_dir" GOTMPDIR="$tmp_dir" TMPDIR="$tmp_dir" go test ./...) 2>&1 | tee "$log"
test_status=${PIPESTATUS[0]}
set -e
duration=$(( $(date +%s) - start ))

if (( test_status == 0 )); then
  printf 'CODEX_TEST_RESULT=passed exit_code=0 duration_seconds=%s\n' "$duration"
  exit 0
fi

denied=0
assertions=0
rg -qi '(operation not permitted|permission denied|read-only file system|network is unreachable|bind:.*(permission|operation not permitted)|failed to create.*(namespace|network))' "$log" && denied=1
rg -q '^--- FAIL:|^FAIL[[:space:]]' "$log" && assertions=1
case "$denied:$assertions" in
  1:1) kind=sandbox_denial_and_test_failure ;;
  1:0) kind=sandbox_denial ;;
  *) kind=test_failure ;;
esac
printf 'CODEX_TEST_RESULT=%s exit_code=%s duration_seconds=%s\n' "$kind" "$test_status" "$duration"
exit "$test_status"
