package namespace

import (
	"context"
	"io"
	"net"
	"sync"

	"namespacelabs.dev/integrations/api"
	"namespacelabs.dev/integrations/api/compute"
	"namespacelabs.dev/integrations/auth"
	"namespacelabs.dev/integrations/nsc/grpcapi"
	"namespacelabs.dev/integrations/nsc/ingress"
	computev1beta "namespacelabs.dev/integrations/proto/namespace/cloud/compute/v1beta"
)

// The SDK falls back to stderr when NS_GRPC_DEBUG is set and then prints whole responses, credentials included.
var silenceDiagnostics = sync.OnceFunc(func() { grpcapi.DebugWriter = io.Discard })

func loadUserToken() (api.TokenSource, error) {
	return auth.LoadUserToken()
}

func dialCompute(ctx context.Context, endpoint string, tokens api.TokenSource) (computev1beta.ComputeServiceClient, io.Closer, error) {
	client, err := compute.NewClientWithEndpoint(ctx, endpoint, tokens)
	if err != nil {
		return nil, nil, err
	}
	return client.Compute, client, nil
}

func dialGateway(ctx context.Context, debugLog io.Writer, tokens api.TokenSource, endpoint string) (net.Conn, error) {
	return ingress.DialEndpoint(ctx, debugLog, tokens, endpoint)
}
