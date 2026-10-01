package cli

import (
	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/providers/sprites"
)

func newProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "proxy -- <command> [args...]",
		Short:  "Run an ssh ProxyCommand that stops when ssh does",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return sprites.Proxy(cmd.Context(), args)
		},
	}
}
