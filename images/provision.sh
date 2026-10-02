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
prerequisites=(ca-certificates curl git jq python3 unzip xz-utils)
tmp_dir="$(mktemp -d)"
trap 'status=$?; drain_artifacts || status=$?; rm -rf "$tmp_dir"; exit "$status"' EXIT

{{template "artifacts.sh"}}
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

provision_packages() {
  local -a t64_packages
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  mapfile -t t64_packages < <(t64{{range .Apt.T64}} {{q .}}{{end}})
  apt-get install -y -qq --no-install-recommends "${prerequisites[@]}"{{range .Apt.Install}} {{q .}}{{end}} \
    "${t64_packages[@]}" > /dev/null
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
  rm -rf /var/lib/apt/lists/*
}

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
  verify_bin {{q .}}{{range $tool.Verify}} {{q .}}{{end}}
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
  if [ ! -f "$image.partial" ] && [ ! -f "$image" ]; then
    echo "cc-remote: no payload is staged at $image.partial" >&2
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
for image in /var/lib/cc-remote/payload/*.sqfs; do
  if [ ! -e "$image" ]; then
    continue
  fi
  sha256="$(basename "$image" .sqfs)"
  dir="/opt/cc-remote/payload/$sha256"
  mkdir -p "$dir"
  if mountpoint -q "$dir"; then
    device="$(findmnt -no SOURCE "$dir")"
    verify "$sha256" "$device"
  else
    verify "$sha256" "$image"
    mount -t squashfs -o ro,loop "$image" "$dir"
  fi
done
SH
  mkdir -p "$payload"
  install -m 0755 "$tmp_dir/payload-mount.sh" /opt/cc-remote/payload-mount.sh
  (
    flock 9
    if [ -f "$image.partial" ]; then
      verify_payload "$sha256" "$image.partial"
      mv -f "$image.partial" "$image"
    elif ! mountpoint -q "$payload"; then
      verify_payload "$sha256" "$image"
    fi
    if mountpoint -q "$payload"; then
      device="$(findmnt -no SOURCE "$payload")"
      verify_payload "$sha256" "$device"
    else
      mount -t squashfs -o ro,loop "$image" "$payload"
    fi
  ) 9> "$payload_store/.lock"
  if ! jq -se 'length == 1 and (.[0] | type == "object")' "$payload/cc-remote-payload.json" > /dev/null; then
    echo "cc-remote: payload $payload/cc-remote-payload.json is not exactly one JSON object" >&2
    exit 1
  fi
  user_home="$(passwd_home)"
  version="$(os_version)"
  mismatch="$(jq -r --arg dir "$payload" --arg tools "$fingerprint" --arg home "$user_home" --arg arch "$(uname -m)" --arg os "$version" '
    . as $have
    | {schemaVersion: 1, tools: $tools, home: $home, arch: $arch, os: $os}
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
    queue_artifact sprite-env services create cc-remote-payload --cmd sudo --args "-n,sh,-c,/opt/cc-remote/payload-mount.sh && exec sleep infinity" --no-stream
  fi
{{- range .SystemTrees}}
  expose {{.Kind}} {{.Requirement}} {{q .Path}}
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

provision_pack() {
  local fingerprint="${1:?pack needs the tools fingerprint}" user_home version
  local -a paths=()
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq --no-install-recommends squashfs-tools > /dev/null
  user_home="$(passwd_home)"
  version="$(os_version)"
{{- range .SystemTrees}}
  pack_path {{.Requirement}} {{q .Path}}
{{- end}}
{{- range .HomeTrees}}
  pack_path {{.Requirement}} {{under "user_home" .Path}}
{{- end}}
  jq -n --arg tools "$fingerprint" --arg home "$user_home" --arg arch "$(uname -m)" --arg os "$version" \
    '{schemaVersion: 1, tools: $tools, home: $home, arch: $arch, os: $os}' > "$tmp_dir/cc-remote-payload.json"
  install -d /var/lib/cc-remote/build
  tar -C / --numeric-owner --exclude=.in_use --exclude=.orphaned_at --exclude=.lock -cpf - "${paths[@]}" -C "$tmp_dir" cc-remote-payload.json \
    | mksquashfs - /var/lib/cc-remote/build/payload.sqfs -tar -comp zstd -noappend -no-progress -quiet
}

case "$phase" in
  prerequisites) provision_prerequisites ;;
  packages) provision_packages ;;
  tools) provision_tools ;;
  payload) provision_payload "${2:-}" "${3:-}" ;;
  pack) provision_pack "${2:-}" ;;
  *)
    echo "usage: provision.sh prerequisites|packages|tools|payload SHA256 FINGERPRINT|pack FINGERPRINT" >&2
    exit 2
    ;;
esac
