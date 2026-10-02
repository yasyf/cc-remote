package cli

import (
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:               "cc-remote",
		Short:             "Create and manage remote agent workspaces",
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.AddCommand(
		newVersionCmd(),
		newCreateCmd(),
		newResumeCmd(),
		newSuspendCmd(),
		newDestroyCmd(),
		newStatusCmd(),
		newVerifyCmd(),
		newProxyCmd(),
		newImagesCmd(),
		newPayloadCmd(),
		newOrcaCmd(orca.ExecRunner{Command: orca.CLICommand(os.Getenv, runtime.GOOS)}),
	)
	return root
}
