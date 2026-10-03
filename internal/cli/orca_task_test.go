package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

func TestOrcaTunnelIsAPrivatePersistentForward(t *testing.T) {
	tunnel := orcaTunnel{Host: "task-a", Config: "/state/ssh/task-a.ssh", Control: "/state/orca/%C", Log: "/state/orca/task-a.forward.log"}
	want := []string{
		"-F", "/state/ssh/task-a.ssh", "-S", "/state/orca/%C", "-o", "BatchMode=yes",
		"-o", "ControlMaster=yes", "-o", "ControlPersist=yes", "-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-f", "-N", "-L", "127.0.0.1:7001:127.0.0.1:7001",
		"task-a",
	}
	if got := tunnel.openArgs(7001); !slices.Equal(got, want) {
		t.Errorf("openArgs =\n%q\nwant\n%q", got, want)
	}
	command := tunnel.command(orca.KeyWriteScript("/home/agent/.cc-remote/orca/key.Ab12"), nil)
	if command.Name != "ssh" || !slices.Equal(command.Args[len(command.Args)-4:], []string{"task-a", "sh", "-c", "'exec cat > /home/agent/.cc-remote/orca/key.Ab12/key'"}) {
		t.Errorf("command = %v", command)
	}
}

func TestCaptureKey(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	key, err := captureKey(t.Context(), []string{script("one", "printf 'sk-test-1\\n'")})
	if err != nil || string(key) != "sk-test-1" {
		t.Errorf("captureKey = %q, %v", key, err)
	}
	tests := []struct {
		name, body, want string
	}{
		{"failure", "echo sk-leaked; exit 3", "exit status 3"},
		{"two lines", "printf 'sk-a\\nsk-b\\n'", "exactly one non-empty line"},
		{"empty", "true", "exactly one non-empty line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := captureKey(t.Context(), []string{script(tt.name, tt.body), "--service", "cc-remote-test"})
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "--service cc-remote-test") {
				t.Fatalf("captureKey error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "sk-") {
				t.Errorf("the error leaks the key: %v", err)
			}
		})
	}
}

func TestOrcaTaskRoundTripsWithoutSecrets(t *testing.T) {
	dir := state.Dir(t.TempDir())
	if _, err := loadTask(dir, "task-a"); err == nil || !strings.Contains(err.Error(), "orca create") {
		t.Errorf("loadTask of nothing = %v", err)
	}
	driver := orcaDriver{state: dir}
	result := &workspace.Result{Name: "task-a", Provider: "sprites", Profile: "lean", Machine: "task-a", ProjectRoot: "/home/agent/app", SSH: workspace.SSH{Config: "/state/ssh/task-a.ssh"}}
	task, err := driver.task(result, orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Port == 0 || task.Service != orca.RuntimeService || task.Environment != "task-a" || task.Forward.Control != filepath.Join(string(dir), "orca", "%C") {
		t.Errorf("task = %+v", task)
	}
	loaded, err := loadTask(dir, "task-a")
	if err != nil || loaded.Port != task.Port || loaded.Forward != task.Forward || loaded.Agent.Model != "m" {
		t.Errorf("loadTask = %+v, %v", loaded, err)
	}
	info, err := os.Stat(dir.Orca("task-a"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("task file mode = %v, %v", info, err)
	}
}
