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
  local download="$tmp_dir/$name-$version.$format"
  shift 8
  if [ "$format" = deb ] || [ "$(cat "$dir/.cc-remote-digest" 2> /dev/null)" != "$digest" ]; then
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
    rm -f "$download"
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

verify_link() {
  local link="$1" target="$2"
  shift 2
  if [ "$(readlink "$link")" != "$target" ] || [ ! -x "$target" ]; then
    echo "cc-remote: $link does not point at the pinned $target" >&2
    exit 1
  fi
  if [ "$#" -gt 0 ]; then
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
