package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPluginsPutMiseDataOnTheRootDiskOnlyOnASprite(t *testing.T) {
	inv := validInventory()
	tmpl, err := parse(inv)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := execute(tmpl, "plugins.sh", newView(inv, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(rendered)
	start, end := strings.Index(script, "\nmise_data() {\n"), strings.Index(script, "\nclaude_env() {\n")
	if start < 0 || end < start {
		t.Fatalf("plugins.sh has no mise_data before claude_env:\n%s", script)
	}
	for _, c := range []struct{ sprite, existing bool }{{true, false}, {true, true}, {false, false}} {
		root := t.TempDir()
		home, bin, cache := filepath.Join(root, "home"), filepath.Join(root, "bin"), filepath.Join(root, "var-tmp")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		data := filepath.Join(home, ".local", "share", "mise")
		if c.existing {
			if err := os.MkdirAll(filepath.Join(data, "installs", "node"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if c.sprite {
			if err := os.WriteFile(filepath.Join(bin, "sprite-env"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		run := exec.Command("bash", "-c", "set -euo pipefail\nroot_cache_dir="+quote(cache)+"\n"+script[start:end]+"\nmise_data\nmise_data\n")
		run.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
		if out, err := run.CombinedOutput(); err != nil {
			t.Fatalf("%+v: mise_data twice: %v: %s", c, err, out)
		}
		if c.existing {
			if info, err := os.Lstat(filepath.Join(data, "installs", "node")); err != nil || !info.IsDir() {
				t.Errorf("mise_data replaced the existing %s: %v", data, err)
			}
			continue
		}
		if !c.sprite {
			if _, err := os.Lstat(data); !os.IsNotExist(err) {
				t.Errorf("off a Sprite, mise_data made %s: %v", data, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Join(data, "installs", "go"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(cache, "mise", "installs", "go")); err != nil {
			t.Errorf("an install under %s did not land in %s: %v", data, cache, err)
		}
	}
}
