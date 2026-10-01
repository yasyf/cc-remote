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
		newPrepareCmd(),
		newDrainCmd(),
		newStatusCmd(),
		newVerifyCmd(),
		newProxyCmd(),
		newImagesCmd(),
		newOrcaCmd(orca.ExecRunner{Command: orca.CLICommand(os.Getenv, runtime.GOOS)}),
	)
	return root
}
