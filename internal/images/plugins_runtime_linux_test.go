package images

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPluginsOverlapCodexRuntimeWithClaudeReconciliation(t *testing.T) {
	for _, tt := range []struct {
		name           string
		runtimeFailure bool
		claudeFailure  bool
		wantExit       int
	}{
		{name: "success"},
		{name: "runtime digest failure", runtimeFailure: true, wantExit: 1},
		{name: "Claude failure", claudeFailure: true, wantExit: 37},
	} {
		t.Run(tt.name, func(t *testing.T) {
			archive, sum := runtimeTestArchive(t)
			arrivals := make(chan string, 4)
			runtimeStarted := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unlatch := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrivals <- r.URL.Path
				switch r.URL.Path {
				case "/runtime":
					close(runtimeStarted)
					<-release
					if _, err := w.Write(archive); err != nil {
						t.Errorf("write runtime: %v", err)
					}
				case "/claude":
					select {
					case <-runtimeStarted:
					case <-release:
					}
				}
			}))
			defer server.Close()
			defer unlatch()
			if tt.runtimeFailure {
				sum = strings.Repeat("0", 64)
			}
			hostArchive, hostSum := writeArchive(t, "capt-hookd", `#!/bin/sh
set -eu
test "$1" = package-install
test -f "$HOME/.cache/codex-runtimes/codex-primary-runtime/.cc-remote-digest"
touch "$HOME/captain-installed"
mkdir -p "$HOME/.local/share/captain-hook/host"
printf '{"build":"1.0.0"}\n' > "$HOME/.local/share/captain-hook/host/version.json"
`)
			toolArchive, toolSum := writeArchive(t, "tool", "#!/bin/sh\n")
			market := toolsRef
			market.Private = true
			inventory := Inventory{
				Version:      SchemaVersion,
				Tools:        []Artifact{{Name: "tool", Version: "1.0", URL: "file://" + toolArchive, SHA256: toolSum, Format: TarGz, Bins: map[string]string{"tool": "tool"}}},
				Claude:       Claude{Marketplaces: []Marketplace{market}, Plugins: []Plugin{{ID: "hook@tools-market", Version: "1.0.0"}}},
				CodexRuntime: &CodexRuntime{Version: "1.0.0", URL: server.URL + "/runtime", SHA256: sum, Plugins: []string{"documents"}},
				CaptainHook:  &CaptainHook{Version: "1.0.0", URL: "file://" + hostArchive, SHA256: hostSum},
				Prepare:      []string{`test -f "$HOME/captain-installed"`, `IFS= read -r remaining`, `test "$remaining" = retained-input`, `touch "$HOME/prepared"`},
			}
			h := newPluginsHost(t, inventory, marketplaceCatalog("0.7.17"), fakeState{}, nil)
			writePluginTestFile(t, filepath.Join(h.fakes, "claude-inner"), []byte(fakeClaude), 0o700)
			failure := "False"
			if tt.claudeFailure {
				failure = "True"
			}
			wrapper := fmt.Sprintf(`#!/usr/bin/env python3
import os,sys,urllib.request
from pathlib import Path
gate=Path(os.environ["HOME"])/"claude-gate"
if not gate.exists():
 gate.touch()
 urllib.request.urlopen(%q+"/claude").read()
 if %s:
  Path(os.environ["HOME"]).joinpath("claude-failed").touch()
  sys.exit(37)
os.execv(%q,[%q,*sys.argv[1:]])
`, server.URL, failure, filepath.Join(h.fakes, "claude-inner"), filepath.Join(h.fakes, "claude-inner"))
			writePluginTestFile(t, filepath.Join(h.fakes, "claude"), []byte(wrapper), 0o700)
			git := strings.Replace(fakeGit, "case \"$1\" in", `if [ "$1" = -C ] && [ "$3" = fetch ]; then
 test "$GITHUB_TOKEN" = synthetic-token || exit 94
fi
case "$1" in`, 1)
			writePluginTestFile(t, filepath.Join(h.fakes, "git"), []byte(git), 0o700)
			writePluginTestFile(t, filepath.Join(h.fakes, "codex"), []byte(`#!/bin/sh
set -eu
test -z "${GITHUB_TOKEN:-}${github_token:-}"
test -f "$HOME/.cache/codex-runtimes/codex-primary-runtime/.cc-remote-digest"
printf 'codex %s\n' "$*" >> "$FAKE_LOG"
case "$*" in
 "plugin list")
  if [ -f "$HOME/codex-installed" ]; then printf 'documents@openai-primary-runtime installed, enabled 1.0.0\n'; fi ;;
 "plugin add documents@openai-primary-runtime") touch "$HOME/codex-installed" ;;
 *) exit 95 ;;
esac
`), 0o700)
			curl, err := exec.LookPath("curl")
			if err != nil {
				t.Fatal(err)
			}
			curlWrapper := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
if [[ "$*" == *codex-runtime.tar.xz* ]]; then
 test -f "$HOME/.local/share/cc-remote/tools/tool-1.0/.cc-remote-digest"
 test -x "$HOME/.local/bin/tool"
 if IFS= read -r line; then exit 92; fi
 if env | grep -qE '^(github_token|GITHUB_TOKEN)='; then exit 93; fi
fi
exec %s "$@"
`, quote(curl))
			writePluginTestFile(t, filepath.Join(h.fakes, "curl"), []byte(curlWrapper), 0o700)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", filepath.Join(h.fakes, "plugins.sh"), "install", "stamp")
			cmd.Stdin = strings.NewReader("synthetic-token\nretained-input\n")
			cmd.Env = append(os.Environ(), "HOME="+h.home, "PATH="+h.fakes+":"+os.Getenv("PATH"), "FAKE_STATE="+filepath.Join(h.fakes, "state.json"), "FAKE_CATALOG="+filepath.Join(h.fakes, "catalog.json"), "FAKE_LOG="+filepath.Join(h.fakes, "calls.log"), "FAKE_SOURCES="+filepath.Join(h.fakes, "sources"), "REAL_GIT="+realGit)
			done := startArtifactScript(cmd)
			seen := map[string]bool{}
			for len(seen) < 2 {
				select {
				case path := <-arrivals:
					seen[path] = true
				case result := <-done:
					t.Fatalf("install exited before overlap: %v %s", result.err, result.out)
				case <-time.After(10 * time.Second):
					unlatch()
					finishArtifactScript(t, done, true)
					t.Fatal("runtime and Claude reconciliation did not overlap")
				}
			}
			if !seen["/runtime"] || !seen["/claude"] {
				t.Fatalf("unexpected arrivals: %v", seen)
			}
			select {
			case result := <-done:
				t.Fatalf("install did not drain runtime: %v %s", result.err, result.out)
			case <-time.After(100 * time.Millisecond):
			}
			for _, name := range []string{"captain-installed", "prepared", ".cc-remote/ready"} {
				if _, err := os.Stat(filepath.Join(h.home, name)); !os.IsNotExist(err) {
					t.Errorf("pending runtime reached %s: %v", name, err)
				}
			}
			unlatch()
			result := <-done
			if got := exitCode(result.err); got != tt.wantExit {
				t.Fatalf("install exit=%d, want %d: %s", got, tt.wantExit, result.out)
			}
			for _, name := range []string{"captain-installed", "prepared", ".cc-remote/ready"} {
				_, err := os.Stat(filepath.Join(h.home, name))
				if tt.wantExit != 0 && !os.IsNotExist(err) {
					t.Errorf("failed install reached %s: %v", name, err)
				}
				if tt.wantExit == 0 && err != nil {
					t.Errorf("successful install omitted %s: %v", name, err)
				}
			}
			if tt.wantExit == 0 && h.version("hook@tools-market") != "1.0.0" {
				t.Error("Claude plugin pin was not installed")
			}
		})
	}
}

func runtimeTestArchive(t *testing.T) ([]byte, string) {
	t.Helper()
	file, _ := writeArchive(t, "codex-primary-runtime/runtime.json", `{"bundleVersion":"1.0.0"}`)
	compressed, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cmd := exec.Command("xz", "-c")
	cmd.Stdin = reader
	archive, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	return archive, hex.EncodeToString(sum[:])
}
