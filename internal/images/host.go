package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type Exec func(ctx context.Context, argv []string, stdin io.Reader) error

type Capture func(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error)

const (
	PayloadRoot   = "/opt/cc-remote/payload"
	PayloadStore  = "/var/lib/cc-remote/payload"
	PackPath      = "/var/lib/cc-remote/build/payload.sqfs"
	PackagesPack  = "/var/lib/cc-remote/build/packages.tar"
	PackagesStore = "/var/lib/cc-remote/packages"
	ClosurePath   = "/opt/cc-remote/closure"
	HelperStore   = "/var/lib/cc-remote/bootstrap-helpers"
)

type TransferArtifact struct {
	Label  string
	Name   string
	Store  string
	Suffix string
}

var (
	PayloadArtifact  = TransferArtifact{Label: "payload", Name: "payload", Store: PayloadStore, Suffix: ".sqfs"}
	PackagesArtifact = TransferArtifact{Label: "packages", Name: "packages archive", Store: PackagesStore, Suffix: ".tar"}
	HelperArtifact   = TransferArtifact{Label: "bootstrap-helper", Name: "bootstrap helper archive", Store: HelperStore, Suffix: ".tar.gz"}
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

const payloadURLLimit = 8192

func (a TransferArtifact) openStaging() string {
	return `set -euo pipefail
digest="$1"
store=` + a.Store + `
install -d -m 0755 "$store"
staging="$(mktemp "$store/$digest` + a.Suffix + `.XXXXXXXX.partial")"
trap 'rm -f "$staging"' EXIT
work="$(mktemp -d)"
trap 'rm -rf "$work"; rm -f "$staging"' EXIT
`
}

func (a TransferArtifact) admitStaging() string {
	return `read -r got _ < "$work/sum"
if [ "$got" != "$digest" ]; then
  echo "cc-remote: the ` + a.Name + ` has sha256 $got, want $digest" >&2
  exit 1
fi
( flock 9; mv -f "$staging" "$store/$digest` + a.Suffix + `.admitted" ) 9> "$store/.lock"`
}

func (a TransferArtifact) stageScript() string {
	return a.openStaging() + `set +e
cat | tee "$staging" | openssl dgst -sha256 -r > "$work/sum"
codes=("${PIPESTATUS[@]}")
set -e
if [ "${codes[*]}" != "0 0 0" ]; then
  echo "cc-remote: the ` + a.Name + ` stream failed (cat exit ${codes[0]}, tee exit ${codes[1]}, openssl exit ${codes[2]})" >&2
  exit 1
fi
` + a.admitStaging()
}

func (a TransferArtifact) fetchScript() string {
	return a.openStaging() + `size="$2"
set +e
curl -q --config - --silent --fail --proto =https --connect-timeout 30 --max-time 600 \
  --max-filesize "$size" --write-out '%{stderr}%{http_code}' 2> "$work/http" \
  | tee "$staging" | openssl dgst -sha256 -r > "$work/sum"
codes=("${PIPESTATUS[@]}")
set -e
http="$(cat "$work/http")"
[[ $http =~ ^[0-9]{3}$ ]] || http=none
if [ "${codes[*]}" != "0 0 0" ]; then
  hint=""
  if [ "$http" = 403 ]; then
    hint="; a 403 usually means the presigned URL expired"
  fi
  echo "cc-remote: the ` + a.Name + ` download failed (curl exit ${codes[0]}, HTTP $http, tee exit ${codes[1]}, openssl exit ${codes[2]})$hint" >&2
  exit 1
fi
if [ "$http" != 200 ]; then
  echo "cc-remote: the ` + a.Name + ` URL answered HTTP $http, want 200" >&2
  exit 1
fi
got="$(stat -c %s "$staging")"
if [ "$got" != "$size" ]; then
  echo "cc-remote: the ` + a.Name + ` download is $got bytes, want $size" >&2
  exit 1
fi
` + a.admitStaging()
}

const enablePayloadPlugins = `settings="$HOME/.claude/settings.json"
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

const stagePluginsEnabling = stagePlugins + "\n" + enablePayloadPlugins

const diagnoseMemory = `set -euo pipefail
root="${1:-}"
home="${2:-$(getent passwd "$SUDO_USER" | cut -d: -f6)}"
hierarchy="$root/sys/fs/cgroup"
amount='^([0-9]+|max)$'
probe='^[a-z][a-z_]*(:[A-Za-z0-9][A-Za-z0-9._@-]{0,127})?$'

read_amount() {
  local text
  text="$(cat "$dir/$1" 2> /dev/null)" || return 0
  if [[ $text =~ $amount ]]; then
    printf '%s' "$text"
  fi
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
events="$(cat "$dir/memory.events" 2> /dev/null)" || events=""
oom=null
if read_kmsg; then
  oom="$(printf '%s\n' "$kmsg" \
    | sed -n 's/^[0-9]*,[0-9]*,[0-9]*,[^;]*;//p' \
    | { grep -F -e oom-kill -e 'Out of memory' -e 'Killed process' || true; } \
    | tail -n 20 \
    | jq -Rsc 'split("\n") | map(select(. != ""))')"
fi
progress="$(head -c 160 "$root$home/.cc-remote/verify-progress" 2> /dev/null)" || progress=""
if [[ ! $progress =~ $probe ]]; then
  progress=""
fi
jq -cn \
  --arg cgroup "${dir#"$root"}" \
  --arg current "$(read_amount memory.current)" \
  --arg max "$(read_amount memory.max)" \
  --arg peak "$(read_amount memory.peak)" \
  --arg events "$events" \
  --argjson oom "$oom" \
  --arg progress "$progress" \
  'def amount: if . == "" then null elif . == "max" then . else tonumber end;
  {
    cgroup: $cgroup,
    "memory.current": ($current | amount),
    "memory.max": ($max | amount),
    "memory.peak": ($peak | amount),
    "memory.events": ([$events | splits("\n") | select(test("^[a-z_]+ [0-9]+$")) | split(" ") | {(.[0]): (.[1] | tonumber)}] | add),
    oom: $oom,
    progress: (if $progress == "" then null else $progress end)
  }'`

type PayloadURL struct{ raw string }

func ParsePayloadURL(raw string) (PayloadURL, error) {
	if raw == "" {
		return PayloadURL{}, errors.New("payload url: the command printed nothing")
	}
	if len(raw) > payloadURLLimit {
		return PayloadURL{}, fmt.Errorf("payload url: %d bytes is over the %d-byte limit", len(raw), payloadURLLimit)
	}
	for i := range len(raw) {
		if c := raw[i]; c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
			return PayloadURL{}, errors.New("payload url: the URL must be printable ASCII without spaces, quotes or backslashes")
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return PayloadURL{}, errors.New("payload url: the URL must be an absolute https URL with a host and no userinfo")
	}
	return PayloadURL{raw: raw}, nil
}

func (PayloadURL) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "[payload url]")
}

func (u PayloadURL) curlConfig() []byte {
	return []byte(`url = "` + u.raw + "\"\n")
}

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

func (s Scripts) Stage(ctx context.Context, exec Exec, artifact TransferArtifact, stream io.Reader, digest string) error {
	if !sha256Pattern.MatchString(digest) {
		return fmt.Errorf("stage %s: %q is not a sha256 digest", artifact.Name, digest)
	}
	if err := exec(ctx, []string{"sudo", "bash", "-c", artifact.stageScript(), "stage-" + artifact.Label, digest}, stream); err != nil {
		return fmt.Errorf("stage %s %s: %w", artifact.Name, digest, err)
	}
	return nil
}

func (s Scripts) Fetch(ctx context.Context, exec Exec, artifact TransferArtifact, source PayloadURL, digest string, size int64) error {
	if !sha256Pattern.MatchString(digest) {
		return fmt.Errorf("fetch %s: %q is not a sha256 digest", artifact.Name, digest)
	}
	if size <= 0 {
		return fmt.Errorf("fetch %s: size %d is not a byte count", artifact.Name, size)
	}
	if err := exec(ctx, []string{"sudo", "bash", "-c", artifact.fetchScript(), "fetch-" + artifact.Label, digest, strconv.FormatInt(size, 10)}, bytes.NewReader(source.curlConfig())); err != nil {
		return fmt.Errorf("fetch %s %s: %w", artifact.Name, digest, err)
	}
	return nil
}

func (s Scripts) StagePlugins(ctx context.Context, exec Exec, payload string) error {
	argv := []string{"sh", "-c", stagePlugins}
	if payload != "" {
		if !sha256Pattern.MatchString(payload) {
			return fmt.Errorf("stage plugins.sh: payload %q is not a sha256 digest", payload)
		}
		argv = []string{"sh", "-c", stagePluginsEnabling, "stage-plugins", PayloadRoot + "/" + payload}
	}
	if err := exec(ctx, argv, bytes.NewReader(s.Plugins)); err != nil {
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
