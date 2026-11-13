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

func TestAgentCommandPointsCcxAtTheGitHubAppTokenFileKeepWrites(t *testing.T) {
	const file = "/dev/shm/cc-remote/github-app.json"
	if _, err := os.Stat(file); err == nil {
		t.Skip("a worker on this machine reads " + file)
	}
	t.Cleanup(func() { _ = os.Remove(file) })
	worker := "#!/bin/sh\nprintf '%s|%s|%s\\n' \"${CCX_GITHUB_APP_TOKEN_FILE-unset}\" \"$(cat \"$CCX_GITHUB_APP_TOKEN_FILE\" 2>/dev/null)\" \"$(stat -c %a \"$CCX_GITHUB_APP_TOKEN_FILE\" 2>/dev/null)\"\n"
	tests := []struct {
		name, kind, written, want string
	}{
		{"claude before keep writes a token", orca.AgentClaude, "", file + "||"},
		{"codex once keep wrote one", orca.AgentCodex, `{"token":"ghs-sentinel"}`, file + `|{"token":"ghs-sentinel"}|600`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Remove(file)
			if tt.written != "" {
				keep := exec.Command("sh", "-c", orca.GitHubAppScript)
				keep.Stdin = strings.NewReader(tt.written)
				if out, err := keep.CombinedOutput(); err != nil {
					t.Fatalf("keep's write: %v: %s", err, out)
				}
			}
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
			cmd := exec.Command("sh", "-c", agent.Command(dir, orca.ShellPolicy{}))
			cmd.Env = append(withoutCredentials(), "PATH="+bin+":"+os.Getenv("PATH"), "CCX_GITHUB_APP_TOKEN_FILE=stale")
			go func() {
				fifo, err := os.OpenFile(filepath.Join(dir, "key"), os.O_WRONLY, 0)
				if err != nil {
					return
				}
				_, _ = fifo.WriteString("sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n")
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

func TestAgentCommandPointsAWSAtTheSessionItWroteToMemory(t *testing.T) {
	const file, config = "/dev/shm/cc-remote/aws.json", "/dev/shm/cc-remote/aws-config"
	if _, err := os.Stat(file); err == nil {
		t.Skip("a worker on this machine reads " + file)
	}
	t.Cleanup(func() {
		_ = os.Remove(file)
		_ = os.Remove(config)
	})
	worker := `#!/bin/sh
process=$(sed -n 's/^credential_process = //p' "$AWS_CONFIG_FILE" 2>/dev/null)
printf '%s|%s|%s|%s\n' "${AWS_CONFIG_FILE-unset}" "$(sh -c "$process" 2>/dev/null)" "$(stat -c %a ` + file + ` 2>/dev/null)" "${aws-unset}"
`
	session := `{"Version":1,"AccessKeyId":"ASIA-sentinel","SecretAccessKey":"secret","SessionToken":"token","Expiration":"2026-11-12T17:07:15Z"}`
	tests := []struct {
		name, kind, payload, want string
	}{
		{"codex without a session", orca.AgentCodex, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n", "unset|||unset"},
		{"claude with a session", orca.AgentClaude, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n\n\n\n\n" + session + "\n", config + "|" + session + "|600|unset"},
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
			if strings.Contains(command, "ASIA-sentinel") {
				t.Errorf("the command text carries the session: %s", command)
			}
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(withoutCredentials(), "PATH="+bin+":"+os.Getenv("PATH"), "AWS_CONFIG_FILE=stale", "aws=stale")
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
