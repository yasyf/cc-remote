package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cc-remote version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Println(version.String())
		},
	}
}
