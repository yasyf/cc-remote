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
	ready, pairing, err := orca.ParseReady([]byte(readyLine("ws://127.0.0.1:7001")), 7001, 7001)
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
			_, _, err := orca.ParseReady([]byte(tt.line), 7001, 7001)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseReady error = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "private-pairing-data") {
				t.Errorf("the error leaks the pairing data: %v", err)
			}
		})
	}
}

func TestParseReadyChecksTheListenerAndTheAdvertisedPortApart(t *testing.T) {
	line := strings.Replace(readyLine("ws://127.0.0.1:7001"), `"boundEndpoint":"ws://0.0.0.0:7001"`, `"boundEndpoint":"ws://0.0.0.0:18766"`, 1)
	ready, pairing, err := orca.ParseReady([]byte(line), 18766, 7001)
	if err != nil || ready.BoundEndpoint != "ws://0.0.0.0:18766" || ready.AdvertisedEndpoint != "ws://127.0.0.1:7001" || pairing != pairingURL {
		t.Errorf("ParseReady = %+v, %q, %v", ready, pairing, err)
	}
	if _, _, err := orca.ParseReady([]byte(line), 7001, 7001); err == nil || err.Error() != `the Orca runtime listens at "ws://0.0.0.0:18766", not on port 7001` {
		t.Errorf("ParseReady of a runtime on another port = %v", err)
	}
	if _, _, err := orca.ParseReady([]byte(line), 18766, 18766); err == nil || !strings.Contains(err.Error(), "not the forwarded ws://127.0.0.1:18766") {
		t.Errorf("ParseReady advertising another loopback port = %v", err)
	}
}

func TestProbeScriptReadsTheRunningRuntimeWithoutStartingOne(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "-c", orca.ProbeScript).CombinedOutput(); err != nil {
		t.Fatalf("probe script does not parse: %v: %s", err, out)
	}
	for _, launch := range []string{"serve.sh", "setsid", "services create", "nohup", " serve "} {
		if strings.Contains(orca.ProbeScript, launch) {
			t.Errorf("the probe script carries %q, which could start a runtime", launch)
		}
	}
	for _, read := range []string{"flock -n 9", "exit 3", `grep '"type":"orca_server_ready"' serve.json | tail -n 1`} {
		if !strings.Contains(orca.ProbeScript, read) {
			t.Errorf("the probe script lacks %q", read)
		}
	}
}

func TestFirstUseCheckRefusesAnyEarlierRuntimeState(t *testing.T) {
	tests := []struct {
		name   string
		before func(t *testing.T, home string)
		unused bool
	}{
		{"a fresh home", func(*testing.T, string) {}, true},
		{"other cc-remote state", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, ".cc-remote"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".cc-remote", "tailscaled.state"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"an empty runtime directory", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, ".cc-remote", "orca"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a launcher", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, ".cc-remote", "orca"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".cc-remote", "orca", "serve.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"an earlier brief", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, ".cc-remote", "orca", "tasks", "task-a"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a dangling link", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, ".cc-remote"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(home, "gone"), filepath.Join(home, ".cc-remote", "orca")); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			tt.before(t, home)
			check := exec.Command("sh", "-c", "set -eu\n"+orca.FirstUseCheck)
			check.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
			if err := check.Run(); (err == nil) != tt.unused {
				t.Errorf("first-use check = %v, want unused %t", err, tt.unused)
			}
			if _, err := os.Lstat(filepath.Join(home, ".cc-remote", "orca")); tt.unused && !os.IsNotExist(err) {
				t.Errorf("the check created runtime state: %v", err)
			}
		})
	}
	for _, write := range []string{"mkdir", ">", "rm ", "mv ", "chmod", "serve"} {
		if strings.Contains(orca.FirstUseCheck, write) {
			t.Errorf("the first-use check carries %q, which could change runtime state", write)
		}
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
	keyed := `shell_environment_policy.filters={ANTHROPIC_API_KEY="exclude",CLAUDE_CODE_OAUTH_TOKEN="exclude",ANTHROPIC_AUTH_TOKEN="exclude",OPENAI_API_KEY="exclude",CODEX_API_KEY="exclude"}`
	legacy := orca.ShellPolicy{Legacy: true, Exclude: []string{"AWS_*", "GITHUB_TOKEN"}}
	tests := []struct {
		name   string
		agent  orca.Agent
		policy orca.ShellPolicy
		want   []string
	}{
		{"claude with no MCP servers", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}, orca.ShellPolicy{}, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions",
			"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "claude-opus-5-5", "--effort", "xhigh",
		}},
		{"claude with an MCP allowlist", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "high", MCP: []string{"/srv/a.json", "/srv/b.json"}}, orca.ShellPolicy{}, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions",
			"--strict-mcp-config", "--mcp-config", "/srv/a.json", "/srv/b.json",
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "m", "--effort", "high",
		}},
		{"claude ignores a codex shell policy", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}, legacy, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions",
			"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "claude-opus-5-5", "--effort", "xhigh",
		}},
		{"codex", orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6-astra", Effort: "xhigh"}, orca.ShellPolicy{}, []string{
			"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6-astra",
			"-c", `model_reasoning_effort="xhigh"`,
			"-c", `model_provider="cc_remote_openai"`,
			"-c", `model_providers.cc_remote_openai={name="OpenAI remote worker",base_url="https://api.openai.com/v1",env_key="OPENAI_API_KEY",requires_openai_auth=false,wire_api="responses"}`,
			"-c", `cli_auth_credentials_store="ephemeral"`,
			"-c", keyed,
			"-c", "mcp_servers={}",
		}},
		{"codex with an explicit service tier", orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6.1-sol", Effort: "xhigh", Tier: "fast", MCP: []string{`{docs={command="docs-mcp"}}`}}, orca.ShellPolicy{}, []string{
			"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6.1-sol",
			"-c", `model_reasoning_effort="xhigh"`,
			"-c", `model_provider="cc_remote_openai"`,
			"-c", `model_providers.cc_remote_openai={name="OpenAI remote worker",base_url="https://api.openai.com/v1",env_key="OPENAI_API_KEY",requires_openai_auth=false,wire_api="responses"}`,
			"-c", `cli_auth_credentials_store="ephemeral"`,
			"-c", keyed,
			"-c", `mcp_servers={docs={command="docs-mcp"}}`,
			"-c", `service_tier="fast"`,
		}},
		{"codex over a legacy exclusion array", orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6.1-sol", Effort: "xhigh"}, legacy, []string{
			"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6.1-sol",
			"-c", `model_reasoning_effort="xhigh"`,
			"-c", `model_provider="cc_remote_openai"`,
			"-c", `model_providers.cc_remote_openai={name="OpenAI remote worker",base_url="https://api.openai.com/v1",env_key="OPENAI_API_KEY",requires_openai_auth=false,wire_api="responses"}`,
			"-c", `cli_auth_credentials_store="ephemeral"`,
			"-c", `shell_environment_policy.exclude=["AWS_*","GITHUB_TOKEN","ANTHROPIC_API_KEY","CLAUDE_CODE_OAUTH_TOKEN","ANTHROPIC_AUTH_TOKEN","OPENAI_API_KEY","CODEX_API_KEY"]`,
			"-c", "mcp_servers={}",
		}},
		{"codex over a legacy include list alone", orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6.1-sol", Effort: "xhigh"}, orca.ShellPolicy{Legacy: true}, []string{
			"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6.1-sol",
			"-c", `model_reasoning_effort="xhigh"`,
			"-c", `model_provider="cc_remote_openai"`,
			"-c", `model_providers.cc_remote_openai={name="OpenAI remote worker",base_url="https://api.openai.com/v1",env_key="OPENAI_API_KEY",requires_openai_auth=false,wire_api="responses"}`,
			"-c", `cli_auth_credentials_store="ephemeral"`,
			"-c", `shell_environment_policy.exclude=["ANTHROPIC_API_KEY","CLAUDE_CODE_OAUTH_TOKEN","ANTHROPIC_AUTH_TOKEN","OPENAI_API_KEY","CODEX_API_KEY"]`,
			"-c", "mcp_servers={}",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.agent.Validate(); err != nil {
				t.Fatal(err)
			}
			if got := tt.agent.Argv(tt.policy); !slices.Equal(got, tt.want) {
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
		{"fable alias", orca.Agent{Kind: orca.AgentClaude, Model: "fable", Effort: "high"}, "Fable requires an explicit local choice"},
		{"fable family id", orca.Agent{Kind: orca.AgentClaude, Model: "claude-fable-5-1", Effort: "high"}, "Fable requires an explicit local choice"},
		{"capitalized fable", orca.Agent{Kind: orca.AgentClaude, Model: "Fable-5", Effort: "xhigh"}, "Fable requires an explicit local choice"},
		{"fable on codex", orca.Agent{Kind: orca.AgentCodex, Model: "fable5", Effort: "xhigh"}, "Fable requires an explicit local choice"},
		{"claude service tier", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh", Tier: "fast"}, "codex setting"},
		{"quoted service tier", orca.Agent{Kind: orca.AgentCodex, Model: "m", Effort: "e", Tier: `fast"`}, "--service-tier"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.agent.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAgentInlineMCP(t *testing.T) {
	tests := []struct {
		name  string
		agent orca.Agent
		want  string
	}{
		{"no servers", orca.Agent{Kind: orca.AgentClaude}, ""},
		{"claude JSON", orca.Agent{Kind: orca.AgentClaude, MCP: []string{`{"mcpServers":{"docs":{"command":"docs-mcp"}}}`, `{"mcpServers":{}}`}}, ""},
		{"codex table", orca.Agent{Kind: orca.AgentCodex, MCP: []string{`{docs={command="docs-mcp"}}`}}, ""},
		{"claude file", orca.Agent{Kind: orca.AgentClaude, MCP: []string{`{"mcpServers":{}}`, "/Users/me/.mcp.json"}}, "--mcp-config 2 is not an inline JSON object"},
		{"claude braces that are not JSON", orca.Agent{Kind: orca.AgentClaude, MCP: []string{"{docs}"}}, "--mcp-config 1 is not an inline JSON object"},
		{"codex file", orca.Agent{Kind: orca.AgentCodex, MCP: []string{"servers.toml"}}, "--mcp-config 1 is not an inline TOML table"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.agent.InlineMCP()
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("InlineMCP = %v, want %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "/Users/me") {
				t.Errorf("the error repeats the config: %v", err)
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
			worker := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$" + tt.variable + "\" \"${" + tt.cleared + "-unset}\" \"$CAPT_HOOK_ACTOR_JUDGE\" \"$1\"\n"
			if err := os.WriteFile(filepath.Join(bin, tt.agent.Kind), []byte(worker), 0o700); err != nil {
				t.Fatal(err)
			}
			command := tt.agent.Command(dir, orca.ShellPolicy{})
			if !strings.HasPrefix(command, "exec sh -c ") {
				t.Fatalf("worker command relies on the terminal's configured shell: %s", command)
			}
			cmd := exec.Command("sh", "-c", command)
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
			want := "sk-test-key|unset|" + tt.agent.Kind + "|" + tt.agent.Argv(orca.ShellPolicy{})[1] + "\n"
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

func TestAgentCommandHandsEachActorOnlyItsDeliveredJudgeKey(t *testing.T) {
	tests := []struct {
		name, kind, payload, want string
	}{
		{"claude with an OpenAI judge key", orca.AgentClaude, "sk-test-key\nsk-judge-key\n", "sk-test-key|unset|unset|sk-judge-key|unset"},
		{"claude without one", orca.AgentClaude, "sk-test-key\n", "sk-test-key|unset|unset|unset|unset"},
		{"codex with an Anthropic judge key", orca.AgentCodex, "sk-test-key\nsk-judge-key\n", "sk-judge-key|unset|unset|sk-test-key|unset"},
		{"codex without one", orca.AgentCodex, "sk-test-key\n", "unset|unset|unset|sk-test-key|unset"},
	}
	names := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY"}
	report, stale := make([]string, 0, len(names)), make([]string, 0, len(names))
	for _, name := range names {
		report = append(report, `"${`+name+`-unset}"`)
		stale = append(stale, name+"=stale")
	}
	worker := "#!/bin/sh\nprintf '" + strings.Repeat("%s|", len(names)-1) + "%s\\n' " + strings.Join(report, " ") + "\n"
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
			cmd := exec.Command("sh", "-c", agent.Command(dir, orca.ShellPolicy{}))
			cmd.Env = append(append(withoutCredentials(), stale...), "PATH="+bin+":"+os.Getenv("PATH"))
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
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("the key directory survived: %v", err)
			}
		})
	}
}

func TestCodexCommandKeepsTheActorKeysBesideItsToolPolicy(t *testing.T) {
	agent := orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6.1-sol", Effort: "xhigh", Tier: "fast"}
	worker := "#!/bin/sh\nprintf '%s|%s|%s\\n' \"$OPENAI_API_KEY\" \"$ANTHROPIC_API_KEY\" \"$CAPT_HOOK_ACTOR_JUDGE\"\nprintf '%s\\n' \"$@\"\n"
	tests := []struct {
		name   string
		policy orca.ShellPolicy
	}{
		{"keyed", orca.ShellPolicy{}},
		{"legacy", orca.ShellPolicy{Legacy: true, Exclude: []string{"AWS_*"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, dir := t.TempDir(), filepath.Join(t.TempDir(), "key.abc")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(dir, "key"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(worker), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", agent.Command(dir, tt.policy))
			cmd.Env = append(withoutCredentials(), "PATH="+bin+":"+os.Getenv("PATH"))
			go func() {
				fifo, err := os.OpenFile(filepath.Join(dir, "key"), os.O_WRONLY, 0)
				if err != nil {
					return
				}
				_, _ = fifo.WriteString("sk-test-key\nsk-judge-key\n")
				_ = fifo.Close()
			}()
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			argv := agent.Argv(tt.policy)
			if want := "sk-test-key|sk-judge-key|codex\n" + strings.Join(argv[1:], "\n") + "\n"; string(out) != want {
				t.Errorf("worker saw\n%s\nwant\n%s", out, want)
			}
			if !slices.Equal(argv[:5], []string{"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6.1-sol"}) || !slices.Contains(argv, `model_reasoning_effort="xhigh"`) || !slices.Contains(argv, `service_tier="fast"`) {
				t.Errorf("the tool policy displaced the requested model, effort, tier, or bypass: %q", argv)
			}
		})
	}
}

func withoutCredentials() []string {
	return slices.DeleteFunc(os.Environ(), func(pair string) bool {
		name, _, _ := strings.Cut(pair, "=")
		return slices.Contains(orca.CredentialEnv(), name)
	})
}
