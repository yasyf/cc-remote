package registry_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yasyf/cc-remote/internal/providers"
	"github.com/yasyf/cc-remote/internal/providers/namespace"
	"github.com/yasyf/cc-remote/internal/providers/registry"
	"github.com/yasyf/cc-remote/internal/providers/sprites"
)

func host(t *testing.T) registry.Host {
	dir := t.TempDir()
	return registry.Host{StateDir: filepath.Join(dir, "state"), Home: filepath.Join(dir, "home"), Helper: "/opt/cc-remote"}
}

func spritesSection(cli string) func(any) error {
	return func(into any) error {
		config := into.(*sprites.Config)
		config.Org = "acme"
		if cli != "" {
			config.CLI = cli
		}
		return nil
	}
}

func namespaceSection(into any) error {
	config := into.(*namespace.Config)
	config.Platform = "linux/amd64"
	config.VolumeSizeGB = 125
	config.IdleTimeout = 30 * time.Minute
	config.CallTimeout = time.Minute
	config.ReadyTimeout = 10 * time.Minute
	return nil
}

func TestNewSprites(t *testing.T) {
	h := host(t)
	tests := []struct {
		name    string
		section func(any) error
		wantCLI string
	}{
		{"default CLI", spritesSection(""), sprites.DefaultCLI},
		{"configured CLI", spritesSection("/opt/bin/sprite"), "/opt/bin/sprite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := registry.New(sprites.Name, h, tt.section)
			if err != nil {
				t.Fatal(err)
			}
			got := provider.(*sprites.Provider).Config
			want := sprites.Config{
				Org:      "acme",
				CLI:      tt.wantCLI,
				StateDir: h.StateDir,
				Helper:   h.Helper,
			}
			if got != want {
				t.Errorf("config = %+v, want %+v", got, want)
			}
			wantTraits := providers.Traits{TailnetMode: providers.TailnetKernel, Supervisor: providers.SupervisorSpriteEnv}
			if traits := provider.Traits(); traits != wantTraits {
				t.Errorf("Traits() = %+v, want %+v", traits, wantTraits)
			}
		})
	}
}

func TestNewNamespace(t *testing.T) {
	h := host(t)
	provider, err := registry.New(namespace.Name, h, namespaceSection)
	if err != nil {
		t.Fatal(err)
	}
	got := provider.(*namespace.Provider).Config
	if got.CLI != namespace.DefaultCLI || got.SSHDir != filepath.Join(h.Home, ".namespace", "ssh") || got.StateDir != h.StateDir {
		t.Errorf("config = %+v, want the default CLI, ssh dir under home, and the host state dir", got)
	}
	wantTraits := providers.Traits{
		TailnetMode: providers.TailnetUserspace,
		Supervisor:  providers.SupervisorSetsid,
	}
	if traits := provider.Traits(); traits != wantTraits {
		t.Errorf("Traits() = %+v, want %+v", traits, wantTraits)
	}
}

func TestNewRejects(t *testing.T) {
	h := host(t)
	malformed := errors.New("field orgg not found")
	tests := []struct {
		name    string
		kind    string
		section func(any) error
		want    string
	}{
		{"unknown kind", "fly", spritesSection(""), `unknown provider kind "fly"; want sprites or namespace`},
		{"malformed section", sprites.Name, func(any) error { return malformed }, "providers.sprites: field orgg not found"},
		{"sprites without an org", sprites.Name, func(any) error { return nil }, "providers.sprites: sprites needs an org; set it in the sprites config"},
		{"namespace without timeouts", namespace.Name, func(into any) error {
			if err := namespaceSection(into); err != nil {
				return err
			}
			into.(*namespace.Config).ReadyTimeout = 0
			return nil
		}, "providers.namespace: namespace needs a positive callTimeout and readyTimeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry.New(tt.kind, h, tt.section)
			if err == nil || err.Error() != tt.want {
				t.Errorf("New = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := registry.New(sprites.Name, h, func(any) error { return malformed }); !errors.Is(err, malformed) {
		t.Errorf("New does not wrap the section error: %v", err)
	}
}
