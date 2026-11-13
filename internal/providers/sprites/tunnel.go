package sprites

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/yasyf/cc-remote/internal/providers"
)

const (
	sshPort         = 22
	handshakeWithin = 2 * time.Minute
	keepAliveEvery  = 15 * time.Second
	keepAliveMisses = 3
)

var _ providers.Tunneler = (*Provider)(nil)

type handshakeError struct{ err error }

func (p *Provider) Tunnel(ctx context.Context, id string) (providers.Tunnel, error) {
	keys := p.keys()
	authorized, err := present(keys.identity(id), keys.knownHosts(id))
	if err != nil {
		return nil, err
	}
	if !authorized {
		if _, err := p.target(ctx, id); err != nil {
			return nil, err
		}
	}
	client, err := p.handshake(ctx, id)
	var refused *handshakeError
	if authorized && errors.As(err, &refused) {
		if _, err := p.target(ctx, id); err != nil {
			return nil, errors.Join(refused, err)
		}
		client, err = p.handshake(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	go keepAlive(client)
	return client, nil
}

func present(paths ...string) (bool, error) {
	for _, path := range paths {
		_, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (p *Provider) handshake(ctx context.Context, id string) (*ssh.Client, error) {
	config, err := p.clientConfig(id)
	if err != nil {
		return nil, err
	}
	socket, err := p.dial(ctx, id, sshPort)
	if err != nil {
		return nil, err
	}
	conn := pipe(socket)
	if err := conn.SetDeadline(time.Now().Add(handshakeWithin)); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	transport, channels, requests, err := ssh.NewClientConn(conn, net.JoinHostPort(Alias(id), strconv.Itoa(sshPort)), config)
	if err != nil {
		return nil, &handshakeError{err: errors.Join(fmt.Errorf("ssh to sprite %s: %w", id, err), conn.Close())}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, transport.Close())
	}
	return ssh.NewClient(transport, channels, requests), nil
}

func (p *Provider) clientConfig(id string) (*ssh.ClientConfig, error) {
	keys := p.keys()
	private, err := os.ReadFile(keys.identity(id))
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		return nil, fmt.Errorf("reading the ssh key of sprite %s: %w", id, err)
	}
	known, err := os.ReadFile(keys.knownHosts(id))
	if err != nil {
		return nil, err
	}
	_, _, pinned, _, _, err := ssh.ParseKnownHosts(known)
	if err != nil {
		return nil, fmt.Errorf("reading the pinned host key of sprite %s: %w", id, err)
	}
	return &ssh.ClientConfig{
		User:              user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   ssh.FixedHostKey(pinned),
		HostKeyAlgorithms: []string{pinned.Type()},
	}, nil
}

func pipe(remote io.ReadWriteCloser) net.Conn {
	near, far := net.Pipe()
	go func() {
		_, _ = io.Copy(far, remote)
		_ = far.Close()
	}()
	go func() {
		_, _ = io.Copy(remote, far)
		_ = remote.Close()
	}()
	return near
}

func keepAlive(client *ssh.Client) {
	closed := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(closed)
	}()
	ticker := time.NewTicker(keepAliveEvery)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-closed:
			return
		case <-ticker.C:
		}
		answered := make(chan error, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			answered <- err
		}()
		select {
		case <-closed:
			return
		case err := <-answered:
			if err != nil {
				_ = client.Close()
				return
			}
			misses = 0
		case <-time.After(keepAliveEvery):
			if misses++; misses == keepAliveMisses {
				_ = client.Close()
				return
			}
		}
	}
}

func (e *handshakeError) Error() string { return e.err.Error() }

func (e *handshakeError) Unwrap() error { return e.err }
