package sprites

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/yasyf/cc-remote/internal/providers"
)

type proxyServer struct {
	t     *testing.T
	token string
	serve func(net.Conn)
	mu    sync.Mutex
	asked []proxyInit
}

func (s *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/sprites/alpha/proxy" {
		http.Error(w, "sprite not found", http.StatusNotFound)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		s.t.Error(err)
		return
	}
	defer func() { _ = conn.Close() }()
	var init proxyInit
	if err := conn.ReadJSON(&init); err != nil {
		s.t.Error(err)
		return
	}
	s.mu.Lock()
	s.asked = append(s.asked, init)
	s.mu.Unlock()
	if err := conn.WriteJSON(proxyReply{Status: "connected", Target: "10.0.0.1:" + strconv.Itoa(init.Port)}); err != nil {
		s.t.Error(err)
		return
	}
	near, far := net.Pipe()
	go s.serve(far)
	go func() {
		defer func() { _ = near.Close() }()
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				if _, err := near.Write(data); err != nil {
					return
				}
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := near.Read(buf)
		if err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			return
		}
	}
}

func (s *proxyServer) ports() []proxyInit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]proxyInit(nil), s.asked...)
}

func echoConn(conn net.Conn) {
	_, _ = io.Copy(conn, conn)
	_ = conn.Close()
}

func proxied(t *testing.T, p *Provider, serve func(net.Conn)) *proxyServer {
	t.Helper()
	proxy := &proxyServer{t: t, token: "test-token", serve: serve}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	p.base = server.URL
	return proxy
}

func TestDialCarriesAPortOverTheProxySocket(t *testing.T) {
	p, _ := newProvider(t)
	proxy := proxied(t, p, echoConn)

	conn, err := p.dial(t.Context(), "alpha", 7001)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "hello\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "hello\n" {
		t.Errorf("the proxied port answered %q, %v; want the echo", line, err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if asked := proxy.ports(); len(asked) != 1 || asked[0] != (proxyInit{Host: proxyHost, Port: 7001}) {
		t.Errorf("the proxy was asked for %+v, want 127.0.0.1:7001 once", asked)
	}

	if _, err := p.dial(t.Context(), "gone", 7001); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("dial of a missing sprite = %v, want ErrNotFound", err)
	}
	p.token = func() string { return "stale" }
	var answered *StatusError
	if _, err := p.dial(t.Context(), "alpha", 7001); !errors.As(err, &answered) || answered.Status != http.StatusUnauthorized {
		t.Errorf("dial with a rejected token = %v, want a 401 StatusError", err)
	}
}

type sshServer struct {
	t       *testing.T
	config  *ssh.ServerConfig
	mu      sync.Mutex
	targets []string
}

func newSSHServer(t *testing.T, p *Provider, id string) *sshServer {
	t.Helper()
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ssh.NewSignerFromKey(hostKey)
	if err != nil {
		t.Fatal(err)
	}
	clientPublic, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(clientKey, "cc-remote "+id)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := ssh.NewPublicKey(clientPublic)
	if err != nil {
		t.Fatal(err)
	}
	keys := p.keys()
	if err := os.MkdirAll(keys.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keys.identity(id), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keys.knownHosts(id), []byte(Alias(id)+" "+string(ssh.MarshalAuthorizedKey(host.PublicKey()))), 0o600); err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != user || !bytes.Equal(key.Marshal(), authorized.Marshal()) {
			return nil, errors.New("unauthorized")
		}
		return &ssh.Permissions{}, nil
	}}
	config.AddHostKey(host)
	return &sshServer{t: t, config: config}
}

func (s *sshServer) serve(conn net.Conn) {
	_, channels, requests, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		s.t.Error(err)
		return
	}
	go ssh.DiscardRequests(requests)
	for opened := range channels {
		var target struct {
			Host       string
			Port       uint32
			OriginHost string
			OriginPort uint32
		}
		if opened.ChannelType() != "direct-tcpip" || ssh.Unmarshal(opened.ExtraData(), &target) != nil {
			_ = opened.Reject(ssh.Prohibited, "only direct-tcpip")
			continue
		}
		s.mu.Lock()
		s.targets = append(s.targets, net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port))))
		s.mu.Unlock()
		channel, requests, err := opened.Accept()
		if err != nil {
			s.t.Error(err)
			continue
		}
		go ssh.DiscardRequests(requests)
		go func() {
			lines := bufio.NewReader(channel)
			line, _ := lines.ReadString('\n')
			_, _ = io.WriteString(channel, line)
			_ = channel.CloseWrite()
			_, _ = io.Copy(io.Discard, lines)
			_ = channel.Close()
		}()
	}
}

func TestTunnelCarriesConnectionsOverSSHToTheSpritesPortsAndEndsThem(t *testing.T) {
	p, _ := newProvider(t)
	server := newSSHServer(t, p, "alpha")
	proxy := proxied(t, p, server.serve)

	tunnel, err := p.Tunnel(t.Context(), "alpha")
	if err != nil {
		t.Fatalf("Tunnel = %v", err)
	}
	t.Cleanup(func() { _ = tunnel.Close() })
	for range 2 {
		conn, err := tunnel.Dial("tcp", "127.0.0.1:7001")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, "hello\n"); err != nil {
			t.Fatal(err)
		}
		answer, err := io.ReadAll(conn)
		if err != nil || string(answer) != "hello\n" {
			t.Errorf("the tunneled port answered %q, %v; want the line and then the end the Sprite sent", answer, err)
		}
		_ = conn.Close()
	}
	if asked := proxy.ports(); len(asked) != 1 || asked[0] != (proxyInit{Host: proxyHost, Port: sshPort}) {
		t.Errorf("the proxy was asked for %+v, want one connection to port 22", asked)
	}
	server.mu.Lock()
	targets := append([]string(nil), server.targets...)
	server.mu.Unlock()
	if len(targets) != 2 || targets[0] != "127.0.0.1:7001" || targets[1] != "127.0.0.1:7001" {
		t.Errorf("the Sprite's sshd was asked for %q, want 127.0.0.1:7001 twice over one connection", targets)
	}
}
