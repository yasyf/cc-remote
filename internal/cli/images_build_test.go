package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yasyf/cc-remote/internal/config"
)

const buildInventory = "version: 1\nimage:\n  name: agent-host\n  base: ubuntu:24.04@sha256:" +
	"008173c23f95b170204355c12626cb5a965d779a7e1283b09e9cffbb1bf33ca3\n  user: agent\n  workspaceDir: /workspaces\n"

func namespaceConfig(t *testing.T, platform string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inventory.yaml"), []byte(buildInventory), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	text := fmt.Sprintf(`repository: https://github.com/example/app
ref: main
provider: namespace
profile: lean
state_dir: %s
providers:
  namespace:
    endpoint: https://compute.example.test
    platform: %q
    exportPort: 18766
    volumeSizeGB: 125
    duration: 4h
    callTimeout: 60s
    readyTimeout: 10m
workspace_dirs:
  namespace: /workspaces
profiles:
  lean:
    machine:
      namespace: { image: agent-host-lean, size: l }
inventory: ./inventory.yaml
`, filepath.Join(dir, "state"), platform)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImageBuildTargetsTheConfiguredPlatformNotTheHost(t *testing.T) {
	fingerprints := map[string]string{}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		cfg, err := config.Load(namespaceConfig(t, platform))
		if err != nil {
			t.Fatal(err)
		}
		image, err := namespaceImage(cfg, "lean")
		if err != nil {
			t.Fatalf("%s: %v", platform, err)
		}
		args := image.BuildArgs("/ctx", "")
		if i := slices.Index(args, "--platform"); i < 0 || args[i+1] != platform || image.Platform != platform {
			t.Errorf("%s: build args %q", platform, args)
		}
		if !strings.Contains(string(image.Manifest), `"platform": "`+platform+`"`) {
			t.Errorf("%s: manifest %s", platform, image.Manifest)
		}
		fingerprints[platform] = image.Fingerprint()
	}
	if fingerprints["linux/amd64"] == fingerprints["linux/arm64"] {
		t.Error("amd64 and arm64 images share one identity")
	}
}

func TestImageBuildRefusesAnUnsupportedPlatformBeforeAnyProviderCall(t *testing.T) {
	for _, platform := range []string{"linux/s390x", "darwin/arm64"} {
		root := NewRootCmd()
		root.SetArgs([]string{"images", "build", "--config", namespaceConfig(t, platform), "--profile", "lean"})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("image platform %q", platform)) {
			t.Errorf("%s: Execute() error = %v", platform, err)
		}
	}
}
