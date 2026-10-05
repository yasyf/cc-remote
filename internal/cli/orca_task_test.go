package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/remote"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/tailnet"
	"github.com/yasyf/cc-remote/internal/workspace"
	"github.com/yasyf/cc-remote/internal/workspace/workspacetest"
)

type taskOrcaRunner func(context.Context, ...string) ([]byte, error)

func (f taskOrcaRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return f(ctx, args...)
}

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

func TestOrcaTunnelLaunchesInItsOwnSession(t *testing.T) {
	bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "identity")
	script := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = check ]; then exit 1; fi\ndone\nprintf '%s\\n' \"$$\" \"$(ps -o pgid= -p $$)\" > \"$FORWARD_IDENTITY\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FORWARD_IDENTITY", marker)
	control, err := state.NewOrcaControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Dir(control)) })
	tunnel := orcaTunnel{Host: "task-a", Config: "/state/ssh/task-a.ssh", Control: control, Log: filepath.Join(t.TempDir(), "forward.log")}
	if err := tunnel.ensure(t.Context(), 7001); err != nil {
		t.Fatal(err)
	}
	identity, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(identity))
	if len(fields) != 2 || fields[0] != fields[1] {
		t.Errorf("forward PID and process group = %q, want the forward to lead its own group", identity)
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

func TestCaptureKeysTakesTheOtherProvidersJudgeKeyOnlyWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	script := func(name, line string) []string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '"+line+"\\n'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return []string{path}
	}
	anthropic, openai := script("anthropic", "sk-anthropic-sentinel"), script("openai", "sk-openai-sentinel")
	tests := []struct {
		name       string
		kind       string
		keys       map[string][]string
		key, judge string
	}{
		{"claude with an OpenAI judge", orca.AgentClaude, map[string][]string{config.KeyAnthropic: anthropic, config.KeyOpenAI: openai}, "sk-anthropic-sentinel", "sk-openai-sentinel"},
		{"claude alone", orca.AgentClaude, map[string][]string{config.KeyAnthropic: anthropic}, "sk-anthropic-sentinel", ""},
		{"codex with a Claude judge", orca.AgentCodex, map[string][]string{config.KeyOpenAI: openai, config.KeyAnthropic: anthropic}, "sk-openai-sentinel", "sk-anthropic-sentinel"},
		{"codex alone", orca.AgentCodex, map[string][]string{config.KeyOpenAI: openai}, "sk-openai-sentinel", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, judge, err := captureKeys(t.Context(), tt.keys, orca.Agent{Kind: tt.kind})
			if err != nil || string(key) != tt.key || string(judge) != tt.judge {
				t.Errorf("captureKeys = %q, %q, %v; want %q, %q", key, judge, err, tt.key, tt.judge)
			}
		})
	}
	failing := filepath.Join(dir, "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho sk-leaked-sentinel; exit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []orca.Agent{{Kind: orca.AgentClaude}, {Kind: orca.AgentCodex}} {
		keys := map[string][]string{agent.KeyProvider(): script("own-"+agent.Kind, "sk-own-sentinel"), agent.JudgeKeyProvider(): {failing}}
		key, judge, err := captureKeys(t.Context(), keys, agent)
		if err == nil || !strings.HasPrefix(err.Error(), "judge key command") || key != nil || judge != nil {
			t.Errorf("%s captureKeys with a failing judge = %q, %q, %v", agent.Kind, key, judge, err)
		}
		if err != nil && strings.Contains(err.Error(), "sentinel") {
			t.Errorf("the error leaks a key: %v", err)
		}
	}
}

func TestOrcaTaskRoundTripsWithoutSecrets(t *testing.T) {
	dir := state.Dir(filepath.Join(t.TempDir(), strings.Repeat("nested", 20)))
	if _, err := loadTask(dir, "task-a"); err == nil || !strings.Contains(err.Error(), "orca create") {
		t.Errorf("loadTask of nothing = %v", err)
	}
	driver := orcaDriver{state: dir}
	result := &workspace.Result{Name: "task-a", Provider: "sprites", Profile: "lean", Machine: "task-a", ProjectRoot: "/home/agent/app", SSH: &workspace.SSH{Config: "/state/ssh/task-a.ssh"}}
	task, err := driver.task(result, orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Dir(task.Forward.Control)) })
	if task.Port == 0 || task.Service != orca.RuntimeService || task.Environment != "task-a" || task.Gateway != nil || state.EnsureOrcaControl(task.Forward.Control) != nil || task.Forward.Config != "/state/ssh/task-a.ssh" {
		t.Errorf("task = %+v", task)
	}
	loaded, err := loadTask(dir, "task-a")
	if err != nil || loaded.Port != task.Port || *loaded.Forward != *task.Forward || loaded.Gateway != nil || loaded.Agent.Model != "m" {
		t.Errorf("loadTask = %+v, %v", loaded, err)
	}
	info, err := os.Stat(dir.Orca("task-a"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("task file mode = %v, %v", info, err)
	}
}

func TestKeyCleanupHasItsOwnDeadlineAfterCancellation(t *testing.T) {
	bin, marker := t.TempDir(), filepath.Join(t.TempDir(), "started")
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf started > \"$CLEANUP_MARKER\"\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("CLEANUP_MARKER", marker)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	err := (&orcaTask{Workspace: "task-a", Forward: &orcaTunnel{Host: "task-a"}}).dropKey(ctx, "/home/agent/.cc-remote/orca/key.test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dropKey = %v, want its own deadline", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cleanup did not run after cancellation: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*keyDropTimeout {
		t.Errorf("cleanup took %v for a %v deadline", elapsed, keyDropTimeout)
	}
}

func TestReconnectOnlyRestoresTheRecordedTransport(t *testing.T) {
	for _, runtimeID := range []string{"rt-1", "rt-2"} {
		t.Run(runtimeID, func(t *testing.T) {
			dir, bin := state.Dir(t.TempDir()), t.TempDir()
			control, err := state.NewOrcaControl()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(filepath.Dir(control)) })
			if err := os.Remove(filepath.Dir(control)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\ncase \" $* \" in *' -O check '*) exit 1;; esac\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			path := filepath.Join(t.TempDir(), "config.yaml")
			config := fmt.Sprintf("repository: https://github.com/example/app\nref: main\nprovider: absent\nprofile: lean\ninventory: missing.yaml\nproviders:\n  absent: {}\nworkspace_dirs:\n  absent: /home/agent\nprofiles:\n  lean: {}\nstate_dir: %q\n", dir)
			if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: "task-a", Provider: "absent", Profile: "lean", Environment: "task-a", RuntimeID: "rt-1", Port: 7001, Forward: &orcaTunnel{Host: "task-a", Config: "/recorded/config", Control: control, Log: dir.OrcaForwardLog("task-a")}}
			if err := state.Save(dir.Orca("task-a"), task); err != nil {
				t.Fatal(err)
			}
			calls := 0
			runner := taskOrcaRunner(func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if !slices.Equal(args, []string{"status", "--environment", "task-a", "--json"}) {
					t.Fatalf("unexpected runtime operation: %v", args)
				}
				return fmt.Appendf(nil, `{"ok":true,"result":{"runtime":{"reachable":true,"runtimeId":%q}},"_meta":{"runtimeId":%q}}`, runtimeID, runtimeID), nil
			})
			cmd := newOrcaReconnectCmd(runner)
			cmd.SetArgs([]string{"task-a", "--config", path})
			cmd.SetOut(io.Discard)
			err = cmd.ExecuteContext(t.Context())
			if (err == nil) != (runtimeID == "rt-1") || calls != 1 {
				t.Fatalf("reconnect = %v, calls = %d", err, calls)
			}
			loaded, err := loadTask(dir, "task-a")
			if err != nil || loaded.RuntimeID != "rt-1" || *loaded.Forward != *task.Forward {
				t.Errorf("recorded task changed: %+v, %v", loaded, err)
			}
			if info, err := os.Stat(filepath.Dir(control)); err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("recorded control parent was not restored: %v, %v", info, err)
			}
		})
	}
}

func TestPromptKeepsAnAcceptedReceiptWithoutResending(t *testing.T) {
	calls := 0
	runner := taskOrcaRunner(func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if slices.Contains(args, "--retry-request") {
			t.Fatalf("prompt replayed: %v", args)
		}
		return []byte(`{"ok":true,"result":{"send":{"handle":"term-1","accepted":true,"prompt":{"requestId":"req-1","stages":["input_accepted"],"provider":"claude","processIncarnation":"p1"}}},"_meta":{"runtimeId":"rt-1"}}`), nil
	})
	driver := orcaDriver{state: state.Dir(t.TempDir()), client: orca.NewClient(runner)}
	task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: "task-a", Environment: "task-a", RuntimeID: "rt-1", Terminal: "term-1"}
	receipt, err := driver.prompt(t.Context(), task, "do the task")
	if !errors.Is(err, ErrNotSubmitted) || receipt == nil || receipt.RequestID != "req-1" || calls != 1 || strings.Contains(err.Error(), "--retry-request") {
		t.Fatalf("prompt = %+v, %v; calls = %d", receipt, err, calls)
	}
	loaded, err := loadTask(driver.state, "task-a")
	if err != nil || len(loaded.Receipts) != 1 || loaded.Receipts[0].RequestID != "req-1" || loaded.Receipts[0].ProcessIncarnation != "p1" || loaded.Receipts[0].Submitted {
		t.Fatalf("stored receipt = %+v, %v", loaded, err)
	}
}

func TestCaptainPinsSelectOnlyCaptainHookPlugins(t *testing.T) {
	inventory := images.Inventory{Claude: images.Claude{Plugins: []images.Plugin{
		{ID: "cc-context@cc-context", Version: "0.66.5"},
		{ID: "captain-hook@captain-hook", Version: "12.79.15"},
		{ID: "captain-hooks@other", Version: "1.0.0"},
		{ID: "observer@captain-hook", Version: "1.0.0"},
	}}}
	if got := captainPins(inventory); !slices.Equal(got, []orca.Pin{{ID: "captain-hook@captain-hook", Version: "12.79.15"}}) {
		t.Errorf("captainPins = %+v", got)
	}
	if got := captainPins(images.Inventory{}); len(got) != 0 {
		t.Errorf("an inventory without Captain Hook selected %+v", got)
	}
}

type scriptedOrca struct {
	t       *testing.T
	replies map[string]string
	calls   []string
}

func (s *scriptedOrca) Run(_ context.Context, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	s.calls = append(s.calls, command)
	reply, ok := s.replies[command]
	if !ok {
		s.t.Errorf("unexpected orca call: %s", command)
		return nil, fmt.Errorf("unexpected orca call: %s", command)
	}
	return []byte(reply), nil
}

func TestLaunchStopsAtIdleAndOnlyCreateSendsAPrompt(t *testing.T) {
	agent := orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh", MCP: []string{`{"mcpServers":{"docs":{"command":"docs-mcp"}}}`}}
	brief := []byte("Read every line.\n\n'quoted' \"double\" $(touch pwned) `touch pwned`\n")
	runtime := orca.Runtime{Entry: "tools/orca/AppRun"}
	ready := `{"type":"orca_server_ready","schemaVersion":1,"runtimeId":"rt-1","boundEndpoint":"ws://0.0.0.0:7001","advertisedEndpoint":"ws://127.0.0.1:7001","pairing":{"available":true,"url":"orca://pair?code=private"}}`
	envelope := func(runtimeID, result string) string {
		return `{"ok":true,"result":` + result + `,"_meta":{"runtimeId":"` + runtimeID + `"}}`
	}
	tests := []struct {
		name    string
		create  bool
		runtime string
		idle    bool
		corrupt bool
		want    string
		orca    int
		ssh     []string
		timings []string
	}{
		{name: "prepare", runtime: "rt-1", idle: true, orca: 7, ssh: []string{"check", "key-dir", "key-write", "head", "brief"}, timings: []string{"startup.config ok=true", "task.head ok=true", "task.sendBrief ok=true"}},
		{name: "create", create: true, runtime: "rt-1", idle: true, orca: 8, ssh: []string{"check", "key-dir", "key-write"}, timings: []string{"startup.config ok=true"}},
		{name: "busy worker", runtime: "rt-1", want: "was not tui-idle", orca: 7, ssh: []string{"check", "key-dir", "key-write"}, timings: []string{"startup.config ok=true"}},
		{name: "other runtime", runtime: "rt-2", want: `answered from runtime "rt-2", not rt-1`, orca: 2, ssh: []string{"check"}},
		{name: "unverified brief", runtime: "rt-1", idle: true, corrupt: true, want: "not the brief's", orca: 7, ssh: []string{"check", "key-dir", "key-write", "head", "brief"}, timings: []string{"startup.config ok=true", "task.head ok=true", "task.sendBrief ok=false"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := fakeRemote(t)
			if tt.corrupt {
				t.Setenv("REMOTE_CORRUPT", "1")
			}
			root, head := gitCheckout(t)
			control, err := state.NewOrcaControl()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(filepath.Dir(control)) })
			provider := &providertest.Fake{Handle: func(string, []string, []byte) providers.Result {
				return providers.Result{Stdout: []byte("starting\n" + ready + "\n")}
			}}
			if _, err := provider.Create(t.Context(), providers.Spec{Name: "task-a"}); err != nil {
				t.Fatal(err)
			}
			worktree := "repo-1::" + root
			scope := " --environment task-a --json"
			commands := []string{
				"environment add --name task-a --pairing-code orca://pair?code=private --json",
				"status" + scope,
				"repo add --path " + root + scope,
				"worktree list --repo id:repo-1" + scope,
				"terminal create --worktree id:" + worktree + " --title task-a --command " + agent.Command(fakeKeyDir, orca.ShellPolicy{}) + scope,
				"terminal read --terminal term-1 --screen" + scope,
				"terminal wait --terminal term-1 --for tui-idle --timeout-ms 60000" + scope,
				"terminal send --terminal term-1 --text do the task --enter --wait-submit 15" + scope,
			}
			fake := &scriptedOrca{t: t, replies: map[string]string{
				commands[0]: envelope("local", `{"environment":{"id":"env-1","name":"task-a"}}`),
				commands[1]: envelope(tt.runtime, `{"runtime":{"state":"ready","reachable":true,"runtimeId":"`+tt.runtime+`"}}`),
				commands[2]: envelope("rt-1", `{"repo":{"id":"repo-1","path":"`+root+`"}}`),
				commands[3]: envelope("rt-1", `{"worktrees":[{"id":"`+worktree+`","repoId":"repo-1","path":"`+root+`"}]}`),
				commands[4]: envelope("rt-1", `{"terminal":{"handle":"term-1"}}`),
				commands[5]: envelope("rt-1", `{"terminal":{"handle":"term-1","status":"running","source":"screen","tail":["Opus 5.5 (xhigh) · API Usage Billing","> "]}}`),
				commands[6]: envelope("rt-1", fmt.Sprintf(`{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":%t,"status":"running","exitCode":null,"blockedReason":"busy"}}`, tt.idle)),
				commands[7]: envelope("rt-1", `{"send":{"handle":"term-1","accepted":true,"prompt":{"requestId":"req-1","stages":["input_accepted","turn_started"],"provider":"claude","processIncarnation":"p1"}}}`),
			}}
			var logs bytes.Buffer
			session := &workspace.Session{Config: &config.Config{}, Provider: provider, Log: slog.New(slog.NewJSONHandler(&logs, nil))}
			driver := orcaDriver{client: orca.NewClient(fake), state: state.Dir(t.TempDir()), log: session.Log}
			task := &orcaTask{
				SchemaVersion: orcaTaskSchema, Workspace: "task-a", Provider: "fake", Profile: "lean", Machine: "task-a", ProjectRoot: root,
				Service: orca.RuntimeService, Port: 7001, Environment: "task-a", Agent: agent,
				Forward: &orcaTunnel{Host: "task-a", Config: "/state/ssh/task-a.ssh", Control: control, Log: filepath.Join(t.TempDir(), "forward.log")},
			}
			err = driver.launch(t.Context(), session, task, runtime, []byte("sk-test-key"), nil, "task-a")
			if err == nil && tt.create {
				_, err = driver.prompt(t.Context(), task, "do the task")
			}
			if err == nil && !tt.create {
				err = driver.publish(t.Context(), task, brief)
			}
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("launch = %v, want %q", err, tt.want)
			}
			if !slices.Equal(fake.calls, commands[:tt.orca]) {
				t.Errorf("orca calls =\n%q\nwant\n%q", fake.calls, commands[:tt.orca])
			}
			runtime.Port = 7001
			if got := provider.Calls(); !slices.Equal(got, []string{"create task-a", fmt.Sprintf("exec task-a %q", []string{"sh", "-c", runtime.EnsureScript()})}) {
				t.Errorf("provider calls = %q", got)
			}
			if got := sshCalls(t); !slices.Equal(got, tt.ssh) {
				t.Errorf("ssh calls = %q, want %q", got, tt.ssh)
			}
			if got := timingRecords(t, &logs, "sk-test-key", "private", "pwned", "claude", "term-1", root, head); !slices.Equal(got, tt.timings) {
				t.Errorf("timings = %q, want %q", got, tt.timings)
			}
			loaded, err := loadTask(driver.state, "task-a")
			if err != nil || loaded.RuntimeID != "rt-1" || loaded.Agent.Model != agent.Model || loaded.Agent.Effort != agent.Effort || !slices.Equal(loaded.Agent.MCP, agent.MCP) {
				t.Fatalf("stored task = %+v, %v", loaded, err)
			}
			dir := filepath.Join(home, ".cc-remote", "orca", "tasks", "task-a")
			switch tt.name {
			case "prepare":
				want := artifactOf(dir+"/brief.md", brief)
				if !loaded.Prepared || *loaded.Brief != want || loaded.BaseCommit != head || loaded.Terminal != "term-1" || loaded.WorktreeID != worktree || loaded.Receipts != nil {
					t.Errorf("prepared task = %+v, brief %+v", loaded, loaded.Brief)
				}
			case "create":
				if loaded.Prepared || loaded.Brief != nil || len(loaded.Receipts) != 1 || !loaded.Receipts[0].Submitted || loaded.Receipts[0].RequestID != "req-1" {
					t.Errorf("created task = %+v", loaded)
				}
			case "busy worker":
				if loaded.Prepared || loaded.Brief != nil || loaded.BaseCommit != "" || loaded.Terminal != "term-1" || loaded.Receipts != nil {
					t.Errorf("partial task = %+v", loaded)
				}
			case "other runtime":
				if loaded.Prepared || loaded.EnvironmentID != "env-1" || loaded.RepoID != "" || loaded.Terminal != "" {
					t.Errorf("partial task = %+v", loaded)
				}
			case "unverified brief":
				if loaded.Prepared || loaded.Brief != nil || loaded.BaseCommit != head || loaded.Receipts != nil {
					t.Errorf("partial task = %+v", loaded)
				}
			}
			if _, err := os.Stat(dir); (err == nil) != (tt.name == "prepare" || tt.name == "unverified brief") {
				t.Errorf("task directory %s: %v", dir, err)
			}
			if _, err := os.Stat(filepath.Join(home, "pwned")); !os.IsNotExist(err) {
				t.Errorf("the brief ran as shell: %v", err)
			}
		})
	}
}

func TestPrepareRefusesBadInputBeforeAnyEffect(t *testing.T) {
	fakeRemote(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	brief, empty := write("brief.md", "do the work\n"), write("empty.md", " \n\t")
	claude := []string{"--model", "claude-opus-5-5", "--effort", "xhigh"}
	codex := []string{"--agent", "codex", "--model", "gpt-6.1-sol", "--effort", "xhigh"}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"fable alias", []string{"--model", "fable", "--effort", "high", "--brief-file", brief}, "Fable requires an explicit local choice"},
		{"fable family", []string{"--model", "claude-fable-5-1", "--effort", "high", "--brief-file", brief}, "Fable requires an explicit local choice"},
		{"fable on codex", []string{"--agent", "codex", "--model", "Fable-5", "--effort", "xhigh", "--brief-file", brief}, "Fable requires an explicit local choice"},
		{"claude service tier", slices.Concat(claude, []string{"--service-tier", "fast", "--brief-file", brief}), "codex setting"},
		{"claude MCP file", slices.Concat(claude, []string{"--mcp-config", "/Users/me/.mcp.json", "--brief-file", brief}), "not an inline JSON object"},
		{"codex MCP file", slices.Concat(codex, []string{"--mcp-config", "servers.toml", "--brief-file", brief}), "not an inline TOML table"},
		{"empty brief", slices.Concat(claude, []string{"--brief-file", empty}), "is empty"},
		{"missing brief", slices.Concat(claude, []string{"--brief-file", filepath.Join(dir, "absent.md")}), "read the brief"},
		{"valid claude reaches the config", slices.Concat(claude, []string{"--mcp-config", `{"mcpServers":{}}`, "--brief-file", brief}), "read config"},
		{"codex fast tier reaches the config", slices.Concat(codex, []string{"--service-tier", "fast", "--mcp-config", "{}", "--brief-file", brief}), "read config"},
		{"existing with a ref", slices.Concat(claude, []string{"--existing", "--ref", "main", "--brief-file", brief}), "takes no --ref"},
		{"existing with an empty ref", slices.Concat(claude, []string{"--existing", "--ref=", "--brief-file", brief}), "takes no --ref"},
		{"existing reaches the config", slices.Concat(claude, []string{"--existing", "--brief-file", brief}), "read config"},
		{"warm with existing", slices.Concat(claude, []string{"--warm", "--existing", "--brief-file", brief}), "pass only one"},
		{"warm with a ref reaches the config", slices.Concat(claude, []string{"--warm", "--ref", "feature", "--brief-file", brief}), "read config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			runner := taskOrcaRunner(func(context.Context, ...string) ([]byte, error) {
				calls++
				return nil, errors.New("no runtime operation is expected")
			})
			cmd := newOrcaPrepareCmd(runner)
			cmd.SetArgs(append([]string{"task-a", "--config", filepath.Join(dir, "absent.yaml")}, tt.args...))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.ExecuteContext(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) || calls != 0 {
				t.Fatalf("prepare = %v, calls = %d, want %q", err, calls, tt.want)
			}
			if strings.Contains(err.Error(), "do the work") {
				t.Errorf("the error repeats the brief: %v", err)
			}
			if got := sshCalls(t); len(got) != 0 {
				t.Errorf("ssh calls = %q", got)
			}
		})
	}
}

func TestPrepareAndCreateShareTheirLaunchFlags(t *testing.T) {
	prepare, create := newOrcaPrepareCmd(nil), newOrcaCreateCmd(nil)
	for _, name := range []string{"config", "provider", "profile", "ref", "agent", "model", "effort", "service-tier", "mcp-config", "title"} {
		if prepare.Flags().Lookup(name) == nil || create.Flags().Lookup(name) == nil {
			t.Errorf("--%s is not on both prepare and create", name)
		}
	}
	if prepare.Flags().Lookup("prompt-file") != nil || create.Flags().Lookup("brief-file") != nil {
		t.Error("prepare takes a prompt or create takes a brief")
	}
	if existing := prepare.Flags().Lookup("existing"); existing == nil || existing.DefValue != "false" || create.Flags().Lookup("existing") != nil {
		t.Error("--existing is not a prepare-only opt-in that defaults to false")
	}
	if warm := prepare.Flags().Lookup("warm"); warm == nil || warm.DefValue != "false" || create.Flags().Lookup("warm") != nil {
		t.Error("--warm is not a prepare-only opt-in that defaults to false")
	}
	collect := newOrcaCollectCmd(nil)
	for _, name := range []string{"report-file", "patch-file", "output"} {
		if collect.Flags().Lookup(name) == nil {
			t.Errorf("collect lacks --%s", name)
		}
	}
}

func TestSendRefusesAPreparedTask(t *testing.T) {
	fakeRemote(t)
	dir := state.Dir(t.TempDir())
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(prompt, []byte("next step\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := &orcaTask{
		SchemaVersion: orcaTaskSchema, Workspace: "task-a", Environment: "task-a", RuntimeID: "rt-1", Terminal: "term-1",
		Prepared: true, Brief: &orcaArtifact{Path: "/home/agent/.cc-remote/orca/tasks/task-a/brief.md"}, BaseCommit: strings.Repeat("a", 40),
	}
	if err := state.Save(dir.Orca("task-a"), task); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := taskOrcaRunner(func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("no runtime operation is expected")
	})
	cmd := newOrcaSendCmd(runner)
	cmd.SetArgs([]string{"task-a", "--config", taskConfig(t, dir), "--prompt-file", prompt})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "worker-start") || calls != 0 {
		t.Fatalf("send = %v, calls = %d", err, calls)
	}
	if got := sshCalls(t); len(got) != 0 {
		t.Errorf("ssh calls = %q", got)
	}
	if loaded, err := loadTask(dir, "task-a"); err != nil || loaded.Receipts != nil || !loaded.Prepared {
		t.Errorf("stored task = %+v, %v", loaded, err)
	}
}

var (
	firstAgent   = orca.Agent{Kind: orca.AgentClaude, Model: "claude-opus-5-5", Effort: "xhigh"}
	serveCommand = regexp.MustCompile(` serve --port (\d+) --pairing-address (?:ws://127\.0\.0\.1:(\d+)|127\.0\.0\.1) `)
)

type gate struct {
	reached, release chan struct{}
	once             sync.Once
}

func newGate() *gate {
	return &gate{reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *gate) hold() {
	g.once.Do(func() {
		close(g.reached)
		<-g.release
	})
}

type firstWorker struct {
	t        *testing.T
	local    *workspacetest.LocalExec
	provider *providertest.Fake
	session  *workspace.Session
	platform workspace.Platform
	keys     string
	root     string
	head     string
	gateway  bool
	paused   *gate
}

type effects struct {
	provider int
	keys     string
	task     string
	record   string
	ssh      int
}

func newFirstWorker(t *testing.T, compute *providers.ComputeInstance, created bool) *firstWorker {
	t.Helper()
	fakeRemote(t)
	w := &firstWorker{
		t:        t,
		local:    workspacetest.NewLocalExec(t),
		gateway:  compute != nil,
		platform: workspace.Platform{Daemon: tailnet.Daemon{Mode: tailnet.Userspace, Supervisor: tailnet.Setsid}},
	}
	w.provider = &providertest.Fake{Compute: compute, Handle: w.handle, Now: func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }}
	w.session = workspacetest.Open(t, workspacetest.Harness{Provider: w.provider, Kind: "fake", Platform: w.platform, Root: w.local.Root + "/machines"})
	dir := t.TempDir()
	w.keys = filepath.Join(dir, "keys.log")
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("#!/bin/sh\necho read >> "+remote.Quote(w.keys)+"\necho sk-test-key\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	w.session.Config.Orca.Keys = map[string][]string{config.KeyAnthropic: {key}}
	w.root = w.session.ProjectRoot()
	t.Setenv("REMOTE_HOME", w.local.Home("task-a"))
	if created {
		if _, err := w.session.Create(t.Context(), "task-a", workspace.Source{Ref: "main"}); err != nil {
			t.Fatal(err)
		}
		if err := w.session.Suspend(t.Context(), "task-a"); err != nil {
			t.Fatal(err)
		}
		w.head = gitOutput(t, w.root, "rev-parse", "HEAD")
	}
	return w
}

func readyAt(bound, advertised string) string {
	return `{"type":"orca_server_ready","schemaVersion":1,"runtimeId":"rt-1","boundEndpoint":"ws://0.0.0.0:` + bound + `","advertisedEndpoint":"ws://127.0.0.1:` + advertised + `","pairing":{"available":true,"url":"orca://pair?code=private"}}`
}

func (w *firstWorker) handle(id string, cmd []string, stdin []byte) providers.Result {
	if w.paused != nil {
		w.paused.hold()
	}
	script := cmd[len(cmd)-1]
	if serve := serveCommand.FindStringSubmatch(script); serve != nil {
		return providers.Result{Stdout: []byte("starting\n" + readyAt(serve[1], cmp.Or(serve[2], serve[1])) + "\n")}
	}
	if w.gateway && (strings.Contains(script, "mkfifo") || strings.Contains(script, fakeKeyDir) || strings.Contains(script, "rev-parse --verify HEAD") || strings.Contains(script, briefName)) {
		return viaFakeRemote(script, stdin)
	}
	return w.local.Handle(id, cmd, stdin)
}

func (w *firstWorker) native() (*scriptedOrca, []string) {
	return w.nativeAs("task-a")
}

func (w *firstWorker) nativeAs(name string) (*scriptedOrca, []string) {
	envelope := func(runtimeID, result string) string {
		return `{"ok":true,"result":` + result + `,"_meta":{"runtimeId":"` + runtimeID + `"}}`
	}
	worktree := "repo-1::" + w.root
	scope := " --environment " + name + " --json"
	commands := []string{
		"environment add --name " + name + " --pairing-code orca://pair?code=private --json",
		"status" + scope,
		"repo add --path " + w.root + scope,
		"worktree list --repo id:repo-1" + scope,
		"terminal create --worktree id:" + worktree + " --title " + name + " --command " + firstAgent.Command(fakeKeyDir, orca.ShellPolicy{}) + scope,
		"terminal read --terminal term-1 --screen" + scope,
		"terminal wait --terminal term-1 --for tui-idle --timeout-ms 60000" + scope,
	}
	return &scriptedOrca{t: w.t, replies: map[string]string{
		commands[0]: envelope("local", `{"environment":{"id":"env-1","name":"`+name+`"}}`),
		commands[1]: envelope("rt-1", `{"runtime":{"state":"ready","reachable":true,"runtimeId":"rt-1"}}`),
		commands[2]: envelope("rt-1", `{"repo":{"id":"repo-1","path":"`+w.root+`"}}`),
		commands[3]: envelope("rt-1", `{"worktrees":[{"id":"`+worktree+`","repoId":"repo-1","path":"`+w.root+`"}]}`),
		commands[4]: envelope("rt-1", `{"terminal":{"handle":"term-1"}}`),
		commands[5]: envelope("rt-1", `{"terminal":{"handle":"term-1","status":"running","source":"screen","tail":["Opus 5.5 (xhigh) · API Usage Billing","> "]}}`),
		commands[6]: envelope("rt-1", `{"wait":{"handle":"term-1","condition":"tui-idle","satisfied":true,"status":"running","exitCode":null,"blockedReason":""}}`),
	}}, commands
}

func (w *firstWorker) booting(native *scriptedOrca, command string) (*gate, orca.Runner) {
	paused := newGate()
	return paused, taskOrcaRunner(func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == command {
			paused.hold()
		}
		return native.Run(ctx, args...)
	})
}

func (w *firstWorker) contender() *workspace.Session {
	w.t.Helper()
	session, err := workspace.Open(w.session.Config, w.provider, "fake", "lean", w.platform)
	if err != nil {
		w.t.Fatal(err)
	}
	session.Log = slog.New(slog.DiscardHandler)
	return session
}

func (w *firstWorker) prepare(ctx context.Context, session *workspace.Session, runner orca.Runner, out io.Writer, name string, existing bool) error {
	launch := orcaLaunch{agent: firstAgent, existing: existing}
	return launch.start(ctx, out, session, runner, orca.Runtime{Entry: "tools/orca/AppRun"}, name, func(ctx context.Context, driver orcaDriver, task *orcaTask) error {
		return driver.publish(ctx, task, []byte("Read every line.\n"))
	})
}

func (w *firstWorker) effects() effects {
	keys, _ := os.ReadFile(w.keys)
	task, _ := os.ReadFile(w.session.Config.State().Orca("task-a"))
	record, _ := os.ReadFile(w.session.Config.State().Workspace("task-a"))
	return effects{provider: len(w.provider.Calls()), keys: string(keys), task: string(task), record: string(record), ssh: len(sshCalls(w.t))}
}

func (w *firstWorker) reach(paused *gate, done <-chan error) {
	w.t.Helper()
	select {
	case <-paused.reached:
	case err := <-done:
		w.t.Fatalf("the first launch returned %v before its pause", err)
	}
}

func (w *firstWorker) refusedWithoutEffects(run func(orca.Runner) error, want string) {
	w.t.Helper()
	before := w.effects()
	native := 0
	runner := taskOrcaRunner(func(context.Context, ...string) ([]byte, error) {
		native++
		return nil, errors.New("a refused command reached the Orca CLI")
	})
	done := make(chan error, 1)
	go func() { done <- run(runner) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Minute):
		w.t.Fatal("the contender waited for the task instead of refusing")
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		w.t.Errorf("contender = %v, want %q", err, want)
	}
	if after := w.effects(); after != before || native != 0 {
		w.t.Errorf("the refused contender changed %+v into %+v with %d Orca CLI calls", before, after, native)
	}
}

func (w *firstWorker) free(name string) {
	w.t.Helper()
	unlock, held, err := state.TryLock(w.session.Config.State().Orca(name) + ".lock")
	if err != nil || !held {
		w.t.Fatalf("the task lock for %s is still held after its command returned: %t, %v", name, held, err)
	}
	unlock()
}

func (w *firstWorker) prepared(out []byte) *orcaTask {
	w.t.Helper()
	loaded, err := loadTask(w.session.Config.State(), "task-a")
	if err != nil {
		w.t.Fatal(err)
	}
	if loaded.Forward != nil {
		w.t.Cleanup(func() { _ = os.Remove(filepath.Dir(loaded.Forward.Control)) })
	}
	home, err := filepath.EvalSymlinks(w.local.Home("task-a"))
	if err != nil {
		w.t.Fatal(err)
	}
	want := artifactOf(home+"/.cc-remote/orca/tasks/task-a/brief.md", []byte("Read every line.\n"))
	if !loaded.Prepared || loaded.Brief == nil || *loaded.Brief != want || loaded.BaseCommit != w.head || loaded.Terminal != "term-1" || loaded.Receipts != nil || loaded.Agent.Model != firstAgent.Model || loaded.Agent.Effort != firstAgent.Effort {
		w.t.Errorf("prepared task = %+v, brief %+v; want %+v at %s", loaded, loaded.Brief, want, w.head)
	}
	var emitted orcaTask
	if err := json.Unmarshal(out, &emitted); err != nil || emitted.BaseCommit != loaded.BaseCommit || !emitted.Prepared || emitted.Brief == nil || *emitted.Brief != want {
		w.t.Errorf("emitted task = %s, %v", out, err)
	}
	stored, err := os.ReadFile(w.session.Config.State().Orca("task-a"))
	if err != nil || bytes.Contains(stored, []byte("sk-test-key")) || bytes.Contains(out, []byte("sk-test-key")) {
		w.t.Errorf("the key reached the task record or its output: %s, %v", stored, err)
	}
	return loaded
}

func TestExistingPrepareStartsTheFirstWorkerOnTheRetainedCheckout(t *testing.T) {
	w := newFirstWorker(t, nil, true)
	if err := os.WriteFile(filepath.Join(w.root, "README"), []byte("work in progress\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.root, "notes.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := gitOutput(t, w.root, "status", "--porcelain", "--untracked-files=all")
	record := &workspace.Record{}
	if _, err := state.Load(w.session.Config.State().Workspace("task-a"), record); err != nil {
		t.Fatal(err)
	}
	machine, err := w.provider.Get(t.Context(), record.Machine)
	if err != nil || record.Ready || record.CreatedAt.Equal(machine.CreatedAt) {
		t.Fatalf("record %+v and machine %+v, %v: want a dormant record whose timestamp differs from the native creation time", record, machine, err)
	}
	calls, scripts := len(w.provider.Calls()), len(w.local.Scripts("task-a"))
	native, commands := w.native()
	var out bytes.Buffer
	if err := w.prepare(t.Context(), w.session, native, &out, "task-a", true); err != nil {
		t.Fatal(err)
	}
	w.prepared(out.Bytes())
	if !slices.Equal(native.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant\n%q", native.calls, commands)
	}
	lifecycle := w.provider.Calls()[calls:]
	if len(lifecycle) == 0 || lifecycle[0] != "wake task-a" {
		t.Errorf("provider calls = %q, want the recorded machine woken first", lifecycle)
	}
	for _, call := range lifecycle {
		if strings.HasPrefix(call, "create ") || strings.HasPrefix(call, "destroy ") {
			t.Errorf("an existing workspace's first worker ran %q", call)
		}
	}
	for _, script := range w.local.Scripts("task-a")[scripts:] {
		for _, refresh := range []string{"clone --quiet", "fetch --quiet", "checkout --quiet --force"} {
			if strings.Contains(script, refresh) {
				t.Errorf("the retained checkout was refreshed by %q", script)
			}
		}
	}
	if got := gitOutput(t, w.root, "status", "--porcelain", "--untracked-files=all"); got != status {
		t.Errorf("checkout status = %q, want the retained %q", got, status)
	}
	if got := gitOutput(t, w.root, "rev-parse", "HEAD"); got != w.head {
		t.Errorf("HEAD moved to %s from %s", got, w.head)
	}
	if got := sshCalls(t); !slices.Equal(got, []string{"check", "key-dir", "key-write", "head", "brief"}) {
		t.Errorf("ssh calls = %q", got)
	}
	if keys, err := os.ReadFile(w.keys); err != nil || string(keys) != "read\n" {
		t.Errorf("key reads = %q, %v; want exactly one", keys, err)
	}
	w.free("task-a")
}

func TestExistingPrepareHoldsTheTaskFromAdmissionThroughOutput(t *testing.T) {
	w := newFirstWorker(t, nil, true)
	native, commands := w.native()
	booting, runner := w.booting(native, commands[5])
	resuming := newGate()
	w.paused = resuming
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- w.prepare(t.Context(), w.session, runner, &out, "task-a", true) }()
	contend := func(name, want string) {
		t.Helper()
		session := w.contender()
		w.refusedWithoutEffects(func(runner orca.Runner) error {
			return w.prepare(t.Context(), session, runner, io.Discard, name, true)
		}, want)
	}
	w.reach(resuming, done)
	if _, err := os.Stat(w.session.Config.State().Orca("task-a")); !os.IsNotExist(err) {
		t.Fatalf("a Sprite task was recorded while its workspace resumes: %v", err)
	}
	contend("task-a", "another cc-remote command holds the Orca task for task-a")
	contend("task-b", "no workspace is recorded for task-b")
	close(resuming.release)
	w.reach(booting, done)
	if _, err := loadTask(w.session.Config.State(), "task-a"); err != nil {
		t.Fatalf("no partial task was saved before the bootstrap: %v", err)
	}
	contend("task-a", "another cc-remote command holds the Orca task for task-a")
	close(booting.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	w.prepared(out.Bytes())
	if !slices.Equal(native.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant one worker's\n%q", native.calls, commands)
	}
	contend("task-a", "task-a already has an Orca task")
	w.free("task-a")
}

func TestCancelledExistingPrepareKeepsItsPartialTaskAndReleasesTheTask(t *testing.T) {
	w := newFirstWorker(t, nil, true)
	native, commands := w.native()
	booting, runner := w.booting(native, commands[5])
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- w.prepare(ctx, w.session, runner, &out, "task-a", true) }()
	w.reach(booting, done)
	cancel()
	close(booting.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare = %v, want the cancellation", err)
	}
	loaded, err := loadTask(w.session.Config.State(), "task-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Dir(loaded.Forward.Control)) })
	var emitted orcaTask
	if err := json.Unmarshal(out.Bytes(), &emitted); err != nil || emitted.Terminal != "term-1" || emitted.Prepared || loaded.Terminal != "term-1" || loaded.Prepared || loaded.Brief != nil {
		t.Errorf("partial task = %+v, emitted %s, %v", loaded, out.Bytes(), err)
	}
	for _, call := range w.provider.Calls() {
		if strings.HasPrefix(call, "destroy ") {
			t.Errorf("a cancelled first worker removed its machine: %q", call)
		}
	}
	w.free("task-a")
	later := w.contender()
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.prepare(t.Context(), later, runner, io.Discard, "task-a", true)
	}, "task-a already has an Orca task")
	if !slices.Equal(native.calls, commands) {
		t.Errorf("orca calls =\n%q\nwant one terminal and no resend\n%q", native.calls, commands)
	}
}

func TestExistingPrepareRefusesAMalformedTaskAndReleasesItsLock(t *testing.T) {
	w := newFirstWorker(t, nil, true)
	path := w.session.Config.State().Orca("task-a")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion": 1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.prepare(t.Context(), w.session, runner, io.Discard, "task-a", true)
	}, path)
	w.free("task-a")
}

func TestExistingPrepareAdmitsOnlyTheRecordedOwnedMachine(t *testing.T) {
	tests := []struct {
		name string
		edit func(t *testing.T, w *firstWorker, record *workspace.Record)
		as   string
		want string
	}{
		{"an unrecorded name", nil, "task-b", "no workspace is recorded for task-b"},
		{"another profile", func(_ *testing.T, _ *firstWorker, record *workspace.Record) { record.Profile = "other" }, "task-a", "workspace task-a is a fake/other workspace, not fake/lean"},
		{"an unverified create", func(_ *testing.T, _ *firstWorker, record *workspace.Record) { record.Unverified = true }, "task-a", "never confirmed"},
		{"a record of another name", func(_ *testing.T, _ *firstWorker, record *workspace.Record) { record.Name = "task-c" }, "task-a", "names no machine of its own"},
		{"no recorded source", func(_ *testing.T, _ *firstWorker, record *workspace.Record) { record.Source = workspace.Source{} }, "task-a", "holds no valid source"},
		{"a missing machine", func(t *testing.T, w *firstWorker, record *workspace.Record) {
			if err := w.provider.Destroy(t.Context(), record.Machine); err != nil {
				t.Fatal(err)
			}
		}, "task-a", "find machine task-a of workspace task-a"},
		{"a same-name replacement", func(t *testing.T, w *firstWorker, record *workspace.Record) {
			if err := w.provider.Destroy(t.Context(), record.Machine); err != nil {
				t.Fatal(err)
			}
			if _, err := w.provider.Create(t.Context(), providers.Spec{Name: record.Machine}); err != nil {
				t.Fatal(err)
			}
		}, "task-a", "fake machine task-a does not carry the ownership of workspace task-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newFirstWorker(t, nil, true)
			if tt.edit != nil {
				path := w.session.Config.State().Workspace("task-a")
				record := &workspace.Record{}
				if _, err := state.Load(path, record); err != nil {
					t.Fatal(err)
				}
				tt.edit(t, w, record)
				if err := state.Save(path, record); err != nil {
					t.Fatal(err)
				}
			}
			w.refusedWithoutEffects(func(runner orca.Runner) error {
				return w.prepare(t.Context(), w.session, runner, io.Discard, tt.as, true)
			}, tt.want)
			if _, err := os.Stat(w.session.Config.State().Orca(tt.as)); !os.IsNotExist(err) {
				t.Errorf("a refused workspace got an Orca task: %v", err)
			}
			w.free(tt.as)
		})
	}
}

func TestExistingPrepareRefusesAForeignCheckoutOrRuntimeBeforeTheKey(t *testing.T) {
	kept := []byte("#!/bin/sh\necho kept\n")
	tests := []struct {
		name string
		edit func(t *testing.T, w *firstWorker)
		want string
	}{
		{"earlier runtime state", func(t *testing.T, w *firstWorker) {
			dir := filepath.Join(w.local.Home("task-a"), ".cc-remote", "orca")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "serve.sh"), kept, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "check the retained checkout on task-a exited 7: the machine already holds cc-remote Orca runtime state"},
		{"a dangling runtime link", func(t *testing.T, w *firstWorker) {
			dir := filepath.Join(w.local.Home("task-a"), ".cc-remote")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(dir, "orca")); err != nil {
				t.Fatal(err)
			}
		}, "check the retained checkout on task-a exited 7"},
		{"another origin", func(t *testing.T, w *firstWorker) {
			gitOutput(t, w.root, "remote", "set-url", "origin", "https://github.com/someone/secret-fork")
		}, "check the retained checkout on task-a exited 6: the checkout's origin is not the configured repository"},
		{"no checkout at the root", func(t *testing.T, w *firstWorker) {
			if err := os.RemoveAll(filepath.Join(w.root, ".git")); err != nil {
				t.Fatal(err)
			}
		}, "check the retained checkout on task-a exited 5: the configured project root is not the top level of a git checkout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newFirstWorker(t, nil, true)
			tt.edit(t, w)
			calls := 0
			runner := taskOrcaRunner(func(context.Context, ...string) ([]byte, error) {
				calls++
				return nil, errors.New("no Orca CLI call is expected")
			})
			var out bytes.Buffer
			err := w.prepare(t.Context(), w.session, runner, &out, "task-a", true)
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "secret-fork") || strings.Contains(err.Error(), "fatal:") {
				t.Fatalf("prepare = %v, want %q without remote output", err, tt.want)
			}
			if _, err := os.Stat(w.keys); !os.IsNotExist(err) || calls != 0 || out.Len() != 0 {
				t.Errorf("the refusal read the key (%v), made %d Orca CLI calls, or printed %q", err, calls, out.Bytes())
			}
			for _, call := range w.provider.Calls() {
				if strings.Contains(call, "serve.sh.tmp") {
					t.Errorf("the refusal rewrote the runtime launcher: %q", call)
				}
			}
			if got := sshCalls(t); len(got) != 0 {
				t.Errorf("ssh calls = %q, want no key pipe", got)
			}
			if _, err := os.Stat(w.session.Config.State().Orca("task-a")); !os.IsNotExist(err) {
				t.Errorf("a refused first worker left a task record: %v", err)
			}
			if launcher := filepath.Join(w.local.Home("task-a"), ".cc-remote", "orca", "serve.sh"); tt.name == "earlier runtime state" {
				if got, err := os.ReadFile(launcher); err != nil || !bytes.Equal(got, kept) {
					t.Errorf("the earlier launcher = %q, %v; want it kept", got, err)
				}
			}
			w.free("task-a")
		})
	}
}

func TestDefaultPrepareStillCreatesAFreshWorkspaceUnderTheTaskLock(t *testing.T) {
	w := newFirstWorker(t, nil, false)
	native, commands := w.native()
	var out bytes.Buffer
	if err := w.prepare(t.Context(), w.session, native, &out, "task-a", false); err != nil {
		t.Fatal(err)
	}
	w.head = gitOutput(t, w.root, "rev-parse", "HEAD")
	w.prepared(out.Bytes())
	creates := 0
	for _, call := range w.provider.Calls() {
		switch {
		case call == "create task-a":
			creates++
		case strings.HasPrefix(call, "wake "):
			t.Errorf("a default prepare resumed a machine: %q", call)
		}
	}
	if creates != 1 || !slices.Equal(native.calls, commands) {
		t.Errorf("creates = %d, orca calls =\n%q\nwant one create and\n%q", creates, native.calls, commands)
	}
	w.free("task-a")
	later := w.contender()
	w.refusedWithoutEffects(func(runner orca.Runner) error {
		return w.prepare(t.Context(), later, runner, io.Discard, "task-a", false)
	}, "task-a already has an Orca task")
}
