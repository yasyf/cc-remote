package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	link  func(call int, run *exec.Cmd, stdout, stderr *bytes.Buffer) providers.Result
}

func newStepMachine(t *testing.T, link func(call int, run *exec.Cmd, stdout, stderr *bytes.Buffer) providers.Result) *stepMachine {
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
	var stdout, stderr bytes.Buffer
	run.Stdout, run.Stderr = &stdout, &stderr
	if err := run.Start(); err != nil {
		return providers.Result{}, err
	}
	return m.link(m.calls, run, &stdout, &stderr), ctx.Err()
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

func TestAStepStreamsItsWholeOutputAndExitStatusAcrossDroppedConnections(t *testing.T) {
	cuts := []int{0, 3, 5, 9}
	var m *stepMachine
	m = newStepMachine(t, func(call int, run *exec.Cmd, stdout, stderr *bytes.Buffer) providers.Result {
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
		return providers.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	})
	var out, notes bytes.Buffer
	step := `cat; echo ran >> "$HOME/runs"; until test -e "$HOME/resume"; do sleep 0.01; done; echo finished; exit 3`
	err := runStep(t.Context(), m, "m", "s1", 0, []string{"sh", "-c", step}, []byte("started\n"), &out, &notes)
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
	if notes.String() != "cc-remote: step s1 on m\n" {
		t.Fatalf("notes = %q, want the step's id", notes.String())
	}
	entries, err := os.ReadDir(filepath.Join(m.home, ".cc-remote", "steps", "s1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if saved, _ := os.ReadFile(filepath.Join(m.home, ".cc-remote", "steps", "s1", entry.Name())); bytes.Contains(saved, []byte("started")) && entry.Name() != "output" {
			t.Fatalf("%s holds the forwarded stdin; it must stay in memory", entry.Name())
		}
	}
	if _, err := os.Stat(stepMemory + "/steps/s1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stdin held in memory outlived the command's open: %v", err)
	}

	var attached bytes.Buffer
	err = runStep(t.Context(), m, "m", "s1", len("started\n"), nil, nil, &attached, io.Discard)
	if exited, ok := errors.AsType[*CommandExit](err); !ok || exited.Code != 3 || attached.String() != "finished\n" {
		t.Fatalf("attach = %q, %v; want the output after the offset and exit status 3", attached.String(), err)
	}
}

func TestAttachingAStepThatNeverStartedFails(t *testing.T) {
	m := newStepMachine(t, func(_ int, run *exec.Cmd, stdout, stderr *bytes.Buffer) providers.Result {
		_ = run.Wait()
		return providers.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: run.ProcessState.ExitCode()}
	})
	err := runStep(t.Context(), m, "m", "nothing", 0, nil, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no step nothing started") {
		t.Fatalf("runStep = %v, want the missing step named", err)
	}
}

func TestAnInterruptedStepStopsItsCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var m *stepMachine
	m = newStepMachine(t, func(call int, run *exec.Cmd, stdout, _ *bytes.Buffer) providers.Result {
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
	if err := runStep(ctx, m, "m", "s2", 0, []string{"sh", "-c", step}, nil, io.Discard, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("runStep = %v, want context.Canceled", err)
	}
	waitForFile(t, filepath.Join(m.home, "stopped"))
}
