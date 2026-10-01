package workspace_test

import (
	"testing"

	"github.com/yasyf/cc-remote/internal/providers/providertest"
	"github.com/yasyf/cc-remote/internal/tailnet"
	"github.com/yasyf/cc-remote/internal/workspace"
	"github.com/yasyf/cc-remote/internal/workspace/workspacetest"
)

func TestFakeProviderSatisfiesTheLifecycleContract(t *testing.T) {
	workspacetest.Run(t, func(t *testing.T) workspacetest.Harness {
		local := workspacetest.NewLocalExec(t)
		return workspacetest.Harness{
			Provider: &providertest.Fake{Handle: local.Handle},
			Kind:     "fake",
			Platform: workspace.Platform{Daemon: tailnet.Daemon{Mode: tailnet.Userspace, Supervisor: tailnet.Setsid}},
			Root:     local.Root + "/machines",
			Spares:   2,
		}
	})
}
