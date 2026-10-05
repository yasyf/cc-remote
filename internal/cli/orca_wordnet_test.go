package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	captainSHA       = "008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3"
	captainInventory = "version: 1\nsystem:\n  - { name: uv, version: 0.12.21, url: https://example.com/uv, sha256: " + captainSHA + ", format: binary }\n" +
		"configure:\n  env: [WEB_PORT]\ncaptainHook: { version: 12.88.9, url: https://example.com/captain-hook.tar.gz, sha256: " + captainSHA + " }\n"
	wordnetRan = "other: wordnet"
	wordnetOp  = `"$HOME/.daemonkit/tools/capt-hook/"12.88.9`
)

type wordnetGuest struct {
	mu      sync.Mutex
	exit    int
	scripts []string
}

func (g *wordnetGuest) prepare(keys, script string) providers.Result {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scripts = append(g.scripts, script)
	for path, entry := range map[string]string{keys: "wordnet\n", os.Getenv("SSH_LOG"): "wordnet\x00"} {
		log, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = log.WriteString(entry)
			err = errors.Join(err, log.Close())
		}
		if err != nil {
			return providers.Result{Stderr: []byte(err.Error()), ExitCode: 1}
		}
	}
	if g.exit != 0 {
		return providers.Result{Stderr: []byte("cc-remote: synthetic guest WordNet refusal"), ExitCode: g.exit}
	}
	return providers.Result{}
}

func (g *wordnetGuest) fail(code int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.exit = code
}

func (g *wordnetGuest) ran(t *testing.T, w *firstWorker, want int) {
	t.Helper()
	g.mu.Lock()
	scripts := slices.Clone(g.scripts)
	g.mu.Unlock()
	if len(scripts) != want {
		t.Errorf("the guest received %d WordNet preparations, want %d", len(scripts), want)
	}
	for _, script := range scripts {
		if !strings.HasPrefix(script, "set -eu\nunset ") || !strings.Contains(script, `hook=$(readlink -e "$tool/bin/hook")`) || !strings.Contains(script, "model_cache.ensure_wn_lexicon()") {
			t.Errorf("the guest received %q, not the controller's WordNet script", script)
		}
	}
	for _, script := range w.local.Scripts("task-a") {
		if strings.Contains(script, wordnetOp) {
			t.Errorf("the WordNet script ran on the test host: %q", script)
		}
	}
}

func newWordnetWorker(t *testing.T, created bool) (*firstWorker, *wordnetGuest) {
	t.Helper()
	guest := &wordnetGuest{}
	w := newFirstWorkerWith(t, nil, created, captainInventory, func(w *firstWorker) {
		handle := w.provider.Handle
		w.provider.Handle = func(id string, cmd []string, stdin []byte) providers.Result {
			switch line := strings.Join(cmd, " "); {
			case strings.Contains(line, "plugins.sh publish ") || strings.Contains(line, "plugins.sh ready "):
				return providers.Result{}
			case strings.Contains(line, wordnetOp):
				return guest.prepare(w.keys, cmd[len(cmd)-1])
			}
			return handle(id, cmd, stdin)
		}
		version := filepath.Join(w.local.Home("task-a"), ".local", "share", "captain-hook", "host", "version.json")
		if err := os.MkdirAll(filepath.Dir(version), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(version, []byte(`{"schema":1,"build":"12.88.9"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	return w, guest
}

func keyLog(t *testing.T, w *firstWorker) string {
	t.Helper()
	keys, err := os.ReadFile(w.keys)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(keys)
}

func refusingOrca(calls *int) taskOrcaRunner {
	return func(context.Context, ...string) ([]byte, error) {
		*calls++
		return nil, errors.New("no Orca CLI call is expected")
	}
}

func TestEveryFirstWorkerPreparesWordnetBeforeItsKeyIsDelivered(t *testing.T) {
	for _, tt := range []struct {
		name     string
		existing bool
		keys     string
		prepared int
	}{
		{"created", false, "read\nwordnet\n", 1},
		{"existing", true, "wordnet\nwordnet\nread\n", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, guest := newWordnetWorker(t, tt.existing)
			native, commands := w.native()
			var out bytes.Buffer
			if err := w.prepare(t.Context(), w.session, native, &out, "task-a", tt.existing); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(native.calls, commands) {
				t.Errorf("orca calls =\n%q\nwant\n%q", native.calls, commands)
			}
			if got := keyLog(t, w); got != tt.keys {
				t.Errorf("WordNet runs and key reads = %q, want %q", got, tt.keys)
			}
			calls := sshCalls(t)
			last, dir := -1, slices.Index(calls, "key-dir")
			for i, call := range calls {
				if call == wordnetRan {
					last = i
				}
			}
			if last < 0 || dir < 0 || last > dir || slices.Index(calls, "key-write") < dir {
				t.Errorf("ssh calls = %q, want the first worker's WordNet before its key directory and write", calls)
			}
			guest.ran(t, w, tt.prepared)
			w.free("task-a")
		})
	}
}

func TestAFailedWordnetDeliversNoKeyToAFirstWorker(t *testing.T) {
	for _, tt := range []struct {
		name     string
		existing bool
		keys     string
		prepared int
	}{
		{"created", false, "read\nwordnet\n", 1},
		{"existing", true, "wordnet\nwordnet\n", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w, guest := newWordnetWorker(t, tt.existing)
			guest.fail(3)
			calls := 0
			var out bytes.Buffer
			err := w.prepare(t.Context(), w.session, refusingOrca(&calls), &out, "task-a", tt.existing)
			if err == nil || !strings.Contains(err.Error(), "prepare WordNet: sh on task-a exited 3") {
				t.Fatalf("prepare = %v, want the WordNet exit", err)
			}
			if calls != 0 || out.Len() != 0 {
				t.Errorf("a failed WordNet made %d Orca CLI calls and printed %q", calls, out.String())
			}
			if got := keyLog(t, w); got != tt.keys {
				t.Errorf("WordNet runs and key reads = %q, want %q", got, tt.keys)
			}
			if ssh := sshCalls(t); slices.Contains(ssh, "key-dir") || slices.Contains(ssh, "key-write") {
				t.Errorf("ssh calls = %q, want no key directory or write", ssh)
			}
			guest.ran(t, w, tt.prepared)
			w.free("task-a")
		})
	}
}
