#!/bin/bash
set -euo pipefail

# Recover a Mac whose installed agentmux predates the self-updater.
repo_url=${AGENTMUX_BOOTSTRAP_REPO:-https://github.com/m-rk/agentmux.git}
dry_run=0
if [[ $(uname -s) != Darwin ]]; then
  echo "this bootstrap supports macOS only" >&2
  exit 1
fi
if [[ ${1:-} == --dry-run ]]; then
  dry_run=1
elif [[ $# -ne 0 ]]; then
  echo "usage: bootstrap-mac.sh [--dry-run]" >&2
  exit 2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentmux-bootstrap.XXXXXX")
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

echo "This bootstrap installs agentmux from origin/main and restarts the agentmux daemon and gateway."
if (( dry_run )); then
  echo "DRY RUN: git init $tmp/repo"
  echo "DRY RUN: git fetch origin +refs/heads/main:refs/remotes/origin/main"
  echo "DRY RUN: git worktree add --detach $tmp/src origin/main"
  echo "DRY RUN: go build -o $tmp/agentmux ./cmd/agentmux (from daemon/)"
  echo "DRY RUN: $tmp/agentmux daemon install"
  echo "DRY RUN: launchctl kickstart -k gui/$(id -u)/com.m-rk.agentmux.gateway"
  echo "DRY RUN: print installed commit"
  exit 0
fi

read -r -p "Continue? [y/N] " answer
[[ $answer == y || $answer == Y || $answer == yes || $answer == YES ]] || { echo "Cancelled."; exit 1; }

mkdir "$tmp/repo"
git -C "$tmp/repo" init -q
git -C "$tmp/repo" remote add origin "$repo_url"
git -C "$tmp/repo" fetch --quiet origin +refs/heads/main:refs/remotes/origin/main
commit=$(git -C "$tmp/repo" rev-parse refs/remotes/origin/main)
git -C "$tmp/repo" worktree add --quiet --detach "$tmp/src" "$commit"
cd "$tmp/src/daemon"
go build -o "$tmp/agentmux" ./cmd/agentmux
"$tmp/agentmux" daemon install
launchctl kickstart -k "gui/$(id -u)/com.m-rk.agentmux.gateway"
echo "Installed agentmux from origin/main at $commit"
