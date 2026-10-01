#!/bin/bash

set -euo pipefail

phase="${1:?usage: plugins.sh install|configure|verify}"

state_dir="$HOME/.cc-remote"
share_dir="$HOME/.local/share/cc-remote"
tool_dir="$share_dir/tools"
marketplace_dir="$share_dir/marketplaces"
bin_dir="$HOME/.local/bin"
system_tool_dir=/opt/cc-remote/tools
system_bin_dir=/usr/local/bin
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
export PATH="$bin_dir:$PATH"

{{template "artifacts.sh"}}
shq() {
  printf "'%s'" "${1//\'/\'\\\'\'}"
}

require_env() {
  if [ -z "${!1:-}" ]; then
    echo "plugins: configure needs $1 in its environment" >&2
    exit 1
  fi
}

plugin_root() {
  claude plugin list --json | jq -er --arg id "$1" '.[] | select(.id == $id) | .installPath'
}

pinned() {
  sort <<'PINS'
{{range .Claude.Plugins}}{{.ID}} {{.Version}}
{{end -}}
PINS
}

healthy() {
  local plugins
  plugins="$(claude plugin list --json)"
  jq -r --args '.[] | select((.id | IN($ARGS.positional[])) and .enabled and (.errors | length) == 0) | "\(.id) \(.version)"'{{range .Claude.Plugins}} {{q .ID}}{{end}} <<< "$plugins" \
    | sort
}

check_plugins() {
  local installed
  installed="$(healthy)"
  if ! diff <(pinned) - <<< "$installed" >&2; then
    echo "plugins: the installed, enabled and loadable plugins differ from the pins" >&2
    exit 1
  fi
}

with_github_token() {
  local askpass="$tmp_dir/askpass"
  if [ -z "$github_token" ]; then
    echo "plugins: a private marketplace needs the GitHub token on stdin" >&2
    exit 1
  fi
  cat > "$askpass" <<'SH'
#!/bin/sh
case "$1" in
  "Username for 'https://github.com': ") echo x-access-token ;;
  "Password for 'https://x-access-token@github.com': ") printf '%s\n' "$GITHUB_TOKEN" ;;
  *) exit 1 ;;
esac
SH
  chmod 700 "$askpass"
  GITHUB_TOKEN="$github_token" GIT_ASKPASS="$askpass" GIT_TERMINAL_PROMPT=0 \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.followRedirects GIT_CONFIG_VALUE_0=false "$@"
}

checkout() {
  local repo="$2" ref="$3" access="$4" dir="$marketplace_dir/$1"
  if [ "$(git -C "$dir" rev-parse HEAD 2> /dev/null)" = "$ref" ]; then
    return
  fi
  rm -rf "$dir"
  git init -q "$dir"
  if [ "$access" = private ]; then
    with_github_token git -C "$dir" fetch -q --depth 1 "https://github.com/$repo.git" "$ref"
  else
    git -C "$dir" fetch -q --depth 1 "https://github.com/$repo.git" "$ref"
  fi
  git -C "$dir" checkout -q FETCH_HEAD
}

register_marketplace() {
  if grep -qxF "$1" <<< "$2"; then
    claude plugin marketplace update "$1"
  else
    claude plugin marketplace add "$marketplace_dir/$1"
  fi
}

install_plugin() {
  local current
  current="$(claude plugin list --json | jq -r --arg id "$1" '.[] | select(.id == $id) | .version')"
  if [ -z "$current" ]; then
    claude plugin install "$1"
  elif [ "$current" != "$2" ]; then
    claude plugin update "$1"
  fi
}

install_plugins() {
  local known
  known="$(claude plugin marketplace list --json | jq -r '.[].name')"
{{- range .Claude.Marketplaces}}
  register_marketplace {{q .Name}} "$known"
{{- end}}
{{- range .Claude.Plugins}}
  install_plugin {{q .ID}} {{q .Version}}
{{- end}}
  check_plugins
}

uv_tool() {
  local python="$2" spec="$3" stamp="$state_dir/uv-tools/$1"
  shift 3
  if [ "$(cat "$stamp" 2> /dev/null)" != "$python $spec $*" ]; then
    uv tool install --quiet --force --python "$python" "$@" "$spec"
    mkdir -p "$state_dir/uv-tools"
    printf '%s\n' "$python $spec $*" > "$stamp"
  fi
}

verify_uv_tool() {
  local name="$1" python="$2" spec="$3"
  shift 3
  if [ "$(cat "$state_dir/uv-tools/$name" 2> /dev/null)" != "$python $spec $*" ]; then
    echo "cc-remote: uv tool $name is not installed as $spec" >&2
    exit 1
  fi
}

codex_runtime_root="$HOME/.cache/codex-runtimes/codex-primary-runtime"

install_codex_runtime() {
  local version="$1" url="$2" digest="$3" config="$HOME/.codex/config.toml" download="$tmp_dir/codex-runtime.tar.xz" plugin
  shift 3
  if [ "$(cat "$codex_runtime_root/.cc-remote-digest" 2> /dev/null)" != "$digest" ]; then
    fetch "$url" "$download" sha256 "$digest"
    rm -rf "$codex_runtime_root"
    mkdir -p "$(dirname "$codex_runtime_root")"
    tar -xJf "$download" -C "$(dirname "$codex_runtime_root")"
    rm -f "$download"
    printf '%s\n' "$digest" > "$codex_runtime_root/.cc-remote-digest"
  fi
  mkdir -p "$HOME/.codex"
  grep -qsF '[marketplaces.openai-primary-runtime]' "$config" \
    || printf '\n[marketplaces.openai-primary-runtime]\nsource_type = "local"\nsource = "%s"\n' "$codex_runtime_root/plugins/openai-primary-runtime" >> "$config"
  for plugin in "$@"; do
    codex_runtime_plugin "$plugin" "$version" || codex plugin add "$plugin@openai-primary-runtime"
  done
}

codex_runtime_plugin() {
  codex plugin list | awk -v id="$1@openai-primary-runtime" -v want="installed, enabled $2" \
    '$1 == id && $2 " " $3 " " $4 == want { found = 1 } END { exit !found }'
}

verify_codex_runtime() {
  local version="$1" digest="$2" plugin
  shift 2
  verify_pin "$codex_runtime_root" "$digest"
  if [ "$(jq -r .bundleVersion "$codex_runtime_root/runtime.json" 2> /dev/null)" != "$version" ]; then
    echo "cc-remote: the Codex runtime is not at $version" >&2
    exit 1
  fi
  for plugin in "$@"; do
    if ! codex_runtime_plugin "$plugin" "$version"; then
      echo "cc-remote: Codex runtime plugin $plugin is not installed and enabled at $version" >&2
      exit 1
    fi
  done
}

captain_hook_build() {
  jq -r .build "$HOME/.local/share/captain-hook/host/version.json" 2> /dev/null
}

install_captain_hook() {
  local version="$1" url="$2" digest="$3" download="$tmp_dir/captain-hook.tar.gz"
  if [ "$(captain_hook_build)" != "$version" ]; then
    fetch "$url" "$download" sha256 "$digest"
    tar -xzf "$download" -C "$tmp_dir" capt-hookd
    "$tmp_dir/capt-hookd" package-install
  fi
}

verify_captain_hook() {
  if [ "$(captain_hook_build)" != "$1" ]; then
    echo "cc-remote: the Captain Hook host is not at $1" >&2
    exit 1
  fi
}

claude_env() {
  local settings="$HOME/.claude/settings.json"
  mkdir -p "$HOME/.claude"
  if [ ! -f "$settings" ]; then
    echo '{}' > "$settings"
  fi
  jq --arg key "$1" --arg value "$2" '.env[$key] = $value' "$settings" > "$settings.tmp"
  mv "$settings.tmp" "$settings"
}

synckit_state() {
  local synckit="${XDG_CONFIG_HOME:-$HOME/.config}/synckit"
  if [ ! -f "$synckit/state.json" ]; then
    mkdir -p "$synckit"
    chmod 700 "$synckit"
    (umask 077 && printf '{"host_registry":{"self":"%s@%s","hosts":[]},"schema":{"identity":"synckit-state-v1","version":1,"fingerprint":"%s"},"synckit":{}}\n' "$(id -un)" "$(hostname)" "$1" > "$synckit/state.json")
  fi
}

service() {
  local name="$1" recipe="exec" word
  shift
  for word in "$@"; do
    recipe="$recipe $(shq "$word")"
  done
  printf '%s\n' "$recipe" > "$state_dir/services/$name.tmp"
  mv "$state_dir/services/$name.tmp" "$state_dir/services/$name"
}

write_supervisor() {
  local name
  mkdir -p "$state_dir/services"
  cat > "$state_dir/supervise.py.tmp" <<'PY'
{{template "supervise.py"}}PY
  chmod 755 "$state_dir/supervise.py.tmp"
  mv "$state_dir/supervise.py.tmp" "$state_dir/supervise.py"
  printf '#!/bin/sh\n' > "$state_dir/start.sh.tmp"
  for name in "$@"; do
    printf 'nohup setsid %s %s > /dev/null 2>&1 < /dev/null &\n' "$(shq "$state_dir/supervise.py")" "$(shq "$name")" >> "$state_dir/start.sh.tmp"
  done
  chmod 755 "$state_dir/start.sh.tmp"
  mv "$state_dir/start.sh.tmp" "$state_dir/start.sh"
}

start_services() {
  local name
  if command -v sprite-env > /dev/null; then
    for name in "$@"; do
      sprite-env services get "cc-remote-$name" > /dev/null 2>&1 \
        || sprite-env services create "cc-remote-$name" --cmd "$state_dir/supervise.py" --args "$name" --no-stream
    done
  else
    "$state_dir/start.sh"
  fi
}

run_install() {
  local github_token installed
  IFS= read -r github_token
  mkdir -p "$bin_dir"
{{- range .Tools}}
  {{install . "tool_dir" "bin_dir"}}
{{- end}}
{{- range .Links}}
  ln -sfn "$system_bin_dir/"{{q .}} "$bin_dir/"{{q .}}
{{- end}}
{{- range .Claude.Marketplaces}}
  checkout {{q .Name}} {{q .GitHub}} {{q .Ref}} {{if .Private}}private{{else}}public{{end}}
{{- end}}
{{- if .Claude.Plugins}}
  installed="$(healthy)"
  if [ "$installed" != "$(pinned)" ]; then
    install_plugins
  fi
{{- end}}
{{- range .Python.User}}
  uv_tool {{q .Name}} {{q $.Python.Version}} {{spec .}}{{range .Args}} {{q .}}{{end}}
{{- end}}
{{- with .CodexRuntime}}
  install_codex_runtime {{q .Version}} {{q .URL}} {{q .SHA256}}{{range .Plugins}} {{q .}}{{end}}
{{- end}}
{{- with .CaptainHook}}
  install_captain_hook {{q .Version}} {{q .URL}} {{q .SHA256}}
{{- end}}
  run_verify
}

run_configure() {
  :
{{- range .Configure.Env}}
  require_env {{q .}}
{{- end}}
{{- range $key, $value := .Claude.Env}}
  claude_env {{q $key}} {{expand $value}}
{{- end}}
{{- range .Configure.Run}}
  {{.}}
{{- end}}
{{- if .Services}}
  local executable
  write_supervisor{{range .Services}} {{q .Name}}{{end}}
{{- range .Services}}
  executable={{executable .}}
  service {{q .Name}}{{if .Env}} env{{range $key, $value := .Env}} {{expand (printf "%s=%s" $key $value)}}{{end}}{{end}} "$executable"{{range slice .Command 1}} {{q .}}{{end}}
{{- end}}
  start_services{{range .Services}} {{q .Name}}{{end}}
{{- end}}
{{- with .Cookiesync}}
  synckit_state {{q .SchemaFingerprint}}
  cookiesync install
{{- end}}
}

run_verify() {
  :
{{- range .System}}
{{- range verify . "system_tool_dir" "system_bin_dir"}}
  {{.}}
{{- end}}
{{- end}}
{{- range .Python.System}}
{{- $tool := .}}
{{- range .Bins}}
  verify_bin {{q .}}{{range $tool.Verify}} {{q .}}{{end}}
{{- end}}
{{- end}}
{{- range .Tools}}
{{- range verify . "tool_dir" "bin_dir"}}
  {{.}}
{{- end}}
{{- end}}
{{- range .Links}}
  verify_link "$bin_dir/"{{q .}} "$system_bin_dir/"{{q .}}
{{- end}}
{{- if .Claude.Plugins}}
  check_plugins
{{- range .Claude.Plugins}}
{{- $plugin := .}}
{{- range .Bins}}
  "$(plugin_root {{q $plugin.ID}})/"{{q .}} --version > /dev/null
{{- end}}
{{- end}}
{{- end}}
{{- range .Python.User}}
  verify_uv_tool {{q .Name}} {{q $.Python.Version}} {{spec .}}{{range .Args}} {{q .}}{{end}}
{{- $tool := .}}
{{- range .Bins}}
  verify_bin {{q .}}{{range $tool.Verify}} {{q .}}{{end}}
{{- end}}
{{- end}}
{{- with .CodexRuntime}}
  verify_codex_runtime {{q .Version}} {{q .SHA256}}{{range .Plugins}} {{q .}}{{end}}
{{- end}}
{{- with .CaptainHook}}
  verify_captain_hook {{q .Version}}
{{- end}}
}

case "$phase" in
  install) run_install ;;
  configure) run_configure ;;
  verify) run_verify ;;
  *)
    echo "usage: plugins.sh install|configure|verify" >&2
    exit 2
    ;;
esac
