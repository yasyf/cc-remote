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
			r, err := render(cfg, profile, machine.Image != "")
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
