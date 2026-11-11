package orca_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func TestAgentCommandPointsCcxAtTheGitHubAppTokenItWroteToMemory(t *testing.T) {
	const file = "/dev/shm/cc-remote/github-app.json"
	if _, err := os.Stat(file); err == nil {
		t.Skip("a worker on this machine reads " + file)
	}
	t.Cleanup(func() { _ = os.Remove(file) })
	worker := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"${CCX_GITHUB_APP_TOKEN_FILE-unset}\" \"$(cat \"$CCX_GITHUB_APP_TOKEN_FILE\" 2>/dev/null)\" \"$(stat -c %a \"$CCX_GITHUB_APP_TOKEN_FILE\" 2>/dev/null)\" \"${app-unset}\"\n"
	tests := []struct {
		name, kind, payload, want string
	}{
		{"claude with a pushed token", orca.AgentClaude, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n\n\n\n\n{\"token\":\"ghs-sentinel\"}\n", file + `|{"token":"ghs-sentinel"}|600|unset`},
		{"codex without one", orca.AgentCodex, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n", "unset|||unset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := orca.Agent{Kind: tt.kind, Model: "m", Effort: "e"}
			bin, dir := t.TempDir(), filepath.Join(t.TempDir(), "key.abc")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(dir, "key"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, tt.kind), []byte(worker), 0o700); err != nil {
				t.Fatal(err)
			}
			command := agent.Command(dir, orca.ShellPolicy{})
			if strings.Contains(command, "ghs-sentinel") {
				t.Errorf("the command text carries the token: %s", command)
			}
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(withoutCredentials(), "PATH="+bin+":"+os.Getenv("PATH"), "CCX_GITHUB_APP_TOKEN_FILE=stale", "app=stale")
			go func() {
				fifo, err := os.OpenFile(filepath.Join(dir, "key"), os.O_WRONLY, 0)
				if err != nil {
					return
				}
				_, _ = fifo.WriteString(tt.payload)
				_ = fifo.Close()
			}()
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(out)); got != tt.want {
				t.Errorf("worker saw %q, want %q", got, tt.want)
			}
		})
	}
}
