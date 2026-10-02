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
	Path     string         `json:"path"`
	Packages *builtPackages `json:"packages,omitempty"`
}

type builtPackages struct {
	workspace.PackagesBuild
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
	var out, packagesOut string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Install the profile's tools on a fresh machine and download them as a SquashFS payload",
		Long: `build installs the profile's full tool inventory, private marketplaces
included, on a fresh build machine, packs the installed trees into a SquashFS
image, and streams it to --out. A private marketplace's GitHub token comes from
GH_TOKEN, GITHUB_TOKEN, or git.token_command and reaches only the install's
stdin. When the inventory declares apt.payload, the build also streams the
resident packages archive to --packages-out. The build machine is always
destroyed. It prints the payload's sha256, size, tool fingerprint, and path as
JSON, with the archive's sha256, size, and path under packages; set the profile
machine's payload path and sha256, and its payload.packages, to them.`,
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
			buildPayload := func(w, packages io.Writer) (workspace.PayloadBuild, error) {
				return workspace.BuildPayload(cmd.Context(), cfg, provider, flags.provider, flags.profile, workspace.GitToken(cfg), w, packages, cmd.ErrOrStderr())
			}
			if packagesOut == "" {
				build, err := writePayload(path, func(w io.Writer) (workspace.PayloadBuild, error) { return buildPayload(w, nil) })
				if err != nil {
					return err
				}
				return emit(cmd.OutOrStdout(), builtPayload{PayloadBuild: build, Path: path})
			}
			packagesPath, err := filepath.Abs(packagesOut)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", packagesOut, err)
			}
			build, err := writePayload(path, func(w io.Writer) (workspace.PayloadBuild, error) {
				return writePayload(packagesPath, func(packages io.Writer) (workspace.PayloadBuild, error) { return buildPayload(w, packages) })
			})
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), builtPayload{PayloadBuild: build, Path: path, Packages: &builtPackages{PackagesBuild: build.Packages, Path: packagesPath}})
		},
	}
	flags.bind(cmd)
	cmd.Flags().StringVar(&out, "out", "", "file to write the SquashFS payload to; it must not exist")
	_ = cmd.MarkFlagRequired("out")
	cmd.Flags().StringVar(&packagesOut, "packages-out", "", "file to write the resident packages archive to when the inventory declares apt.payload; it must not exist")
	return cmd
}

func writePayload(path string, build func(io.Writer) (workspace.PayloadBuild, error)) (workspace.PayloadBuild, error) {
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return workspace.PayloadBuild{}, fmt.Errorf("stat the payload directory: %w", err)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return workspace.PayloadBuild{}, fmt.Errorf("refusing to write the payload into %s: its mode %v lets its group or other users replace the private payload, so choose a directory only you can write", dir, mode)
	}
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
