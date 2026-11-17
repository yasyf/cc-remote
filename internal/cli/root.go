package cli

import (
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func NewRootCmd() *cobra.Command {
	runner := orca.ExecRunner{Command: orca.CLICommand(os.Getenv, runtime.GOOS)}
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
		newDestroyCmd(runner),
		newExtendCmd(),
		newStatusCmd(),
		newVerifyCmd(),
		newProxyCmd(),
		newKeyCmd(),
		newKeysCmd(),
		newImagesCmd(),
		newPayloadCmd(),
		newJobsCmd(),
		newStepCmd(),
		newPoolCmd(),
		newOrcaCmd(runner),
	)
	return root
}
