package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/cli"
	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/frontends/orca"
	"github.com/yasyf/cc-remote/internal/version"
)

var exampleConfig = filepath.Join("..", "..", "examples", "config.yaml")

const (
	examplePlaceholder = "<registry/repository@sha256:digest>"
	syntheticImage     = "registry.example.test/agent@sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

func pinnedExample(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(exampleConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(examplePlaceholder)) {
		t.Fatalf("%s no longer carries the image placeholder %s", exampleConfig, examplePlaceholder)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, bytes.ReplaceAll(raw, []byte(examplePlaceholder), []byte(syntheticImage)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := execCLI(args...)
	if err != nil {
		t.Fatalf("cc-remote %v: %v", args, err)
	}
	return out
}

func execCLI(args ...string) (string, error) {
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

const spritesAndNamespace = `repository: https://github.com/example/app
ref: main
provider: sprites
profile: lean
inventory: ./inventory.yaml
providers:
  sprites:
    org: example
  namespace:
    endpoint: https://compute.example.test
    platform: linux/amd64
    exportPort: 18766
    volumeSizeGB: 125
    duration: 4h
    callTimeout: 60s
    readyTimeout: 10m
workspace_dirs:
  sprites: /home/sprite
  namespace: /workspaces
profiles:
%s
`

func writeConfig(t *testing.T, profiles string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, fmt.Appendf(nil, spritesAndNamespace, profiles), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheExampleImagePlaceholderIsNotAPin(t *testing.T) {
	_, err := execCLI("orca", "recipes", "--config", exampleConfig)
	if want := `namespace image "` + examplePlaceholder + `" must be pinned by its @sha256 digest`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("recipes from the checked-in example = %v, want %q", err, want)
	}
}

func TestOrcaRecipes(t *testing.T) {
	example := pinnedExample(t)
	cfg, err := config.Load(example)
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := orca.Recipes(orca.SourceOf(cfg))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := orca.Lifecycle{Binary: "cc-remote", Config: example}
	want, err := orca.MergeYAML(nil, lifecycle, recipes)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("prints the environmentRecipes block", func(t *testing.T) {
		if got := runCLI(t, "orca", "recipes", "--config", example); got != string(want) {
			t.Errorf("stdout =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("pins the config it resolved from the environment", func(t *testing.T) {
		raw, err := os.ReadFile(example)
		if err != nil {
			t.Fatal(err)
		}
		custom := filepath.Join(t.TempDir(), "custom.yaml")
		if err := os.WriteFile(custom, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(config.EnvPath, custom)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		pinned, err := orca.MergeYAML(nil, orca.Lifecycle{Binary: "cc-remote", Config: custom}, recipes)
		if err != nil {
			t.Fatal(err)
		}
		got := runCLI(t, "orca", "recipes")
		if got != string(pinned) {
			t.Errorf("stdout =\n%s\nwant\n%s", got, pinned)
		}
		if !strings.Contains(got, "--config "+custom) {
			t.Errorf("recipes omit --config %s, so Orca's environment would pick another config:\n%s", custom, got)
		}
	})
	t.Run("writes --binary into the recipes as given", func(t *testing.T) {
		for binary, create := range map[string]string{
			"./tools/cc-remote/bin/cc-remote": "./tools/cc-remote/bin/cc-remote create",
			"./tools/my tools/cc-remote":      `'./tools/my tools/cc-remote' create`,
		} {
			entries, err := orca.ParseYAML([]byte(runCLI(t, "orca", "recipes", "--config", example, "--binary", binary)))
			if err != nil {
				t.Fatal(err)
			}
			want := create + " --provider sprites --profile lean --connection ssh --config " + example
			if len(entries) == 0 || entries[0].Create != want {
				t.Errorf("--binary %q: first create = %+v, want %q", binary, entries, want)
			}
		}
	})
	t.Run("rewrites orca.yaml in place and keeps other keys", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "orca.yaml")
		if err := os.WriteFile(path, []byte("setup: ./setup.sh\nenvironmentRecipes: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := runCLI(t, "orca", "recipes", "--config", example, "--orca-yaml", path); got != "" {
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
		runCLI(t, "orca", "recipes", "--config", example, "--plugin", dir)
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

func TestOrcaRecipesFromProfiles(t *testing.T) {
	tests := []struct {
		name     string
		profiles string
		want     []string
		wantErr  string
	}{
		{
			name:     "an empty profile pairs only with the default provider",
			profiles: "  lean: {}",
			want:     []string{"sprites-lean-ssh"},
		},
		{
			name: "each profile pairs only with the providers it configures",
			profiles: `  lean:
    machine: { sprites: {} }
  full:
    machine: { namespace: { image: "registry.example.test/agent@sha256:0000000000000000000000000000000000000000000000000000000000000000", size: 8x16 } }`,
			want: []string{"sprites-lean-ssh", "namespace-full-server"},
		},
		{
			name: "a configured Namespace machine without an image is an error",
			profiles: `  lean:
    machine: { sprites: {}, namespace: { size: 8x16 } }`,
			wantErr: "profile lean on namespace: a namespace spec needs an image",
		},
		{
			name: "an unpinned Namespace image is an error",
			profiles: `  lean:
    machine: { sprites: {}, namespace: { image: cc-remote-linux, size: 8x16 } }`,
			wantErr: `profile lean on namespace: namespace image "cc-remote-linux" must be pinned by its @sha256 digest`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := execCLI("orca", "recipes", "--config", writeConfig(t, tt.profiles))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("recipes error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			entries, err := orca.ParseYAML([]byte(out))
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(entries))
			for _, e := range entries {
				got = append(got, e.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("recipe ids = %v, want %v", got, tt.want)
			}
		})
	}
}
