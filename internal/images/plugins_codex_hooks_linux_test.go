package images

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const codexCaptainID = "captain-hook@hooks"

var codexCaptainEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

func codexHooksHost(t *testing.T) (pluginsHost, Inventory) {
	t.Helper()
	inv := Inventory{
		Version: SchemaVersion,
		Claude: Claude{
			Marketplaces: []Marketplace{{Name: "hooks", GitHub: "owner/hooks", Branch: "main"}},
			Plugins:      []Plugin{{ID: codexCaptainID, Version: "12.79.15"}},
		},
		CodexRuntime: &CodexRuntime{Version: "1.0.0", URL: "https://example.invalid/runtime.tar.xz", SHA256: digest},
	}
	catalog := map[string]any{"github:owner/hooks#main": map[string]any{"name": "hooks", "plugins": map[string]string{"captain-hook": "12.79.15"}}}
	state := fakeState{
		Marketplaces: []map[string]any{{"name": "hooks", "source": "github", "repo": "owner/hooks", "ref": "main", "key": "github:owner/hooks#main", "snapshot": map[string]string{"captain-hook": "12.79.15"}}},
		Plugins:      []map[string]any{installedPlugin(codexCaptainID, "12.79.15")},
	}
	h := newPluginsHost(t, inv, catalog, state, map[string]any{"enabledPlugins": map[string]any{codexCaptainID: true}})
	root := filepath.Join(h.home, pluginCacheDir(inv.Claude.Plugins[0]))
	h.editState(func(s fakeState) { s.Plugins[0]["installPath"] = root })
	for path, data := range map[string]string{
		".cache/codex-runtimes/codex-primary-runtime/.cc-remote-digest": digest + "\n",
		".cache/codex-runtimes/codex-primary-runtime/runtime.json":      `{"bundleVersion":"1.0.0"}`,
		".codex/config.toml": "model = \"gpt-6.1-sol\"\n[features]\ncodex_hooks = true\n",
		".codex/auth.json":   `{"fixture":"preserve-auth"}`,
		".codex/plugins/cache/openai-primary-runtime/fixture": "runtime plugin",
		".claude/plugins/marketplaces/hooks/fixture":          "marketplace",
		".claude/plugins/known_marketplaces.json":             "{}",
	} {
		writePluginTestFile(t, filepath.Join(h.home, path), []byte(data), 0o600)
	}
	writePluginTestFile(t, filepath.Join(root, ".claude-plugin/plugin.json"), []byte(`{"name":"captain-hook","version":"12.79.15"}`), 0o600)
	writePluginTestFile(t, filepath.Join(root, "bin/hook"), []byte("#!/bin/sh\nexit 0\n"), 0o700)
	writePluginTestFile(t, filepath.Join(h.home, ".claude/plugins/installed_plugins.json"), mustJSON(t, map[string]any{
		"version": 2, "plugins": map[string]any{codexCaptainID: []any{map[string]any{"scope": "user", "installPath": root, "version": "12.79.15"}}},
	}), 0o600)
	return h, inv
}

func runtimeCodexHooks() map[string]any {
	hooks := map[string]any{}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "PermissionRequest", "SessionEnd", "PreCompact"} {
		hooks[event] = []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": "python3 /runtime/orca-owner/" + event + ".py", "timeout": 30}}}}
	}
	hooks["PreToolUse"] = append(hooks["PreToolUse"].([]any), map[string]any{"matcher": "Bash", "hooks": []any{
		map[string]any{"type": "command", "command": "foreign-first"},
		map[string]any{"type": "command", "command": "foreign-second"},
	}})
	return map[string]any{"hooks": hooks, "owner": "runtime"}
}

func readHookDocument(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func readHookFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPluginsCodexHooksPreserveRuntimeEntriesAndRepeatInstall(t *testing.T) {
	for _, tt := range []struct {
		name                string
		present, quotedRoot bool
	}{
		{name: "new hooks file"},
		{name: "eight Orca entries and foreign ordering", present: true},
		{name: "quoted metadata path", quotedRoot: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, inv := codexHooksHost(t)
			root := filepath.Join(h.home, pluginCacheDir(inv.Claude.Plugins[0]))
			if tt.quotedRoot {
				quoted := filepath.Join(h.home, "plugin's released root")
				if err := os.Rename(root, quoted); err != nil {
					t.Fatal(err)
				}
				root = quoted
				h.editState(func(s fakeState) { s.Plugins[0]["installPath"] = root })
			}
			path := filepath.Join(h.home, ".codex/hooks.json")
			want := map[string]any{"hooks": map[string]any{}}
			if tt.present {
				want = runtimeCodexHooks()
				writePluginTestFile(t, path, mustJSON(t, want), 0o600)
			}
			auth := readHookFile(t, filepath.Join(h.home, ".codex/auth.json"))
			if out, err := h.plugins("install"); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			hooks := want["hooks"].(map[string]any)
			for _, event := range codexCaptainEvents {
				entries, _ := hooks[event].([]any)
				hooks[event] = append(entries, map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "CAPT_HOOK_PROVIDER=codex " + quote(root+"/bin/hook") + " run " + event,
				}}})
			}
			installed := readHookFile(t, path)
			if got := readHookDocument(t, installed); !reflect.DeepEqual(got, readHookDocument(t, mustJSON(t, want))) {
				t.Fatalf("installed hooks = %s, want %s", installed, mustJSON(t, want))
			}
			if out, err := h.plugins("install"); err != nil {
				t.Fatalf("repeat install: %v\n%s", err, out)
			}
			if got := readHookFile(t, path); !bytes.Equal(got, installed) {
				t.Errorf("repeat install changed hooks: %s", got)
			}
			if got := readHookFile(t, filepath.Join(h.home, ".codex/auth.json")); !bytes.Equal(got, auth) {
				t.Error("install changed Codex auth")
			}
		})
	}
}

func TestPluginsCodexHooksRejectStalePluginRoot(t *testing.T) {
	for _, change := range []string{"missing executable", "wrong manifest version", "missing metadata path"} {
		t.Run(change, func(t *testing.T) {
			h, inv := codexHooksHost(t)
			root := filepath.Join(h.home, pluginCacheDir(inv.Claude.Plugins[0]))
			switch change {
			case "missing executable":
				if err := os.Remove(filepath.Join(root, "bin/hook")); err != nil {
					t.Fatal(err)
				}
			case "wrong manifest version":
				writePluginTestFile(t, filepath.Join(root, ".claude-plugin/plugin.json"), []byte(`{"name":"captain-hook","version":"12.79.14"}`), 0o600)
			case "missing metadata path":
				h.editState(func(s fakeState) { s.Plugins[0]["installPath"] = filepath.Join(h.home, "absent") })
			}
			path := filepath.Join(h.home, ".codex/hooks.json")
			before := mustJSON(t, runtimeCodexHooks())
			writePluginTestFile(t, path, before, 0o600)
			if out, err := h.plugins("install"); exitCode(err) != 1 || !strings.Contains(out, "is not executable at 12.79.15") {
				t.Fatalf("install accepted %s: %v\n%s", change, err, out)
			}
			if got := readHookFile(t, path); !bytes.Equal(got, before) {
				t.Error("failed install changed live hooks")
			}
		})
	}
}

func packCodexHooks(t *testing.T, h pluginsHost, inv Inventory) (string, string, error) {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{
		"id": "#!/bin/sh\necho 0\n", "apt-get": "#!/bin/sh\nexit 0\n", "mksquashfs": "#!/bin/sh\nexec cat > \"$2\"\n",
	} {
		writePluginTestFile(t, filepath.Join(h.fakes, name), []byte(content), 0o700)
	}
	scripts, err := Render(inv, "agents")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(scripts.ProvisionScript), "/var/lib/cc-remote/build", filepath.Join(root, "build"))
	writePluginTestFile(t, filepath.Join(h.fakes, "provision.sh"), []byte(script), 0o700)
	cmd := h.command("bash", filepath.Join(h.fakes, "provision.sh"), "pack", "tools-fingerprint")
	cmd.Env = append(cmd.Env, "SUDO_USER=builder")
	out, err := cmd.CombinedOutput()
	return filepath.Join(root, "build/payload.sqfs"), string(out), err
}

func TestProvisionPackCapturesOnlyGenericCodexHooks(t *testing.T) {
	h, inv := codexHooksHost(t)
	path := filepath.Join(h.home, ".codex/hooks.json")
	writePluginTestFile(t, path, mustJSON(t, runtimeCodexHooks()), 0o600)
	if out, err := h.plugins("install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	live := readHookFile(t, path)
	configPath := filepath.Join(h.home, ".codex/config.toml")
	config := readHookFile(t, configPath)
	archive, out, err := packCodexHooks(t, h, inv)
	if err != nil {
		t.Fatalf("pack: %v\n%s", err, out)
	}
	reader := tar.NewReader(bytes.NewReader(readHookFile(t, archive)))
	var captured []byte
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(header.Name, "/.codex/auth.json") || strings.Contains(header.Name, "orca-owner") {
			t.Errorf("packed per-host file %s", header.Name)
		}
		if header.Name == strings.TrimPrefix(path, "/") {
			if captured != nil {
				t.Fatal("packed hooks twice")
			}
			captured, err = io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	doc := readHookDocument(t, captured)
	hooks := doc["hooks"].(map[string]any)
	if len(doc) != 1 || len(hooks) != 5 || bytes.Contains(captured, []byte("orca-owner")) || bytes.Contains(captured, []byte("foreign-")) {
		t.Fatalf("captured runtime hooks or metadata: %s", captured)
	}
	liveHooks := readHookDocument(t, live)["hooks"].(map[string]any)
	for _, event := range codexCaptainEvents {
		entries := liveHooks[event].([]any)
		if !reflect.DeepEqual(hooks[event], []any{entries[len(entries)-1]}) {
			t.Errorf("captured %s = %v, want the installed generic entry", event, hooks[event])
		}
	}
	for file, want := range map[string][]byte{path: live, configPath: config} {
		if got := readHookFile(t, file); !bytes.Equal(got, want) {
			t.Errorf("pack changed %s", file)
		}
	}
}

func TestProvisionPackRejectsStaleCodexHookSources(t *testing.T) {
	for _, change := range []string{"registry version", "registry path", "manifest version", "handler path", "duplicate handler", "extra handler field"} {
		t.Run(change, func(t *testing.T) {
			h, inv := codexHooksHost(t)
			if out, err := h.plugins("install"); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			root := filepath.Join(h.home, pluginCacheDir(inv.Claude.Plugins[0]))
			path := filepath.Join(h.home, ".codex/hooks.json")
			switch change {
			case "registry version", "registry path":
				registry := filepath.Join(h.home, ".claude/plugins/installed_plugins.json")
				doc := readHookDocument(t, readHookFile(t, registry))
				entry := doc["plugins"].(map[string]any)[codexCaptainID].([]any)[0].(map[string]any)
				if change == "registry version" {
					entry["version"] = "12.79.14"
				} else {
					entry["installPath"] = filepath.Join(h.home, "runtime/other-captain")
				}
				writePluginTestFile(t, registry, mustJSON(t, doc), 0o600)
			case "manifest version":
				writePluginTestFile(t, filepath.Join(root, ".claude-plugin/plugin.json"), []byte(`{"name":"captain-hook","version":"12.79.14"}`), 0o600)
			default:
				doc := readHookDocument(t, readHookFile(t, path))
				hooks := doc["hooks"].(map[string]any)
				entries := hooks["Stop"].([]any)
				entry := entries[0].(map[string]any)
				if change == "duplicate handler" {
					hooks["Stop"] = append(entries, entry)
				} else if change == "extra handler field" {
					entry["runtimeOwner"] = "other-host"
				} else {
					entry["hooks"].([]any)[0].(map[string]any)["command"] = "CAPT_HOOK_PROVIDER=codex '/runtime/other-captain/bin/hook' run Stop"
				}
				writePluginTestFile(t, path, mustJSON(t, doc), 0o600)
			}
			before := readHookFile(t, path)
			archive, out, err := packCodexHooks(t, h, inv)
			if exitCode(err) == 0 {
				t.Fatalf("pack accepted %s: %s", change, out)
			}
			if _, err := os.Stat(archive); !os.IsNotExist(err) {
				t.Errorf("failed pack published an archive: %v", err)
			}
			if got := readHookFile(t, path); !bytes.Equal(got, before) {
				t.Error("pack changed live hooks")
			}
		})
	}
}

func TestProvisionPackRejectsCodexRuntimeTrustInTOML(t *testing.T) {
	for _, tt := range []struct {
		name, config string
		wantErr      bool
	}{
		{name: "clean generic config", config: "model = \"gpt-6.1-sol\"\n[hooks]\nenabled = true\n[features]\ncodex_hooks = true\n"},
		{name: "hooks text is not state", config: "instructions = 'hooks.state is configured by native review'\n# [hooks.state]\n"},
		{name: "trust table", config: "[hooks.state]\ntrusted_hashes = ['fixture']\n", wantErr: true},
		{name: "inline trust", config: "hooks = { state = { trusted_hashes = ['fixture'] } }\n", wantErr: true},
		{name: "inline runtime handler", config: "[[hooks.Stop]]\n[[hooks.Stop.hooks]]\ntype = 'command'\ncommand = '/runtime/orca-owner/stop'\n", wantErr: true},
		{name: "profile runtime handler", config: "[profiles.runtime.hooks]\nStop = [{ hooks = [{ type = 'command', command = '/runtime/orca-owner/stop' }] }]\n", wantErr: true},
		{name: "project trust", config: "[projects.'/runtime/workspace']\ntrust_level = 'trusted'\n", wantErr: true},
		{name: "invalid TOML", config: "hooks = {", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, inv := codexHooksHost(t)
			if out, err := h.plugins("install"); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			path := filepath.Join(h.home, ".codex/config.toml")
			writePluginTestFile(t, path, []byte(tt.config), 0o600)
			archive, out, err := packCodexHooks(t, h, inv)
			if (err != nil) != tt.wantErr {
				t.Fatalf("pack = %v\n%s, want error %v", err, out, tt.wantErr)
			}
			if tt.wantErr {
				if _, err := os.Stat(archive); !os.IsNotExist(err) {
					t.Errorf("rejected config produced an archive: %v", err)
				}
			}
			if got := readHookFile(t, path); string(got) != tt.config {
				t.Error("pack changed live config")
			}
		})
	}
}
