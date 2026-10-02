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
