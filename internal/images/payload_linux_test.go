package images

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	fakeMount      = `#!/bin/bash
set -euo pipefail
printf '%s\n' "$5" >> "$TEST_ROOT/mounts"
cp "$5" "$TEST_ROOT/loop0"
printf '%s\n' "$TEST_ROOT/loop0" > "$6/.source"
jq -n --arg tools "$FINGERPRINT" --arg home "$(getent passwd "$SUDO_USER" | cut -d: -f6)" --arg arch "$(uname -m)" --arg os "$(. /etc/os-release && printf '%s' "$VERSION_ID")" \
  '{schemaVersion: 1, tools: $tools, home: $home, arch: $arch, os: $os}' > "$6/cc-remote-payload.json"
touch "$6/.mounted"
`
)

var stockSettings = map[string]any{
	"permissions": map[string]any{"allow": []any{"Bash(git status:*)"}},
	"hooks":       map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "true"}}}}},
}

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
				for _, rel := range []string{payloadTool, payloadHook + "/capt-hook"} {
					if err := os.MkdirAll(filepath.Dir(filepath.Join(h.home, rel)), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(root, "older", h.home, rel), filepath.Join(h.home, rel)); err != nil {
						t.Fatal(err)
					}
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

func TestPluginsInstallOverStockSettingsNeedsThePayloadMerge(t *testing.T) {
	pins := map[string]any{"hook@tools-market": true}
	tests := []struct {
		name        string
		merge       bool
		wantErr     string
		absentCalls []string
	}{
		{name: "without the merge", wantErr: "plugins: the installed, enabled and loadable plugins differ from the pins", absentCalls: []string{"claude plugin install", "claude plugin update"}},
		{name: "after the merge", merge: true, absentCalls: []string{"claude plugin install", "claude plugin update", "claude plugin marketplace update"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installed := fakeState{
				Marketplaces: []map[string]any{{"name": "tools-market", "source": "directory", "path": "HOME/" + payloadMarket, "key": "dir:tools-market", "snapshot": map[string]string{"hook": "1.0.0"}}},
				Plugins:      []map[string]any{installedPlugin("hook@tools-market", "1.0.0")},
			}
			h := newPluginsHost(t, payloadInventory(), marketplaceCatalog("0.7.17"), installed, stockSettings)
			settings := filepath.Join(h.home, ".claude", "settings.json")
			if err := os.Chmod(settings, 0o644); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(t.TempDir(), digest)
			writePayload(t, payload, h.home, "")
			writePluginTestFile(t, filepath.Join(payload, settings), mustJSON(t, map[string]any{"enabledPlugins": pins}), 0o644)
			writePluginTestFile(t, filepath.Join(payload, h.home, ".claude/plugins/installed_plugins.json"), mustJSON(t, map[string]any{
				"version": 2,
				"plugins": map[string]any{"hook@tools-market": []any{map[string]any{"scope": "user", "installPath": filepath.Join(h.home, payloadCache), "version": "1.0.0"}}},
			}), 0o644)
			writePluginTestFile(t, filepath.Join(h.fakes, "curl"), []byte(failingCurl), 0o700)
			writePluginTestFile(t, filepath.Join(h.home, ".local/share/captain-hook/host/version.json"), []byte(installedHooks), 0o600)
			if tt.merge {
				if out, err := h.run("sh", "-c", enablePayloadPlugins, "enable-payload-plugins", payload); err != nil {
					t.Fatalf("enable payload plugins: %v\n%s", err, out)
				}
			}
			out, err := h.plugins("install", payload)
			calls := h.calls()
			for _, absent := range tt.absentCalls {
				if slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, absent) }) {
					t.Errorf("calls include %q:\n%s", absent, strings.Join(calls, "\n"))
				}
			}
			if tt.wantErr != "" {
				if exitCode(err) != 1 || !strings.Contains(out, tt.wantErr) {
					t.Fatalf("install = %v\n%s\nwant exit 1 with %q", err, out, tt.wantErr)
				}
				if got, err := os.ReadFile(settings); err != nil || string(got) != string(mustJSON(t, stockSettings)) {
					t.Errorf("settings = %q, %v; want the stock file untouched", got, err)
				}
				h.unready()
				return
			}
			if err != nil {
				t.Fatalf("install failed: %v\n%s\ncalls:\n%s", err, out, strings.Join(calls, "\n"))
			}
			got := h.settings()
			want := map[string]any{"permissions": stockSettings["permissions"], "hooks": stockSettings["hooks"], "enabledPlugins": pins}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("settings = %v, want %v", got, want)
			}
		})
	}
}

func TestEnablePayloadPluginsMergesOnlyTheImagesEnabledPlugins(t *testing.T) {
	hooks := []byte(`{"hooks":{}}`)
	userFile := func(user, _ string) string { return user }
	imageFile := func(_, image string) string { return image }
	tests := []struct {
		name    string
		user    []byte
		image   []byte
		want    map[string]any
		refuses func(user, image string) string
	}{
		{
			name: "stock settings gain only the image's enabled plugins",
			user: mustJSON(t, stockSettings),
			image: mustJSON(t, map[string]any{
				"enabledPlugins":         map[string]any{"a@m": true, "b@m": true},
				"permissions":            map[string]any{"allow": []any{"Bash(*)"}},
				"extraKnownMarketplaces": map[string]any{"m": map[string]any{"source": map[string]any{"source": "github", "repo": "owner/m"}}},
				"env":                    map[string]any{"IMAGE": "1"},
			}),
			want: map[string]any{
				"permissions":    stockSettings["permissions"],
				"hooks":          stockSettings["hooks"],
				"enabledPlugins": map[string]any{"a@m": true, "b@m": true},
			},
		},
		{
			name:  "image flags win and other user flags stay",
			user:  []byte(`{"enabledPlugins":{"a@m":false,"c@m":true}}`),
			image: []byte(`{"enabledPlugins":{"a@m":true}}`),
			want:  map[string]any{"enabledPlugins": map[string]any{"a@m": true, "c@m": true}},
		},
		{
			name:  "an image without enabledPlugins adds an empty map",
			user:  hooks,
			image: hooks,
			want:  map[string]any{"hooks": map[string]any{}, "enabledPlugins": map[string]any{}},
		},
		{name: "without user settings nothing is written", image: []byte(`{"enabledPlugins":{"a@m":true}}`)},
		{name: "an image without settings leaves the user's file byte-identical", user: mustJSON(t, stockSettings)},
		{name: "a null user file is refused", user: []byte("null"), image: hooks, refuses: userFile},
		{name: "a user file with two documents is refused", user: []byte("{}{}"), image: hooks, refuses: userFile},
		{name: "a user file whose enabledPlugins is false is refused", user: []byte(`{"enabledPlugins":false}`), image: hooks, refuses: userFile},
		{name: "a user file whose enabledPlugins is null is refused", user: []byte(`{"enabledPlugins":null}`), image: hooks, refuses: userFile},
		{name: "a user file whose enabledPlugins is an array is refused", user: []byte(`{"enabledPlugins":[]}`), image: hooks, refuses: userFile},
		{name: "an empty user file is refused", user: []byte{}, image: hooks, refuses: userFile},
		{name: "a user file with a syntax error is refused", user: []byte("{"), image: hooks, refuses: userFile},
		{name: "an image whose enabledPlugins is null is refused", user: hooks, image: []byte(`{"enabledPlugins":null}`), refuses: imageFile},
		{name: "an image with two documents is refused", user: hooks, image: []byte(`{"enabledPlugins":{"a@m":true}}{"enabledPlugins":{"b@m":true}}`), refuses: imageFile},
		{name: "an image array is refused", user: hooks, image: []byte("[]"), refuses: imageFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, home := t.TempDir(), t.TempDir()
			settings := filepath.Join(home, ".claude", "settings.json")
			image := filepath.Join(payload, settings)
			if tt.user != nil {
				writePluginTestFile(t, settings, tt.user, 0o644)
				if err := os.Chmod(settings, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.image != nil {
				writePluginTestFile(t, image, tt.image, 0o644)
			}
			cmd := exec.Command("sh", "-c", enablePayloadPlugins, "enable-payload-plugins", payload)
			cmd.Env = append(os.Environ(), "HOME="+home)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			err := cmd.Run()
			if tt.refuses != nil {
				want := "cc-remote: " + tt.refuses(settings, image) + " is not one JSON object whose enabledPlugins, when present, is an object"
				if exitCode(err) != 1 || !strings.Contains(stderr.String(), want) {
					t.Errorf("enable payload plugins = %v\n%s\nwant exit 1 with %q", err, stderr.String(), want)
				}
			} else if err != nil {
				t.Fatalf("enable payload plugins: %v\n%s", err, stderr.String())
			}
			if leftovers, err := filepath.Glob(settings + ".*"); err != nil || len(leftovers) != 0 {
				t.Errorf("left %q, %v beside the settings", leftovers, err)
			}
			after, err := os.ReadFile(settings)
			if tt.user == nil {
				if !os.IsNotExist(err) {
					t.Errorf("wrote settings %q, %v; want none", after, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(settings)
			if err != nil {
				t.Fatal(err)
			}
			if tt.image == nil || tt.refuses != nil {
				if string(after) != string(tt.user) || info.Mode().Perm() != 0o644 {
					t.Errorf("settings became %q mode %v, want the untouched %q mode 0644", after, info.Mode().Perm(), tt.user)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(after, &got); err != nil {
				t.Fatalf("decode %q: %v", after, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("settings = %v, want %v", got, tt.want)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("settings mode = %v, want 0600", info.Mode().Perm())
			}
		})
	}
}

func TestProvisionPayloadVerifiesEveryImageBeforeItMounts(t *testing.T) {
	good, bad := "hsqs verified payload", "hsqs tampered payload"
	sum := sha256.Sum256([]byte(good))
	sha := hex.EncodeToString(sum[:])
	staged, stored, loop := filepath.Join("store", sha+".sqfs.partial"), filepath.Join("store", sha+".sqfs"), "loop0"
	tests := []struct {
		name    string
		boot    bool
		partial string
		cached  string
		mounted string
		hashed  []string
		mounts  bool
		wantErr string
		after   string
	}{
		{name: "a staged payload is verified once and mounted", partial: good, hashed: []string{staged}, mounts: true, after: good},
		{name: "a staged payload replaces a corrupt cached image", partial: good, cached: bad, hashed: []string{staged}, mounts: true, after: good},
		{name: "a staged payload replaces the file under a verified mount", partial: good, cached: bad, mounted: good, hashed: []string{staged, loop}, after: good},
		{name: "a staged payload does not vouch for a tampered mount", partial: good, cached: bad, mounted: bad, hashed: []string{staged, loop}, wantErr: "cc-remote: %[3]s does not match its sha256 %[2]s", after: good},
		{name: "a corrupt staged payload is fatal", partial: bad, hashed: []string{staged}, wantErr: "cc-remote: %[1]s.partial does not match its sha256 %[2]s"},
		{name: "a cached image is verified before it mounts", cached: good, hashed: []string{stored}, mounts: true, after: good},
		{name: "a corrupt cached image is fatal", cached: bad, hashed: []string{stored}, wantErr: "cc-remote: %[1]s does not match its sha256 %[2]s", after: bad},
		{name: "an existing mount is verified through its loop device", cached: good, mounted: good, hashed: []string{loop}, after: good},
		{name: "a tampered existing mount is fatal", cached: good, mounted: bad, hashed: []string{loop}, wantErr: "cc-remote: %[3]s does not match its sha256 %[2]s", after: good},
		{name: "nothing staged is fatal", wantErr: "cc-remote: no payload is staged at %[1]s.partial"},
		{name: "boot verifies a stored image before it mounts", boot: true, cached: good, hashed: []string{stored}, mounts: true, after: good},
		{name: "boot refuses a corrupt stored image", boot: true, cached: bad, hashed: []string{stored}, wantErr: "cc-remote: %[1]s does not match its sha256 %[2]s", after: bad},
		{name: "boot verifies an existing mount through its loop device", boot: true, cached: good, mounted: good, hashed: []string{loop}, after: good},
		{name: "boot refuses a tampered existing mount", boot: true, cached: good, mounted: bad, hashed: []string{loop}, wantErr: "cc-remote: %[3]s does not match its sha256 %[2]s", after: good},
	}
	scripts, err := Render(Inventory{Version: SchemaVersion}, "agents")
	if err != nil {
		t.Fatal(err)
	}
	_, helper, opened := strings.Cut(string(scripts.ProvisionScript), "<<'SH'\n")
	helper, _, closed := strings.Cut(helper, "\nSH\n")
	if !opened || !closed {
		t.Fatalf("the provision script writes no boot helper:\n%s", scripts.ProvisionScript)
	}
	sha256sum, err := exec.LookPath("sha256sum")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes := t.TempDir(), t.TempDir()
			store, payloads := filepath.Join(root, "store"), filepath.Join(root, "payload")
			image, dir := filepath.Join(store, sha+".sqfs"), filepath.Join(payloads, sha)
			for name, content := range map[string]string{
				"id":         "#!/bin/sh\necho 0\n",
				"mountpoint": "#!/bin/sh\n[ -e \"$2/.mounted\" ]\n",
				"findmnt":    "#!/bin/sh\n[ \"$*\" = \"-no SOURCE $3\" ] || exit 2\nexec cat \"$3/.source\"\n",
				"mount":      fakeMount,
				"sprite-env": "#!/bin/sh\nexit 0\n",
				"sha256sum":  "#!/bin/sh\nline=\"$(cat)\"\nprintf '%s\\n' \"$line\" >> \"$TEST_ROOT/hashes\"\nprintf '%s\\n' \"$line\" | exec " + sha256sum + " \"$@\"\n",
			} {
				writePluginTestFile(t, filepath.Join(fakes, name), []byte(content), 0o700)
			}
			if err := os.MkdirAll(store, 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.partial != "" {
				writePluginTestFile(t, image+".partial", []byte(tt.partial), 0o644)
			}
			if tt.cached != "" {
				writePluginTestFile(t, image, []byte(tt.cached), 0o644)
			}
			env := append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root, "SUDO_USER=root", "FINGERPRINT=tools-fingerprint")
			if tt.mounted != "" {
				served := filepath.Join(root, "served.sqfs")
				writePluginTestFile(t, served, []byte(tt.mounted), 0o644)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				premount := exec.Command(filepath.Join(fakes, "mount"), "-t", "squashfs", "-o", "ro,loop", served, dir)
				premount.Env = env
				if out, err := premount.CombinedOutput(); err != nil {
					t.Fatalf("premount: %v\n%s", err, out)
				}
				if err := os.Remove(filepath.Join(root, "mounts")); err != nil {
					t.Fatal(err)
				}
			}
			var cmd *exec.Cmd
			if tt.boot {
				cmd = exec.Command("sh", "-c", strings.NewReplacer(
					"/var/lib/cc-remote/payload/", store+"/",
					"/opt/cc-remote/payload/", payloads+"/",
				).Replace(helper))
			} else {
				provision := strings.NewReplacer(
					"payload_root=/opt/cc-remote/payload\n", "payload_root="+quote(payloads)+"\n",
					"payload_store=/var/lib/cc-remote/payload\n", "payload_store="+quote(store)+"\n",
					` /opt/cc-remote/payload-mount.sh`+"\n", " "+quote(filepath.Join(root, "payload-mount.sh"))+"\n",
				).Replace(string(scripts.ProvisionScript))
				cmd = exec.Command("bash", "-c", provision, "provision.sh", PhasePayload, sha, "tools-fingerprint")
			}
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if tt.wantErr != "" {
				if want := fmt.Sprintf(tt.wantErr, image, sha, filepath.Join(root, loop)); exitCode(err) != 1 || !strings.Contains(string(out), want) {
					t.Fatalf("payload = %v\n%s\nwant exit 1 with %q", err, out, want)
				}
			} else if err != nil {
				t.Fatalf("payload failed: %v\n%s", err, out)
			}
			var hashed []string
			for _, rel := range tt.hashed {
				hashed = append(hashed, sha+"  "+filepath.Join(root, rel))
			}
			if got := logLines(t, filepath.Join(root, "hashes")); !slices.Equal(got, hashed) {
				t.Errorf("hashed %q, want %q", got, hashed)
			}
			var mounted []string
			if tt.mounts {
				mounted = []string{image}
			}
			if got := logLines(t, filepath.Join(root, "mounts")); !slices.Equal(got, mounted) {
				t.Errorf("mounted %q, want %q", got, mounted)
			}
			cached, err := os.ReadFile(image)
			switch {
			case tt.after == "" && !os.IsNotExist(err):
				t.Errorf("the cache holds %q, %v; want no image", cached, err)
			case tt.after != "" && (err != nil || string(cached) != tt.after):
				t.Errorf("the cache holds %q, %v; want %q", cached, err, tt.after)
			}
			if _, err := os.Stat(image + ".partial"); tt.wantErr == "" && !os.IsNotExist(err) {
				t.Errorf("the staged payload outlived a successful phase: %v", err)
			}
		})
	}
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
