package namespace

import (
	"io"
	"slices"

	"github.com/yasyf/cc-remote/internal/providers"
)

var quietNative = []string{"NS_GRPC_DEBUG=0", "NSC_GRPC_DEBUG_REQUESTS=0", "NSC_GRPC_DEBUG_RESPONSES=0"}

func (p *Provider) container(compute providers.ComputeInstance, cmd []string, stdin io.Reader) providers.Command {
	return providers.Command{
		Name:  p.CLI,
		Args:  []string{"ssh", "--container_name", compute.Container, "-T", compute.InstanceID, providers.ShellQuote(cmd...)},
		Env:   slices.Concat(quietNative, []string{"NSC_ENDPOINT=" + compute.Endpoint}),
		Stdin: stdin,
	}
}

func (p *Provider) host(compute providers.ComputeInstance, cmd []string, stdin io.Reader) providers.Command {
	return providers.Command{
		Name:  p.CLI,
		Args:  []string{"ssh", "-T", compute.InstanceID, providers.ShellQuote(cmd...)},
		Env:   slices.Concat(quietNative, []string{"NSC_ENDPOINT=" + compute.Endpoint}),
		Stdin: stdin,
	}
}
