package namespace

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"namespacelabs.dev/integrations/nsc/grpcapi"
	computev1beta "namespacelabs.dev/integrations/proto/namespace/cloud/compute/v1beta"
	sessions "namespacelabs.dev/integrations/proto/namespace/private/sessions"
)

const (
	sentinelKey   = "cc-remote-sentinel-ssh-private-key"
	sentinelToken = "cc-remote-sentinel-tenant-token"
)

type sentinelCompute struct {
	computev1beta.UnimplementedComputeServiceServer
}

func (sentinelCompute) DescribeInstance(context.Context, *computev1beta.DescribeInstanceRequest) (*computev1beta.DescribeInstanceResponse, error) {
	return &computev1beta.DescribeInstanceResponse{
		Metadata: &computev1beta.InstanceMetadata{InstanceId: "inst1"},
		ExtendedMetadata: &computev1beta.InstanceExtendedMetadata{
			SshMetadata: &computev1beta.InstanceExtendedMetadata_SshMetadata{SshPrivateKey: []byte(sentinelKey)},
		},
	}, nil
}

type sentinelSessions struct {
	sessions.UnimplementedUserSessionsServiceServer
}

func (sentinelSessions) IssueTenantTokenFromSession(context.Context, *sessions.IssueTenantTokenFromSessionRequest) (*sessions.IssueTenantTokenFromSessionResponse, error) {
	return &sessions.IssueTenantTokenFromSessionResponse{TenantToken: sentinelToken}, nil
}

func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	captured := make(chan []byte, 1)
	go func() {
		out, _ := io.ReadAll(read)
		captured <- out
	}()
	original := os.Stderr
	os.Stderr = write
	func() {
		defer func() { os.Stderr = original }()
		run()
	}()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	return string(<-captured)
}

func TestSDKDiagnosticsNeverPrintCredentials(t *testing.T) {
	for _, flag := range []string{"NS_GRPC_DEBUG", "NSC_GRPC_DEBUG_REQUESTS", "NSC_GRPC_DEBUG_RESPONSES"} {
		t.Setenv(flag, "1")
	}
	newProvider(t)
	if grpcapi.DebugWriter != io.Discard {
		t.Fatalf("grpcapi.DebugWriter = %v, want the deliberate discard sink", grpcapi.DebugWriter)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	computev1beta.RegisterComputeServiceServer(server, sentinelCompute{})
	sessions.RegisterUserSessionsServiceServer(server, sentinelSessions{})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	ctx := t.Context()
	stderr := captureStderr(t, func() {
		conn, err := grpcapi.NewConnectionWithEndpointWithTransportCredentials(ctx, listener.Addr().String(), nil, insecure.NewCredentials())
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		compute := computev1beta.NewComputeServiceClient(conn)
		described, err := compute.DescribeInstance(ctx, &computev1beta.DescribeInstanceRequest{InstanceId: "inst1"})
		if err != nil || string(described.GetExtendedMetadata().GetSshMetadata().GetSshPrivateKey()) != sentinelKey {
			t.Errorf("Describe = %v, %v; want the sentinel delivered to the caller", described, err)
		}
		issued, err := sessions.NewUserSessionsServiceClient(conn).IssueTenantTokenFromSession(ctx, &sessions.IssueTenantTokenFromSessionRequest{})
		if err != nil || issued.GetTenantToken() != sentinelToken {
			t.Errorf("IssueTenantTokenFromSession = %v, %v; want the sentinel delivered to the caller", issued, err)
		}
		if _, err := compute.DestroyInstance(ctx, &computev1beta.DestroyInstanceRequest{InstanceId: "inst1"}); status.Code(err) != codes.Unimplemented {
			t.Errorf("Destroy = %v, want the ordinary error still returned", err)
		}
	})
	for _, sentinel := range []string{sentinelKey, base64.StdEncoding.EncodeToString([]byte(sentinelKey)), sentinelToken} {
		if strings.Contains(stderr, sentinel) {
			t.Errorf("stderr carries the credential %q:\n%s", sentinel, stderr)
		}
	}
}
