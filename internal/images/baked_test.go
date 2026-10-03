package images

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	assets "github.com/yasyf/cc-remote/images"
)

func TestRenderImageBakesTheProfilesEffectiveTools(t *testing.T) {
	inventory := validInventory()
	inventory.Apt.Payload = &AptPayload{Resident: []string{"bubblewrap"}, Closure: []string{"libnss3"}}
	inventory.Profiles = map[string]Profile{"stack": {Tools: []Artifact{{Name: "kind", Version: "0.29.0", URL: "https://example.com/kind", SHA256: digest, Format: Binary}}}}
	image, err := RenderImage(inventory, "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	effective := inventory
	effective.Apt.Payload = nil
	scripts, err := Render(effective, "agents")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(image.Plugins, scripts.Plugins) || image.Profile != "agents" {
		t.Errorf("the image bakes another plugins.sh than the profile's payload-free one")
	}
	want := `{
  "schema": 1,
  "profile": "agents",
  "platform": "linux/amd64",
  "inputs": "` + image.Inputs() + `",
  "tools": "` + scripts.Fingerprint() + `",
  "user": "agent",
  "home": "/home/agent",
  "artifacts": [
    "jq 1.8.2 sha256:` + digest + `",
    "cookiesync 0.30.0 sha256:` + digest + `",
    "codex-runtime 26.1.0 sha256:` + digest + `",
    "captain-hook 1.0.0 sha256:` + digest + `"
  ],
  "marketplaces": [
    "market owner/market ref:` + commit + `"
  ],
  "plugins": [
    "hooks@market 1.0.0"
  ]
}
`
	if got := string(image.Manifest); got != want {
		t.Errorf("manifest =\n%s\nwant\n%s", got, want)
	}
	stack, err := RenderImage(inventory, "stack", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if stack.Fingerprint() == image.Fingerprint() || stack.Repository() == image.Repository() {
		t.Errorf("the stack and agents images share fingerprint %s or repository %s", image.Fingerprint(), image.Repository())
	}
	if !strings.Contains(string(stack.Manifest), `"profile": "stack"`) || !strings.Contains(string(stack.Manifest), "kind 0.29.0 sha256:"+digest) {
		t.Errorf("stack manifest = %s", stack.Manifest)
	}
	if Adopt(image.Manifest, stack.Manifest) == nil || Adopt(stack.Manifest, image.Manifest) == nil {
		t.Error("one profile adopted the other's image")
	}
}

func TestProfilesWithTheSameToolsStillBakeApart(t *testing.T) {
	agents, err := RenderImage(validInventory(), "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	tests, err := RenderImage(validInventory(), "tests", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(agents.Plugins, tests.Plugins) {
		t.Fatal("profiles without extra tools rendered different plugins.sh")
	}
	if agents.Fingerprint() == tests.Fingerprint() {
		t.Error("two profiles share one image identity")
	}
	err = Adopt(agents.Manifest, tests.Manifest)
	if err == nil || !strings.Contains(err.Error(), `baked as profile "tests"`) || !strings.Contains(err.Error(), `not profile "agents"`) {
		t.Errorf("Adopt = %v", err)
	}
}

func TestImageIdentityHoldsNoReadyStamp(t *testing.T) {
	inventory := validInventory()
	image, err := RenderImage(inventory, "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatal(err)
	}
	before := image.Fingerprint()
	stamp := Stamp(scripts, &image, "", "")
	for _, f := range image.files() {
		if bytes.Contains(f.data, []byte(stamp)) {
			t.Errorf("image file %s holds the ready stamp", f.name)
		}
	}
	if !bytes.Contains(image.Manifest, []byte(scripts.Fingerprint())) {
		t.Error("the manifest does not name the tool fingerprint")
	}
	if image.Fingerprint() != before {
		t.Error("computing the ready stamp moved the image fingerprint")
	}
	moved := image
	moved.Manifest = append(slices.Clone(image.Manifest), ' ')
	if moved.Fingerprint() == before || Stamp(scripts, &moved, "", "") == stamp {
		t.Error("a changed manifest left the image identity or the ready stamp in place")
	}
}

func TestAdopt(t *testing.T) {
	want, err := RenderImage(validInventory(), "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	drifted := validInventory()
	drifted.Tools[0].Version = "1.8.3"
	other, err := RenderImage(drifted, "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, baked, err string
	}{
		{"exact", string(want.Manifest), ""},
		{"missing", "", "carries no baked manifest at /opt/cc-remote/baked.json"},
		{"blank", "\n", "carries no baked manifest"},
		{"other tools", string(other.Manifest), `it was baked as profile "agents" on linux/amd64 with image inputs`},
		{"garbage", "{", "does not decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Adopt(want.Manifest, []byte(tt.baked))
			if tt.err == "" {
				if err != nil {
					t.Errorf("Adopt = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("Adopt = %v, want %q", err, tt.err)
			}
		})
	}
}

func TestReadBakedNeverFailsOnAMissingManifest(t *testing.T) {
	var argv []string
	out, err := ReadBaked(t.Context(), func(_ context.Context, cmd []string, _ io.Reader) ([]byte, error) {
		argv = cmd
		return []byte("{}\n"), nil
	})
	if err != nil || string(out) != "{}\n" {
		t.Errorf("ReadBaked = %q, %v", out, err)
	}
	if !slices.Equal(argv, []string{"sh", "-c", "cat /opt/cc-remote/baked.json 2> /dev/null || true"}) {
		t.Errorf("argv = %q", argv)
	}
}

func TestPluginPinRefreshMovesTheImageIdentity(t *testing.T) {
	base := validInventory()
	old, err := RenderImage(base, "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	oldScripts, err := Render(base, "agents")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Inventory)
		pin    string
	}{
		{"plugin version", func(i *Inventory) { i.Claude.Plugins[0].Version = "1.0.1" }, "hooks@market 1.0.1"},
		{"plugin marketplace ref", func(i *Inventory) { i.Claude.Marketplaces[0].Ref = strings.Repeat("4", 40) }, "market owner/market ref:" + strings.Repeat("4", 40)},
		{"branch marketplace", func(i *Inventory) {
			i.Claude.Marketplaces = append(i.Claude.Marketplaces, Marketplace{Name: "official", GitHub: "owner/official", Branch: "main"})
		}, "official owner/official branch:main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refreshed := validInventory()
			tt.mutate(&refreshed)
			image, err := RenderImage(refreshed, "agents", DefaultPlatform)
			if err != nil {
				t.Fatal(err)
			}
			scripts, err := Render(refreshed, "agents")
			if err != nil {
				t.Fatal(err)
			}
			if scripts.Fingerprint() == oldScripts.Fingerprint() {
				t.Fatal("the pin refresh left the tool fingerprint in place")
			}
			if image.Fingerprint() == old.Fingerprint() {
				t.Error("the pin refresh left the image fingerprint in place")
			}
			if Stamp(scripts, &image, "", "") == Stamp(oldScripts, &old, "", "") {
				t.Error("the pin refresh left the ready stamp in place")
			}
			if !strings.Contains(string(image.Manifest), tt.pin) || strings.Contains(string(old.Manifest), tt.pin) {
				t.Errorf("the refreshed manifest does not record %q:\n%s", tt.pin, image.Manifest)
			}
			if err := Adopt(image.Manifest, old.Manifest); err == nil {
				t.Error("an image baked before the pin refresh was adopted")
			}
		})
	}
}

func TestImageInputsMoveIdentityAndRefuseTheOldManifest(t *testing.T) {
	base := validInventory()
	old, err := RenderImage(base, "agents", DefaultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	overlay := func(t *testing.T, name, suffix string) fs.FS {
		t.Helper()
		files := fstest.MapFS{}
		if err := fs.WalkDir(assets.FS, ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := fs.ReadFile(assets.FS, path)
			if path == name {
				data = append(data, suffix...)
			}
			files[path] = &fstest.MapFile{Data: data}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return files
	}
	tests := []struct {
		name      string
		render    func(t *testing.T) (Context, error)
		sameTools bool
	}{
		{"base", func(*testing.T) (Context, error) {
			inv := validInventory()
			inv.Image.Base = "ubuntu:26.04@sha256:" + strings.Repeat("5", 64)
			return RenderImage(inv, "agents", DefaultPlatform)
		}, true},
		{"layer", func(*testing.T) (Context, error) {
			inv := validInventory()
			inv.Image.Layer = append(inv.Image.Layer, "RUN true")
			return RenderImage(inv, "agents", DefaultPlatform)
		}, true},
		{"user", func(*testing.T) (Context, error) {
			inv := validInventory()
			inv.Image.User = "dev"
			return RenderImage(inv, "agents", DefaultPlatform)
		}, true},
		{"platform", func(*testing.T) (Context, error) {
			return RenderImage(validInventory(), "agents", "linux/arm64")
		}, true},
		{"start bytes", func(t *testing.T) (Context, error) {
			return renderImage(overlay(t, "start.sh", "\n# changed\n"), validInventory(), "agents", DefaultPlatform)
		}, true},
		{"finalize bytes", func(t *testing.T) (Context, error) {
			return renderImage(overlay(t, "namespace/finalize.sh", "\ntrue\n"), validInventory(), "agents", DefaultPlatform)
		}, true},
		{"plugins.sh bytes", func(t *testing.T) (Context, error) {
			return renderImage(overlay(t, "plugins.sh", "\n# changed\n"), validInventory(), "agents", DefaultPlatform)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			image, err := tt.render(t)
			if err != nil {
				t.Fatal(err)
			}
			var have, want Baked
			if err := errors.Join(json.Unmarshal(image.Manifest, &have), json.Unmarshal(old.Manifest, &want)); err != nil {
				t.Fatal(err)
			}
			if (have.Tools == want.Tools) != tt.sameTools {
				t.Errorf("tools %.12s against %.12s, want same tools %t", have.Tools, want.Tools, tt.sameTools)
			}
			if have.Inputs == want.Inputs || image.Inputs() != have.Inputs || image.Fingerprint() == old.Fingerprint() {
				t.Errorf("inputs %.12s against %.12s, fingerprint %.12s against %.12s; want both moved and the manifest to name the inputs", have.Inputs, want.Inputs, image.Fingerprint(), old.Fingerprint())
			}
			err = Adopt(image.Manifest, old.Manifest)
			if err == nil || !strings.Contains(err.Error(), "with image inputs "+want.Inputs[:12]) || !strings.Contains(err.Error(), "with image inputs "+have.Inputs[:12]) {
				t.Errorf("Adopt = %v, want a refusal naming both image inputs", err)
			}
		})
	}
}

func TestRenderImageTargetsOnlyASupportedPlatform(t *testing.T) {
	arm, err := RenderImage(validInventory(), "agents", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(arm.BuildArgs("/ctx", ""), "--platform"); i < 0 || arm.BuildArgs("/ctx", "")[i+1] != "linux/arm64" || !strings.Contains(string(arm.Manifest), `"platform": "linux/arm64"`) {
		t.Errorf("arm64 build args %q, manifest %s", arm.BuildArgs("/ctx", ""), arm.Manifest)
	}
	for _, platform := range []string{"", "linux/riscv64", "darwin/arm64", "arm64"} {
		if _, err := RenderImage(validInventory(), "agents", platform); err == nil || !strings.Contains(err.Error(), "image platform") {
			t.Errorf("RenderImage(%q) = %v, want an unsupported platform", platform, err)
		}
	}
}
