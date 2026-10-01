#!/bin/bash

set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "provision: run as root" >&2
  exit 1
fi
if [ -e /var/lib/tailscale ]; then
  echo "provision: /var/lib/tailscale exists, so this machine already joined a tailnet; every workspace enrolls its own node after its claim" >&2
  exit 1
fi

tool_dir=/opt/cc-remote/tools
bin_dir=/usr/local/bin
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

{{template "artifacts.sh"}}
t64() {
  local name
  for name in "$@"; do
    if apt-cache show "${name}t64" > /dev/null 2>&1; then
      echo "${name}t64"
    else
      echo "$name"
    fi
  done
}

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
mapfile -t t64_packages < <(t64{{range .Apt.T64}} {{q .}}{{end}})
apt-get install -y -qq --no-install-recommends ca-certificates curl git jq python3 unzip xz-utils{{range .Apt.Install}} {{q .}}{{end}} \
  "${t64_packages[@]}" > /dev/null
{{- range .Apt.Remove}}
if dpkg -s {{q .}} > /dev/null 2>&1; then
  apt-get remove -y -qq {{q .}} > /dev/null
fi
{{- end}}
{{range .System}}
echo {{q (printf "--- Installing %s %s" .Name .Version)}}
{{install . "tool_dir" "bin_dir"}}
{{- end}}
rm -rf /var/lib/apt/lists/*
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
{{range .System}}
{{- range verify . "tool_dir" "bin_dir"}}
{{.}}
{{- end}}
{{- end}}
{{- range .Python.System}}
{{- $tool := .}}
{{- range .Bins}}
verify_bin {{q .}}{{range $tool.Verify}} {{q .}}{{end}}
{{- end}}
{{- end}}
