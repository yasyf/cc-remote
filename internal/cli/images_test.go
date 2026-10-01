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
	context, err := images.RenderImage(inventory)
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{"Dockerfile", "plugins.sh", "provision.sh", "start.sh"}; !slices.Equal(names, want) {
		t.Errorf("rendered files = %v, want %v", names, want)
	}
}
