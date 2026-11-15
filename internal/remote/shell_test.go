package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestCreateServiceCountsOnlyAnExistingServiceAsCreated(t *testing.T) {
	const exists = "curl: (22) The requested URL returned error: 409\n"
	tests := []struct {
		name, stdout, stderr string
		exit, wantExit       int
	}{
		{name: "a create that starts the service passes its answer through", stdout: "{\"type\":\"started\"}\n"},
		{name: "a 409 saying the service already exists counts as created", stdout: "service already exists: cc-remote-capt\n", stderr: exists, exit: 22},
		{name: "a 409 saying another service already exists fails", stdout: "service already exists: cc-remote-other\n", stderr: exists, exit: 22, wantExit: 22},
		{name: "a 409 with another body fails", stdout: "service is being deleted: cc-remote-capt\n", stderr: exists, exit: 22, wantExit: 22},
		{name: "another status saying the service already exists fails", stdout: "service already exists: cc-remote-capt\n", stderr: "curl: (22) The requested URL returned error: 500\n", exit: 22, wantExit: 22},
		{name: "a failure before any answer fails", stderr: "error: service name required\n", exit: 1, wantExit: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			fake := filepath.Join(dir, "sprite-env")
			body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + Quote(dir) + "/calls\nprintf %s " + Quote(tt.stdout) + "\nprintf %s " + Quote(tt.stderr) + " >&2\nexit " + strconv.Itoa(tt.exit) + "\n"
			if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", Script(CreateServiceFunc(fake), "create_sprite_service cc-remote-capt --cmd capt", "echo created"))
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if got := cmd.ProcessState.ExitCode(); got != tt.wantExit {
				t.Fatalf("exit = %d (%v), want %d; stdout %q, stderr %q", got, err, tt.wantExit, stdout.String(), stderr.String())
			}
			wantStdout, wantStderr := tt.stdout+"created\n", tt.stderr
			switch {
			case tt.wantExit != 0:
				wantStdout = tt.stdout
			case tt.exit != 0:
				wantStdout, wantStderr = "created\n", ""
			}
			if stdout.String() != wantStdout || stderr.String() != wantStderr {
				t.Errorf("stdout %q, stderr %q; want %q, %q", stdout.String(), stderr.String(), wantStdout, wantStderr)
			}
			if calls, err := os.ReadFile(filepath.Join(dir, "calls")); err != nil || string(calls) != "services create cc-remote-capt --cmd capt\n" {
				t.Errorf("sprite-env saw %q (%v), want one create", calls, err)
			}
		})
	}
}
