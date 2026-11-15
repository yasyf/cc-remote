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
  status=0
  ` + spriteEnv + ` services create "$@" > "$buffers/out" 2> "$buffers/err" || status=$?
  if [ "$status" -eq 22 ] && [ "$(cat "$buffers/out")" = "service already exists: $1" ] && [ "$(cat "$buffers/err")" = "curl: (22) The requested URL returned error: 409" ]; then
    status=0
  else
    cat "$buffers/out"
    cat "$buffers/err" >&2
  fi
  rm -rf "$buffers"
  exit "$status"
)`
}
