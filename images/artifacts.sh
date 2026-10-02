fetch() {
  local url="$1" file="$2" algorithm="$3" digest="$4"
  curl -sSfL --retry 3 --retry-all-errors --retry-delay 2 "$url" -o "$file"
  if ! echo "$digest  $file" | "${algorithm}sum" -c --status -; then
    echo "cc-remote: $url does not match its pinned $algorithm $digest" >&2
    exit 1
  fi
}

install_artifact() {
  local name="$1" version="$2" url="$3" algorithm="$4" digest="$5" format="$6" dir="$7" links="$8"
  local staging download
  shift 8
  if [ "$format" = deb ] || [ "$(cat "$dir/.cc-remote-digest" 2> /dev/null)" != "$digest" ]; then
    staging="$(mktemp -d "$tmp_dir/artifact.XXXXXX")"
    download="$staging/$name-$version.$format"
    fetch "$url" "$download" "$algorithm" "$digest"
    rm -rf "$dir"
    mkdir -p "$dir"
    case "$format" in
      binary) install -m 0755 "$download" "$dir/$name" ;;
      gzip)
        gunzip -c "$download" > "$dir/$name"
        chmod 0755 "$dir/$name"
        ;;
      tar.gz) tar -xzf "$download" -C "$dir" ;;
      tar.xz) tar -xJf "$download" -C "$dir" ;;
      zip) unzip -q "$download" -d "$dir" ;;
      deb) apt-get install -y -qq "$download" > /dev/null ;;
    esac
    rm -rf "$staging"
    printf '%s\n' "$digest" > "$dir/.cc-remote-digest"
  fi
  mkdir -p "$links"
  while [ "$#" -gt 0 ]; do
    case "$2" in
      /*) ln -sfn "$2" "$links/$1" ;;
      *) ln -sfn "$dir/$2" "$links/$1" ;;
    esac
    shift 2
  done
}

verify_pin() {
  if [ "$(cat "$1/.cc-remote-digest" 2> /dev/null)" != "$2" ]; then
    echo "cc-remote: $1 is not installed at its pinned digest $2" >&2
    exit 1
  fi
}

link_check=full

verify_link() {
  local link="$1" target="$2"
  shift 2
  if [ "$(readlink "$link")" != "$target" ] || { [ "$link_check" = full ] && [ ! -x "$target" ]; }; then
    echo "cc-remote: $link does not point at the pinned $target" >&2
    exit 1
  fi
  if [ "$link_check" = full ] && [ "$#" -gt 0 ]; then
    "$link" "$@" > /dev/null
  fi
}

verify_bin() {
  local bin="$1"
  shift
  if ! command -v "$bin" > /dev/null; then
    echo "cc-remote: $bin is not on PATH" >&2
    exit 1
  fi
  if [ "$#" -gt 0 ]; then
    "$bin" "$@" > /dev/null
  fi
}

artifact_pids=()
artifact_status=0

wait_artifact() {
  local pid="${artifact_pids[0]}" status
  artifact_pids=("${artifact_pids[@]:1}")
  if wait "$pid"; then
    return 0
  else
    status=$?
    if [ "$artifact_status" -eq 0 ]; then
      artifact_status="$status"
    fi
    return "$status"
  fi
}

drain_artifacts() {
  while [ "${#artifact_pids[@]}" -gt 0 ]; do
    wait_artifact || :
  done
  return "$artifact_status"
}

queue_artifact() {
  if [ "${#artifact_pids[@]}" -eq 4 ]; then
    if ! wait_artifact; then
      drain_artifacts
      return
    fi
  fi
  "$@" < /dev/null &
  artifact_pids+=("$!")
}

expose() {
  local kind="$1" requirement="$2" path="$3" source="$payload$3"
  if [ ! -e "$source" ] && [ ! -L "$source" ]; then
    if [ "$requirement" = required ]; then
      echo "cc-remote: payload $payload lacks $path" >&2
      exit 1
    fi
    return
  fi
  if [ -L "$path" ]; then
    case "$(readlink "$path")" in
      "$(dirname "$payload")"/*) rm -f "$path" ;;
      *) return ;;
    esac
  elif [ "$kind" = children ] && [ -d "$path" ]; then
    if [ -z "$(find "$path" -mindepth 1 -maxdepth 1 -type l -lname "$(dirname "$payload")/*" -print -delete)" ]; then
      return
    fi
  elif [ -e "$path" ]; then
    return
  fi
  mkdir -p "$(dirname "$path")"
  case "$kind" in
    link) ln -s "$source" "$path" ;;
    copy) cp -a "$source" "$path" ;;
    children)
      mkdir -p "$path"
      find "$source" -mindepth 1 -maxdepth 1 ! -name .lock -exec ln -s -t "$path" {} +
      ;;
  esac
}

binrun_launcher() {
  if [ ! -f "$1" ]; then
    echo "cc-remote: $1 is not a file" >&2
    exit 1
  fi
  grep -qF "DESCRIPTOR=\"\$ROOT/$2.binrun\"" "$1" && grep -qF "exec \"\$RUNNER_BIN\" \"\$DESCRIPTOR\" \"\$@\"" "$1"
}

native_platform() {
  case "$(uname -m)" in
    x86_64) echo linux-x86_64 ;;
    aarch64) echo linux-aarch64 ;;
    *)
      echo "cc-remote: binrun descriptors have no platform for $(uname -m)" >&2
      exit 1
      ;;
  esac
}

runner_arch() {
  case "$(uname -m)" in
    x86_64) echo amd64 ;;
    aarch64) echo arm64 ;;
    *)
      echo "cc-remote: binrun has no release for $(uname -m)" >&2
      exit 1
      ;;
  esac
}

binrun_entry() {
  local platform
  platform="$(native_platform)" || exit
  if ! sed '1{/^#!/d;}' "$1" | jq -er --arg platform "$platform" '
    def word: type == "string" and test("^[A-Za-z0-9._-]+$");
    if .schema == 1 and .kind == "release-binary"
      and (.name | word)
      and (.version.static | type == "string" and length > 0)
      and (.platforms | length > 0)
      and all(.platforms[]; .size > 0 and .hash == "sha256" and (.digest | test("^[0-9a-f]{64}$")))
    then . else error("not a static release-binary descriptor") end
    | .name as $name
    | .platforms[$platform] // error("no \($platform) entry")
    | if (.path | type == "string" and test("^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$") and (split("/") | all(. != "." and . != "..")))
      and ((.format // "") | IN("", "tar.gz", "zip"))
      and (.providers | type == "array" and length > 0)
      and (.providers[0] | .type == "github-release" and (.repo | type == "string" and test("^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")) and (.tag | word) and (.name | word))
    then (.format // ""), .digest, .path, $name, .providers[0].tag, "https://github.com/\(.providers[0].repo)/releases/download/\(.providers[0].tag)/\(.providers[0].name)"
    else error("malformed \($platform) entry") end
  '; then
    echo "cc-remote: $1 is not a pinned release-binary descriptor for $platform" >&2
    exit 1
  fi
}

native_root() {
  printf '%s/.daemonkit/cache/%s/%s\n' "$1" "${2:0:2}" "$2"
}

native_tree() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ -z "$(find "$1" -mindepth 1 ! -type f ! -type d -print -quit)" ]
}

native_hit() {
  local root="$1" path="$2" digest="$3" name="$4" tag="$5"
  native_tree "$root" && [ -f "$root/$path" ] && [ -x "$root/$path" ] && [ -f "$root/meta.json" ] \
    && jq -se --arg digest "$digest" --arg name "$name" --arg tag "$tag" \
      'length == 1 and .[0].digest == $digest and .[0].name == $name and .[0].tag == $tag' "$root/meta.json" > /dev/null
}

launcher_var() {
  local value
  value="$(sed -n "s/^$2=\"\\([^\"]*\\)\"\$/\\1/p" "$1")"
  case "$value" in
    "" | *$'\n'*) ;;
    *)
      if grep -qxE "$3" <<< "$value"; then
        printf '%s\n' "$value"
        return
      fi
      ;;
  esac
  echo "cc-remote: $1 does not pin $2 once" >&2
  exit 1
}

runner_pin() {
  local arch
  arch="$(runner_arch)" || exit
  launcher_var "$1" RUNNER_REPO '[A-Za-z0-9._-]+/[A-Za-z0-9._-]+'
  launcher_var "$1" RUNNER_TAG 'v[A-Za-z0-9._-]+'
  launcher_var "$1" "RUNNER_SHA_linux_$arch" '[0-9a-f]{64}'
}

runner_dir() {
  local tag
  tag="$(launcher_var "$2" RUNNER_TAG 'v[A-Za-z0-9._-]+')" || exit
  printf '%s/.daemonkit/binrun/%s\n' "$1" "$tag"
}

runner_tree() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ -f "$1/binrun" ] && [ -x "$1/binrun" ] && [ ! -L "$1/binrun" ] \
    && [ -z "$(find "$1" -mindepth 1 ! -path "$1/binrun" -print -quit)" ]
}
