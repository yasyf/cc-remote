package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/version"
)

func newOrcaCmd(runner orca.Runner) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "orca",
		Short: "Connect workspaces to the Orca desktop app",
	}
	cmd.AddCommand(
		newOrcaRecipesCmd(),
		newOrcaWaitCmd(runner),
		newOrcaVerifyCmd(runner),
		newOrcaGoneCmd(runner),
	)
	return cmd
}

type orcaConfig struct {
	path string
}

func (o *orcaConfig) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.path, "config", "", "config file (default $CC_REMOTE_CONFIG or ~/.config/cc-remote/config.yaml); "+
		"recipes pass this path to cc-remote as given, so name it relative to the repo root")
}

func (o *orcaConfig) load() (*config.Config, []orca.Recipe, orca.Lifecycle, error) {
	path := o.path
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, orca.Lifecycle{}, err
	}
	recipes, err := orca.Recipes(orca.SourceOf(cfg))
	lifecycle := orca.DefaultLifecycle()
	lifecycle.Config = o.path
	return cfg, recipes, lifecycle, err
}

func newOrcaRecipesCmd() *cobra.Command {
	var orcaYAML, pluginDir string
	var source orcaConfig
	cmd := &cobra.Command{
		Use:   "recipes",
		Short: "Generate Orca environment recipes from the cc-remote config",
		Long: "Print the orca.yaml environmentRecipes block, rewrite that block in an existing orca.yaml with --orca-yaml, " +
			"or write an Orca plugin that contributes the same recipes through contributes.vmRecipes with --plugin.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, recipes, lifecycle, err := source.load()
			if err != nil {
				return err
			}
			if pluginDir != "" {
				plugin, err := orca.PluginOf(cfg, version.Version)
				if err != nil {
					return err
				}
				if err := writePlugin(pluginDir, plugin, lifecycle, recipes); err != nil {
					return err
				}
			}
			if orcaYAML != "" {
				return rewriteOrcaYAML(orcaYAML, lifecycle, recipes)
			}
			if pluginDir != "" {
				return nil
			}
			out, err := orca.MergeYAML(nil, lifecycle, recipes)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	}
	cmd.Flags().StringVar(&orcaYAML, "orca-yaml", "", "rewrite environmentRecipes in this orca.yaml, creating it if absent")
	cmd.Flags().StringVar(&pluginDir, "plugin", "", "write orca-plugin.json and recipes/ into this directory")
	source.bind(cmd)
	return cmd
}

func newOrcaWaitCmd(runner orca.Runner) *cobra.Command {
	var repoID string
	var timeout time.Duration
	var source orcaConfig
	cmd := &cobra.Command{
		Use:   "wait <recipe-id> <workspace>",
		Short: "Preflight a recipe, then wait for Orca to list the SSH workspace it creates from it",
		Long: "Check that the Orca runtime is ready, that orca.yaml carries the recipe as cc-remote generates it, " +
			"and that orca vm recipe doctor reports no failure. " +
			"Then wait until Orca lists an SSH workspace with that name.\n\n" +
			"Orca alone creates the workspace: pick the recipe under Run on in the New Workspace composer. " +
			"Orca runs the recipe's create and registers the SSH target it returns. The orca CLI has no " +
			"recipe flag on worktree create and no command that adds an SSH host, so this command only waits.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, recipes, lifecycle, err := source.load()
			if err != nil {
				return err
			}
			recipe, err := orca.FindRecipe(recipes, args[0])
			if err != nil {
				return err
			}
			checkout := ""
			if repoID == "" {
				if checkout, err = primaryCheckout(cmd.Context()); err != nil {
					return err
				}
			}
			preflight := orca.Preflight{
				Recipe:    recipe,
				Lifecycle: lifecycle,
				Workspace: args[1],
				RepoID:    repoID,
				Checkout:  checkout,
			}
			workspace, err := orca.NewClient(runner).Wait(cmd.Context(), preflight, orca.Poll{Interval: 10 * time.Second, Timeout: timeout})
			if err != nil {
				return err
			}
			return printJSON(cmd, workspace)
		},
	}
	cmd.Flags().StringVar(&repoID, "repo", "", "Orca repo id; defaults to the repo whose path is this checkout's primary worktree")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long to wait for Orca to list the workspace")
	source.bind(cmd)
	return cmd
}

func newOrcaVerifyCmd(runner orca.Runner) *cobra.Command {
	var repoID, open string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "verify <workspace>",
		Short: "Prove an SSH workspace works from the Orca client",
		Long: "Check that the workspace's SSH target is connected, open a file in the Orca editor, and run a remote-check " +
			"probe in a new Orca terminal on the workspace. Exit 0 only when the probe exits 0 on the remote host. " +
			"The probe terminal stays open and its handle is printed as probeTerminal.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hostname, err := os.Hostname()
			if err != nil {
				return err
			}
			opts := orca.VerifyOptions{Workspace: args[0], RepoID: repoID, Open: open, LocalHostname: hostname}
			verification, err := orca.NewClient(runner).Verify(cmd.Context(), opts, orca.Poll{Interval: 2 * time.Second, Timeout: timeout})
			if err != nil {
				return err
			}
			return printJSON(cmd, verification)
		},
	}
	cmd.Flags().StringVar(&repoID, "repo", "", "Orca repo id, when several repos have a workspace with this name")
	cmd.Flags().StringVar(&open, "open", "README.md", "file to open in the Orca editor")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "how long to wait for the probe to exit")
	return cmd
}

func newOrcaGoneCmd(runner orca.Runner) *cobra.Command {
	return &cobra.Command{
		Use:   "gone <host>",
		Short: "Prove Orca lists no workspace and no SSH target on a deleted workspace's host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			removal, err := orca.NewClient(runner).Gone(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, removal)
		},
	}
}

func writePlugin(dir string, plugin orca.Plugin, lifecycle orca.Lifecycle, recipes []orca.Recipe) error {
	files, err := orca.PluginFiles(plugin, lifecycle, recipes)
	if err != nil {
		return err
	}
	for _, f := range files {
		path := filepath.Join(dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.Data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func rewriteOrcaYAML(path string, lifecycle orca.Lifecycle, recipes []orca.Recipe) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := orca.MergeYAML(existing, lifecycle, recipes)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return os.WriteFile(path, out, 0o600)
}

func primaryCheckout(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("find this checkout's primary worktree (pass --repo outside a git checkout): %w", err)
	}
	paths := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(paths) != 3 {
		return "", fmt.Errorf("git rev-parse printed %q, not a git dir, common dir, and toplevel", out)
	}
	if paths[0] == paths[1] {
		return paths[2], nil
	}
	out, err = exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		return "", fmt.Errorf("list this repo's worktrees: %w", err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	path, ok := strings.CutPrefix(first, "worktree ")
	if !ok {
		return "", fmt.Errorf("git worktree list --porcelain began with %q, not a worktree line", first)
	}
	return path, nil
}

func printJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
