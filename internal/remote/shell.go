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
