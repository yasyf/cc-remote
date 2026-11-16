package remote

import (
	"regexp"
	"strings"
)

const (
	StateDir = "$HOME/.cc-remote"
	Prefix   = "cc-remote"
)

var bare = regexp.MustCompile(`^[A-Za-z0-9_./:=+@%,-]+$`)

func Quote(word string) string {
	if bare.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

func QuoteAll(words []string) string {
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, Quote(word))
	}
	return strings.Join(quoted, " ")
}

func Script(lines ...string) string {
	return strings.Join(append([]string{"set -eu"}, lines...), "\n")
}

func CreateServiceFunc(spriteEnv string) string {
	return `create_sprite_service() (
  umask 077
  buffers="$(mktemp -d)"
  trap 'rm -rf "$buffers"' EXIT
  flag= cmd= args= needs=
  for word in "$@"; do
    case "$flag" in
      --cmd) cmd=$word ;;
      --args) args=$word ;;
      --needs) needs=${needs:+$needs,}$word ;;
    esac
    flag=$word
  done
  for try in 1 2; do
    status=0
    ` + spriteEnv + ` services create "$@" > "$buffers/out" 2> "$buffers/err" || status=$?
    if [ "$status" -eq 22 ] && [ "$(cat "$buffers/out")" = "service already exists: $1" ] && [ "$(cat "$buffers/err")" = "curl: (22) The requested URL returned error: 409" ]; then
      status=0
    else
      cat "$buffers/out"
      cat "$buffers/err" >&2
      [ "$status" -eq 0 ] && grep -q '"Service already running with that command' "$buffers/out" || exit "$status"
    fi
    if [ "$try" -eq 2 ] || ! ` + spriteEnv + ` services get "$1" > "$buffers/current" 2> /dev/null || [ "$(jq -r '[.cmd, (.args // [] | join(",")), (.needs // [] | join(",")), (.state.status | IN("running", "starting"))] | map(tostring) | join("\n")' "$buffers/current")" = "$(printf '%s\n%s\n%s\ntrue' "$cmd" "$args" "$needs")" ]; then
      exit 0
    fi
    ` + spriteEnv + ` services delete "$1" > /dev/null
  done
)`
}
