package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yasyf/cc-remote/internal/cli"
	"github.com/yasyf/cc-remote/internal/images"
)

const exampleInventory = "../../examples/inventory.yaml"

func TestImagesFingerprintMatchesRender(t *testing.T) {
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"images", "fingerprint", "--inventory", exampleInventory, "--profile", "stack"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var got struct{ Tools, Image string }
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}

	inventory, err := images.Load(exampleInventory)
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := images.Render(inventory, "stack")
	if err != nil {
		t.Fatal(err)
	}
	context, err := images.RenderImage(inventory, "stack", images.DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tools != scripts.Fingerprint() || got.Image != context.Fingerprint() {
		t.Errorf("fingerprint printed %+v, want tools %s image %s", got, scripts.Fingerprint(), context.Fingerprint())
	}
}

func TestImagesRenderWritesTheScriptsAndContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	root := cli.NewRootCmd()
	root.SetArgs([]string{"images", "render", "--inventory", exampleInventory, "--profile", "agents", "--out", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for sub, want := range map[string][]string{
		"":          {"namespace", "plugins.sh", "provision.sh"},
		"namespace": {"Dockerfile", "baked.json", "finalize.sh", "plugins.sh", "provision.sh", "start.sh"},
	} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if !slices.Equal(names, want) {
			t.Errorf("rendered files in %q = %v, want %v", sub, names, want)
		}
	}
	inventory, err := images.Load(exampleInventory)
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := images.Render(inventory, "agents")
	if err != nil {
		t.Fatal(err)
	}
	image, err := images.RenderImage(inventory, "agents", images.DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{"plugins.sh": scripts.Plugins, "provision.sh": scripts.ProvisionScript, "namespace/plugins.sh": image.Plugins, "namespace/baked.json": image.Manifest} {
		if got, err := os.ReadFile(filepath.Join(dir, path)); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s differs from its render: %v", path, err)
		}
	}
	if bytes.Equal(scripts.Plugins, image.Plugins) {
		t.Error("the payload inventory's Sprites plugins.sh equals the payload-free image one")
	}
}

func TestImagesBuildRefusesAnUnknownProfileBeforeBuilding(t *testing.T) {
	root := cli.NewRootCmd()
	root.SetArgs([]string{"images", "build", "--config", exampleConfig, "--profile", "nope"})
	if err := root.Execute(); err == nil || err.Error() != "config has no nope profile" {
		t.Errorf("Execute() error = %v", err)
	}
}
