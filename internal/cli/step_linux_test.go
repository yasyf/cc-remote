package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
)

type stepMachine struct {
	*providertest.Fake
	home  string
	calls int
	link  func(call int, run *exec.Cmd, stdout *bytes.Buffer) providers.Result
}

func newStepMachine(t *testing.T, link func(call int, run *exec.Cmd, stdout *bytes.Buffer) providers.Result) *stepMachine {
	redial := stepRedial
	stepRedial = time.Millisecond
	t.Cleanup(func() { stepRedial = redial })
	return &stepMachine{Fake: &providertest.Fake{Facts: providers.Traits{Supervisor: providers.SupervisorSetsid}}, home: t.TempDir(), link: link}
}

func (m *stepMachine) Exec(ctx context.Context, _ string, cmd []string, stdin io.Reader) (providers.Result, error) {
	m.calls++
	run := exec.Command(cmd[0], cmd[1:]...)
	run.Env = []string{"HOME=" + m.home, "PATH=" + os.Getenv("PATH")}
	run.Stdin = stdin
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout bytes.Buffer
	run.Stdout, run.Stderr = &stdout, io.Discard
	if err := run.Start(); err != nil {
		return providers.Result{}, err
	}
	return m.link(m.calls, run, &stdout), ctx.Err()
}

func hangUp(run *exec.Cmd) {
	_ = syscall.Kill(-run.Process.Pid, syscall.SIGKILL)
	_ = run.Wait()
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s never appeared", path)
}

func TestAStepReplaysItsWholeOutputAndExitStatusAcrossDroppedConnections(t *testing.T) {
	cuts := []int{0, 3, 5, 9}
	var m *stepMachine
	m = newStepMachine(t, func(call int, run *exec.Cmd, stdout *bytes.Buffer) providers.Result {
		if call == 1 {
			waitForFile(t, filepath.Join(m.home, "runs"))
			hangUp(run)
			if err := os.WriteFile(filepath.Join(m.home, "resume"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return providers.Result{Stdout: stdout.Bytes(), ExitCode: 255}
		}
		_ = run.Wait()
		if call-2 < len(cuts) {
			return providers.Result{Stdout: stdout.Bytes()[:min(stdout.Len(), cuts[call-2])], ExitCode: 255}
		}
		return providers.Result{Stdout: stdout.Bytes()}
	})
	var out bytes.Buffer
	step := `cat; echo ran >> "$HOME/runs"; until test -e "$HOME/resume"; do sleep 0.01; done; echo finished; exit 3`
	err := runStep(t.Context(), m, "m", []string{"sh", "-c", step}, []byte("started\n"), &out)
	if exited, ok := errors.AsType[*CommandExit](err); !ok || exited.Code != 3 {
		t.Fatalf("runStep = %v, want exit status 3", err)
	}
	if out.String() != "started\nfinished\n" {
		t.Fatalf("output = %q, want the whole output once", out.String())
	}
	if runs, err := os.ReadFile(filepath.Join(m.home, "runs")); err != nil || string(runs) != "ran\n" {
		t.Fatalf("runs = %q, %v, want the step to run once", runs, err)
	}
	if want := 2 + len(cuts); m.calls != want {
		t.Fatalf("calls = %d, want %d", m.calls, want)
	}
}

func TestAnInterruptedStepStopsItsCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var m *stepMachine
	m = newStepMachine(t, func(call int, run *exec.Cmd, stdout *bytes.Buffer) providers.Result {
		if call == 1 {
			waitForFile(t, filepath.Join(m.home, "started"))
			cancel()
			hangUp(run)
			return providers.Result{ExitCode: 255}
		}
		_ = run.Wait()
		return providers.Result{Stdout: stdout.Bytes()}
	})
	step := `trap 'touch "$HOME/stopped"; exit 143' TERM; touch "$HOME/started"; while :; do sleep 0.01; done`
	if err := runStep(ctx, m, "m", []string{"sh", "-c", step}, nil, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("runStep = %v, want context.Canceled", err)
	}
	waitForFile(t, filepath.Join(m.home, "stopped"))
}
