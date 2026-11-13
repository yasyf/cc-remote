package sprites

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/yasyf/cc-remote/internal/providers"
)

type proxyServer struct {
	t     *testing.T
	token string
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
	if err := conn.WriteJSON(proxyReply{Status: "connected", Target: "127.0.0.1:7001"}); err != nil {
		s.t.Error(err)
		return
	}
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := conn.WriteMessage(kind, data); err != nil {
			return
		}
	}
}

func TestDialBridgesAPortOverTheProxySocket(t *testing.T) {
	p, _ := newProvider(t)
	proxy := &proxyServer{t: t, token: "test-token"}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	p.base = server.URL

	conn, err := p.Dial(t.Context(), "alpha", 7001)
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
	proxy.mu.Lock()
	asked := proxy.asked
	proxy.mu.Unlock()
	if len(asked) != 1 || asked[0] != (proxyInit{Host: proxyHost, Port: 7001}) {
		t.Errorf("the proxy was asked for %+v, want 127.0.0.1:7001 once", asked)
	}

	if _, err := p.Dial(t.Context(), "gone", 7001); !errors.Is(err, providers.ErrNotFound) {
		t.Errorf("Dial of a missing sprite = %v, want ErrNotFound", err)
	}
	p.token = func() string { return "stale" }
	var answered *StatusError
	if _, err := p.Dial(t.Context(), "alpha", 7001); !errors.As(err, &answered) || answered.Status != http.StatusUnauthorized {
		t.Errorf("Dial with a rejected token = %v, want a 401 StatusError", err)
	}
}
