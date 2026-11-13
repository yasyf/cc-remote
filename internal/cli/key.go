package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/state"
)

func newKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "key -- <command> [args...]",
		Short: "Print what an orca.keys command prints, answering aws ssm get-parameter and aws configure export-credentials in-process",
		Long: `key runs one orca.keys command the way a launch captures it and prints its output, so a shell
script or another tool's config can read a secret the same way. An aws ssm get-parameter command
with --query Parameter.Value --output text, or an aws configure export-credentials command with
--format process, never starts the aws CLI: cc-remote calls AWS itself and keeps each profile's
temporary credentials, never a parameter's value, in a 0600 file under its state directory until
5 minutes before they expire. Any other command runs as written.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), keyTimeout)
			defer cancel()
			out, err := keyOutput(ctx, state.Default(), args)
			defer clear(out)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	}
}
