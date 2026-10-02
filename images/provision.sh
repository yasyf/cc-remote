#!/bin/bash

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "provision: run as root" >&2
  exit 1
fi

phase="${1:-}"
tool_dir=/opt/cc-remote/tools
bin_dir=/usr/local/bin
payload_root=/opt/cc-remote/payload
payload_store=/var/lib/cc-remote/payload
{{- with .Closure}}
closure_root=/opt/cc-remote/closure
closure_loader_conf=/etc/ld.so.conf.d/zz-cc-remote-closure.conf
closure_fonts_conf=/etc/fonts/conf.d/99-cc-remote-closure.conf
closure_lock=/var/lib/cc-remote/ldconfig.lock
closure_registered=/var/lib/cc-remote/closure.registered
build_dir=/var/lib/cc-remote/build
debs_dir=/opt/cc-remote/debs
closure_packages=({{range $i, $name := .Closure}}{{if $i}} {{end}}{{q $name}}{{end}})
{{- end}}
prerequisites=(ca-certificates curl git jq python3 unzip xz-utils)
tmp_dir="$(mktemp -d)"
trap 'status=$?; drain_artifacts || status=$?; rm -rf "$tmp_dir"; exit "$status"' EXIT

{{template "artifacts.sh" .}}
t64() {
  local name packages
  local -a aliases=()
  for name in "$@"; do
    aliases+=("${name}t64")
  done
  if [ "${#aliases[@]}" -eq 0 ]; then
    return
  fi
  packages="$(apt-cache -o APT::Cache::ShowVirtuals=true show "${aliases[@]}" 2> /dev/null)" || packages=""
  for name in "$@"; do
    if grep -qFx "Package: ${name}t64" <<< "$packages"; then
      echo "${name}t64"
    else
      echo "$name"
    fi
  done
}

has_prerequisites() {
  local bin
  for bin in curl git jq python3 unzip xz; do
    if ! command -v "$bin" > /dev/null; then
      return 1
    fi
  done
  [ -f /etc/ssl/certs/ca-certificates.crt ]
}

provision_prerequisites() {
  if has_prerequisites; then
    return
  fi
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends "${prerequisites[@]}" > /dev/null
}

{{- with .Closure}}

installed_packages() {
  dpkg-query -W -f '${db:Status-Abbrev}\t${Package}\n'
}

base_packages() {
  dpkg-query -W -f '${db:Status-Status}\t${Package}=${Version}\n' | sed -n 's/^installed\t//p' | LC_ALL=C sort
}

closure_package() {
  local package
  for package in "${closure_packages[@]}"; do
    if [ "$package" = "$1" ]; then
      return 0
    fi
  done
  return 1
}

record_deb() {
  local package
  package="$(dpkg-deb -f "$1" Package)" || exit
  if [ -z "$package" ]; then
    echo "cc-remote: $1 names no Package" >&2
    exit 1
  fi
  printf '%s\n' "$package" >> "$deb_seeds"
}

resident_packages() {
  local package
{{- if .Resident}}
  printf '%s\n'{{range .Resident}} {{q .}}{{end}}
{{- end}}
  for package in "$@"; do
    if ! closure_package "$package"; then
      printf '%s\n' "$package"
    fi
  done
}

stage_deb() {
  local file="$build_dir/artifacts/$1-$2.deb"
  fetch "$3" "$file" "$4" "$5"
  record_deb "$file"
  staged+=("$file")
}

uncaptured() {
  awk -F= -v list="$tmp_dir/captured" 'BEGIN { while ((getline name < list) > 0) captured[name] } !($1 in captured)'
}

check_base() {
  local manifest="$1" drift
  {
    jq -r '.debs[].package, .artifacts[].package' "$manifest"
{{- if $.Apt.Remove}}
    printf '%s\n'{{range $.Apt.Remove}} {{q .}}{{end}}
{{- end}}
  } > "$tmp_dir/captured"
  jq -r '.base[]' "$manifest" | uncaptured > "$tmp_dir/base.payload"
  base_packages | uncaptured > "$tmp_dir/base.machine"
  drift="$(LC_ALL=C comm -3 "$tmp_dir/base.payload" "$tmp_dir/base.machine" | sed -e 's/^\t/+/' -e t -e 's/^/-/')"
  if [ -n "$drift" ]; then
    echo "cc-remote: the packages on this machine differ from the base its payload captured the resident packages against (-payload +machine), so they cannot install offline; rebuild the payload on this base:" >&2
    printf '%s\n' "$drift" >&2
    exit 1
  fi
}

provision_packages() {
  local mode="${1:-full}" sha256="${2:-}" listing status package payload manifest digest file deb_dir
  local -a packages=("${prerequisites[@]}"{{range $.Apt.Install}} {{q .}}{{end}}) t64_packages shadowed=() resident=() staged=() captured=() deb_flags=()
  case "$mode" in
    full) ;;
    resident)
      if [ -z "$sha256" ]; then
        echo "provision: packages resident takes the payload sha256" >&2
        exit 2
      fi
      ;;
    *)
      echo "provision: packages takes full or resident, not $mode" >&2
      exit 2
      ;;
  esac
  export DEBIAN_FRONTEND=noninteractive
  if [ "$mode" = resident ]; then
    payload="$payload_root/$sha256"
    deb_dir="$payload$debs_dir"
    manifest="$deb_dir/debs.json"
    deb_flags=(--no-download)
    if ! mountpoint -q "$payload"; then
      echo "cc-remote: payload $sha256 is not mounted at $payload, so its captured packages are unreachable" >&2
      exit 1
    fi
    check_base "$manifest"
    jq -r '.debs[] | "\(.sha256) \(.file)"' "$manifest" > "$tmp_dir/debs"
    install -d -m 0755 "$tmp_dir/apt-archives/partial"
    while read -r digest file; do
      verify_payload "$digest" "$deb_dir/$file"
      install -m 0644 "$deb_dir/$file" "$tmp_dir/apt-archives/$file"
      captured+=("$deb_dir/$file")
    done < "$tmp_dir/debs"
    apt-get install -y -qq --no-download --no-install-recommends -o Dir::Cache::Archives="$tmp_dir/apt-archives/" "${captured[@]}" > /dev/null
  else
    apt-get update -qq
    mapfile -t t64_packages < <(t64{{range $.Apt.T64}} {{q .}}{{end}})
    packages+=("${t64_packages[@]}")
    deb_dir="$build_dir/artifacts"
    install -d -m 0755 "$build_dir" "$build_dir/debs/partial" "$deb_dir"
    deb_seeds="$build_dir/seeds"
    resident_packages "${packages[@]}" > "$deb_seeds"
    mapfile -t resident < "$deb_seeds"
    installed_packages > "$build_dir/packages.before"
    base_packages > "$build_dir/base"
{{- range $.System}}
{{- if eq .Format "deb"}}
    {{stage .}}
{{- end}}
{{- end}}
    apt-get install -y -qq --no-install-recommends --download-only -o Dir::Cache::Archives="$build_dir/debs/" "${resident[@]}" "${staged[@]}" > /dev/null
    apt-get install -y -qq --no-install-recommends "${packages[@]}"{{range .Resident}} {{q .}}{{end}} > /dev/null
  fi
{{- else}}

provision_packages() {
  local -a t64_packages
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  mapfile -t t64_packages < <(t64{{range .Apt.T64}} {{q .}}{{end}})
  apt-get install -y -qq --no-install-recommends "${prerequisites[@]}"{{range .Apt.Install}} {{q .}}{{end}} \
    "${t64_packages[@]}" > /dev/null
{{- end}}
{{- range .Apt.Remove}}
  if dpkg -s {{q .}} > /dev/null 2>&1; then
    apt-get remove -y -qq {{q .}} > /dev/null
  fi
{{- end}}
{{- range .System}}
{{- if eq .Format "deb"}}

  echo {{q (printf "--- Installing %s %s" .Name .Version)}}
  {{install . "tool_dir" "bin_dir"}}
{{- end}}
{{- end}}
{{- range .System}}
{{- if eq .Format "deb"}}
{{- range verify . "tool_dir" "bin_dir"}}
  {{.}}
{{- end}}
{{- end}}
{{- end}}
{{- with .Closure}}
  if [ "$mode" = full ]; then
    installed_packages > "$build_dir/packages.after"
  else
    listing="$(installed_packages)" || {
      echo "cc-remote: cannot list the installed packages" >&2
      exit 1
    }
    while IFS=$'\t' read -r status package; do
      case "${status:1:1}" in
        n | c) ;;
        *)
          if closure_package "$package"; then
            shadowed+=("$package")
          fi
          ;;
      esac
    done <<< "$listing"
    if [ "${#shadowed[@]}" -gt 0 ]; then
      echo "cc-remote: the resident install left closure packages installed, whose system copies would shadow the payload; move them to apt.payload.resident or recreate the machine:" >&2
      dpkg-query -W -f '${Package} ${Status} ${Version}\n' "${shadowed[@]}" >&2
      exit 1
    fi
  fi
{{- end}}
  rm -rf /var/lib/apt/lists/*
}

{{- with .Closure}}

provision_loader() {
  install -d -m 0755 "$(dirname "$closure_lock")"
  flock "$closure_lock" ldconfig
  : > "$closure_registered"
}

closure_link() {
  if [ "$(readlink "$1")" != "$2" ]; then
    echo "cc-remote: $1 is not a link to $2, so the payload cannot own it" >&2
    exit 1
  fi
}

closure_project() {
  local target="$closure_root$1" closure resolved
  if [ ! -d "$target" ]; then
    echo "cc-remote: the closure has no directory $1 to project" >&2
    exit 1
  fi
  closure="$(realpath -e "$closure_root")"
  resolved="$(realpath -e "$target")"
  case "$resolved" in
    "$closure"/*) ;;
    *)
      echo "cc-remote: $1 resolves to $resolved, outside the closure, so the payload cannot project it" >&2
      exit 1
      ;;
  esac
  if [ -L "$1" ]; then
    closure_link "$1" "$target"
    return
  fi
  if [ -e "$1" ]; then
    echo "cc-remote: $1 exists and is not the closure's link, so the payload cannot project it" >&2
    exit 1
  fi
  install -d -m 0755 "$(dirname "$1")"
  ln -s "$target" "$1"
}

write_conf() {
  local path="$1" content="$2" staged
  if [ -L "$path" ] || { [ -e "$path" ] && [ "$(cat "$path")" != "$content" ]; }; then
    echo "cc-remote: $path exists and is not the closure configuration this payload writes" >&2
    exit 1
  fi
  install -d -m 0755 "$(dirname "$path")"
  staged="$(mktemp "$(dirname "$path")/.cc-remote.XXXXXX")"
  printf '%s\n' "$content" > "$staged"
  chmod 0644 "$staged"
  mv -f "$staged" "$path"
}

fonts_conf() {
  cat <<XML
<?xml version="1.0"?>
<!DOCTYPE fontconfig SYSTEM "urn:fontconfig:fonts.dtd">
<fontconfig>
  <dir>$closure_root/usr/share/fonts</dir>
  <cachedir>$closure_root/var/cache/fontconfig</cachedir>
  <include ignore_missing="yes">$closure_root/etc/fonts/conf.d</include>
</fontconfig>
XML
}

provision_capture() {
  cat > "$tmp_dir/closure.json" <<'JSON'
{{json .}}
JSON
  cat > "$tmp_dir/capture.py" <<'PY'
{{template "capture.py"}}PY
  cat > "$tmp_dir/loader.py" <<'PY'
{{template "loader.py"}}PY
  python3 "$tmp_dir/capture.py" "$tmp_dir/closure.json" / "$closure_root" "$build_dir" "$1" "$tmp_dir/loader.py" "$debs_dir"
}
{{- end}}

provision_tools() {
  :
{{- range .System}}
{{- if ne .Format "deb"}}
  echo {{q (printf "--- Installing %s %s" .Name .Version)}}
  queue_artifact {{install . "tool_dir" "bin_dir"}}
{{- end}}
{{- end}}
  drain_artifacts
{{- range .Python.System}}

  echo {{q (printf "--- Installing %s" .Name)}}
  UV_TOOL_DIR=/opt/uv/tools UV_TOOL_BIN_DIR="$bin_dir" UV_PYTHON_INSTALL_DIR=/opt/uv/python \
    "$bin_dir/uv" tool install --quiet --python {{q $.Python.Version}} {{spec .}}{{range .Args}} {{q .}}{{end}}
{{- end}}
{{- with .Claude.ManagedSettings}}

  install -d -m 0755 /etc/claude-code
  cat > /etc/claude-code/managed-settings.json <<'JSON'
{{json .}}
JSON
{{- end}}
{{- if .Closure}}
  local link_check=spelling
{{- end}}
{{- range .System}}
{{- if ne .Format "deb"}}
{{- range verify . "tool_dir" "bin_dir"}}
  {{.}}
{{- end}}
{{- end}}
{{- end}}
{{- range .Python.System}}
{{- $tool := .}}
{{- range .Bins}}
  verify_bin {{q .}}{{if not $.Closure}}{{range $tool.Verify}} {{q .}}{{end}}{{end}}
{{- end}}
{{- end}}
}

passwd_home() {
  getent passwd "$SUDO_USER" | cut -d: -f6
}

os_version() {
  (
    # shellcheck source=/dev/null
    . /etc/os-release
    printf '%s\n' "$VERSION_ID"
  )
}

verify_payload() {
  if ! echo "$1  $2" | sha256sum -c --status -; then
    echo "cc-remote: $2 does not match its sha256 $1" >&2
    exit 1
  fi
}

provision_payload() {
  local sha256="${1:?payload needs the image sha256}" fingerprint="${2:?payload needs the tools fingerprint}"
  local image="$payload_store/$sha256.sqfs" payload="$payload_root/$sha256" user_home version mismatch device
  if [ ! -f "$image.admitted" ] && [ ! -f "$image" ]; then
    echo "cc-remote: no payload is admitted at $image.admitted" >&2
    exit 1
  fi
  cat > "$tmp_dir/payload-mount.sh" <<'SH'
#!/bin/sh
set -eu
verify() {
  if ! echo "$1  $2" | sha256sum -c --status -; then
    echo "cc-remote: $2 does not match its sha256 $1" >&2
    exit 1
  fi
}
exec 9> /var/lib/cc-remote/payload/.lock
flock 9
{{- if .Closure}}
boot="$(cat /proc/sys/kernel/random/boot_id)"
{{- end}}
for image in /var/lib/cc-remote/payload/*.sqfs; do
  if [ ! -e "$image" ]; then
    continue
  fi
  sha256="$(basename "$image" .sqfs)"
  dir="/opt/cc-remote/payload/$sha256"
  mkdir -p "$dir"
  if mountpoint -q "$dir"; then
{{- if .Closure}}
    if [ "$(cat "$image.boot" 2> /dev/null)" != "$boot" ]; then
      verify "$sha256" "$(findmnt -no SOURCE "$dir")"
    fi
{{- else}}
    device="$(findmnt -no SOURCE "$dir")"
    verify "$sha256" "$device"
{{- end}}
  else
    verify "$sha256" "$image"
    mount -t squashfs -o ro{{if .Closure}},nosuid,nodev{{end}},loop "$image" "$dir"
  fi
{{- if .Closure}}
  echo "$boot" > "$image.boot"
{{- end}}
done
{{- if .Closure}}
if [ -e /var/lib/cc-remote/closure.registered ]; then
  flock /var/lib/cc-remote/ldconfig.lock ldconfig
fi
{{- end}}
SH
  mkdir -p "$payload"
  install -m 0755 "$tmp_dir/payload-mount.sh" /opt/cc-remote/payload-mount.sh
  (
    flock 9
    if [ -f "$image.admitted" ]; then
      mv -f "$image.admitted" "$image"
    elif ! mountpoint -q "$payload"; then
      verify_payload "$sha256" "$image"
    fi
    if mountpoint -q "$payload"; then
      device="$(findmnt -no SOURCE "$payload")"
      verify_payload "$sha256" "$device"
    else
      mount -t squashfs -o ro{{if .Closure}},nosuid,nodev{{end}},loop "$image" "$payload"
    fi
{{- if .Closure}}
    cat /proc/sys/kernel/random/boot_id > "$image.boot"
{{- end}}
  ) 9> "$payload_store/.lock"
  if ! jq -se 'length == 1 and (.[0] | type == "object")' "$payload/cc-remote-payload.json" > /dev/null; then
    echo "cc-remote: payload $payload/cc-remote-payload.json is not exactly one JSON object" >&2
    exit 1
  fi
  user_home="$(passwd_home)"
  version="$(os_version)"
  mismatch="$(jq -r --arg dir "$payload" --arg tools "$fingerprint" --arg home "$user_home" --arg arch "$(uname -m)" --arg os "$version" '
    . as $have
    | {schemaVersion: {{if .Closure}}2{{else}}1{{end}}, tools: $tools, home: $home, arch: $arch, os: $os}
    | to_entries[]
    | select($have[.key] != .value)
    | "cc-remote: payload \($dir) has \(.key) \($have[.key] | tojson), want \(.value | tojson)"
  ' "$payload/cc-remote-payload.json")"
  if [ -n "$mismatch" ]; then
    printf '%s\n' "$mismatch" >&2
    exit 1
  fi
  if ! command -v sprite-env > /dev/null; then
    echo "cc-remote: the payload needs sprite-env to remount it at boot" >&2
    exit 1
  fi
  if ! sprite-env services get cc-remote-payload > /dev/null 2>&1; then
    queue_artifact sprite-env services create cc-remote-payload --cmd sudo --args "-n,sh,-c,/opt/cc-remote/payload-mount.sh && exec sleep infinity" --duration 1ms --no-stream
  fi
{{- range .SystemTrees}}
  expose {{.Kind}} {{.Requirement}} {{q .Path}}
{{- end}}
{{- with .Closure}}
  closure_link "$closure_root" "$payload$closure_root"
{{- range .Links}}
{{- if eq .Requirement "optional"}}
  if [ -d "$closure_root/"{{q .Target}} ]; then
    closure_link {{q .Path}} "$closure_root/"{{q .Target}}
  fi
{{- else}}
  closure_link {{q .Path}} "$closure_root/"{{q .Target}}
{{- end}}
{{- end}}
{{- range .Projections}}
  closure_project {{q .}}
{{- end}}
  write_conf "$closure_loader_conf" "$closure_root/usr/lib/$(uname -m)-linux-gnu"
  write_conf "$closure_fonts_conf" "$(fonts_conf)"
{{- end}}
  drain_artifacts
}

pack_path() {
  if [ -e "$2" ] || [ -L "$2" ]; then
    paths+=("${2#/}")
  elif [ "$1" = required ]; then
    echo "cc-remote: the build machine lacks $2" >&2
    exit 1
  fi
}

pack_once() {
  local path
  for path in "${paths[@]}"; do
    if [ "$path" = "${1#/}" ]; then
      return
    fi
  done
  pack_path required "$1"
}

pack_native() {
  local dir="$1" bin="$2" text root runner
  local -a entry
  binrun_launcher "$dir/$bin" "$bin" || return 0
  text="$(binrun_entry "$dir/$bin.binrun")" || exit
  mapfile -t entry <<< "$text"
  root="$(native_root "$user_home" "${entry[1]}")"
  runner="$(runner_dir "$user_home" "$dir/$bin")" || exit
  if ! native_hit "$root" "${entry[2]}" "${entry[1]}" "${entry[3]}" "${entry[4]}" || ! runner_tree "$runner"; then
    echo "cc-remote: the build machine lacks a verified ${entry[3]} ${entry[4]} native at $root" >&2
    exit 1
  fi
  pack_once "$root"
  pack_once "$runner"
}

provision_pack() {
  local fingerprint="${1:?pack needs the tools fingerprint}" user_home version
  local -a paths=()
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends squashfs-tools > /dev/null
  user_home="$(passwd_home)"
  version="$(os_version)"
{{- if .Closure}}
  provision_capture "$user_home"
  pack_path required "$debs_dir"
{{- end}}
{{- range .SystemTrees}}
  pack_path {{.Requirement}} {{q .Path}}
{{- end}}
{{- range .HomeTrees}}
  pack_path {{.Requirement}} {{under "user_home" .Path}}
{{- end}}
{{- range .Natives}}
  pack_native {{under "user_home" .Dir}} {{q .Bin}}
{{- end}}
  jq -n --arg tools "$fingerprint" --arg home "$user_home" --arg arch "$(uname -m)" --arg os "$version" \
    '{schemaVersion: {{if .Closure}}2{{else}}1{{end}}, tools: $tools, home: $home, arch: $arch, os: $os}' > "$tmp_dir/cc-remote-payload.json"
  install -d /var/lib/cc-remote/build
  tar -C / --numeric-owner --exclude=.in_use --exclude=.orphaned_at --exclude=.lock -cpf - "${paths[@]}" -C "$tmp_dir" cc-remote-payload.json \
    | mksquashfs - /var/lib/cc-remote/build/payload.sqfs -tar -comp zstd -noappend -no-progress -quiet
}

case "$phase" in
  prerequisites) provision_prerequisites ;;
{{- if .Closure}}
  packages) provision_packages "${2:-}" "${3:-}" ;;
  loader) provision_loader ;;
{{- else}}
  packages) provision_packages ;;
{{- end}}
  tools) provision_tools ;;
  payload) provision_payload "${2:-}" "${3:-}" ;;
  pack) provision_pack "${2:-}" ;;
  *)
    echo "usage: provision.sh prerequisites|packages{{if .Closure}} [full|resident SHA256]|loader{{end}}|tools|payload SHA256 FINGERPRINT|pack FINGERPRINT" >&2
    exit 2
    ;;
esac
