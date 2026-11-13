package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/state"
)

var (
	proxyEvery   = time.Second
	proxyRetry   = 10 * time.Second
	proxyRelease = 10 * time.Second
)

type taskProvider func(task *orcaTask) (providers.Provider, error)

type proxies struct {
	dir  state.Dir
	log  *slog.Logger
	open taskProvider

	held    map[string]*heldProxy
	skipped map[string]bool
	tried   map[string]time.Time

	mu     sync.Mutex
	failed map[string]error
	logged map[string]string
}

type heldProxy struct {
	task     *orcaTask
	listener net.Listener
	unlock   func()
	cancel   context.CancelFunc
	bridges  sync.WaitGroup
	serving  chan struct{}
	tunneler providers.Tunneler

	dialing sync.Mutex
	tunnel  providers.Tunnel
}

func newProxies(dir state.Dir, log *slog.Logger, open taskProvider) *proxies {
	return &proxies{
		dir: dir, log: log, open: open,
		held: map[string]*heldProxy{}, skipped: map[string]bool{}, tried: map[string]time.Time{},
		failed: map[string]error{}, logged: map[string]string{},
	}
}

func (t *orcaTask) hold(ctx context.Context, config string) error {
	if !t.up(ctx) {
		if err := state.Write(t.dir.OrcaKept(t.Workspace), []byte(config)); err != nil {
			return err
		}
		if err := spawnKeeper(t.dir, config); err != nil {
			return fmt.Errorf("start the keeper that holds %s:%d for %s: %w", orca.Loopback, t.Port, t.Workspace, err)
		}
		deadline := time.After(gatewayStart)
		for !t.up(ctx) && !t.Forward.up(ctx) {
			select {
			case <-deadline:
				return fmt.Errorf("orca keep did not hold %s:%d for %s within %s (see %s)", orca.Loopback, t.Port, t.Workspace, gatewayStart, t.dir.OrcaKeeperLog())
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(proxyEvery):
			}
		}
	}
	return nil
}

func (t *orcaTask) release(ctx context.Context) error {
	if t.Forward != nil {
		if err := retire(ctx, *t.Forward); err != nil {
			return err
		}
	}
	if _, err := os.Stat(t.dir.OrcaKept(t.Workspace)); !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	deadline := time.After(proxyRelease)
	for t.up(ctx) {
		select {
		case <-deadline:
			return fmt.Errorf("orca keep still holds %s:%d for %s %s after its task was released (see %s)", orca.Loopback, t.Port, t.Workspace, proxyRelease, t.dir.OrcaKeeperLog())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gatewayPoll):
		}
	}
	return nil
}

func retire(ctx context.Context, tunnel orcaTunnel) error {
	masters, err := os.ReadDir(filepath.Dir(tunnel.Control))
	if errors.Is(err, fs.ErrNotExist) || err == nil && len(masters) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	return tunnel.close(ctx)
}

func (p *proxies) run(ctx context.Context) {
	ticker := time.NewTicker(proxyEvery)
	defer ticker.Stop()
	for {
		p.sync(ctx)
		select {
		case <-ctx.Done():
			for name := range p.held {
				p.drop(name)
			}
			return
		case <-ticker.C:
		}
	}
}

func (p *proxies) sync(ctx context.Context) {
	names, err := keptNames(p.dir)
	if err != nil {
		p.log.Warn("listing the kept tasks failed, and the next second lists them again", "error", err)
		return
	}
	kept := map[string]bool{}
	for _, name := range names {
		kept[name] = true
	}
	for name, held := range p.held {
		select {
		case <-held.serving:
			p.drop(name)
			p.tried[name] = time.Now()
			continue
		default:
		}
		if !kept[name] {
			p.drop(name)
			p.log.Info("closed the forward of a released task", "workspace", name)
		}
	}
	for name := range p.skipped {
		if !kept[name] {
			delete(p.skipped, name)
		}
	}
	for _, name := range names {
		if p.held[name] != nil || p.skipped[name] || time.Since(p.tried[name]) < proxyRetry {
			continue
		}
		p.tried[name] = time.Now()
		task, err := loadTask(p.dir, name)
		if err != nil {
			p.fail(name, err)
			continue
		}
		if !task.proxied() {
			p.skipped[name] = true
			continue
		}
		checking, cancel := context.WithTimeout(ctx, answerWithin)
		recorded := task.Forward != nil && task.Forward.up(checking)
		cancel()
		if recorded {
			continue
		}
		if err := p.hold(ctx, task); err != nil {
			p.fail(name, err)
			continue
		}
		delete(p.tried, name)
		p.log.Info("holding a kept forward", "workspace", name, "port", task.Port)
	}
}

func (p *proxies) hold(ctx context.Context, task *orcaTask) error {
	provider, err := p.open(task)
	if err != nil {
		return err
	}
	tunneler, ok := provider.(providers.Tunneler)
	if !ok {
		return fmt.Errorf("provider %s opens no tunnel to a machine", task.Provider)
	}
	address := net.JoinHostPort(orca.Loopback, strconv.Itoa(task.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("hold %s: %w; the recorded port stays and no other process is stopped", address, err)
	}
	lock := p.dir.OrcaProxyLock(task.Workspace)
	unlock, free, err := state.TryLock(lock)
	if err == nil && !free {
		err = fmt.Errorf("another process holds %s", lock)
	}
	if err != nil {
		return errors.Join(err, listener.Close())
	}
	ctx, cancel := context.WithCancel(ctx)
	held := &heldProxy{task: task, listener: listener, unlock: unlock, cancel: cancel, serving: make(chan struct{}), tunneler: tunneler}
	p.held[task.Workspace] = held
	p.succeed(task.Workspace)
	go p.serve(ctx, held)
	return nil
}

func (p *proxies) drop(name string) {
	held := p.held[name]
	delete(p.held, name)
	held.cancel()
	_ = held.listener.Close()
	<-held.serving
	held.close()
	held.bridges.Wait()
	held.unlock()
}

func (p *proxies) serve(ctx context.Context, held *heldProxy) {
	defer close(held.serving)
	for {
		local, err := held.listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				p.fail(held.task.Workspace, fmt.Errorf("accepting on %s: %w", held.listener.Addr(), err))
			}
			return
		}
		held.bridges.Go(func() { p.bridge(ctx, held, local) })
	}
}

func (p *proxies) bridge(ctx context.Context, held *heldProxy, local net.Conn) {
	defer func() { _ = local.Close() }()
	task := held.task
	remote, err := held.dial(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.fail(task.Workspace, fmt.Errorf("reaching port %d on %s: %w", task.Port, task.Machine, err))
		}
		return
	}
	p.succeed(task.Workspace)
	defer func() { _ = remote.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = local.Close() })
	defer stop()
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_, _ = io.Copy(remote, local)
		_ = closeWrite(remote)
	}()
	_, _ = io.Copy(local, remote)
	_ = closeWrite(local)
	<-sent
}

func (h *heldProxy) dial(ctx context.Context) (net.Conn, error) {
	h.dialing.Lock()
	defer h.dialing.Unlock()
	if h.tunnel == nil {
		tunnel, err := h.tunneler.Tunnel(ctx, h.task.Machine)
		if err != nil {
			return nil, err
		}
		h.tunnel = tunnel
		go h.forget(tunnel)
	}
	conn, err := h.tunnel.Dial("tcp", net.JoinHostPort(orca.Loopback, strconv.Itoa(h.task.Port)))
	var refused *ssh.OpenChannelError
	if err != nil && !errors.As(err, &refused) {
		_ = h.tunnel.Close()
		h.tunnel = nil
	}
	return conn, err
}

func (h *heldProxy) forget(tunnel providers.Tunnel) {
	_ = tunnel.Wait()
	h.dialing.Lock()
	defer h.dialing.Unlock()
	if h.tunnel == tunnel {
		h.tunnel = nil
	}
}

func (h *heldProxy) close() {
	h.dialing.Lock()
	defer h.dialing.Unlock()
	if h.tunnel != nil {
		_ = h.tunnel.Close()
		h.tunnel = nil
	}
}

func closeWrite(conn net.Conn) error {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return conn.Close()
}

func (p *proxies) fail(name string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed[name] = err
	if p.logged[name] == err.Error() {
		return
	}
	p.logged[name] = err.Error()
	p.log.Warn("a kept forward failed, and keep tries it again", "workspace", name, "error", err)
}

func (p *proxies) succeed(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.failed, name)
	delete(p.logged, name)
}

func (p *proxies) failure(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failed[name]
}
