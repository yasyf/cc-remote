package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/workspace"
)

type builtPayload struct {
	workspace.PayloadBuild
	Path string `json:"path"`
}

func newPayloadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "payload",
		Short: "Build the private SquashFS tool payload that fresh machines mount",
	}
	cmd.AddCommand(newPayloadBuildCmd())
	return cmd
}

func newPayloadBuildCmd() *cobra.Command {
	var flags selection
	var out string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Install the profile's tools on a fresh machine and download them as a SquashFS payload",
		Long: `build installs the profile's full tool inventory, private marketplaces
included, on a fresh build machine, packs the installed trees into a SquashFS
image, and streams it to --out. A private marketplace's GitHub token comes from
GH_TOKEN, GITHUB_TOKEN, or git.token_command and reaches only the install's
stdin. The build machine is always destroyed. It prints the payload's sha256,
size, tool fingerprint, and path as JSON; set the profile machine's payload path
and sha256 to them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := flags.load()
			if err != nil {
				return err
			}
			provider, err := openProvider(cfg, flags.provider)
			if err != nil {
				return err
			}
			path, err := filepath.Abs(out)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", out, err)
			}
			build, err := writePayload(path, func(w io.Writer) (workspace.PayloadBuild, error) {
				return workspace.BuildPayload(cmd.Context(), cfg, provider, flags.provider, flags.profile, workspace.GitToken(cfg), w, cmd.ErrOrStderr())
			})
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), builtPayload{PayloadBuild: build, Path: path})
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&out, "out", "", "file to write the SquashFS payload to; it must not exist")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func writePayload(path string, build func(io.Writer) (workspace.PayloadBuild, error)) (workspace.PayloadBuild, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return workspace.PayloadBuild{}, fmt.Errorf("create %s: %w", path, err)
	}
	built, err := build(file)
	if err := errors.Join(err, file.Close()); err != nil {
		return workspace.PayloadBuild{}, errors.Join(err, os.Remove(path))
	}
	return built, nil
}
