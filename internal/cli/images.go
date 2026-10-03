package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
	"github.com/yasyf/cc-remote/internal/providers/namespace"
	"github.com/yasyf/cc-remote/internal/workspace"
)

func newImagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "images",
		Short: "Render, fingerprint, and build agent-host images from a tool inventory",
	}
	cmd.AddCommand(newImagesRenderCmd(), newImagesFingerprintCmd(), newImagesBuildCmd())
	return cmd
}

func newImagesRenderCmd() *cobra.Command {
	var inventoryPath, profile, platform, out string
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Write one profile's provision and plugin scripts, and its Namespace image context under namespace/",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			inventory, err := images.Load(inventoryPath)
			if err != nil {
				return err
			}
			scripts, err := images.Render(inventory, profile)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(out, 0o750); err != nil {
				return fmt.Errorf("create %s: %w", out, err)
			}
			if err := os.WriteFile(filepath.Join(out, "plugins.sh"), scripts.Plugins, 0o600); err != nil {
				return fmt.Errorf("write plugins.sh: %w", err)
			}
			if err := os.WriteFile(filepath.Join(out, "provision.sh"), scripts.ProvisionScript, 0o600); err != nil {
				return fmt.Errorf("write provision.sh: %w", err)
			}
			if inventory.Image == nil {
				return nil
			}
			context, err := images.RenderImage(inventory, profile, platform)
			if err != nil {
				return err
			}
			dir := filepath.Join(out, "namespace")
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
			return context.Write(dir)
		},
	}
	cmd.Flags().StringVar(&inventoryPath, "inventory", "", "path to the tool inventory YAML")
	cmd.Flags().StringVar(&profile, "profile", "", "profile whose extra tools to include")
	bindPlatform(cmd, &platform)
	cmd.Flags().StringVar(&out, "out", "", "directory to write the rendered files into")
	for _, flag := range []string{"inventory", "profile", "out"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func newImagesFingerprintCmd() *cobra.Command {
	var inventoryPath, profile, platform string
	cmd := &cobra.Command{
		Use:   "fingerprint",
		Short: "Print the tool fingerprint for one profile and the image fingerprint as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inventory, err := images.Load(inventoryPath)
			if err != nil {
				return err
			}
			scripts, err := images.Render(inventory, profile)
			if err != nil {
				return err
			}
			result := struct {
				Tools string `json:"tools"`
				Image string `json:"image,omitempty"`
			}{Tools: scripts.Fingerprint()}
			if inventory.Image != nil {
				context, err := images.RenderImage(inventory, profile, platform)
				if err != nil {
					return err
				}
				result.Image = context.Fingerprint()
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(result)
		},
	}
	cmd.Flags().StringVar(&inventoryPath, "inventory", "", "path to the tool inventory YAML")
	cmd.Flags().StringVar(&profile, "profile", "", "profile whose extra tools to include")
	bindPlatform(cmd, &platform)
	for _, flag := range []string{"inventory", "profile"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func newImagesBuildCmd() *cobra.Command {
	var configPath, profile, region string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build and push one profile's Namespace image and print its verified digest",
		Long: `build renders the profile's image context for providers.namespace.platform, builds and pushes it
for that platform with nsc build under a tag of its image fingerprint, then validates the repository
and digest returned by the workspace registry. A private marketplace's GitHub token comes from
git.token_command and reaches the build only as an nsc build
secret file that is removed once the build returns. stdout carries the receipt: the digest reference
to put in the profile's machine image, and the baked manifest that workspaces of this profile adopt.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmp.Or(configPath, config.DefaultPath()))
			if err != nil {
				return err
			}
			context, err := namespaceImage(cfg, cmp.Or(profile, cfg.Profile))
			if err != nil {
				return err
			}
			built, err := context.Build(cmd.Context(), images.NamespaceCLI{Stderr: cmd.ErrOrStderr(), Region: region}, workspace.GitToken(cfg))
			if err != nil {
				return err
			}
			return emit(cmd.OutOrStdout(), built)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file (default $CC_REMOTE_CONFIG or ~/.config/cc-remote/config.yaml)")
	cmd.Flags().StringVar(&profile, "profile", "", "profile whose tools to bake (default config.profile)")
	cmd.Flags().StringVar(&region, "region", "", "nsc --region for the build and registry calls (default nsc's own)")
	return cmd
}

func bindPlatform(cmd *cobra.Command, platform *string) {
	cmd.Flags().StringVar(platform, "platform", images.DefaultPlatform, "platform the Namespace image context targets: "+strings.Join(images.Platforms, " or ")+"; images build reads providers.namespace.platform instead")
}

func namespaceImage(cfg *config.Config, profile string) (images.Context, error) {
	if _, err := cfg.ProfileNamed(profile); err != nil {
		return images.Context{}, err
	}
	provider, err := openProvider(cfg, namespace.Name)
	if err != nil {
		return images.Context{}, err
	}
	inventory, err := images.Load(cfg.ScriptPath(cfg.Inventory))
	if err != nil {
		return images.Context{}, err
	}
	return images.RenderImage(inventory, profile, provider.Traits().Platform)
}
