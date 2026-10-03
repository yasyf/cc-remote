package namespace

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"namespacelabs.dev/integrations/api"

	"github.com/yasyf/cc-remote/internal/providers"
)

func TestGatewayEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		compute providers.ComputeInstance
		want    string
	}{
		{"exported", providers.ComputeInstance{InstanceID: "inst1", IngressDomain: testDomain, ExportedPort: firstHost}, "wss://gate.us.test.nscluster.cloud/inst1/30000"},
		{"no domain", providers.ComputeInstance{InstanceID: "inst1", Container: "agent", ContainerPort: testPort, ExportedPort: firstHost}, ""},
		{"no host port", providers.ComputeInstance{InstanceID: "inst1", Container: "agent", ContainerPort: testPort, IngressDomain: testDomain}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GatewayEndpoint(tt.compute)
			if tt.want == "" {
				if err == nil || err.Error() != "namespace instance inst1 reports no ingress domain or exported host port for container agent port 18766" {
					t.Errorf("GatewayEndpoint = %q, %v", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("GatewayEndpoint = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

type pipeGateway struct {
	dialed chan string
	remote chan net.Conn
}

func newPipeGateway() *pipeGateway {
	return &pipeGateway{dialed: make(chan string, 4), remote: make(chan net.Conn, 4)}
}

func (g *pipeGateway) dial(_ context.Context, debugLog io.Writer, _ api.TokenSource, endpoint string) (net.Conn, error) {
	if debugLog != io.Discard {
		return nil, errors.New("the gateway debug log is not discarded")
	}
	local, remote := net.Pipe()
	g.remote <- remote
	g.dialed <- endpoint
	return local, nil
}

func closed(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		t.Errorf("read after the bridge stopped = %v, want the half closed", err)
	}
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func TestForwardBridgesEachConnectionToTheCurrentExportedPort(t *testing.T) {
	p, _, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	gateway := newPipeGateway()
	p.Gateway = gateway.dial
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	loopback := listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- p.Forward(ctx, "inst1", listener) }()
	var hosts []int
	for round := range 2 {
		if round == 1 {
			if err := p.Suspend(t.Context(), "inst1"); err != nil {
				t.Fatal(err)
			}
			if err := p.Wake(t.Context(), "inst1"); err != nil {
				t.Fatal(err)
			}
		}
		local, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		endpoint, remote := <-gateway.dialed, <-gateway.remote
		host := firstHost + round
		if want := "wss://gate." + testDomain + "/inst1/" + strconv.Itoa(host); endpoint != want {
			t.Errorf("round %d dialed %s, want %s", round, endpoint, want)
		}
		hosts = append(hosts, host)
		go func() { _, _ = io.Copy(remote, remote) }()
		if _, err := local.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		echo := make([]byte, 4)
		if _, err := io.ReadFull(local, echo); err != nil || string(echo) != "ping" {
			t.Errorf("round %d echo = %q, %v", round, echo, err)
		}
		if err := local.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range hosts {
		if host == testPort || host == loopback {
			t.Errorf("container port %d, host port %d, and loopback port %d are not distinct", testPort, host, loopback)
		}
	}
	if testPort == loopback {
		t.Errorf("the loopback port %d reuses the container port", loopback)
	}
	cancel()
	select {
	case err := <-served:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Forward after cancel = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Forward did not return after its caller canceled")
	}
}

type failingListener struct {
	net.Listener
	accepted chan net.Conn
	fail     chan struct{}
	err      error
	shut     chan struct{}
}

func (l *failingListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.accepted:
		return conn, nil
	case <-l.fail:
		return nil, l.err
	case <-l.shut:
		return nil, net.ErrClosed
	}
}

func (l *failingListener) Close() error {
	select {
	case <-l.shut:
	default:
		close(l.shut)
	}
	return nil
}

func TestForwardReturnsATerminalAcceptErrorAndClosesItsBridges(t *testing.T) {
	p, _, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	gateway := newPipeGateway()
	p.Gateway = gateway.dial
	sentinel := errors.New("accept: too many open files")
	listener := &failingListener{accepted: make(chan net.Conn, 1), fail: make(chan struct{}), err: sentinel, shut: make(chan struct{})}
	client, local := net.Pipe()
	listener.accepted <- local
	served := make(chan error, 1)
	go func() { served <- p.Forward(t.Context(), "inst1", listener) }()
	<-gateway.dialed
	remote := <-gateway.remote
	close(listener.fail)
	select {
	case err := <-served:
		if !errors.Is(err, sentinel) || errors.Is(err, context.Canceled) {
			t.Errorf("Forward = %v, want the listener's own error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Forward waited on a live bridge instead of returning the listener failure")
	}
	closed(t, client)
	closed(t, remote)
	select {
	case <-listener.shut:
	default:
		t.Error("Forward left its listener open")
	}
}

func TestForwardKeepsServingAfterOneBridgeEnds(t *testing.T) {
	p, _, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	gateway := newPipeGateway()
	p.Gateway = gateway.dial
	listener := &failingListener{accepted: make(chan net.Conn, 2), fail: make(chan struct{}), err: errors.New("unused"), shut: make(chan struct{})}
	first, firstLocal := net.Pipe()
	listener.accepted <- firstLocal
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- p.Forward(ctx, "inst1", listener) }()
	<-gateway.dialed
	firstRemote := <-gateway.remote
	if err := firstRemote.Close(); err != nil {
		t.Fatal(err)
	}
	closed(t, first)
	second, secondLocal := net.Pipe()
	listener.accepted <- secondLocal
	<-gateway.dialed
	secondRemote := <-gateway.remote
	select {
	case err := <-served:
		t.Fatalf("Forward stopped after one bridge ended: %v", err)
	default:
	}
	cancel()
	if err := <-served; !errors.Is(err, context.Canceled) {
		t.Errorf("Forward after cancel = %v", err)
	}
	closed(t, second)
	closed(t, secondRemote)
}

type login struct{ generation int }

func (login) IssueToken(context.Context, time.Duration, bool) (string, error) {
	return "synthetic-bearer", nil
}

func echo(t *testing.T, local net.Conn, message string) {
	t.Helper()
	if _, err := local.Write([]byte(message)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(message))
	if _, err := io.ReadFull(local, got); err != nil || string(got) != message {
		t.Errorf("echo = %q, %v; want %q", got, err, message)
	}
}

func TestEachGatewayConnectionLoadsTheCurrentLogin(t *testing.T) {
	p, _, _ := newProvider(t)
	if _, err := p.Create(t.Context(), spec("alpha", nil)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	current, loggedOut := 0, false
	p.Tokens = func() (api.TokenSource, error) {
		mu.Lock()
		defer mu.Unlock()
		if loggedOut {
			return nil, errors.New("you are not logged in to Namespace; try running `nsc login`")
		}
		return login{current}, nil
	}
	var dialedWith []api.TokenSource
	gateway := newPipeGateway()
	p.Gateway = func(ctx context.Context, debugLog io.Writer, tokens api.TokenSource, endpoint string) (net.Conn, error) {
		mu.Lock()
		dialedWith = append(dialedWith, tokens)
		mu.Unlock()
		return gateway.dial(ctx, debugLog, tokens, endpoint)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- p.Forward(ctx, "inst1", listener) }()
	connect := func() net.Conn {
		local, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		<-gateway.dialed
		remote := <-gateway.remote
		go func() { _, _ = io.Copy(remote, remote) }()
		return local
	}
	first := connect()
	echo(t, first, "before")
	mu.Lock()
	current = 1
	mu.Unlock()
	second := connect()
	echo(t, second, "after")
	echo(t, first, "still")
	mu.Lock()
	loggedOut = true
	mu.Unlock()
	refused, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	if err := refused.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := refused.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Errorf("a connection without a login read %v, want it closed", err)
	}
	echo(t, second, "serving")
	mu.Lock()
	if want := []api.TokenSource{login{0}, login{1}}; !slices.Equal(dialedWith, want) {
		t.Errorf("gateway dials used %v, want the login current at each connection %v", dialedWith, want)
	}
	mu.Unlock()
	select {
	case err := <-served:
		t.Fatalf("Forward stopped while the login changed: %v", err)
	default:
	}
	cancel()
	if err := <-served; !errors.Is(err, context.Canceled) {
		t.Errorf("Forward after cancel = %v", err)
	}
	for _, conn := range []net.Conn{first, second, refused} {
		_ = conn.Close()
	}
}
