package namespace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"sync"

	"namespacelabs.dev/integrations/api"
	"namespacelabs.dev/integrations/network/netcopy"

	"github.com/yasyf/cc-remote/internal/providers"
)

type Gateway func(ctx context.Context, debugLog io.Writer, tokens api.TokenSource, endpoint string) (net.Conn, error)

func GatewayEndpoint(compute providers.ComputeInstance) (string, error) {
	if compute.IngressDomain == "" || compute.ExportedPort <= 0 {
		return "", fmt.Errorf("namespace instance %s reports no ingress domain or exported host port for container %s port %d", compute.InstanceID, compute.Container, compute.ContainerPort)
	}
	endpoint := url.URL{Scheme: "wss", Host: "gate." + compute.IngressDomain, Path: "/" + compute.InstanceID + "/" + strconv.Itoa(compute.ExportedPort)}
	return endpoint.String(), nil
}

func (p *Provider) Forward(ctx context.Context, id string, listener net.Listener) error {
	saved, err := p.load(id)
	if err != nil {
		return err
	}
	if saved == nil {
		return fmt.Errorf("namespace instance %s: %w", id, providers.ErrNotFound)
	}
	serving, cancel := context.WithCancel(ctx)
	var bridges sync.WaitGroup
	defer func() {
		cancel()
		_ = listener.Close()
		bridges.Wait()
	}()
	stop := context.AfterFunc(serving, func() { _ = listener.Close() })
	defer stop()
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("the gateway listener for namespace instance %s stopped accepting: %w", id, err)
		}
		bridges.Go(func() {
			if err := p.bridge(serving, *saved, local); err != nil {
				slog.Warn("a gateway connection failed", "instance", id, "err", err)
			}
		})
	}
}

func (p *Provider) bridge(ctx context.Context, saved instance, local net.Conn) error {
	described, err := p.describe(ctx, saved)
	if err != nil {
		return errors.Join(err, local.Close())
	}
	project(&saved.Instance, described)
	endpoint, err := GatewayEndpoint(saved.Instance)
	if err != nil {
		return errors.Join(err, local.Close())
	}
	tokens, err := p.Tokens()
	if err != nil {
		return errors.Join(fmt.Errorf("loading the Namespace login for %s: %w", endpoint, err), local.Close())
	}
	remote, err := p.Gateway(ctx, io.Discard, tokens, endpoint)
	if err != nil {
		return errors.Join(fmt.Errorf("dialing %s: %w", endpoint, err), local.Close())
	}
	stop := context.AfterFunc(ctx, func() { _, _ = local.Close(), remote.Close() })
	defer stop()
	copied := netcopy.CopyConns(nil, remote, local)
	_, _ = local.Close(), remote.Close()
	slog.Debug("a gateway connection ended", "endpoint", endpoint, "err", copied)
	return nil
}
