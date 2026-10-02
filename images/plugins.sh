#!/bin/bash

set -euo pipefail

phase="${1:?usage: plugins.sh install [PAYLOAD_DIR]|publish STAMP|ready STAMP|configure|verify}"

state_dir="$HOME/.cc-remote"
share_dir="$HOME/.local/share/cc-remote"
tool_dir="$share_dir/tools"
marketplace_dir="$share_dir/marketplaces"
bin_dir="$HOME/.local/bin"
system_tool_dir=/opt/cc-remote/tools
system_bin_dir=/usr/local/bin
tmp_dir="$(mktemp -d)"
trap 'status=$?; drain_artifacts || status=$?; rm -rf "$tmp_dir"; exit "$status"' EXIT
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

claude_json() {
  local output
  output="$(claude "$@")" || exit
  if ! jq -se 'length == 1' <<< "$output" > /dev/null; then
    echo "plugins: claude $* did not print exactly one JSON document" >&2
    exit 1
  fi
  printf '%s\n' "$output"
}

plugin_root() {
  local plugins
  plugins="$(claude_json plugin list --json)" || exit
  plugin_path "$plugins" "$1"
}

plugin_path() {
  jq -er --arg id "$2" '.[] | select(.id == $id) | .installPath' <<< "$1"
}

pinned_blob() {
  local dir="$1" ref="$2" path="$3" mode oid target
  read -r mode _ oid _ <<< "$(GIT_LITERAL_PATHSPECS=1 git -C "$dir" ls-tree "$ref" -- "$path")"
  if [ "$mode" = 120000 ]; then
    target="$(git -C "$dir" cat-file blob "$oid")"
    case "$target" in
      "" | /* | */ | . | .. | */. | */..) return 1 ;;
    esac
    read -r mode _ oid _ <<< "$(GIT_LITERAL_PATHSPECS=1 git -C "$dir" ls-tree "$ref" -- "${path%/*}/$target")"
  fi
  case "$mode" in
    100644 | 100755) printf '%s %s\n' "$mode" "$oid" ;;
    *) return 1 ;;
  esac
}

verify_plugin_bin() {
  local id="$1" bin="$2" ref="$3" root dir source launcher descriptor
  root="$(plugin_path "$4" "$id")"
  if [ ! -x "$root/$bin" ]; then
    echo "cc-remote: plugin $id has no executable $bin" >&2
    exit 1
  fi
  if [ -n "$ref" ] && [ "${id%@*}" != captain-hook ]; then
    dir="$marketplace_dir/${id#*@}"
    source="$(git -C "$dir" cat-file blob "$ref:.claude-plugin/marketplace.json" | jq -er --arg name "${id%@*}" '.plugins[] | select(.name == $name) | .source | select(type == "string")')"
    if ! launcher="$(pinned_blob "$dir" "$ref" "$source/$bin")"; then
      echo "cc-remote: plugin $id $bin is not a committed file at $ref" >&2
      exit 1
    fi
    git -C "$dir" cat-file blob "${launcher#* }" > "$tmp_dir/launcher"
    if grep -qF "DESCRIPTOR=\"\$ROOT/$bin.binrun\"" "$tmp_dir/launcher" \
      && grep -qF "exec \"\$RUNNER_BIN\" \"\$DESCRIPTOR\" \"\$@\"" "$tmp_dir/launcher"; then
      if ! descriptor="$(pinned_blob "$dir" "$ref" "$source/$bin.binrun")" \
        || [ "$launcher" != "100755 $(git -C "$dir" hash-object --no-filters "$root/$bin")" ] \
        || [ "$descriptor" != "${descriptor% *} $(git -C "$dir" hash-object --no-filters "$root/$bin.binrun")" ]; then
        echo "cc-remote: plugin $id $bin differs from its pinned launcher or descriptor" >&2
        exit 1
      fi
      sed '1{/^#!/d;}' "$root/$bin.binrun" | jq -e '
        .schema == 1 and .kind == "release-binary"
        and (.version.static | type == "string" and length > 0)
        and (.platforms | length > 0)
        and all(.platforms[]; .size > 0 and .hash == "sha256" and (.digest | test("^[0-9a-f]{64}$")))
      ' > /dev/null
      return
    fi
  fi
  "$root/$bin" --version > /dev/null
}

pinned() {
  sort <<'PINS'
{{range .Claude.Plugins}}{{.ID}} {{.Version}}
{{end -}}
PINS
}

healthy_plugins() {
  jq -r --args '.[] | select((.id | IN($ARGS.positional[])) and .enabled and (.errors | length) == 0) | "\(.id) \(.version)"'{{range .Claude.Plugins}} {{q .ID}}{{end}} <<< "$1" \
    | sort
}

check_plugins() {
  local plugins
  plugins="$(claude_json plugin list --json)" || exit
  verify_plugins "$plugins"
}

verify_plugins() {
  local installed
  installed="$(healthy_plugins "$1")"
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

recorded_source() {
  local known
  known="$(claude_json plugin marketplace list --json)" || exit
  marketplace_source "$known" "$1"
}

marketplace_source() {
  jq -r --arg name "$2" '.[] | select(.name == $name) | [.source, (.repo // .path), .ref] | map(select(. != null)) | join(" ")' <<< "$1"
}

pin_marketplace() {
  local name="$1" want="$2" source="$3" recorded
  recorded="$(recorded_source "$name")"
  if [ "$recorded" != "$want" ]; then
    if [ -n "$recorded" ]; then
      claude plugin marketplace remove "$name"
    fi
    claude plugin marketplace add "$source"
  fi
}

verify_marketplace() {
  local recorded
  recorded="$(marketplace_source "$3" "$1")"
  if [ "$recorded" != "$2" ]; then
    echo "cc-remote: marketplace $1 is not registered from $2" >&2
    exit 1
  fi
}

held_declaration() {
  jq -cn --arg repo "$1" --arg branch "$2" '{source: {source: "github", repo: $repo, ref: $branch}, autoUpdate: false}'
}

pin_ref_marketplace() {
  pin_marketplace "$1" "directory $marketplace_dir/$1" "$marketplace_dir/$1"
}

verify_ref_marketplace() {
  verify_marketplace "$1" "directory $marketplace_dir/$1" "$3"
  if [ "$(git -C "$marketplace_dir/$1" rev-parse HEAD)" != "$2" ]; then
    echo "cc-remote: marketplace $1 is not checked out at $2" >&2
    exit 1
  fi
}

pin_branch_marketplace() {
  local name="$1" repo="$2" branch="$3"
  pin_marketplace "$name" "github $repo $branch" "$repo#$branch"
  edit_settings --arg name "$name" --argjson held "$(held_declaration "$repo" "$branch")" ".extraKnownMarketplaces[\$name] = \$held"
}

verify_branch_marketplace() {
  local name="$1" repo="$2" branch="$3"
  verify_marketplace "$name" "github $repo $branch" "$4"
  if ! jq -se --arg name "$name" --argjson held "$(held_declaration "$repo" "$branch")" 'length == 1 and .[0].extraKnownMarketplaces[$name] == $held' "$HOME/.claude/settings.json" > /dev/null; then
    echo "cc-remote: marketplace $name is not declared in Claude settings at branch $branch with auto-update off" >&2
    exit 1
  fi
}

install_plugin() {
  local current
  current="$(jq -r --arg id "$1" '.[] | select(.id == $id) | .version' <<< "$3")"
  if [ -z "$current" ]; then
    claude plugin install "$1"
  elif [ "$current" != "$2" ]; then
    claude plugin update "$1"
  fi
}

stale_marketplace() {
  local working="$1" pin
  shift
  for pin in "$@"; do
    if ! grep -qxF "$pin" <<< "$working"; then
      return 0
    fi
  done
  return 1
}

install_plugins() {
  local working
  working="$(healthy_plugins "$1")"
{{- range .Claude.Marketplaces}}
{{- $name := .Name}}
{{- with pins .Name}}
  if stale_marketplace "$working"{{range .}} {{q .}}{{end}}; then
    claude plugin marketplace update {{q $name}}
  fi
{{- end}}
{{- end}}
{{- range .Claude.Plugins}}
  install_plugin {{q .ID}} {{q .Version}} "$1"
{{- end}}
  check_plugins
}

uv_launcher() {
  local bin="$1" python="$2" spec="$3" arg uv
  shift 3
  uv="$(command -v uv)" || return
  printf '#!/bin/sh\nexec %s tool run --python %s' "$(shq "$uv")" "$(shq "$python")"
  for arg in "$@"; do
    printf ' %s' "$(shq "$arg")"
  done
  printf ' --from %s %s "$@"\n' "$(shq "$spec")" "$(shq "$bin")"
}

install_uv_launcher() {
  uv_launcher "$@" > "$tmp_dir/uv-$1"
  chmod 755 "$tmp_dir/uv-$1"
  mv -f "$tmp_dir/uv-$1" "$bin_dir/$1"
}

verify_uv_launcher() {
  if [ ! -x "$bin_dir/$1" ] || ! uv_launcher "$@" | cmp -s "$bin_dir/$1" -; then
    echo "cc-remote: Python tool $1 does not have its pinned first-use launcher" >&2
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
  jq -sr 'if length == 1 then .[0].build else error("expected one JSON document") end' "$HOME/.local/share/captain-hook/host/version.json"
}

install_captain_hook() {
  local version="$1" url="$2" digest="$3" download="$tmp_dir/captain-hook.tar.gz"
  if [ "$(captain_hook_build 2> /dev/null)" != "$version" ]; then
    fetch "$url" "$download" sha256 "$digest"
    tar -xzf "$download" -C "$tmp_dir" capt-hookd
    "$tmp_dir/capt-hookd" package-install
  fi
}

verify_captain_hook() {
  local build
  build="$(captain_hook_build)"
  if [ "$build" != "$1" ]; then
    echo "cc-remote: the Captain Hook host is not at $1" >&2
    exit 1
  fi
}

edit_settings() (
  local settings="$HOME/.claude/settings.json" edited
  umask 077
  mkdir -p "$HOME/.claude"
  if [ ! -f "$settings" ]; then
    echo '{}' > "$settings"
  fi
  edited="$(mktemp "$settings.XXXXXX")"
  jq "$@" "$settings" > "$edited"
  mv "$edited" "$settings"
)

claude_env() {
  edit_settings --arg key "$1" --arg value "$2" ".env[\$key] = \$value"
}

synckit_state() {
  local synckit="${XDG_CONFIG_HOME:-$HOME/.config}/synckit"
  if [ ! -f "$synckit/state.json" ]; then
    mkdir -p "$synckit"
    chmod 700 "$synckit"
    (umask 077 && printf '{"host_registry":{"self":"%s@%s","hosts":[]},"schema":{"identity":"synckit-state-v1","version":1,"fingerprint":"%s"},"synckit":{}}\n' "$(id -un)" "$(hostname)" "$1" > "$synckit/state.json")
  fi
  if ! jq -se --arg fingerprint "$1" 'length == 1 and .[0].schema.identity == "synckit-state-v1" and .[0].schema.fingerprint == $fingerprint' "$synckit/state.json" > /dev/null; then
    echo "cc-remote: $synckit/state.json is not synckit-state-v1 at schema fingerprint $1" >&2
    exit 1
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
  local -a needs=()
  if command -v sprite-env > /dev/null; then
    if sprite-env services get cc-remote-payload > /dev/null 2>&1; then
      needs=(--needs cc-remote-payload)
    fi
    for name in "$@"; do
      if ! sprite-env services get "cc-remote-$name" > /dev/null 2>&1; then
        queue_artifact sprite-env services create "cc-remote-$name" --cmd "$state_dir/supervise.py" --args "$name" "${needs[@]}" --no-stream
      fi
    done
    drain_artifacts
  else
    "$state_dir/start.sh"
  fi
}

run_install() {
  local github_token plugins
  rm -f "$state_dir/ready"
  if [ "$#" -gt 0 ]; then
    local payload="$1"
{{- range .HomeTrees}}
    expose {{.Kind}} {{.Requirement}} {{home .Path}}
{{- end}}
  fi
  IFS= read -r github_token
  mkdir -p "$bin_dir"
{{- range .Tools}}
  queue_artifact {{install . "tool_dir" "bin_dir"}}
{{- end}}
  drain_artifacts
{{- range .Links}}
  ln -sfn "$system_bin_dir/"{{q .}} "$bin_dir/"{{q .}}
{{- end}}
{{- with .CodexRuntime}}
  queue_artifact install_codex_runtime {{q .Version}} {{q .URL}} {{q .SHA256}}{{range .Plugins}} {{q .}}{{end}}
{{- end}}
{{- range .Claude.Marketplaces}}
{{- if .Ref}}
  checkout {{q .Name}} {{q .GitHub}} {{q .Ref}} {{if .Private}}private{{else}}public{{end}}
  pin_ref_marketplace {{q .Name}}
{{- else}}
  pin_branch_marketplace {{q .Name}} {{q .GitHub}} {{q .Branch}}
{{- end}}
{{- end}}
{{- if .Claude.Plugins}}
  plugins="$(claude_json plugin list --json)" || exit
  if [ "$(healthy_plugins "$plugins")" != "$(pinned)" ]; then
    install_plugins "$plugins"
  fi
{{- end}}
{{- range .Python.User}}
{{- $tool := .}}
{{- range .Bins}}
  install_uv_launcher {{q .}} {{q $.Python.Version}} {{spec $tool}}{{range $tool.Args}} {{q .}}{{end}}
{{- end}}
{{- end}}
  drain_artifacts
{{- with .CaptainHook}}
  install_captain_hook {{q .Version}} {{q .URL}} {{q .SHA256}}
{{- end}}
{{- if .Prepare}}
  (
    cd "$HOME"
{{- range .Prepare}}
    {{.}}
{{- end}}
  )
{{- end}}
  local link_check=spelling
  verify_user
}

run_publish() {
  local stamp="${1:?publish needs the ready stamp}"
  verify_system
  verify_user_links
  mkdir -p "$state_dir"
  printf '%s\n' "$stamp" > "$state_dir/ready"
}

run_ready() {
  local stamp="${1:?ready needs the stamp to compare}"
  if [ "$(cat "$state_dir/ready" 2> /dev/null)" != "$stamp" ]; then
    echo "plugins: this host was not prepared from stamp $stamp" >&2
    exit 1
  fi
}

run_configure() {
  :
{{- range .Configure.Env}}
  require_env {{q .}}
{{- end}}
{{- range $key, $value := .Claude.Env}}
  claude_env {{q $key}} {{expand $value}}
{{- end}}
{{- with .Cookiesync}}
  synckit_state {{q .SchemaFingerprint}}
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
{{- if .Cookiesync}}
  cookiesync install
{{- end}}
}

verify_system() {
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
}

verify_user_links() {
  :
{{- range .Tools}}
{{- range verify . "tool_dir" "bin_dir"}}
  {{.}}
{{- end}}
{{- end}}
{{- range .Links}}
  verify_link "$bin_dir/"{{q .}} "$system_bin_dir/"{{q .}}
{{- end}}
}

verify_user() {
  verify_user_links
{{- if .Claude.Marketplaces}}
  local known
  known="$(claude_json plugin marketplace list --json)" || exit
{{- end}}
{{- range .Claude.Marketplaces}}
{{- if .Ref}}
  verify_ref_marketplace {{q .Name}} {{q .Ref}} "$known"
{{- else}}
  verify_branch_marketplace {{q .Name}} {{q .GitHub}} {{q .Branch}} "$known"
{{- end}}
{{- end}}
{{- if .Claude.Plugins}}
  local plugins
  plugins="$(claude_json plugin list --json)" || exit
  verify_plugins "$plugins"
{{- range .Claude.Plugins}}
{{- $plugin := .}}
{{- range .Bins}}
  verify_plugin_bin {{q $plugin.ID}} {{q .}} {{q (pluginRef $plugin)}} "$plugins"
{{- end}}
{{- end}}
{{- end}}
{{- range .Python.User}}
{{- $tool := .}}
{{- range .Bins}}
  verify_uv_launcher {{q .}} {{q $.Python.Version}} {{spec $tool}}{{range $tool.Args}} {{q .}}{{end}}
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
  install) run_install "${@:2}" ;;
  publish) run_publish "${2:-}" ;;
  ready) run_ready "${2:-}" ;;
  configure) run_configure ;;
  verify)
    verify_system
    verify_user
    ;;
  *)
    echo "usage: plugins.sh install [PAYLOAD_DIR]|publish STAMP|ready STAMP|configure|verify" >&2
    exit 2
    ;;
esac
