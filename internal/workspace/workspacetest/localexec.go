package workspacetest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yasyf/cc-remote/internal/providers"
)

type LocalExec struct {
	Root string
	Path string

	mu      sync.Mutex
	scripts map[string][]string
	stdins  map[string][]string
}

func NewLocalExec(t *testing.T) *LocalExec {
	t.Helper()
	return &LocalExec{Root: t.TempDir(), Path: os.Getenv("PATH"), scripts: map[string][]string{}, stdins: map[string][]string{}}
}

func (l *LocalExec) Home(id string) string {
	return filepath.Join(l.Root, "homes", id)
}

func (l *LocalExec) Handle(id string, cmd []string, stdin []byte) providers.Result {
	home := l.Home(id)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return providers.Result{Stderr: []byte(err.Error()), ExitCode: 1}
	}
	l.mu.Lock()
	l.scripts[id] = append(l.scripts[id], strings.Join(cmd, " "))
	l.stdins[id] = append(l.stdins[id], string(stdin))
	l.mu.Unlock()
	if cmd[0] == "sudo" {
		return providers.Result{}
	}
	run := exec.CommandContext(context.Background(), cmd[0], cmd[1:]...)
	run.Env = []string{"HOME=" + home, "PATH=" + l.Path, "TMPDIR=" + l.Root, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1"}
	run.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	run.Stdout, run.Stderr = &stdout, &stderr
	err := run.Run()
	result := providers.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		result.ExitCode = exit.ExitCode()
	case err != nil:
		result.Stderr = append(result.Stderr, err.Error()...)
		result.ExitCode = 1
	}
	return result
}

func (l *LocalExec) Scripts(id string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.scripts[id]...)
}

func (l *LocalExec) Stdins(id string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.stdins[id]...)
}
