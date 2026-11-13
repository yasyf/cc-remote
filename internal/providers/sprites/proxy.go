package sprites

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	proxyHost    = "127.0.0.1"
	proxyPing    = 20 * time.Second
	proxyClosing = time.Second
	proxyAnswer  = 2 * time.Minute
	proxyBuffer  = 256 << 10
)

var proxyDialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: time.Minute,
	ReadBufferSize:   proxyBuffer,
	WriteBufferSize:  proxyBuffer,
}

type proxyInit struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type proxyReply struct {
	Status string `json:"status"`
	Target string `json:"target"`
}

type socket struct {
	conn    *websocket.Conn
	reader  io.Reader
	writing sync.Mutex
	done    chan struct{}
	closed  sync.Once
}

type piped struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	cancel context.CancelFunc
	closed sync.Once
}

func (p *Provider) dial(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	if providers.CheckName(id, nameLimit) != nil {
		return nil, fmt.Errorf("sprite %q: %w", id, providers.ErrNotFound)
	}
	token := p.token()
	if token == "" {
		return p.dialCLI(ctx, id, port)
	}
	return p.dialSocket(ctx, id, port, token)
}

func (p *Provider) dialSocket(ctx context.Context, id string, port int, token string) (io.ReadWriteCloser, error) {
	target := "ws" + strings.TrimPrefix(p.base, "http") + "/v1/sprites/" + id + "/proxy"
	conn, response, err := dialCancelable(ctx, target, http.Header{"Authorization": {"Bearer " + token}})
	if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("sprite %s: %w", id, providers.ErrNotFound)
		}
		return nil, fmt.Errorf("%w opening the proxy to port %d on %s: %s", &StatusError{Status: response.StatusCode}, port, id, bytes.TrimSpace(body))
	}
	if err != nil {
		return nil, fmt.Errorf("opening the proxy to port %d on sprite %s: %w", port, id, err)
	}
	s := &socket{conn: conn, done: make(chan struct{})}
	go s.ping(ctx)
	if err := s.handshake(port); err != nil {
		return nil, errors.Join(fmt.Errorf("opening the proxy to port %d on sprite %s: %w", port, id, err), s.Close())
	}
	return s, nil
}

func (s *socket) handshake(port int) error {
	if err := s.conn.WriteJSON(proxyInit{Host: proxyHost, Port: port}); err != nil {
		return err
	}
	if err := s.conn.SetReadDeadline(time.Now().Add(proxyAnswer)); err != nil {
		return err
	}
	var reply proxyReply
	if err := s.conn.ReadJSON(&reply); err != nil {
		return err
	}
	if reply.Status != "connected" {
		return fmt.Errorf("the proxy answered %q, not connected", reply.Status)
	}
	return s.conn.SetReadDeadline(time.Time{})
}

func (s *socket) ping(ctx context.Context) {
	ticker := time.NewTicker(proxyPing)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			_ = s.Close()
			return
		case <-ticker.C:
		}
		if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(proxyPing)); err != nil {
			_ = s.Close()
			return
		}
	}
}

func (s *socket) Read(b []byte) (int, error) {
	for {
		if s.reader != nil {
			n, err := s.reader.Read(b)
			if errors.Is(err, io.EOF) {
				s.reader = nil
				if n == 0 {
					continue
				}
				err = nil
			}
			return n, err
		}
		kind, reader, err := s.conn.NextReader()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		if kind == websocket.BinaryMessage {
			s.reader = reader
		}
	}
}

func (s *socket) Write(b []byte) (int, error) {
	s.writing.Lock()
	defer s.writing.Unlock()
	if err := s.conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (s *socket) Close() error {
	var err error
	s.closed.Do(func() {
		close(s.done)
		_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(proxyClosing))
		err = s.conn.Close()
	})
	return err
}

func (p *Provider) dialCLI(ctx context.Context, id string, port int) (io.ReadWriteCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, p.CLI, "proxy", "-o", p.Org, "-s", id, "-W", ":"+strconv.Itoa(port))
	cmd.WaitDelay = proxyClosing
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("opening the proxy to port %d on sprite %s: %w", port, id, err)
	}
	return &piped{cmd: cmd, stdin: stdin, stdout: stdout, cancel: cancel}, nil
}

func (c *piped) Read(b []byte) (int, error) { return c.stdout.Read(b) }

func (c *piped) Write(b []byte) (int, error) { return c.stdin.Write(b) }

func (c *piped) Close() error {
	c.closed.Do(func() {
		_ = c.stdin.Close()
		c.cancel()
		_ = c.cmd.Wait()
	})
	return nil
}

func dialCancelable(ctx context.Context, target string, header http.Header) (*websocket.Conn, *http.Response, error) {
	var mu sync.Mutex
	var raw []net.Conn
	dialer := *proxyDialer
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			mu.Lock()
			raw = append(raw, conn)
			mu.Unlock()
		}
		return conn, err
	}
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range raw {
			_ = conn.Close()
		}
	})
	defer stop()
	return dialer.DialContext(ctx, target, header)
}
