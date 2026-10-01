package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/images"
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
	var inventoryPath, profile, out string
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Write the provision and plugin scripts, and the Namespace image context, for one profile",
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
			if inventory.Image == nil {
				return os.WriteFile(filepath.Join(out, "provision.sh"), scripts.Provision, 0o600)
			}
			context, err := images.RenderImage(inventory)
			if err != nil {
				return err
			}
			return context.Write(out)
		},
	}
	cmd.Flags().StringVar(&inventoryPath, "inventory", "", "path to the tool inventory YAML")
	cmd.Flags().StringVar(&profile, "profile", "", "profile whose extra tools to include")
	cmd.Flags().StringVar(&out, "out", "", "directory to write the rendered files into")
	for _, flag := range []string{"inventory", "profile", "out"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func newImagesFingerprintCmd() *cobra.Command {
	var inventoryPath, profile string
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
				context, err := images.RenderImage(inventory)
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
	for _, flag := range []string{"inventory", "profile"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func newImagesBuildCmd() *cobra.Command {
	var inventoryPath string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build and publish the Namespace devbox image the inventory describes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inventory, err := images.Load(inventoryPath)
			if err != nil {
				return err
			}
			context, err := images.RenderImage(inventory)
			if err != nil {
				return err
			}
			return context.Build(cmd.Context(), images.DevboxCLI{Stdout: cmd.ErrOrStderr(), Stderr: cmd.ErrOrStderr()})
		},
	}
	cmd.Flags().StringVar(&inventoryPath, "inventory", "", "path to the tool inventory YAML")
	_ = cmd.MarkFlagRequired("inventory")
	return cmd
}
