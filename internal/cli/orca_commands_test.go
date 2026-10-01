package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-remote/internal/cli"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

func defaultRecipes(t *testing.T) []orca.Recipe {
	t.Helper()
	recipes, err := orca.Recipes(orca.DefaultSource())
	if err != nil {
		t.Fatal(err)
	}
	return recipes
}

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
	want, err := orca.MergeYAML(nil, orca.DefaultLifecycle(), defaultRecipes(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("prints the environmentRecipes block", func(t *testing.T) {
		if got := runCLI(t, "orca", "recipes"); got != string(want) {
			t.Errorf("stdout =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("rewrites orca.yaml in place and keeps other keys", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "orca.yaml")
		if err := os.WriteFile(path, []byte("setup: ./setup.sh\nenvironmentRecipes: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := runCLI(t, "orca", "recipes", "--orca-yaml", path); got != "" {
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
		runCLI(t, "orca", "recipes", "--plugin", dir)
		files, err := orca.PluginFiles(orca.DefaultPlugin(), orca.DefaultLifecycle(), defaultRecipes(t))
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
