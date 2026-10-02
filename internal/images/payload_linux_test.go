package images

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	payloadTool    = ".local/share/cc-remote/tools/tool-1.0"
	payloadMarket  = ".local/share/cc-remote/marketplaces/tools-market"
	payloadCache   = ".claude/plugins/cache/tools-market/hook/1.0.0"
	payloadHook    = ".daemonkit/tools/capt-hook/1.0.0"
	payloadPython  = ".local/share/uv/python"
	failingCurl    = "#!/bin/sh\necho \"curl $*\" >> \"$FAKE_LOG\"\nexit 22\n"
	installedHooks = `{"build":"1.0.0"}`
)

func payloadInventory() Inventory {
	return Inventory{
		Version: SchemaVersion,
		Tools:   []Artifact{{Name: "tool", Version: "1.0", URL: "https://example.invalid/tool", SHA256: digest, Format: Binary}},
		Claude: Claude{
			Marketplaces: []Marketplace{toolsRef},
			Plugins:      []Plugin{{ID: "hook@tools-market", Version: "1.0.0"}},
		},
		CaptainHook: &CaptainHook{Version: "1.0.0", URL: "https://example.invalid/hook.tar.gz", SHA256: digest},
	}
}

func writePayload(t *testing.T, payload, home, omit string) {
	t.Helper()
	files := map[string]string{
		payloadTool + "/.cc-remote-digest":         digest + "\n",
		payloadTool + "/tool":                      "#!/bin/sh\n",
		payloadMarket + "/.fake-head":              commit + "\n",
		payloadCache + "/plugin.json":              `{"name":"hook"}`,
		".claude/plugins/installed_plugins.json":   `{"version":2,"plugins":{}}`,
		".claude/plugins/known_marketplaces.json":  `{}`,
		".claude/settings.json":                    `{}`,
		payloadHook + "/capt-hook":                 "#!/bin/sh\n",
		payloadHook + "/.lock":                     "",
		payloadPython + "/cpython-3.13/bin/python": "#!/bin/sh\n",
	}
	for rel, data := range files {
		if rel == omit || strings.HasPrefix(rel, omit+"/") {
			continue
		}
		writePluginTestFile(t, filepath.Join(payload, home, rel), []byte(data), 0o755)
	}
}

func TestPluginsInstallExposesThePayload(t *testing.T) {
	tests := []struct {
		name    string
		omit    string
		stale   bool
		wantErr string
	}{
		{name: "every tree"},
		{name: "link into an older payload", stale: true},
		{name: "optional tree missing", omit: payloadPython},
		{name: "required tree missing", omit: payloadCache, wantErr: payloadCache},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newPluginsHost(t, payloadInventory(), marketplaceCatalog("0.7.17"), fakeState{}, nil)
			root := t.TempDir()
			payload := filepath.Join(root, digest)
			writePayload(t, payload, h.home, tt.omit)
			writePluginTestFile(t, filepath.Join(h.fakes, "curl"), []byte(failingCurl), 0o700)
			writePluginTestFile(t, filepath.Join(h.home, ".local/share/captain-hook/host/version.json"), []byte(installedHooks), 0o600)
			writePluginTestFile(t, filepath.Join(h.home, ".cc-remote", "ready"), []byte(digest+"\n"), 0o600)
			if tt.stale {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(h.home, payloadTool)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "older", h.home, payloadTool), filepath.Join(h.home, payloadTool)); err != nil {
					t.Fatal(err)
				}
			}
			out, err := h.plugins("install", payload)
			h.unready()
			if tt.wantErr != "" {
				want := "cc-remote: payload " + payload + " lacks " + filepath.Join(h.home, tt.wantErr)
				if exitCode(err) != 1 || !strings.Contains(out, want) {
					t.Fatalf("install = %v\n%s\nwant exit 1 with %q", err, out, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			if slices.ContainsFunc(h.calls(), func(call string) bool { return strings.HasPrefix(call, "curl ") }) {
				t.Errorf("install downloaded an artifact the payload holds:\n%s", strings.Join(h.calls(), "\n"))
			}
			for _, rel := range []string{payloadTool, payloadMarket, payloadHook + "/capt-hook"} {
				if got, err := os.Readlink(filepath.Join(h.home, rel)); err != nil || got != filepath.Join(payload, h.home, rel) {
					t.Errorf("%s links to %q, %v; want the payload copy", rel, got, err)
				}
			}
			for _, rel := range []string{payloadCache, payloadHook} {
				if info, err := os.Lstat(filepath.Join(h.home, rel)); err != nil || !info.IsDir() {
					t.Errorf("%s is %v, %v; want a real directory", rel, info, err)
				}
			}
			if got, err := os.ReadFile(filepath.Join(h.home, payloadCache, "plugin.json")); err != nil || string(got) != `{"name":"hook"}` {
				t.Errorf("copied plugin cache = %q, %v", got, err)
			}
			for _, rel := range []string{".claude/plugins/installed_plugins.json", ".claude/plugins/known_marketplaces.json", ".claude/settings.json"} {
				if info, err := os.Lstat(filepath.Join(h.home, rel)); err != nil || !info.Mode().IsRegular() {
					t.Errorf("%s is %v, %v; want a regular file", rel, info, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(h.home, payloadHook, ".lock")); !os.IsNotExist(err) {
				t.Errorf("exposed the payload's .lock: %v", err)
			}
			python, err := os.Readlink(filepath.Join(h.home, payloadPython))
			switch {
			case tt.omit == payloadPython && !os.IsNotExist(err):
				t.Errorf("exposed the missing optional %s: %q, %v", payloadPython, python, err)
			case tt.omit != payloadPython && (err != nil || python != filepath.Join(payload, h.home, payloadPython)):
				t.Errorf("%s links to %q, %v; want the payload copy", payloadPython, python, err)
			}
		})
	}
}
