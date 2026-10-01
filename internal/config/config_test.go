package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `
repository: https://github.com/example/app
ref: main
provider: fake
profile: lean
providers:
  fake: { org: o }
workspace_dirs:
  fake: /home/fake
profiles:
  lean:
    prepare: [true]
    machine:
      fake: { size: s }
inventory: inventory.yaml
`

func TestExampleConfigLoadsWithDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "sprites" || cfg.Profile != "lean" || cfg.Ref != "main" {
		t.Errorf("defaults = %s/%s@%s", cfg.Provider, cfg.Profile, cfg.Ref)
	}
	if cfg.Profiles["lean"].Checkout != Shallow || cfg.Profiles["full"].Checkout != Full {
		t.Errorf("checkouts = %q, %q", cfg.Profiles["lean"].Checkout, cfg.Profiles["full"].Checkout)
	}
	if got := strings.Join(cfg.Profiles["full"].Prepare, "\n"); got != "./tools/bootstrap-project.sh\n./tools/install-project-deps.sh" {
		t.Errorf("full prepare = %q", got)
	}
	if cfg.Tailnet == nil || cfg.Tailnet.Tag != "tag:cc-remote" || cfg.Tailnet.API != DefaultTailnetAPI || cfg.Tailnet.KeychainService != DefaultKeychainService {
		t.Errorf("tailnet = %+v", cfg.Tailnet)
	}
	if !filepath.IsAbs(cfg.Path) || cfg.ScriptPath("./tools/x.sh") != filepath.Join(filepath.Dir(cfg.Path), "tools", "x.sh") {
		t.Errorf("path = %q, script = %q", cfg.Path, cfg.ScriptPath("./tools/x.sh"))
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"Forge", "forge", "yasyf", "poetic", "ts.net\n", "tag:orca"} {
		if strings.Contains(string(raw), private) {
			t.Errorf("the example carries the private value %q", private)
		}
	}
}

func TestParseFillsDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles["lean"].Checkout != Shallow || strings.Join(cfg.Git.TokenCommand, " ") != "gh auth token" || cfg.Tailnet != nil {
		t.Errorf("cfg = %+v", cfg)
	}
	profile, err := cfg.ProfileNamed("lean")
	if err != nil || profile.Machine["fake"].Size != "s" {
		t.Errorf("profile = %+v, %v", profile, err)
	}
	if cfg.ProjectRoot("fake") != "/home/fake/app" {
		t.Errorf("ProjectRoot = %q", cfg.ProjectRoot("fake"))
	}
	if raw, err := cfg.RawProviderSection("fake"); err != nil || strings.TrimSpace(string(raw)) != "{org: o}" {
		t.Errorf("raw section = %q, %v", raw, err)
	}
	if _, err := cfg.ProfileNamed("full"); err == nil {
		t.Error("an unknown profile resolved")
	}
}

func TestProviderSectionDecodesStrictly(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	decode, err := cfg.ProviderSection("fake")
	if err != nil {
		t.Fatal(err)
	}
	var loose struct {
		Org string `yaml:"org"`
	}
	if err := decode(&loose); err != nil || loose.Org != "o" {
		t.Errorf("decoded %+v, %v", loose, err)
	}
	var strict struct {
		Other string `yaml:"other"`
	}
	if err := decode(&strict); err == nil || !strings.Contains(err.Error(), "providers.fake") {
		t.Errorf("an unknown field passed: %v", err)
	}
	if _, err := cfg.ProviderSection("other"); err == nil {
		t.Error("an unknown provider resolved")
	}
}

func TestParseRefusesWhatCannotRun(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		want string
	}{
		{"unknown field", func(s string) string { return s + "bogus: 1\n" }, "bogus"},
		{"non-https repository", func(s string) string { return strings.Replace(s, "https://", "git@", 1) }, "https"},
		{"no ref", func(s string) string { return strings.Replace(s, "ref: main\n", "", 1) }, "ref"},
		{"no inventory", func(s string) string { return strings.Replace(s, "inventory: inventory.yaml\n", "", 1) }, "inventory"},
		{"unknown default provider", func(s string) string { return strings.Replace(s, "provider: fake", "provider: other", 1) }, "provider \"other\""},
		{"unknown default profile", func(s string) string { return strings.Replace(s, "profile: lean", "profile: full", 1) }, "profile \"full\""},
		{"bad checkout", func(s string) string {
			return strings.Replace(s, "prepare: [true]", "prepare: [true]\n    checkout: deep", 1)
		}, "checkout"},
		{"relative workspace dir", func(s string) string { return strings.Replace(s, "fake: /home/fake", "fake: home", 1) }, "workspace_dirs.fake"},
		{"workspace dir for unknown provider", func(s string) string {
			return strings.Replace(s, "fake: /home/fake", "fake: /home/fake\n  other: /x", 1)
		}, "workspace_dirs names provider"},
		{"long profile name", func(s string) string {
			return strings.ReplaceAll(s, "lean", "a-profile-name-over-twenty")
		}, "profile \"a-profile-name-over-twenty\""},
		{"machine for unknown provider", func(s string) string {
			return strings.Replace(s, "fake: { size: s }", "fake: { size: s }\n      other: {}", 1)
		}, "machine names provider"},
		{"removed spares", func(s string) string { return s + "spares:\n  fake: { lean: 1 }\n" }, "field spares not found"},
		{"removed warm", func(s string) string {
			return strings.Replace(s, "prepare: [true]", "prepare: [true]\n    warm: [true]", 1)
		}, "field warm not found"},
		{"removed warm inputs", func(s string) string {
			return strings.Replace(s, "prepare: [true]", "prepare: [true]\n    warm_inputs: [package.json]", 1)
		}, "field warm_inputs not found"},
		{"removed identity", func(s string) string { return s + "identity:\n  forbidden_paths: []\n" }, "field identity not found"},
		{"second document", func(s string) string { return s + "---\nrepository: https://github.com/x/y\n" }, "one YAML document"},
		{"provider name with a slash", func(s string) string { return strings.ReplaceAll(s, "fake", "fa/ke") }, "provider \"fa/ke\""},
		{"provider name with a dot", func(s string) string { return strings.ReplaceAll(s, "fake", "fa.ke") }, "provider \"fa.ke\""},
		{"profile name with a dot", func(s string) string { return strings.ReplaceAll(s, "lean", "le.an") }, "profile \"le.an\""},
		{"tailnet without tag prefix", func(s string) string { return s + "tailnet:\n  tag: cc-remote\n" }, "tailnet.tag"},
		{"forward without env name", func(s string) string { return s + "forwards:\n  - { label: a, env: lower }\n" }, "forward"},
		{"duplicate forward label", func(s string) string {
			return s + "forwards:\n  - { label: a, env: A }\n  - { label: a, env: B }\n"
		}, "forward"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.edit(minimal)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestDefaultPathFollowsEnvThenXDG(t *testing.T) {
	t.Setenv(EnvPath, "/etc/cc-remote.yaml")
	if DefaultPath() != "/etc/cc-remote.yaml" {
		t.Errorf("DefaultPath() = %q", DefaultPath())
	}
	t.Setenv(EnvPath, "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if DefaultPath() != "/xdg/cc-remote/config.yaml" {
		t.Errorf("DefaultPath() = %q", DefaultPath())
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	if DefaultPath() != "/home/u/.config/cc-remote/config.yaml" {
		t.Errorf("DefaultPath() = %q", DefaultPath())
	}
}

func TestStateDirExpandsHome(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("XDG_STATE_HOME", "")
	cfg, err := Parse([]byte(minimal + "state_dir: ~/state\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.State()) != "/home/u/state" {
		t.Errorf("State() = %q", cfg.State())
	}
	cfg.StateDir = ""
	if string(cfg.State()) != "/home/u/.local/state/cc-remote" {
		t.Errorf("State() = %q", cfg.State())
	}
	if RepositoryName("https://github.com/example/app.git") != "app" || RepositoryName("https://github.com/example/app") != "app" {
		t.Error("RepositoryName")
	}
}
