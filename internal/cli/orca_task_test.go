package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
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

func TestOrcaTaskRoundTripsWithoutSecrets(t *testing.T) {
	dir := state.Dir(filepath.Join(t.TempDir(), strings.Repeat("nested", 20)))
	if _, err := loadTask(dir, "task-a"); err == nil || !strings.Contains(err.Error(), "orca create") {
		t.Errorf("loadTask of nothing = %v", err)
	}
	driver := orcaDriver{state: dir}
	control, err := state.NewOrcaControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Dir(control)) })
	result := &workspace.Result{Name: "task-a", Provider: "sprites", Profile: "lean", Machine: "task-a", ProjectRoot: "/home/agent/app", SSH: workspace.SSH{Config: "/state/ssh/task-a.ssh"}}
	task, err := driver.task(result, orca.Agent{Kind: orca.AgentClaude, Model: "m", Effort: "e"}, control)
	if err != nil {
		t.Fatal(err)
	}
	if task.Port == 0 || task.Service != orca.RuntimeService || task.Environment != "task-a" || task.Forward.Control != control {
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
	err := (orcaTunnel{Host: "task-a"}).dropKey(ctx, "/home/agent/.cc-remote/orca/key.test")
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
			task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: "task-a", Provider: "absent", Profile: "lean", Environment: "task-a", RuntimeID: "rt-1", Port: 7001, Forward: orcaTunnel{Host: "task-a", Config: "/recorded/config", Control: control, Log: dir.OrcaForwardLog("task-a")}}
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
			if err != nil || loaded.RuntimeID != "rt-1" || loaded.Forward != task.Forward {
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
