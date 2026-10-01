package remote

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQuoteRoundTripsThroughSh(t *testing.T) {
	words := []string{"plain", "/usr/local/bin", "has space", "it's", `"double"`, "$HOME", "a;b", "", "tab\there", "x=y"}
	out, err := exec.Command("sh", "-c", `for w in `+QuoteAll(words)+`; do printf '%s\n' "$w"; done`).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(words) {
		t.Fatalf("sh split %d words from %d: %q", len(got), len(words), got)
	}
	for i, word := range words {
		if got[i] != word {
			t.Errorf("word %d: sh read %q, want %q", i, got[i], word)
		}
	}
}

func TestQuoteLeavesBareWordsAlone(t *testing.T) {
	for _, word := range []string{"abc", "/a/b.c", "k=v", "x:y", "a,b"} {
		if Quote(word) != word {
			t.Errorf("Quote(%q) = %q", word, Quote(word))
		}
	}
	if Quote("a b") != "'a b'" || Quote("") != "''" {
		t.Errorf("Quote quoted %q and %q", Quote("a b"), Quote(""))
	}
}

func TestScriptFailsFast(t *testing.T) {
	script := Script("false", "echo unreachable")
	if !strings.HasPrefix(script, "set -eu\n") {
		t.Fatalf("script = %q", script)
	}
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err == nil || strings.Contains(string(out), "unreachable") {
		t.Errorf("the script ran past a failed line: %v %q", err, out)
	}
}
