package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-remote/internal/cli"
	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/version"
)

var exampleConfig = filepath.Join("..", "..", "examples", "config.yaml")

func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("cc-remote %v: %v", args, err)
	}
	return out.String()
}

func TestOrcaRecipes(t *testing.T) {
	cfg, err := config.Load(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := orca.Recipes(orca.SourceOf(cfg))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := orca.Lifecycle{Binary: "cc-remote", Config: exampleConfig}
	want, err := orca.MergeYAML(nil, lifecycle, recipes)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("prints the environmentRecipes block", func(t *testing.T) {
		if got := runCLI(t, "orca", "recipes", "--config", exampleConfig); got != string(want) {
			t.Errorf("stdout =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("defaults to the user config path and passes no --config", func(t *testing.T) {
		t.Setenv(config.EnvPath, exampleConfig)
		bare, err := orca.MergeYAML(nil, orca.DefaultLifecycle(), recipes)
		if err != nil {
			t.Fatal(err)
		}
		if got := runCLI(t, "orca", "recipes"); got != string(bare) {
			t.Errorf("stdout =\n%s\nwant\n%s", got, bare)
		}
	})
	t.Run("rewrites orca.yaml in place and keeps other keys", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "orca.yaml")
		if err := os.WriteFile(path, []byte("setup: ./setup.sh\nenvironmentRecipes: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := runCLI(t, "orca", "recipes", "--config", exampleConfig, "--orca-yaml", path); got != "" {
			t.Errorf("stdout = %q, want nothing", got)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "setup: ./setup.sh\n"+string(want) {
			t.Errorf("orca.yaml =\n%s", got)
		}
	})
	t.Run("writes the plugin", func(t *testing.T) {
		dir := t.TempDir()
		runCLI(t, "orca", "recipes", "--config", exampleConfig, "--plugin", dir)
		plugin, err := orca.PluginOf(cfg, version.Version)
		if err != nil {
			t.Fatal(err)
		}
		files, err := orca.PluginFiles(plugin, lifecycle, recipes)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			got, err := os.ReadFile(filepath.Join(dir, f.Path))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, f.Data) {
				t.Errorf("%s differs from the generator", f.Path)
			}
		}
	})
}
