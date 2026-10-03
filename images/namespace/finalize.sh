#!/bin/bash

set -euo pipefail

usage="usage: finalize.sh system ROOT | finalize.sh home HOME"
mode="${1:?$usage}"
target="${2:?$usage}"

check_system() {
  local root="${1%/}" key
  shopt -s nullglob
  for key in "$root"/etc/ssh/ssh_host_*; do
    echo "cc-remote: the image layer kept SSH host key $key" >&2
    exit 1
  done
  shopt -u nullglob
}

finalize_home() {
  local home="$1" path left
  local -a excluded preserved
  local -A kept
  excluded=(
    "$home/.claude.json"
    "$home/.claude/backups"
    "$home/.claude/.credentials.json"
    "$home/.claude/projects"
    "$home/.claude/todos"
    "$home/.claude/shell-snapshots"
    "$home/.claude/statsig"
    "$home/.claude/session-env"
    "$home/.claude/debug"
    "$home/.claude/ide"
    "$home/.claude/history.jsonl"
    "$home/.claude/plugins/data"
    "$home/.codex/auth.json"
    "$home/.codex/sessions"
    "$home/.codex/log"
    "$home/.codex/history.jsonl"
    "$home/.daemonkit/a"
    "$home/.cc-remote/ready"
    "$home/.cc-remote/services"
    "$home/.cc-remote/start.sh"
    "$home/.cc-remote/supervise.py"
    "$home/.cc-remote/orca"
    "$home/.config/gh/hosts.yml"
    "$home/.git-credentials"
  )
  preserved=(
    "$home/.codex/hooks.json"
    "$home/.codex/config.toml"
    "$home/.claude/settings.json"
  )
  shopt -s nullglob
  excluded+=("$home"/.claude.json.* "$home"/.cc-remote/tailscaled.*)
  shopt -u nullglob

  for path in "${preserved[@]}"; do
    if [ -e "$path" ]; then
      kept["$path"]="$(sha256sum < "$path")"
    fi
  done
  rm -rf -- "${excluded[@]}"
  if [ -d "$home/.claude/plugins" ]; then
    find "$home/.claude/plugins" \( -name .in_use -o -name .orphaned_at \) -delete
  fi
  if [ -d "$home/.daemonkit" ]; then
    find "$home/.daemonkit" \( -name '*.pid' -o -name '*.lock' \) -not -path "$home/.daemonkit/cache/*/*/*" -delete
  fi
  find "$home" -type s -delete

  for path in "${excluded[@]}"; do
    if [ -e "$path" ] || [ -L "$path" ]; then
      echo "cc-remote: image finalization left $path" >&2
      exit 1
    fi
  done
  left="$(find "$home" \( -type s -o -name .in_use -o -name .orphaned_at \) -print -quit)"
  if [ -n "$left" ]; then
    echo "cc-remote: image finalization left $left" >&2
    exit 1
  fi
  for path in "${!kept[@]}"; do
    if [ ! -f "$path" ] || [ "$(sha256sum < "$path")" != "${kept[$path]}" ]; then
      echo "cc-remote: image finalization changed the tool configuration $path" >&2
      exit 1
    fi
  done
}

case "$mode" in
  system) check_system "$target" ;;
  home) finalize_home "$target" ;;
  *)
    echo "$usage" >&2
    exit 2
    ;;
esac
