package workspace

import (
	"path/filepath"
	"slices"
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
			r, err := render(cfg, profile, machine, machine.Payload != nil)
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

func TestOnlyThePayloadAndPackagesDigestsMoveTheStamp(t *testing.T) {
	cfg := payloadConfig(t, "", inventory)
	scripts := fullScripts(t, cfg)
	other := strings.Repeat("ab", 32)
	bare, mounted, remounted := images.Stamp(scripts, nil, "", ""), images.Stamp(scripts, nil, sha, ""), images.Stamp(scripts, nil, other, "")
	paired, repaired := images.Stamp(scripts, nil, sha, packagesSHA), images.Stamp(scripts, nil, sha, other)
	stamps := []string{bare, mounted, remounted, paired, repaired}
	if distinct := slices.Compact(slices.Sorted(slices.Values(stamps))); len(distinct) != len(stamps) {
		t.Fatalf("stamps collide: %q", stamps)
	}
	tests := []struct {
		name    string
		payload *config.Payload
		want    string
	}{
		{"no payload", nil, bare},
		{"a payload", &config.Payload{Source: config.Source{Path: "tools.sqfs", SHA256: sha}}, mounted},
		{"the same digest at another path", &config.Payload{Source: config.Source{Path: "moved/tools.sqfs", SHA256: sha}}, mounted},
		{"another digest", &config.Payload{Source: config.Source{Path: "tools.sqfs", SHA256: other}}, remounted},
		{"a packages archive", &config.Payload{Source: config.Source{Path: "tools.sqfs", SHA256: sha}, Packages: &config.Source{Path: "debs.tar", SHA256: packagesSHA}}, paired},
		{"the same packages digest at another path", &config.Payload{Source: config.Source{Path: "tools.sqfs", SHA256: sha}, Packages: &config.Source{Path: "moved/debs.tar", SHA256: packagesSHA}}, paired},
		{"a direct packages archive at the same digest", &config.Payload{Source: config.Source{Path: "tools.sqfs", SHA256: sha}, Packages: &config.Source{URLCommand: []string{"./packages-url"}, SHA256: packagesSHA, Size: 4}}, paired},
		{"another packages digest", &config.Payload{Source: config.Source{Path: "tools.sqfs", SHA256: sha}, Packages: &config.Source{Path: "debs.tar", SHA256: other}}, repaired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := render(cfg, "lean", config.Machine{Payload: tt.payload}, tt.payload != nil)
			if err != nil {
				t.Fatal(err)
			}
			if r.stamp != tt.want {
				t.Errorf("stamp = %s, want %s", r.stamp, tt.want)
			}
		})
	}
}
