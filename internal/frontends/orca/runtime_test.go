package orca_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

const pairingURL = "orca://pair?code=private-pairing-data"

func readyLine(advertised string) string {
	return `{"type":"orca_server_ready","schemaVersion":1,"runtimeId":"rt-1","endpoint":"ws://0.0.0.0:7001","boundEndpoint":"ws://0.0.0.0:7001","advertisedEndpoint":"` + advertised + `","pairing":{"available":true,"url":"` + pairingURL + `","endpoint":"` + advertised + `","deviceId":"d","scope":"runtime","qr":null}}`
}

func TestParseReady(t *testing.T) {
	ready, pairing, err := orca.ParseReady([]byte(readyLine("ws://127.0.0.1:7001")), 7001)
	if err != nil {
		t.Fatal(err)
	}
	want := orca.Ready{RuntimeID: "rt-1", BoundEndpoint: "ws://0.0.0.0:7001", AdvertisedEndpoint: "ws://127.0.0.1:7001"}
	if ready != want || pairing != pairingURL {
		t.Errorf("ParseReady = %+v, %q", ready, pairing)
	}
	tests := []struct {
		name, line, want string
	}{
		{"other event", strings.Replace(readyLine("ws://127.0.0.1:7001"), "orca_server_ready", "orca_server_stopped", 1), "orca_server_stopped"},
		{"no runtime id", strings.Replace(readyLine("ws://127.0.0.1:7001"), `"runtimeId":"rt-1"`, `"runtimeId":""`, 1), "runtimeId"},
		{"other port", readyLine("ws://127.0.0.1:7002"), "not the forwarded ws://127.0.0.1:7001"},
		{"other host", readyLine("ws://10.0.0.5:7001"), "not the forwarded"},
		{"no pairing", strings.Replace(readyLine("ws://127.0.0.1:7001"), `"available":true`, `"available":false`, 1), "offers no pairing"},
		{"not json", "Orca server ready", "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := orca.ParseReady([]byte(tt.line), 7001)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseReady error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "private-pairing-data") {
				t.Errorf("the error leaks the pairing data: %v", err)
			}
		})
	}
}

func TestRuntimeLauncher(t *testing.T) {
	runtime := orca.Runtime{
		Entry:   ".local/share/cc-remote/tools/orca-runtime-1.4.218/squashfs-root/AppRun",
		Display: []string{"xvfb-run", "--auto-servernum", "--server-args=-nolisten tcp"},
		Args:    []string{"--log-level", "info"},
		Port:    7001,
	}
	want := strings.Join([]string{
		"#!/bin/sh",
		"set -eu",
		"umask 077",
		`export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"`,
		`cd "$HOME/.cc-remote/orca"`,
		"exec 9> serve.lock",
		"flock -n 9 || exit 0",
		`exec xvfb-run --auto-servernum '--server-args=-nolisten tcp' "$HOME/".local/share/cc-remote/tools/orca-runtime-1.4.218/squashfs-root/AppRun serve --port 7001 --pairing-address 127.0.0.1 --user-data-dir "$HOME/.cc-remote/orca/user-data" --json --log-level info > serve.json 2>> serve.log`,
	}, "\n")
	if got := runtime.Launcher(); got != want {
		t.Errorf("Launcher =\n%s\nwant\n%s", got, want)
	}
}

func TestRuntimeEnsureScript(t *testing.T) {
	runtime := orca.Runtime{Entry: "tools/orca/squashfs-root/AppRun", Display: []string{"xvfb-run"}, Port: 7001}
	tests := []struct {
		name       string
		supervised bool
		want       []string
		absent     []string
	}{
		{"sprite-env", true, []string{
			`if ! /.sprite/bin/sprite-env services get cc-remote-orca > /dev/null 2>&1; then`,
			`/.sprite/bin/sprite-env services create cc-remote-orca --cmd "$HOME/.cc-remote/orca/serve.sh" --needs cc-remote-payload --duration 1ms --no-stream >&2`,
			`/.sprite/bin/sprite-env services create cc-remote-orca --cmd "$HOME/.cc-remote/orca/serve.sh" --duration 1ms --no-stream >&2`,
		}, []string{"nohup setsid"}},
		{"setsid", false, []string{
			`nohup setsid "$HOME/.cc-remote/orca/serve.sh" > /dev/null 2>&1 < /dev/null &`,
		}, []string{"sprite-env"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime.Supervised = tt.supervised
			script := runtime.EnsureScript()
			for _, line := range append(tt.want,
				`test -x "$HOME/"tools/orca/squashfs-root/AppRun || {`,
				`grep '"type":"orca_server_ready"' "$HOME/.cc-remote/orca/serve.json" | tail -n 1`,
			) {
				if !strings.Contains(script, line) {
					t.Errorf("script lacks %q:\n%s", line, script)
				}
			}
			for _, word := range tt.absent {
				if strings.Contains(script, word) {
					t.Errorf("script has %q:\n%s", word, script)
				}
			}
			if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
				t.Errorf("sh -n: %v: %s", err, out)
			}
		})
	}
}

func TestAgentArgv(t *testing.T) {
	tests := []struct {
		name  string
		agent orca.Agent
		want  []string
	}{
		{"claude with no MCP servers", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions",
			"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "claude-opus-5-5", "--effort", "xhigh",
		}},
		{"claude with an MCP allowlist", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "high", MCP: []string{"/srv/a.json", "/srv/b.json"}}, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions",
			"--strict-mcp-config", "--mcp-config", "/srv/a.json", "/srv/b.json",
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "m", "--effort", "high",
		}},
		{"codex", orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6-astra", Effort: "xhigh"}, []string{
			"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6-astra",
			"-c", `model_reasoning_effort="xhigh"`,
			"-c", `model_provider="cc_remote_openai"`,
			"-c", `model_providers.cc_remote_openai={name="OpenAI remote worker",base_url="https://api.openai.com/v1",env_key="OPENAI_API_KEY",requires_openai_auth=false,wire_api="responses"}`,
			"-c", `cli_auth_credentials_store="ephemeral"`,
			"-c", "mcp_servers={}",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.agent.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := tt.agent.Argv(); !slices.Equal(got, tt.want) {
				t.Errorf("Argv =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestAgentValidate(t *testing.T) {
	tests := []struct {
		name  string
		agent orca.Agent
		want  string
	}{
		{"unknown agent", orca.Agent{Kind: "gemini", Model: "m", Effort: "e"}, "agent \"gemini\""},
		{"no model", orca.Agent{Kind: orca.AgentClaude, Effort: "high"}, "model"},
		{"no effort", orca.Agent{Kind: orca.AgentCodex, Model: "m"}, "effort"},
		{"quoted effort", orca.Agent{Kind: orca.AgentCodex, Model: "m", Effort: `x"`}, "effort"},
		{"two codex MCP tables", orca.Agent{Kind: orca.AgentCodex, Model: "m", Effort: "e", MCP: []string{"{}", "{}"}}, "one --mcp-config"},
		{"MCP flag", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e", MCP: []string{"--debug"}}, "--mcp-config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.agent.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAgentCommandReadsTheKeyOnceAndExecsTheWorker(t *testing.T) {
	tests := []struct {
		agent    orca.Agent
		variable string
		cleared  string
	}{
		{orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e"}, "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		{orca.Agent{Kind: orca.AgentCodex, Model: "m", Effort: "e"}, "OPENAI_API_KEY", "CODEX_API_KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.agent.Kind, func(t *testing.T) {
			bin, dir := t.TempDir(), filepath.Join(t.TempDir(), "key.abc")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(dir, "key"), 0o600); err != nil {
				t.Fatal(err)
			}
			worker := "#!/bin/sh\nprintf '%s|%s|%s\\n' \"$" + tt.variable + "\" \"${" + tt.cleared + "-unset}\" \"$1\"\n"
			if err := os.WriteFile(filepath.Join(bin, tt.agent.Kind), []byte(worker), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", tt.agent.Command(dir))
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), tt.cleared+"=oauth-token", tt.variable+"=stale")
			go func() {
				fifo, err := os.OpenFile(filepath.Join(dir, "key"), os.O_WRONLY, 0)
				if err != nil {
					return
				}
				_, _ = fifo.WriteString("sk-test-key\n")
				_ = fifo.Close()
			}()
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			want := "sk-test-key|unset|" + tt.agent.Argv()[1] + "\n"
			if string(out) != want {
				t.Errorf("worker saw %q, want %q", out, want)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the key directory survived: %v", err)
			}
		})
	}
}

func TestParseKeyDir(t *testing.T) {
	dir, err := orca.ParseKeyDir([]byte("/home/agent/.cc-remote/orca/key.Ab12Cd34\n"))
	if err != nil || dir != "/home/agent/.cc-remote/orca/key.Ab12Cd34" {
		t.Errorf("ParseKeyDir = %q, %v", dir, err)
	}
	for _, out := range []string{"", "/tmp/key.Ab12", "/home/agent/.cc-remote/orca/key.Ab12; rm -rf /", "relative/.cc-remote/orca/key.Ab"} {
		if _, err := orca.ParseKeyDir([]byte(out)); err == nil {
			t.Errorf("ParseKeyDir accepted %q", out)
		}
	}
	if got := orca.KeyWriteScript(dir); got != "exec cat > /home/agent/.cc-remote/orca/key.Ab12Cd34/key" {
		t.Errorf("KeyWriteScript = %q", got)
	}
}
