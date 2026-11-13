package orca_test

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	managed := strings.Replace(readyLine("ws://127.0.0.1:7001"), `"pairing":`, `"health":{"buildVersion":"0.1.0+15ec2904eb00","nodeVersion":"24.21.0"},"pairing":`, 1)
	if ready, _, err := orca.ParseReady([]byte(managed), 7001, 7001); err != nil || ready.ManagedServer != "0.1.0+15ec2904eb00" {
		t.Errorf("ParseReady of a managed server = %+v, %v", ready, err)
	}
	if on, off := orca.ServerOf("0.1.0+15ec2904eb00"), orca.ServerOf(ready.ManagedServer); on != orca.ServerManaged || off != orca.ServerElectron {
		t.Errorf("ServerOf = %q with a managed server's build and %q without one", on, off)
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

func TestProbeScriptWaitsForTheRunningRuntimeWithoutStartingOne(t *testing.T) {
	if out, err := exec.Command("sh", "-n", "-c", orca.ProbeScript).CombinedOutput(); err != nil {
		t.Fatalf("probe script does not parse: %v: %s", err, out)
	}
	for _, launch := range []string{"serve.sh", "setsid", "services create", "nohup", " serve "} {
		if strings.Contains(orca.ProbeScript, launch) {
			t.Errorf("the probe script carries %q, which could start a runtime", launch)
		}
	}
	for _, read := range []string{"flock -n 9", "exit 3", `until grep -q '"type":"orca_server_ready"'`, "within 120s", "| tail -n 1"} {
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
		Entry:         ".local/share/cc-remote/tools/orca-runtime-1.4.218/squashfs-root/AppRun",
		Display:       []string{"xvfb-run", "--auto-servernum", "--server-args=-nolisten tcp"},
		Args:          []string{"--log-level", "info"},
		Port:          7001,
		GoBuildJobs:   2,
		GoMemoryLimit: 2 << 30,
	}
	want := strings.Join([]string{
		"#!/bin/sh",
		"set -eu",
		"umask 077",
		`export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"`,
		"export GOFLAGS=-p=2",
		"export GOMEMLIMIT=2147483648",
		`cd "$HOME/.cc-remote/orca"`,
		"exec 9> serve.lock",
		"flock -n 9 || exit 0",
		"say() {",
		`  printf '%s\n' "$1" >&2`,
		`  printf '%s\n' "$1" >> serve.log || :`,
		"}",
		"child=",
		`trap 'kill "$child" 2> /dev/null || :; wait "$child" 2> /dev/null || :; exit 143' HUP INT TERM`,
		"delay=1",
		"while :; do",
		"  logged=0",
		"  test ! -f serve.log || logged=$(wc -c < serve.log)",
		"  started=$(date +%s)",
		`  xvfb-run --auto-servernum '--server-args=-nolisten tcp' "$HOME/".local/share/cc-remote/tools/orca-runtime-1.4.218/squashfs-root/AppRun serve --port 7001 --pairing-address 127.0.0.1 --json --log-level info > serve.json 2>> serve.log 9>&- &`,
		"  child=$!",
		"  status=0",
		`  wait "$child" || status=$?`,
		"  test $(($(date +%s) - started)) -lt 60 || delay=1",
		"  enospc=0",
		"  tail -c +$((logged + 1)) serve.log 2> /dev/null | grep -q ENOSPC && enospc=1",
		`  full=$(df -Pk "$HOME" | awk -v enospc="$enospc" 'NR == 2 && (enospc || $4 < 65536) { printf "cc-remote: the Orca runtime is out of disk space: %s on %s has %d MiB free", $6, $1, $4 / 1024 }')`,
		`  test -z "$full" || say "$full"`,
		`  say "cc-remote: the Orca runtime exited $status; starting it again in ${delay}s"`,
		`  sleep "$delay" &`,
		"  child=$!",
		`  wait "$child"`,
		"  delay=$((delay * 2))",
		"  test $delay -le 30 || delay=30",
		"done",
	}, "\n")
	if got := runtime.Launcher(); got != want {
		t.Errorf("Launcher =\n%s\nwant\n%s", got, want)
	}
}

const (
	launcherEntry = "runtime/AppRun"
	countRun      = "n=$(($(cat \"$HOME/runs\" 2> /dev/null || echo 0) + 1))\necho \"$n\" > \"$HOME/runs\"\n"
)

func dfReporting(availableKiB int) string {
	return "#!/bin/sh\ntest \"$1 $2\" = \"-Pk $HOME\" || exit 64\nprintf 'Filesystem 1024-blocks Used Available Capacity Mounted on\\n/dev/vdb 20971520 10485760 " + strconv.Itoa(availableKiB) + " 50%% /home\\n'\n"
}

func startLauncher(t *testing.T, home string, runtime orca.Runtime, scripts map[string]string) string {
	t.Helper()
	files := map[string]string{
		".local/bin/flock": "#!/bin/sh\nexit 0\n",
		".local/bin/sleep": "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$HOME/slept\"\n",
		".local/bin/df":    dfReporting(10 << 20),
	}
	maps.Copy(files, scripts)
	if err := os.MkdirAll(filepath.Join(home, ".cc-remote", "orca"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, script := range files {
		if err := os.MkdirAll(filepath.Join(home, filepath.Dir(path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, path), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	launch := exec.Command("sh", "-c", runtime.Launcher())
	launch.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	launch.Stderr = stderr
	launch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := launch.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Kill(-launch.Process.Pid, syscall.SIGKILL); err != nil {
			t.Errorf("kill the launcher's process group: %v", err)
		}
		_ = launch.Wait()
		_ = stderr.Close()
	})
	return stderr.Name()
}

func awaitFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, _ := os.ReadFile(path)
		if string(got) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %q, want %q", path, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRuntimeLauncherServesOnStockDefaults(t *testing.T) {
	home := filepath.Join(t.TempDir(), "a home with spaces")
	startLauncher(t, home, orca.Runtime{Entry: "orca runtime/orca/AppRun", Port: 7001}, map[string]string{
		"orca runtime/orca/AppRun": "#!/bin/sh\nprintf '%s\\0' \"$@\"\nexec /bin/sleep 600\n",
	})
	awaitFile(t, filepath.Join(home, ".cc-remote", "orca", "serve.json"), strings.Join([]string{"serve", "--port", "7001", "--pairing-address", "127.0.0.1", "--json", ""}, "\x00"))
}

func TestRuntimeLauncherRestartsTheRuntimeWithBackoff(t *testing.T) {
	home := t.TempDir()
	stderr := startLauncher(t, home, orca.Runtime{Entry: launcherEntry, Port: 7001}, map[string]string{
		launcherEntry:     "#!/bin/sh\n" + countRun + "test \"$n\" -lt 9 || exec /bin/sleep 600\nexit 3\n",
		".local/bin/date": "#!/bin/sh\ntest \"$1\" = +%s || exit 64\nn=$(($(cat \"$HOME/dates\" 2> /dev/null || echo 0) + 1))\necho \"$n\" > \"$HOME/dates\"\nif test \"$n\" -lt 16; then echo 1000; else echo 1060; fi\n",
	})
	awaitFile(t, filepath.Join(home, "runs"), "9\n")
	delays := []string{"1", "2", "4", "8", "16", "30", "30", "1"}
	if got, want := readFile(t, filepath.Join(home, "slept")), strings.Join(delays, "\n")+"\n"; got != want {
		t.Errorf("the launcher slept %q between runs, want %q: doubling from 1s, capped at 30s, back to 1s after a 60s run", got, want)
	}
	var lines strings.Builder
	for _, delay := range delays {
		lines.WriteString("cc-remote: the Orca runtime exited 3; starting it again in " + delay + "s\n")
	}
	if got := readFile(t, stderr); got != lines.String() {
		t.Errorf("stderr = %q, want %q", got, lines.String())
	}
	if got := readFile(t, filepath.Join(home, ".cc-remote", "orca", "serve.log")); got != lines.String() {
		t.Errorf("serve.log = %q, want %q", got, lines.String())
	}
}

func TestRuntimeLauncherKeepsSessionsAcrossARestart(t *testing.T) {
	home := t.TempDir()
	stderr := startLauncher(t, home, orca.Runtime{Entry: launcherEntry, Port: 7001}, map[string]string{
		launcherEntry: "#!/bin/sh\n" + countRun +
			"if (true >&9) 2> /dev/null; then echo open; else echo closed; fi >> \"$HOME/lock\"\n" +
			"test \"$n\" -gt 1 && exec /bin/sleep 600\n" +
			"/bin/sleep 600 &\necho \"$!\" > \"$HOME/session\"\nexit 9\n",
	})
	awaitFile(t, filepath.Join(home, "lock"), "closed\nclosed\n")
	session, err := strconv.Atoi(strings.TrimSpace(readFile(t, filepath.Join(home, "session"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(session, 0); err != nil {
		t.Errorf("the session the first run started is gone after the restart: %v", err)
	}
	if got, want := readFile(t, stderr), "cc-remote: the Orca runtime exited 9; starting it again in 1s\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestRuntimeLauncherNamesAFullDisk(t *testing.T) {
	for _, tt := range []struct {
		name, earlier, run string
		availableKiB       int
		want               string
	}{
		{
			"the run reports ENOSPC", "", "echo 'Error: ENOSPC: no space left on device, write' >&2\nexit 1\n", 10 << 20,
			"cc-remote: the Orca runtime is out of disk space: /home on /dev/vdb has 10240 MiB free\ncc-remote: the Orca runtime exited 1; starting it again in 1s\n",
		},
		{
			"df shows under 64 MiB", "", "exit 1\n", 63 << 10,
			"cc-remote: the Orca runtime is out of disk space: /home on /dev/vdb has 63 MiB free\ncc-remote: the Orca runtime exited 1; starting it again in 1s\n",
		},
		{
			"a normal exit after an earlier run's ENOSPC", "Error: ENOSPC: no space left on device, write\n", "exit 0\n", 64 << 10,
			"cc-remote: the Orca runtime exited 0; starting it again in 1s\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			stderr := startLauncher(t, home, orca.Runtime{Entry: launcherEntry, Port: 7001}, map[string]string{
				launcherEntry:               "#!/bin/sh\n" + countRun + "test \"$n\" -gt 1 && exec /bin/sleep 600\n" + tt.run,
				".local/bin/df":             dfReporting(tt.availableKiB),
				".cc-remote/orca/serve.log": tt.earlier,
			})
			awaitFile(t, filepath.Join(home, "runs"), "2\n")
			if got := readFile(t, stderr); got != tt.want {
				t.Errorf("stderr = %q, want %q", got, tt.want)
			}
			if got := readFile(t, filepath.Join(home, ".cc-remote", "orca", "serve.log")); !strings.HasSuffix(got, tt.want) {
				t.Errorf("serve.log = %q, want it to end with %q", got, tt.want)
			}
		})
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
	keyed := `shell_environment_policy.filters={ANTHROPIC_API_KEY="exclude",CLAUDE_CODE_OAUTH_TOKEN="exclude",ANTHROPIC_AUTH_TOKEN="exclude",OPENAI_API_KEY="exclude",CODEX_API_KEY="exclude",TYPESAFE_API_KEY="exclude"}`
	legacy := orca.ShellPolicy{Legacy: true, Exclude: []string{"AWS_*", "GITHUB_TOKEN"}}
	tests := []struct {
		name   string
		agent  orca.Agent
		policy orca.ShellPolicy
		want   []string
	}{
		{"claude with no MCP servers", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}, orca.ShellPolicy{}, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions", "--settings", `{"skipDangerousModePermissionPrompt":true}`,
			"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "claude-opus-5-5", "--effort", "xhigh",
		}},
		{"claude with an MCP allowlist", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "high", MCP: []string{"/srv/a.json", "/srv/b.json"}}, orca.ShellPolicy{}, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions", "--settings", `{"skipDangerousModePermissionPrompt":true}`,
			"--strict-mcp-config", "--mcp-config", "/srv/a.json", "/srv/b.json",
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "m", "--effort", "high",
		}},
		{"claude with lane settings merged under its own", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh", Settings: `{"skipDangerousModePermissionPrompt":false,"spinnerTipsEnabled":false,"permissions":{"deny":["AskUserQuestion","PushNotification"]}}`}, orca.ShellPolicy{}, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions", "--settings", `{"permissions":{"deny":["AskUserQuestion","PushNotification"]},"skipDangerousModePermissionPrompt":true,"spinnerTipsEnabled":false}`,
			"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "claude-opus-5-5", "--effort", "xhigh",
		}},
		{"claude resuming a session", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh", Resume: "5d576546-4c87-4a78-94b4-945eb9e49c86"}, orca.ShellPolicy{}, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions", "--settings", `{"skipDangerousModePermissionPrompt":true}`,
			"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
			"--disallowedTools", "AskUserQuestion,EnterPlanMode,ExitPlanMode", "--model", "claude-opus-5-5", "--effort", "xhigh",
			"--resume", "5d576546-4c87-4a78-94b4-945eb9e49c86",
		}},
		{"claude ignores a codex shell policy", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}, legacy, []string{
			"claude", "--allow-dangerously-skip-permissions", "--permission-mode", "bypassPermissions", "--settings", `{"skipDangerousModePermissionPrompt":true}`,
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
			"-c", `shell_environment_policy.exclude=["AWS_*","GITHUB_TOKEN","ANTHROPIC_API_KEY","CLAUDE_CODE_OAUTH_TOKEN","ANTHROPIC_AUTH_TOKEN","OPENAI_API_KEY","CODEX_API_KEY","TYPESAFE_API_KEY"]`,
			"-c", "mcp_servers={}",
		}},
		{"codex over a legacy include list alone", orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6.1-sol", Effort: "xhigh"}, orca.ShellPolicy{Legacy: true}, []string{
			"codex", "--no-daemon", "--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6.1-sol",
			"-c", `model_reasoning_effort="xhigh"`,
			"-c", `model_provider="cc_remote_openai"`,
			"-c", `model_providers.cc_remote_openai={name="OpenAI remote worker",base_url="https://api.openai.com/v1",env_key="OPENAI_API_KEY",requires_openai_auth=false,wire_api="responses"}`,
			"-c", `cli_auth_credentials_store="ephemeral"`,
			"-c", `shell_environment_policy.exclude=["ANTHROPIC_API_KEY","CLAUDE_CODE_OAUTH_TOKEN","ANTHROPIC_AUTH_TOKEN","OPENAI_API_KEY","CODEX_API_KEY","TYPESAFE_API_KEY"]`,
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
		{"codex settings", orca.Agent{Kind: orca.AgentCodex, Model: "m", Effort: "e", Settings: `{}`}, "claude setting"},
		{"settings not an object", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e", Settings: `["verbose"]`}, "not a JSON object"},
		{"invalid settings", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e", Settings: `{"verbose":`}, "not a JSON object"},
		{"null settings", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e", Settings: `null`}, "not a JSON object"},
		{"codex resume", orca.Agent{Kind: orca.AgentCodex, Model: "m", Effort: "e", Resume: "5d576546-4c87-4a78-94b4-945eb9e49c86"}, "claude setting"},
		{"resume not a session id", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e", Resume: "--fork-session"}, "not a Claude session id"},
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
		updater  string
	}{
		{orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e", Env: []string{"CLAUDE_LONG_RUNNING_LANE=lane 'a'"}}, "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "1"},
		{orca.Agent{Kind: orca.AgentCodex, Model: "m", Effort: "e", Env: []string{"CLAUDE_LONG_RUNNING_LANE=lane 'a'"}}, "OPENAI_API_KEY", "CODEX_API_KEY", "unset"},
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
			worker := "#!/bin/sh\nprintf '%s|%s|%s|%s|%s|%s\\n' \"$" + tt.variable + "\" \"${" + tt.cleared + "-unset}\" \"$CAPT_HOOK_ACTOR_JUDGE\" \"${DISABLE_AUTOUPDATER-unset}\" \"$CLAUDE_LONG_RUNNING_LANE\" \"$1\"\n"
			if err := os.WriteFile(filepath.Join(bin, tt.agent.Kind), []byte(worker), 0o700); err != nil {
				t.Fatal(err)
			}
			command := tt.agent.Command(dir, orca.ShellPolicy{})
			if !strings.HasPrefix(command, "exec sh -c ") {
				t.Fatalf("worker command relies on the terminal's configured shell: %s", command)
			}
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(slices.DeleteFunc(os.Environ(), func(pair string) bool { return strings.HasPrefix(pair, "DISABLE_AUTOUPDATER=") }), "PATH="+bin+":"+os.Getenv("PATH"), tt.cleared+"=oauth-token", tt.variable+"=stale")
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
			want := "sk-test-key|unset|" + tt.agent.Kind + "|" + tt.updater + "|lane 'a'|" + tt.agent.Argv(orca.ShellPolicy{})[1] + "\n"
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
		{"claude with an OpenAI judge key", orca.AgentClaude, "sk-test-key\nsk-judge-key\n", "sk-test-key|unset|unset|sk-judge-key|unset|unset|unset|unset|unset|unset|unset"},
		{"claude without one", orca.AgentClaude, "sk-test-key\n", "sk-test-key|unset|unset|unset|unset|unset|unset|unset|unset|unset|unset"},
		{"codex with an Anthropic judge key", orca.AgentCodex, "sk-test-key\nsk-judge-key\n", "sk-judge-key|unset|unset|sk-test-key|unset|unset|unset|unset|unset|unset|unset"},
		{"codex without one", orca.AgentCodex, "sk-test-key\n", "unset|unset|unset|sk-test-key|unset|unset|unset|unset|unset|unset|unset"},
		{"claude with a judge and a TypeSafe key", orca.AgentClaude, "sk-test-key\nsk-judge-key\nts-key\n", "sk-test-key|unset|unset|sk-judge-key|unset|ts-key|unset|unset|unset|unset|unset"},
		{"codex with only a TypeSafe key", orca.AgentCodex, "sk-test-key\n\nts-key\n", "unset|unset|unset|sk-test-key|unset|ts-key|unset|unset|unset|unset|unset"},
		{"claude with a GitHub token", orca.AgentClaude, "sk-test-key\nsk-judge-key\n\ngho-key\n", "sk-test-key|unset|unset|sk-judge-key|unset|unset|gho-key|unset|unset|unset|!gh auth git-credential"},
		{"codex with only a GitHub token", orca.AgentCodex, "sk-test-key\n\n\ngho-key\n", "unset|unset|unset|sk-test-key|unset|unset|gho-key|unset|unset|unset|!gh auth git-credential"},
		{"claude with a Trunk key", orca.AgentClaude, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\ntrunk-sentinel\n", "sk-test-key|unset|unset|unset|unset|unset|gho-key|unset|trunk-sentinel|unset|!gh auth git-credential"},
		{"codex with only a Trunk key", orca.AgentCodex, "sk-test-key\n\n\n\nAda O'Launcher\nada@example.test\ntrunk-sentinel\n", "unset|unset|unset|sk-test-key|unset|unset|unset|unset|trunk-sentinel|unset|unset"},
		{"claude with a Gemini key", orca.AgentClaude, "sk-test-key\n\n\n\nAda O'Launcher\nada@example.test\n\ngemini-sentinel\n", "sk-test-key|unset|unset|unset|unset|unset|unset|unset|unset|gemini-sentinel|unset"},
		{"codex with a Trunk and a Gemini key", orca.AgentCodex, "sk-test-key\n\n\n\nAda O'Launcher\nada@example.test\ntrunk-sentinel\ngemini-sentinel\n", "unset|unset|unset|sk-test-key|unset|unset|unset|unset|trunk-sentinel|gemini-sentinel|unset"},
	}
	names := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "CODEX_API_KEY", "TYPESAFE_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "TRUNK_MERGE_API_KEY", "GEMINI_API_KEY"}
	report, stale := make([]string, 0, len(names)+1), make([]string, 0, len(names))
	for _, name := range names {
		report = append(report, `"${`+name+`-unset}"`)
		stale = append(stale, name+"=stale")
	}
	report = append(report, `"${GIT_CONFIG_VALUE_3-unset}"`)
	worker := "#!/bin/sh\nprintf '" + strings.Repeat("%s|", len(report)-1) + "%s\\n' " + strings.Join(report, " ") + "\n"
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
			if tt.kind == orca.AgentClaude {
				if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(codexLogin), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-c", agent.Command(dir, orca.ShellPolicy{}))
			cmd.Env = append(append(withoutCredentials(), stale...), "PATH="+bin+":"+os.Getenv("PATH"), "CODEX_HOME="+t.TempDir())
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

const codexLogin = "#!/bin/sh\nprintf '%s|%s\\n' \"$*\" \"$(cat)\" > \"$CODEX_HOME/login\"\n"

func TestClaudeCommandSignsCodexInWithTheJudgeKey(t *testing.T) {
	worker := "#!/bin/sh\ncat \"$CODEX_HOME/login\" 2>/dev/null || echo unset\n"
	tests := []struct {
		name, payload string
		codex         bool
		want          string
	}{
		{"with a judge key", "sk-test-key\nsk-judge-key\n", true, "login --with-api-key|sk-judge-key"},
		{"without one", "sk-test-key\n", true, "unset"},
		{"on a machine without codex", "sk-test-key\nsk-judge-key\n", false, "unset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e"}
			bin, dir := t.TempDir(), filepath.Join(t.TempDir(), "key.abc")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(dir, "key"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(worker), 0o700); err != nil {
				t.Fatal(err)
			}
			if tt.codex {
				if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(codexLogin), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			command := agent.Command(dir, orca.ShellPolicy{})
			if strings.Contains(command, "sk-judge-key") {
				t.Errorf("the command text carries the judge key: %s", command)
			}
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(withoutCredentials(), "PATH="+bin+":/usr/bin:/bin", "CODEX_HOME="+t.TempDir())
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
				t.Errorf("codex login saw %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentCommandGivesEveryWorkerTheLaunchersGitIdentityOverTheMachinesOwn(t *testing.T) {
	machine := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(machine, []byte("[user]\n\tname = Sprite\n\temail = noreply@sprites.dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$(git config --get user.name)\" \"$(git config --get user.email)\" \"$GIT_CONFIG_COUNT\" \"${name-unset}${email-unset}\"\n"
	tests := []struct {
		name, kind, payload, want string
	}{
		{"claude with a GitHub token", orca.AgentClaude, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n", "Ada O'Launcher|ada@example.test|4|unsetunset"},
		{"claude without one", orca.AgentClaude, "sk-test-key\n\n\n\nAda O'Launcher\nada@example.test\n", "Ada O'Launcher|ada@example.test|2|unsetunset"},
		{"codex with a GitHub token", orca.AgentCodex, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n", "Ada O'Launcher|ada@example.test|4|unsetunset"},
		{"codex without one", orca.AgentCodex, "sk-test-key\n\n\n\nAda O'Launcher\nada@example.test\n", "Ada O'Launcher|ada@example.test|2|unsetunset"},
		{"claude with GitHub and Trunk tokens", orca.AgentClaude, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\ntrunk-sentinel\n", "Ada O'Launcher|ada@example.test|4|unsetunset"},
		{"codex with a Trunk token", orca.AgentCodex, "sk-test-key\n\n\n\nAda O'Launcher\nada@example.test\ntrunk-sentinel\n", "Ada O'Launcher|ada@example.test|2|unsetunset"},
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
			if strings.Contains(command, "Ada O") || strings.Contains(command, "ada@example.test") {
				t.Errorf("the command text carries the identity: %s", command)
			}
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(withoutCredentials(), "PATH="+bin+":"+os.Getenv("PATH"), "GIT_CONFIG_GLOBAL="+machine, "GIT_CONFIG_NOSYSTEM=1", "name=stale", "email=stale")
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

func TestAgentCommandSavesTheLaunchersGitHubLoginWhereGhReadsIt(t *testing.T) {
	worker := "#!/bin/sh\nprintf '%s|%s\\n' \"$(gh config get -h github.com user 2>/dev/null)\" \"${login-unset}\"\n"
	tests := []struct {
		name, kind, payload, want string
	}{
		{"claude with a login", orca.AgentClaude, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n\n\nada-launcher\n", "ada-launcher|unset"},
		{"codex with a login", orca.AgentCodex, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\ntrunk-sentinel\ngemini-sentinel\nada-launcher\n", "ada-launcher|unset"},
		{"claude without one", orca.AgentClaude, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\n", "|unset"},
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
			if strings.Contains(command, "ada-launcher") {
				t.Errorf("the command text carries the login: %s", command)
			}
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(withoutCredentials(), "PATH="+bin+":"+os.Getenv("PATH"), "GH_CONFIG_DIR="+t.TempDir(), "login=stale")
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

func TestAgentCommandExportsTheEnvLinesAssignments(t *testing.T) {
	worker := "#!/bin/sh\nprintf '%s|%s|%s\\n' \"${DOOR_TOKEN-unset}\" \"${CI_DOOR_URL-unset}\" \"${environment-unset}\"\n"
	tests := []struct {
		name, kind, payload, want string
	}{
		{"claude with assignments", orca.AgentClaude, "sk-test-key\n\n\n\nAda O'Launcher\nada@example.test\n\n\n\nDOOR_TOKEN=Fw+tok/en== CI_DOOR_URL=https://door.lambda-url.us-west-2.on.aws/\n", "Fw+tok/en==|https://door.lambda-url.us-west-2.on.aws/|unset"},
		{"codex with assignments", orca.AgentCodex, "sk-test-key\n\n\ngho-key\nAda O'Launcher\nada@example.test\ntrunk-sentinel\ngemini-sentinel\nada-launcher\nCI_DOOR_URL=https://door.lambda-url.us-west-2.on.aws/\n", "unset|https://door.lambda-url.us-west-2.on.aws/|unset"},
		{"claude without them", orca.AgentClaude, "sk-test-key\n\n\n\nAda O'Launcher\nada@example.test\n", "unset|unset|unset"},
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
			if strings.Contains(command, "Fw+tok") {
				t.Errorf("the command text carries an assignment: %s", command)
			}
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(withoutCredentials(), "PATH="+bin+":"+os.Getenv("PATH"), "GH_CONFIG_DIR="+t.TempDir(), "environment=stale")
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

func TestEnvLineRefusesAnAssignmentAnotherSlotOwns(t *testing.T) {
	for _, name := range orca.CredentialEnv() {
		if _, err := orca.EnvLine([]byte(name + "=x\n")); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("EnvLine(%s=x) = %v, want refused naming it", name, err)
		}
	}
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "TRUNK_MERGE_API_KEY", "GEMINI_API_KEY", "GIT_CONFIG_KEY_0"} {
		if _, err := orca.EnvLine([]byte("AWS_REGION=us-west-2\n" + name + "=x\n")); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("EnvLine with %s = %v, want refused naming it", name, err)
		}
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

func TestClaudeSettingsOnlySkipTheBypassWarning(t *testing.T) {
	tests := []struct {
		name   string
		agent  orca.Agent
		policy orca.ShellPolicy
	}{
		{"claude", orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}, orca.ShellPolicy{}},
		{"claude with MCP servers", orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "high", MCP: []string{`{"mcpServers":{"docs":{"command":"docs-mcp"}}}`}}, orca.ShellPolicy{}},
		{"codex keyed", orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6.1-sol", Effort: "xhigh"}, orca.ShellPolicy{}},
		{"codex legacy", orca.Agent{Kind: orca.AgentCodex, Model: "gpt-6.1-sol", Effort: "xhigh", Tier: "fast"}, orca.ShellPolicy{Legacy: true, Exclude: []string{"AWS_*"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argv := tt.agent.Argv(tt.policy)
			flags := 0
			for _, arg := range argv {
				if arg == "--settings" {
					flags++
				}
			}
			if tt.agent.Kind == orca.AgentCodex {
				if flags != 0 || slices.ContainsFunc(argv, func(arg string) bool { return strings.Contains(arg, "skipDangerousModePermissionPrompt") }) {
					t.Errorf("codex argv gained Claude settings: %q", argv)
				}
				return
			}
			i := slices.Index(argv, "--settings")
			if flags != 1 || i+1 >= len(argv) {
				t.Fatalf("argv carries %d --settings flags: %q", flags, argv)
			}
			var settings map[string]any
			if err := json.Unmarshal([]byte(argv[i+1]), &settings); err != nil {
				t.Fatal(err)
			}
			if len(settings) != 1 || settings["skipDangerousModePermissionPrompt"] != true {
				t.Errorf("settings = %v, want only skipDangerousModePermissionPrompt true", settings)
			}
			mode := slices.Index(argv, "--permission-mode")
			if mode < 0 || argv[mode+1] != "bypassPermissions" || !slices.Contains(argv, "--allow-dangerously-skip-permissions") {
				t.Errorf("the settings changed the selected permission mode: %q", argv)
			}
			model, effort := slices.Index(argv, "--model"), slices.Index(argv, "--effort")
			if model < 0 || argv[model+1] != tt.agent.Model || effort < 0 || argv[effort+1] != tt.agent.Effort {
				t.Errorf("the settings displaced the requested model or effort: %q", argv)
			}
		})
	}
}

func TestClaudeCommandCarriesItsSettingsThroughTheShell(t *testing.T) {
	agent := orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh", MCP: []string{`{"mcpServers":{"docs":{"command":"docs-mcp","args":["it's"]}}}`}}
	worker := "#!/bin/sh\nprintf '%s|%s|%s\\n' \"$ANTHROPIC_API_KEY\" \"$OPENAI_API_KEY\" \"$CAPT_HOOK_ACTOR_JUDGE\"\nprintf '%s\\n' \"$@\"\n"
	bin, dir := t.TempDir(), filepath.Join(t.TempDir(), "key.abc")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(worker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(codexLogin), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", agent.Command(dir, orca.ShellPolicy{}))
	cmd.Env = append(withoutCredentials(), "PATH="+bin+":"+os.Getenv("PATH"), "CODEX_HOME="+t.TempDir())
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
	argv := agent.Argv(orca.ShellPolicy{})
	if want := "sk-test-key|sk-judge-key|claude\n" + strings.Join(argv[1:], "\n") + "\n"; string(out) != want {
		t.Errorf("worker saw\n%s\nwant\n%s", out, want)
	}
	if !strings.Contains(string(out), "\n--settings\n{\"skipDangerousModePermissionPrompt\":true}\n") {
		t.Errorf("the settings object did not reach the worker as one argument: %s", out)
	}
}

func withoutCredentials() []string {
	return slices.DeleteFunc(os.Environ(), func(pair string) bool {
		name, _, _ := strings.Cut(pair, "=")
		return slices.Contains(orca.CredentialEnv(), name)
	})
}
