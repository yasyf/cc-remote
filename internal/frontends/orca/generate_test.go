package orca_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/frontends/orca"
)

var defaultSprites = orca.Recipe{
	Provider:    "sprites",
	Profile:     "agents",
	Connection:  orca.ConnectionSSH,
	Name:        "Sprites agents over SSH (default)",
	Description: "Default: a Sprite over SSH.",
}

func defaultRecipes(t *testing.T) []orca.Recipe {
	t.Helper()
	recipes, err := orca.Recipes(orca.Source{
		Provider: "sprites",
		Profile:  "agents",
		Profiles: map[string][]string{"agents": {"namespace", "sprites"}, "stack": {"namespace"}},
		Servers:  map[string]bool{"namespace": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return recipes
}

func TestRecipes(t *testing.T) {
	recipes := defaultRecipes(t)
	got := make([]string, 0, len(recipes))
	for _, r := range recipes {
		got = append(got, r.ID()+" | "+r.Name+" | "+r.Description)
	}
	want := []string{
		"sprites-agents-ssh | Sprites agents over SSH (default) | The default. A Sprites machine with the agents profile, reached over SSH.",
		"namespace-agents-server | Namespace agents through its Orca server | A Namespace machine with the agents profile, paired with its own Orca server.",
		"namespace-stack-server | Namespace stack through its Orca server | A Namespace machine with the stack profile, paired with its own Orca server.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Recipes() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRecipesRejectsBadSource(t *testing.T) {
	tests := []struct {
		name string
		src  orca.Source
		want string
	}{
		{
			name: "default profile has no machine on the default provider",
			src:  orca.Source{Provider: "sprites", Profile: "full", Profiles: map[string][]string{"full": {"namespace"}}},
			want: `default profile "full" configures no sprites machine`,
		},
		{
			name: "unsafe profile name",
			src:  orca.Source{Provider: "sprites", Profile: "lean", Profiles: map[string][]string{"lean": {"sprites"}, "a b": {"sprites"}}},
			want: `profile "a b" must match`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := orca.Recipes(tt.src)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Recipes() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRecipeID(t *testing.T) {
	tests := []struct {
		name   string
		recipe orca.Recipe
		want   string
	}{
		{"sprites ssh", defaultSprites, "sprites-agents-ssh"},
		{"namespace stack", orca.Recipe{Provider: "namespace", Profile: "stack", Connection: orca.ConnectionServer}, "namespace-stack-server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.recipe.ID(); got != tt.want {
				t.Errorf("ID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLifecycleCommand(t *testing.T) {
	tests := []struct {
		name      string
		lifecycle orca.Lifecycle
		verb      orca.Verb
		want      string
	}{
		{
			name:      "default binary, discovered config",
			lifecycle: orca.DefaultLifecycle(),
			verb:      orca.Create,
			want:      "cc-remote create --provider sprites --profile agents --connection ssh",
		},
		{
			name:      "paths with spaces and quotes are shell-quoted",
			lifecycle: orca.Lifecycle{Binary: "/opt/my tools/cc-remote", Config: "it's config.yaml"},
			verb:      orca.Create,
			want:      `'/opt/my tools/cc-remote' create --provider sprites --profile agents --connection ssh --config 'it'\''s config.yaml'`,
		},
		{
			name:      "repo shim with explicit config",
			lifecycle: orca.Lifecycle{Binary: "./tools/cc-remote", Config: ".cc-remote/config.yaml"},
			verb:      orca.Destroy,
			want:      "./tools/cc-remote destroy --provider sprites --profile agents --connection ssh --config .cc-remote/config.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.lifecycle.Command(tt.verb, defaultSprites); got != tt.want {
				t.Errorf("Command() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEntriesRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name      string
		lifecycle orca.Lifecycle
		recipes   []orca.Recipe
		want      string
	}{
		{"no recipes", orca.DefaultLifecycle(), nil, "no recipes configured"},
		{"duplicate id", orca.DefaultLifecycle(), []orca.Recipe{defaultSprites, defaultSprites}, "recipe sprites-agents-ssh is configured twice"},
		{"shell metacharacter in profile", orca.DefaultLifecycle(), []orca.Recipe{{Provider: "sprites", Profile: "agents;rm", Connection: orca.ConnectionSSH, Name: "x"}}, `profile "agents;rm"`},
		{"empty name", orca.DefaultLifecycle(), []orca.Recipe{{Provider: "sprites", Profile: "agents", Connection: orca.ConnectionSSH}}, "name is empty"},
		{"no connection", orca.DefaultLifecycle(), []orca.Recipe{{Provider: "sprites", Profile: "agents", Name: "x"}}, `recipe sprites-agents-: connection "" must be ssh or server`},
		{"control character in config", orca.Lifecycle{Binary: "cc-remote", Config: "config\n.yaml"}, []orca.Recipe{defaultSprites}, "contains a control character"},
		{"empty binary", orca.Lifecycle{}, []orca.Recipe{defaultSprites}, "lifecycle binary is empty"},
		{"uppercase provider", orca.DefaultLifecycle(), []orca.Recipe{{Provider: "Sprites", Profile: "agents", Connection: orca.ConnectionSSH, Name: "x"}}, `provider "Sprites" must match`},
		{"path traversal in profile", orca.DefaultLifecycle(), []orca.Recipe{{Provider: "sprites", Profile: "../x", Connection: orca.ConnectionSSH, Name: "x"}}, `profile "../x" must match`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := orca.Entries(tt.lifecycle, tt.recipes)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Entries() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestPluginFiles(t *testing.T) {
	stack := orca.Recipe{Provider: "namespace", Profile: "stack", Connection: orca.ConnectionServer, Name: "Namespace stack through its Orca server"}
	plugin := orca.Plugin{
		ID:          "cc-remote-recipes",
		Publisher:   "example",
		Name:        "project remote workspaces",
		Version:     "1.2.3",
		Description: "Remote workspaces for project, created by cc-remote over SSH.",
		Repository:  "https://github.com/example/project",
	}
	files, err := orca.PluginFiles(plugin, orca.DefaultLifecycle(), []orca.Recipe{defaultSprites, stack})
	if err != nil {
		t.Fatalf("PluginFiles() error = %v", err)
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if got, want := strings.Join(paths, ","), "orca-plugin.json,recipes/sprites-agents-ssh.json,recipes/namespace-stack-server.json"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
	wantManifest := `{
  "manifestVersion": 1,
  "id": "cc-remote-recipes",
  "publisher": "example",
  "name": "project remote workspaces",
  "version": "1.2.3",
  "description": "Remote workspaces for project, created by cc-remote over SSH.",
  "repository": "https://github.com/example/project",
  "engines": {
    "orca": ">=1.4.215"
  },
  "pluginApi": 1,
  "contributes": {
    "vmRecipes": [
      {
        "path": "recipes/sprites-agents-ssh.json"
      },
      {
        "path": "recipes/namespace-stack-server.json"
      }
    ]
  },
  "capabilities": []
}
`
	if got := string(files[0].Data); got != wantManifest {
		t.Errorf("manifest =\n%s\nwant\n%s", got, wantManifest)
	}
	wantRecipe := `{
  "schemaVersion": 1,
  "id": "namespace-stack-server",
  "name": "Namespace stack through its Orca server",
  "checkoutMode": "provisioned-root",
  "create": "cc-remote create --provider namespace --profile stack --connection server",
  "suspend": "cc-remote suspend --provider namespace --profile stack --connection server",
  "resume": "cc-remote resume --provider namespace --profile stack --connection server",
  "destroy": "cc-remote destroy --provider namespace --profile stack --connection server"
}
`
	if got := string(files[2].Data); got != wantRecipe {
		t.Errorf("recipe =\n%s\nwant\n%s", got, wantRecipe)
	}
	wantSSH := `{
  "schemaVersion": 1,
  "id": "sprites-agents-ssh",
  "name": "Sprites agents over SSH (default)",
  "description": "Default: a Sprite over SSH.",
  "checkoutMode": "provisioned-root",
  "create": "cc-remote create --provider sprites --profile agents --connection ssh",
  "suspend": "cc-remote suspend --provider sprites --profile agents --connection ssh",
  "resume": "cc-remote resume --provider sprites --profile agents --connection ssh",
  "destroy": "cc-remote destroy --provider sprites --profile agents --connection ssh"
}
`
	if got := string(files[1].Data); got != wantSSH {
		t.Errorf("ssh recipe =\n%s\nwant\n%s", got, wantSSH)
	}
	var decoded struct {
		Engines struct {
			Orca string `json:"orca"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(files[0].Data, &decoded); err != nil || decoded.Engines.Orca != ">=1.4.215" {
		t.Errorf("engines.orca = %q (err %v), want >=1.4.215", decoded.Engines.Orca, err)
	}
}

func TestMergeYAML(t *testing.T) {
	recipes := []orca.Recipe{defaultSprites}
	generated := `environmentRecipes:
  - id: sprites-agents-ssh
    name: Sprites agents over SSH (default)
    description: 'Default: a Sprite over SSH.'
    checkoutMode: provisioned-root
    create: cc-remote create --provider sprites --profile agents --connection ssh
    suspend: cc-remote suspend --provider sprites --profile agents --connection ssh
    resume: cc-remote resume --provider sprites --profile agents --connection ssh
    destroy: cc-remote destroy --provider sprites --profile agents --connection ssh
`
	tests := []struct {
		name     string
		existing string
		want     string
	}{
		{name: "empty file", existing: "", want: generated},
		{
			name:     "replaces stale recipes and keeps other keys",
			existing: "setup: ./scripts/setup.sh\nenvironmentRecipes:\n  - id: old\n    name: Old\nscripts:\n  test: go test ./...\n",
			want: "setup: ./scripts/setup.sh\n" + generated +
				"scripts:\n  test: go test ./...\n",
		},
		{
			name:     "appends after other keys",
			existing: "setup: ./scripts/setup.sh\n",
			want:     "setup: ./scripts/setup.sh\n" + generated,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := orca.MergeYAML([]byte(tt.existing), orca.DefaultLifecycle(), recipes)
			if err != nil {
				t.Fatalf("MergeYAML() error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("MergeYAML() =\n%s\nwant\n%s", got, tt.want)
			}
			entries, err := orca.ParseYAML(got)
			if err != nil {
				t.Fatalf("ParseYAML() error = %v", err)
			}
			want, _ := orca.Entries(orca.DefaultLifecycle(), recipes)
			if len(entries) != 1 || entries[0] != want[0] {
				t.Errorf("round trip = %+v, want %+v", entries, want)
			}
		})
	}
}

func TestMergeYAMLRefusesToOrphanAnAlias(t *testing.T) {
	existing := "environmentRecipes:\n  - id: old\n    create: &start ./start.sh\nscripts:\n  start: *start\n"
	_, err := orca.MergeYAML([]byte(existing), orca.DefaultLifecycle(), []orca.Recipe{defaultSprites})
	if err == nil || !strings.Contains(err.Error(), "replacing environmentRecipes breaks the rest of orca.yaml") {
		t.Errorf("MergeYAML() error = %v, want the orphaned-alias refusal", err)
	}
}

func TestMergeYAMLRejectsNonMapping(t *testing.T) {
	_, err := orca.MergeYAML([]byte("- a\n- b\n"), orca.DefaultLifecycle(), []orca.Recipe{defaultSprites})
	if !errors.Is(err, orca.ErrNotMapping) {
		t.Errorf("MergeYAML() error = %v, want ErrNotMapping", err)
	}
}

func TestCLICommand(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		goos string
		want string
	}{
		{"explicit command wins", map[string]string{"ORCA_CLI_COMMAND": "/mnt/c/orca.exe", "ORCA_DEV_REPO_ROOT": "/src/orca"}, "linux", "/mnt/c/orca.exe"},
		{"dev checkout", map[string]string{"ORCA_DEV_REPO_ROOT": "/src/orca"}, "darwin", "orca-dev"},
		{"linux outside an Orca terminal", nil, "linux", "orca-ide"},
		{"linux inside an Orca terminal", map[string]string{"ORCA_TERMINAL_HANDLE": "term-1"}, "linux", "orca"},
		{"macOS", nil, "darwin", "orca"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := orca.CLICommand(func(k string) string { return tt.env[k] }, tt.goos); got != tt.want {
				t.Errorf("CLICommand() = %q, want %q", got, tt.want)
			}
		})
	}
}
