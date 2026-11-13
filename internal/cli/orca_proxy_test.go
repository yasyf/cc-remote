package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/providers/sprites"
	"github.com/yasyf/cc-remote/internal/state"
	"github.com/yasyf/cc-remote/internal/workspace"
)

type echoDialer struct {
	*providertest.Fake
	mu    sync.Mutex
	dials []string
}

func (d *echoDialer) Dial(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	d.dials = append(d.dials, id+":"+strconv.Itoa(port))
	d.mu.Unlock()
	near, far := net.Pipe()
	go func() { _, _ = io.Copy(far, far) }()
	context.AfterFunc(ctx, func() { _ = near.Close() })
	return near, nil
}

func (d *echoDialer) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.dials)
}

func fastProxies(t *testing.T) {
	t.Helper()
	every, retry := proxyEvery, proxyRetry
	proxyEvery, proxyRetry = 10*time.Millisecond, 0
	t.Cleanup(func() { proxyEvery, proxyRetry = every, retry })
}

func runProxies(t *testing.T, dir state.Dir, dialer *echoDialer) {
	t.Helper()
	held := newProxies(dir, slog.New(slog.DiscardHandler), func(task *orcaTask) (providers.Provider, error) {
		if task.Provider != sprites.Name {
			return nil, fmt.Errorf("opened a %s provider", task.Provider)
		}
		return dialer, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		held.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func spriteTask(t *testing.T, dir state.Dir, name string, forward *orcaTunnel) *orcaTask {
	t.Helper()
	port, err := workspace.FreePort()
	if err != nil {
		t.Fatal(err)
	}
	task := &orcaTask{SchemaVersion: orcaTaskSchema, Workspace: name, Provider: sprites.Name, Profile: "lean", Machine: name, Environment: name, Port: port, Forward: forward, dir: dir}
	if err := state.Save(dir.Orca(name), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func echoes(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort(orca.Loopback, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial the held port: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "hello\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("the held port answered %q, %v; want the Sprite's echo", line, err)
	}
	return conn
}

func TestEnsureHasTheKeeperHoldASpriteTasksPortAndReleaseClosesItAtOnce(t *testing.T) {
	fastProxies(t)
	starts := standInKeeper(t)
	dir := state.Dir(t.TempDir())
	dialer := &echoDialer{Fake: &providertest.Fake{}}
	runProxies(t, dir, dialer)
	task := spriteTask(t, dir, "lane-a", nil)

	if err := task.ensure(t.Context(), "/recorded/config.yaml"); err != nil {
		t.Fatalf("ensure = %v", err)
	}
	if kept, err := os.ReadFile(dir.OrcaKept("lane-a")); err != nil || string(kept) != "/recorded/config.yaml" || starts.Load() != 1 {
		t.Errorf("ensure kept %q, %v, and started %d keepers; want the config kept and one keeper started", kept, err, starts.Load())
	}
	if !task.up(t.Context()) {
		t.Fatal("the task's forward is down after ensure")
	}
	conn := echoes(t, task.Port)
	if got, want := dialer.dialed(), []string{"lane-a:" + strconv.Itoa(task.Port)}; !slices.Equal(got, want) {
		t.Errorf("dialed %q, want %q", got, want)
	}

	if err := state.Remove(dir.OrcaKept("lane-a")); err != nil {
		t.Fatal(err)
	}
	if err := task.release(t.Context()); err != nil {
		t.Fatalf("release = %v", err)
	}
	if task.up(t.Context()) {
		t.Error("the task's forward is up after its release")
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Errorf("a connection open through the released forward read %v, want it closed", err)
	}
	if conn, err := net.Dial("tcp", net.JoinHostPort(orca.Loopback, strconv.Itoa(task.Port))); err == nil {
		_ = conn.Close()
		t.Error("the released port still accepts connections")
	}
}

func TestReleaseLeavesAKeptSpriteTasksForwardHeld(t *testing.T) {
	fastProxies(t)
	standInKeeper(t)
	dir := state.Dir(t.TempDir())
	runProxies(t, dir, &echoDialer{Fake: &providertest.Fake{}})
	task := spriteTask(t, dir, "lane-a", nil)
	if err := task.ensure(t.Context(), "/recorded/config.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := task.release(t.Context()); err != nil || !task.up(t.Context()) {
		t.Errorf("release of a kept task = %v, up %t; want it left held", err, task.up(t.Context()))
	}
}

func TestKeepClosesASpriteTasksRecordedSSHForwardAndHoldsItsPort(t *testing.T) {
	fastProxies(t)
	standInKeeper(t)
	bin, calls := t.TempDir(), filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	control, err := state.NewOrcaControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(control)) })
	if err := os.WriteFile(filepath.Join(filepath.Dir(control), "master"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := state.Dir(t.TempDir())
	runProxies(t, dir, &echoDialer{Fake: &providertest.Fake{}})
	task := spriteTask(t, dir, "lane-a", &orcaTunnel{Host: "lane-a", Config: "/recorded/ssh", Control: control, Log: dir.OrcaForwardLog("lane-a")})

	if err := task.ensure(t.Context(), "/recorded/config.yaml"); err != nil {
		t.Fatalf("ensure = %v", err)
	}
	if task.Forward != nil {
		t.Errorf("ensure left the SSH forward recorded: %+v", task.Forward)
	}
	echoes(t, task.Port)
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	ssh := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{
		strings.Join(orcaTunnel{Config: "/recorded/ssh", Control: control, Host: "lane-a"}.args("-O", "check"), " "),
		strings.Join(orcaTunnel{Config: "/recorded/ssh", Control: control, Host: "lane-a"}.args("-O", "exit"), " "),
	}
	if !slices.Equal(ssh, want) {
		t.Errorf("ssh ran %q, want the recorded master checked and closed", ssh)
	}
}

func TestRunsOnASpriteTaskGoThroughTheProviderNotSSH(t *testing.T) {
	provider := &providertest.Fake{Handle: func(_ string, cmd []string, _ []byte) providers.Result {
		return providers.Result{Stdout: []byte(strings.Join(cmd, " "))}
	}}
	if _, err := provider.Create(t.Context(), providers.Spec{Name: "lane-a", Profile: "lean"}); err != nil {
		t.Fatal(err)
	}
	task := &orcaTask{Workspace: "lane-a", Provider: sprites.Name, Machine: "lane-a", provider: provider}
	out, err := task.run(t.Context(), "echo hi", nil)
	if err != nil || string(out) != "sh -c echo hi" {
		t.Errorf("run = %q, %v; want the script run through the provider's Exec", out, err)
	}
}
