package images

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type fakeDevbox struct {
	args  []string
	files map[string]string
}

func (f *fakeDevbox) Run(_ context.Context, args ...string) error {
	f.args = args
	f.files = map[string]string{}
	for _, name := range []string{"Dockerfile", "provision.sh", "start.sh"} {
		raw, err := os.ReadFile(filepath.Join(args[2], name))
		if err != nil {
			return err
		}
		f.files[name] = string(raw)
	}
	return nil
}

func TestBuildPublishesTheRenderedContext(t *testing.T) {
	context, err := RenderImage(validInventory())
	if err != nil {
		t.Fatalf("RenderImage: %v", err)
	}
	var devbox fakeDevbox
	if err := context.Build(t.Context(), &devbox); err != nil {
		t.Fatalf("Build: %v", err)
	}
	dir := devbox.args[2]
	want := []string{
		"image", "build", dir,
		"--name", "agent-host",
		"--description", "cc-remote agent host cc-remote-image=" + context.Fingerprint(),
		"--user", "agent",
		"--on_startup", "/usr/local/bin/cc-remote-start",
		"--workspace_dir", "/workspaces",
		"--persistency", "whole",
	}
	if !slices.Equal(devbox.args, want) {
		t.Errorf("devbox args = %q, want %q", devbox.args, want)
	}
	for name, data := range map[string][]byte{"Dockerfile": context.Dockerfile, "provision.sh": context.Provision, "start.sh": context.Start} {
		if devbox.files[name] != string(data) {
			t.Errorf("context %s differs from the rendered file", name)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Build left its context directory %s behind: %v", dir, err)
	}
}
