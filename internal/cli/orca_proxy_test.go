package cli

import (
	"bufio"
	"context"
	"errors"
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

type echoTunnels struct {
	*providertest.Fake
	upstream net.Listener
	mu       sync.Mutex
	opened   []string
	dialed   []string
}

type echoTunnel struct {
	owner  *echoTunnels
	done   chan struct{}
	closed sync.Once
}

func newEchoTunnels(t *testing.T) *echoTunnels {
	t.Helper()
	upstream, err := net.Listen("tcp", net.JoinHostPort(orca.Loopback, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = upstream.Close() })
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			go echo(conn)
		}
	}()
	return &echoTunnels{Fake: &providertest.Fake{}, upstream: upstream}
}

func echo(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	lines := bufio.NewReader(conn)
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			return
		}
		if _, err := io.WriteString(conn, line); err != nil || line == "bye\n" {
			return
		}
	}
}

func (e *echoTunnels) Tunnel(_ context.Context, id string) (providers.Tunnel, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opened = append(e.opened, id)
	return &echoTunnel{owner: e, done: make(chan struct{})}, nil
}

func (e *echoTunnels) calls() ([]string, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.opened), slices.Clone(e.dialed)
}

func (t *echoTunnel) Dial(network, address string) (net.Conn, error) {
	t.owner.mu.Lock()
	t.owner.dialed = append(t.owner.dialed, network+" "+address)
	t.owner.mu.Unlock()
	return net.Dial("tcp", t.owner.upstream.Addr().String())
}

func (t *echoTunnel) Wait() error {
	<-t.done
	return nil
}

func (t *echoTunnel) Close() error {
	t.closed.Do(func() { close(t.done) })
	return nil
}

func fastProxies(t *testing.T) {
	t.Helper()
	every, retry := proxyEvery, proxyRetry
	proxyEvery, proxyRetry = 10*time.Millisecond, 0
	t.Cleanup(func() { proxyEvery, proxyRetry = every, retry })
}

func runProxies(t *testing.T, dir state.Dir, tunnels *echoTunnels) {
	t.Helper()
	held := newProxies(dir, slog.New(slog.DiscardHandler), func(task *orcaTask) (providers.Provider, error) {
		if task.Provider != sprites.Name {
			return nil, fmt.Errorf("opened a %s provider", task.Provider)
		}
		return tunnels, nil
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
	if forward == nil {
		control, err := state.NewOrcaControl()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(control)) })
		forward = &orcaTunnel{Host: name, Config: filepath.Join(t.TempDir(), "absent-ssh-config"), Control: control, Log: dir.OrcaForwardLog(name)}
	}
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

func echoes(t *testing.T, port int) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort(orca.Loopback, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial the held port: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "hello\n"); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(conn)
	if line, err := lines.ReadString('\n'); err != nil || line != "hello\n" {
		t.Fatalf("the held port answered %q, %v; want the Sprite's echo", line, err)
	}
	return conn, lines
}

func TestEnsureHasTheKeeperHoldASpriteTasksPortAndReleaseClosesItAtOnce(t *testing.T) {
	fastProxies(t)
	starts := standInKeeper(t)
	dir := state.Dir(t.TempDir())
	tunnels := newEchoTunnels(t)
	runProxies(t, dir, tunnels)
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
	closing, lines := echoes(t, task.Port)
	if _, err := io.WriteString(closing, "bye\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := lines.ReadString('\n'); err != nil || line != "bye\n" {
		t.Fatalf("the held port answered %q, %v; want the Sprite's last line", line, err)
	}
	if rest, err := lines.ReadString('\n'); !errors.Is(err, io.EOF) {
		t.Errorf("after the Sprite closed its side, the local connection read %q, %v; want EOF", rest, err)
	}
	conn, _ := echoes(t, task.Port)
	address := net.JoinHostPort(orca.Loopback, strconv.Itoa(task.Port))
	if opened, dialed := tunnels.calls(); !slices.Equal(opened, []string{"lane-a"}) || !slices.Equal(dialed, []string{"tcp " + address, "tcp " + address}) {
		t.Errorf("opened tunnels %q and dialed %q; want one tunnel to lane-a carrying both connections to %s", opened, dialed, address)
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
	if _, err := conn.Read(make([]byte, 1)); err == nil || os.IsTimeout(err) {
		t.Errorf("a connection open through the released forward read %v, want it closed", err)
	}
	if conn, err := net.Dial("tcp", address); err == nil {
		_ = conn.Close()
		t.Error("the released port still accepts connections")
	}
}

func TestReleaseLeavesAKeptSpriteTasksForwardHeld(t *testing.T) {
	fastProxies(t)
	standInKeeper(t)
	dir := state.Dir(t.TempDir())
	runProxies(t, dir, newEchoTunnels(t))
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
	runProxies(t, dir, newEchoTunnels(t))
	recorded := &orcaTunnel{Host: "lane-a", Config: "/recorded/ssh", Control: control, Log: dir.OrcaForwardLog("lane-a")}
	task := spriteTask(t, dir, "lane-a", recorded)

	if err := state.Write(dir.OrcaKept("lane-a"), []byte("/recorded/config.yaml")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !task.up(t.Context()) {
		if time.Now().After(deadline) {
			t.Fatal("the keeper never held the migrated task's port")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if loaded, err := loadTask(dir, "lane-a"); err != nil || loaded.Forward == nil || *loaded.Forward != *recorded {
		t.Errorf("after the migration the task records %+v, %v; want its SSH forward kept for a keeper of an earlier release", loaded, err)
	}
	echoes(t, task.Port)
	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	ssh := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{strings.Join(recorded.args("-O", "check"), " "), strings.Join(recorded.args("-O", "exit"), " ")}
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

func TestEnsureAcceptsTheSSHForwardAKeeperOfAnEarlierReleaseOpened(t *testing.T) {
	fastProxies(t)
	starts := standInKeeper(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	dir := state.Dir(t.TempDir())
	task := spriteTask(t, dir, "lane-a", nil)
	if err := task.ensure(t.Context(), "/recorded/config.yaml"); err != nil || starts.Load() != 1 {
		t.Errorf("ensure beside an older keeper's live SSH forward = %v with %d keepers started; want it accepted after starting one", err, starts.Load())
	}
}
