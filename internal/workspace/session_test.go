package workspace

import (
	"path/filepath"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
)

func TestTheExampleConfigRendersItsInventoryForEveryProfileAndProvider(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for profile, spec := range cfg.Profiles {
		for provider, machine := range spec.Machine {
			scripts, stamp, err := render(cfg, profile, machine.Image != "")
			if err != nil {
				t.Errorf("%s on %s: %v", profile, provider, err)
				continue
			}
			if len(stamp) != 64 || len(scripts.Provision) == 0 || len(scripts.Plugins) == 0 {
				t.Errorf("%s on %s rendered stamp %q with %d provision and %d plugin bytes", profile, provider, stamp, len(scripts.Provision), len(scripts.Plugins))
			}
			if err := coverEnv(cfg.Forwards, scripts.Env); err != nil {
				t.Errorf("%s on %s: %v", profile, provider, err)
			}
		}
	}
}
