package workspace

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
	"github.com/yasyf/cc-remote/internal/images"
)

func TestTheExampleConfigRendersItsInventoryForEveryProfileAndProvider(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for profile, spec := range cfg.Profiles {
		for provider, machine := range spec.Machine {
			r, err := render(cfg, profile, machine)
			if err != nil {
				t.Errorf("%s on %s: %v", profile, provider, err)
				continue
			}
			if len(r.stamp) != 64 || len(r.scripts.ProvisionScript) == 0 || len(r.scripts.Plugins) == 0 || (machine.Image != "") != (r.imageSpec != "") {
				t.Errorf("%s on %s rendered stamp %q, image spec %q, %d provision and %d plugin bytes", profile, provider, r.stamp, r.imageSpec, len(r.scripts.ProvisionScript), len(r.scripts.Plugins))
			}
			if err := coverEnv(cfg.Forwards, r.scripts.Env); err != nil {
				t.Errorf("%s on %s: %v", profile, provider, err)
			}
		}
	}
}

func TestOnlyThePayloadDigestMovesTheStamp(t *testing.T) {
	cfg := payloadConfig(t, "", inventory)
	scripts := fullScripts(t, cfg)
	other := strings.Repeat("ab", 32)
	bare, mounted, remounted := images.Stamp(scripts, nil, ""), images.Stamp(scripts, nil, sha), images.Stamp(scripts, nil, other)
	if bare == mounted || mounted == remounted || bare == remounted {
		t.Fatalf("stamps collide: without a payload %s, with %s, with another digest %s", bare, mounted, remounted)
	}
	tests := []struct {
		name    string
		payload *config.Payload
		want    string
	}{
		{"no payload", nil, bare},
		{"a payload", &config.Payload{Path: "tools.sqfs", SHA256: sha}, mounted},
		{"the same digest at another path", &config.Payload{Path: "moved/tools.sqfs", SHA256: sha}, mounted},
		{"another digest", &config.Payload{Path: "tools.sqfs", SHA256: other}, remounted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := render(cfg, "lean", config.Machine{Payload: tt.payload})
			if err != nil {
				t.Fatal(err)
			}
			if r.stamp != tt.want {
				t.Errorf("stamp = %s, want %s", r.stamp, tt.want)
			}
		})
	}
}
