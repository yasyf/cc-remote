package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

type Exec func(ctx context.Context, argv []string, stdin io.Reader) error

type Capture func(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error)

const (
	PayloadRoot  = "/opt/cc-remote/payload"
	PayloadStore = "/var/lib/cc-remote/payload"
	PackPath     = "/var/lib/cc-remote/build/payload.sqfs"
	ClosurePath  = "/opt/cc-remote/closure"
)

const (
	PhasePrerequisites = "prerequisites"
	PhasePackages      = "packages"
	PhaseTools         = "tools"
	PhasePayload       = "payload"
	PhaseLoader        = "loader"
	PhasePack          = "pack"

	PackagesFull     = "full"
	PackagesResident = "resident"
)

const stagePlugins = `set -eu
mkdir -p "$HOME/.cc-remote"
cat > "` + PluginsPath + `.tmp"
mv -f "` + PluginsPath + `.tmp" "` + PluginsPath + `"`

const stagePayload = "install -d -m 0755 " + PayloadStore + " && cat > " + PayloadStore + "/$1.sqfs.partial"

const enablePayloadPlugins = `set -eu
settings="$HOME/.claude/settings.json"
image="$1$settings"
if [ ! -f "$settings" ] || [ ! -f "$image" ]; then
  exit 0
fi
for file in "$settings" "$image"; do
  if ! jq -se 'length == 1 and (.[0] | type == "object" and ((has("enabledPlugins") | not) or (.enabledPlugins | type == "object")))' "$file" > /dev/null 2>&1; then
    echo "cc-remote: $file is not one JSON object whose enabledPlugins, when present, is an object" >&2
    exit 1
  fi
done
umask 077
edited="$(mktemp "$settings.XXXXXX")"
trap 'rm -f "$edited"' EXIT
jq --slurpfile image "$image" '.enabledPlugins = (.enabledPlugins // {}) + ($image[0].enabledPlugins // {})' "$settings" > "$edited"
mv "$edited" "$settings"`

const diagnoseMemory = `set -euo pipefail
root="${1:-}"
home="${2:-$(getent passwd "$SUDO_USER" | cut -d: -f6)}"
hierarchy="$root/sys/fs/cgroup"
anchor="$(realpath -m "$hierarchy")"
amount='^([0-9]+|max)$'
count='^[0-9]+$'
probe='^[a-z][a-z_]*(:[A-Za-z0-9][A-Za-z0-9._@-]{0,127})?$'
uuid='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
cgroup_path='^(/|(/[A-Za-z0-9_@:+-][A-Za-z0-9._@:+-]*)+)$'

valid() {
  if [[ $2 =~ $1 ]]; then
    printf '%s' "$2"
  fi
}

read_amount() {
  local text
  text="$(cat "$1" 2> /dev/null)" || return 0
  valid "$amount" "$text"
}

reading() {
  local events
  events="$(cat "$1/memory.events" 2> /dev/null)" || events=""
  jq -cn \
    --arg cgroup "${1#"$root"}" \
    --arg current "$(read_amount "$1/memory.current")" \
    --arg max "$(read_amount "$1/memory.max")" \
    --arg peak "$(read_amount "$1/memory.peak")" \
    --arg events "$events" \
    'def amount: if . == "" then null elif . == "max" then . else tonumber end;
    {
      cgroup: $cgroup,
      "memory.current": ($current | amount),
      "memory.max": ($max | amount),
      "memory.peak": ($peak | amount),
      "memory.events": ([$events | splits("\n") | select(test("^[a-z_]+ [0-9]+$")) | split(" ") | {(.[0]): (.[1] | tonumber)}] | add)
    }'
}

under_hierarchy() {
  local resolved
  resolved="$(realpath -m "$1")"
  [ "$resolved" = "$anchor" ] || [[ $resolved == "$anchor"/* ]]
}

recorded() {
  sed -n "s/^$1 //p" <<< "$context"
}

read_kmsg() {
  local status
  for _ in 1 2 3; do
    status=0
    kmsg="$(timeout 2 cat "$root/dev/kmsg" 2> /dev/null)" || status=$?
    if [ "$status" = 0 ] || [ "$status" = 124 ]; then
      return 0
    fi
  done
  return 1
}

cgroup="$(sed -n 's/^0:://p' "$root/proc/self/cgroup")"
dir="$hierarchy${cgroup%/}"
while [ ! -e "$dir/memory.current" ] && [ "$dir" != "$hierarchy" ]; do
  dir="${dir%/*}"
done
diagnostic="$(reading "$dir")"
context="$(head -c 4096 "$root$home/.cc-remote/verify-context" 2> /dev/null)" || context=""
boot_then="$(valid "$uuid" "$(recorded boot_id)")"
boot_now="$(cat "$root/proc/sys/kernel/random/boot_id" 2> /dev/null)" || boot_now=""
boot_now="$(valid "$uuid" "$boot_now")"
same_boot=null
if [ -n "$boot_then" ] && [ -n "$boot_now" ]; then
  same_boot=false
  if [ "$boot_then" = "$boot_now" ]; then
    same_boot=true
  fi
fi
exec_cgroup="$(valid "$cgroup_path" "$(recorded cgroup)")"
exec_dir=""
failed_exec=null
if [ -n "$exec_cgroup" ] && under_hierarchy "$hierarchy${exec_cgroup%/}"; then
  exec_dir="$hierarchy${exec_cgroup%/}"
  if [ "$same_boot" = true ] && [ -d "$exec_dir" ]; then
    failed_exec="$(reading "$exec_dir")"
  fi
fi
oom_kills="$(sed -n 's/^oom_kill //p' "$root/proc/vmstat" 2> /dev/null)" || oom_kills=""
oom_kills="$(valid "$count" "$oom_kills")"
oom=null
if read_kmsg; then
  oom="$(printf '%s\n' "$kmsg" \
    | sed -n 's/^[0-9]*,[0-9]*,[0-9]*,[^;]*;//p' \
    | { grep -F -e oom-kill -e 'Out of memory' -e 'Killed process' || true; } \
    | tail -n 20 \
    | jq -Rsc 'split("\n") | map(select(. != ""))')"
fi
progress="$(head -c 160 "$root$home/.cc-remote/verify-progress" 2> /dev/null)" || progress=""
progress="$(valid "$probe" "$progress")"
jq -cn \
  --arg boot_then "$boot_then" \
  --arg boot_now "$boot_now" \
  --argjson same_boot "$same_boot" \
  --arg progress "$progress" \
  --arg failed_exec_cgroup "${exec_dir#"$root"}" \
  --argjson failed_exec "$failed_exec" \
  --argjson diagnostic "$diagnostic" \
  --arg oom_kills "$oom_kills" \
  --argjson oom "$oom" \
  'def text: if . == "" then null else . end;
  {
    bootId: {"then": ($boot_then | text), "now": ($boot_now | text)},
    sameBoot: $same_boot,
    countersResetAfterFailedExec: ($same_boot | if . == null then null else not end),
    progress: ($progress | text),
    failedExecCgroup: ($failed_exec_cgroup | text),
    cgroups: {failedExec: $failed_exec, diagnostic: $diagnostic},
    "vmstat.oom_kill": ($oom_kills | if . == "" then null else tonumber end),
    oom: $oom
  }'`

func (s Scripts) Provision(ctx context.Context, exec Exec, phase string, args ...string) error {
	sudo := []string{"sudo"}
	if phase == PhasePayload {
		// sudo's secure_path drops /.sprite/bin, where the payload phase finds sprite-env.
		sudo = append(sudo, "--preserve-env=PATH")
	}
	argv := slices.Concat(sudo, []string{"bash", "-s", phase}, args)
	if err := exec(ctx, argv, bytes.NewReader(s.ProvisionScript)); err != nil {
		return fmt.Errorf("provision %s: %w", phase, err)
	}
	return nil
}

func (s Scripts) StagePayload(ctx context.Context, exec Exec, image io.Reader, digest string) error {
	if !sha256Pattern.MatchString(digest) {
		return fmt.Errorf("stage payload: %q is not a sha256 digest", digest)
	}
	if err := exec(ctx, []string{"sudo", "sh", "-c", stagePayload, "stage-payload", digest}, image); err != nil {
		return fmt.Errorf("stage payload %s: %w", digest, err)
	}
	return nil
}

func (s Scripts) EnablePayloadPlugins(ctx context.Context, exec Exec, digest string) error {
	if !sha256Pattern.MatchString(digest) {
		return fmt.Errorf("enable payload plugins: %q is not a sha256 digest", digest)
	}
	if err := exec(ctx, []string{"sh", "-c", enablePayloadPlugins, "enable-payload-plugins", PayloadRoot + "/" + digest}, nil); err != nil {
		return fmt.Errorf("enable payload plugins %s: %w", digest, err)
	}
	return nil
}

func (s Scripts) StagePlugins(ctx context.Context, exec Exec) error {
	if err := exec(ctx, []string{"sh", "-c", stagePlugins}, bytes.NewReader(s.Plugins)); err != nil {
		return fmt.Errorf("stage plugins.sh: %w", err)
	}
	return nil
}

func (s Scripts) Install(ctx context.Context, exec Exec, githubToken, payload string) error {
	if strings.ContainsAny(githubToken, "\r\n") {
		return errors.New("plugins install: the GitHub token spans lines")
	}
	args := []string{"install"}
	if payload != "" {
		if !sha256Pattern.MatchString(payload) {
			return fmt.Errorf("plugins install: payload %q is not a sha256 digest", payload)
		}
		args = append(args, PayloadRoot+"/"+payload)
	}
	return runPlugins(ctx, exec, args, nil, strings.NewReader(githubToken+"\n"))
}

func (s Scripts) Natives(ctx context.Context, exec Exec) error {
	return runPlugins(ctx, exec, []string{"natives"}, nil, nil)
}

func (s Scripts) Publish(ctx context.Context, exec Exec, stamp string) error {
	if !sha256Pattern.MatchString(stamp) {
		return fmt.Errorf("plugins publish: stamp %q is not a fingerprint", stamp)
	}
	return runPlugins(ctx, exec, []string{"publish", stamp}, nil, nil)
}

func (s Scripts) Ready(ctx context.Context, exec Exec, stamp string) error {
	if !sha256Pattern.MatchString(stamp) {
		return fmt.Errorf("plugins ready: stamp %q is not a fingerprint", stamp)
	}
	return runPlugins(ctx, exec, []string{"ready", stamp}, nil, nil)
}

func (s Scripts) Configure(ctx context.Context, exec Exec, env map[string]string) error {
	if have, want := slices.Sorted(maps.Keys(env)), slices.Sorted(slices.Values(s.Env)); !slices.Equal(have, want) {
		return fmt.Errorf("plugins configure: got environment %v, the inventory declares %v", have, want)
	}
	assignments := make([]string, 0, len(env))
	for _, key := range slices.Sorted(maps.Keys(env)) {
		assignments = append(assignments, key+"="+env[key])
	}
	return runPlugins(ctx, exec, []string{"configure"}, assignments, nil)
}

func (s Scripts) Verify(ctx context.Context, exec Exec) error {
	return runPlugins(ctx, exec, []string{"verify"}, nil, nil)
}

func (s Scripts) DiagnoseMemory(ctx context.Context, capture Capture) (json.RawMessage, error) {
	out, err := capture(ctx, []string{"sudo", "bash", "-s"}, strings.NewReader(diagnoseMemory))
	if err != nil {
		return nil, fmt.Errorf("diagnose memory: %w", err)
	}
	var evidence bytes.Buffer
	if err := json.Compact(&evidence, out); err != nil {
		return nil, fmt.Errorf("diagnose memory: the evidence is not JSON: %w", err)
	}
	return evidence.Bytes(), nil
}

func runPlugins(ctx context.Context, exec Exec, args, env []string, stdin io.Reader) error {
	argv := slices.Concat([]string{"env"}, env, []string{"bash", "-c", `exec bash "` + PluginsPath + `" "$@"`, "plugins.sh"}, args)
	if err := exec(ctx, argv, stdin); err != nil {
		return fmt.Errorf("plugins %s: %w", args[0], err)
	}
	return nil
}
