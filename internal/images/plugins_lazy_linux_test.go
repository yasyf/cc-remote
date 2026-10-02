package images

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fakeUV = `#!/usr/bin/env python3
import json
import os
import sys

with open(os.environ["FAKE_LOG"] + ".uv", "a") as log:
    log.write(json.dumps(sys.argv[1:]) + "\n")
sys.exit(37)
`

const fakeLazyLauncher = `#!/bin/sh
ROOT="$(dirname "$(dirname "$0")")"
DESCRIPTOR="$ROOT/bin/tool.binrun"
RUNNER_BIN=binrun
exec "$RUNNER_BIN" "$DESCRIPTOR" "$@"
`

const fakeBinrun = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG.binrun"
exit 29
`

const trapBinrun = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG.binrun"
`

func TestPluginsInstallLeavesPythonUserToolsForFirstUse(t *testing.T) {
	for _, marketplace := range []bool{false, true} {
		t.Run(map[bool]string{false: "version", true: "marketplace"}[marketplace], func(t *testing.T) {
			tool := PythonTool{Name: "writer", Package: "writer[lab,scoring]", Version: "1.2.3", Bins: []string{"writer", "writer-score"}, Args: []string{"--torch-backend", "cpu"}, Verify: []string{"--version"}}
			inventory := Inventory{Version: SchemaVersion, Python: Python{Version: "3.13", User: []PythonTool{tool}}}
			catalog := map[string]any{}
			spec := "writer[lab,scoring]==1.2.3"
			if marketplace {
				inventory.Python.User[0].Version = ""
				inventory.Python.User[0].Marketplace = toolsRef.Name
				inventory.Claude.Marketplaces = []Marketplace{toolsRef}
				catalog["dir:"+toolsRef.Name] = map[string]any{"name": toolsRef.Name, "plugins": map[string]string{}}
			}
			h := newPluginsHost(t, inventory, catalog, fakeState{}, nil)
			if marketplace {
				spec = "writer[lab,scoring] @ git+file://" + filepath.Join(h.home, ".local/share/cc-remote/marketplaces", toolsRef.Name) + "@" + commit
			}
			writePluginTestFile(t, filepath.Join(h.fakes, "uv"), []byte(fakeUV), 0o755)
			for _, args := range [][]string{{"install"}, {"publish", "test-stamp"}, {"verify"}, {"ready", "test-stamp"}} {
				if out, err := h.plugins(args[0], args[1:]...); err != nil {
					t.Fatalf("%s: %v\n%s", args[0], err, out)
				}
			}
			uvLog := filepath.Join(h.fakes, "calls.log.uv")
			if _, err := os.Stat(uvLog); !os.IsNotExist(err) {
				t.Fatalf("startup invoked uv: %v", err)
			}
			if _, err := os.Stat(filepath.Join(h.home, ".cc-remote/uv-tools")); !os.IsNotExist(err) {
				t.Fatalf("startup materialized Python tools: %v", err)
			}
			for _, bin := range tool.Bins {
				launcher := filepath.Join(h.home, ".local/bin", bin)
				out, err := h.run(launcher, "draft one", "apostrophe's", "")
				if exitCode(err) != 37 {
					t.Fatalf("%s did not propagate uv's failure: %v\n%s", bin, err, out)
				}
			}
			raw, err := os.ReadFile(uvLog)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) != len(tool.Bins) {
				t.Fatalf("uv calls = %d, want %d", len(lines), len(tool.Bins))
			}
			for index, line := range lines {
				var got []string
				if err := json.Unmarshal([]byte(line), &got); err != nil {
					t.Fatal(err)
				}
				want := []string{"tool", "run", "--python", "3.13", "--torch-backend", "cpu", "--from", spec, tool.Bins[index], "draft one", "apostrophe's", ""}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("first use = %q, want %q", got, want)
				}
			}
			launcher := filepath.Join(h.home, ".local/bin/writer")
			writePluginTestFile(t, launcher, []byte("#!/bin/sh\nexec uv tool run unpinned\n"), 0o755)
			if out, err := h.plugins("verify"); err == nil || !strings.Contains(out, "pinned first-use launcher") {
				t.Fatalf("verify accepted a changed Python launcher: %v\n%s", err, out)
			}
			if after, err := os.ReadFile(uvLog); err != nil || string(after) != string(raw) {
				t.Fatalf("verify executed a changed Python launcher: %v\n%s", err, after)
			}
		})
	}
}

type lazyLayout struct {
	link         string
	untracked    []string
	unexecutable []string
	descriptor   []byte
	files        map[string]string
	services     []Service
}

var lazyLayouts = []struct {
	name   string
	layout lazyLayout
}{
	{"file", lazyLayout{}},
	{"symlink", lazyLayout{link: "../scripts/launch.sh"}},
}

func TestPluginsVerifyPinnedLazyLaunchersWithoutExecutingThem(t *testing.T) {
	for _, tt := range lazyLayouts {
		t.Run(tt.name, func(t *testing.T) {
			h := lazyPluginHost(t, "tool", fakeLazyLauncher, tt.layout)
			for _, args := range [][]string{{"install"}, {"publish", "test-stamp"}, {"verify"}, {"ready", "test-stamp"}} {
				if out, err := h.plugins(args[0], args[1:]...); err != nil {
					t.Fatalf("%s: %v\n%s", args[0], err, out)
				}
			}
			bin := filepath.Join(h.fakes, "plugins/tool/bin/tool")
			if tt.layout.link != "" {
				if err := os.Remove(bin); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(tt.layout.link, bin); err != nil {
					t.Fatal(err)
				}
				if out, err := h.plugins("verify"); err != nil {
					t.Fatalf("verify rejected the installed launcher symlink: %v\n%s", err, out)
				}
			}
			log := filepath.Join(h.fakes, "calls.log.binrun")
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatalf("startup invoked binrun: %v", err)
			}
			out, err := h.run(bin, "first-use")
			if exitCode(err) != 29 {
				t.Fatalf("first use did not propagate binrun's failure: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if want := bin + ".binrun first-use\n"; string(raw) != want {
				t.Fatalf("first use = %q, want %q", raw, want)
			}
		})
	}
}

func TestPluginsRejectChangedLazyLauncherPins(t *testing.T) {
	const drifted = "differs from its pinned launcher or descriptor"
	tests := []struct {
		change  string
		wantErr string
	}{
		{"launcher", drifted},
		{"descriptor", drifted},
		{"missing-descriptor", drifted},
		{"non-executable", "has no executable bin/tool"},
		{"checkout-ref", "is not checked out at"},
		{"invalid-descriptor", drifted},
		{"matching-launcher", drifted},
		{"matching-descriptor", drifted},
	}
	for _, layout := range lazyLayouts {
		for _, tt := range tests {
			t.Run(layout.name+"/"+tt.change, func(t *testing.T) {
				h := lazyPluginHost(t, "tool", fakeLazyLauncher, layout.layout)
				if out, err := h.plugins("install"); err != nil {
					t.Fatalf("install: %v\n%s", err, out)
				}
				bin := filepath.Join(h.fakes, "plugins/tool/bin/tool")
				source := filepath.Join(h.home, ".local/share/cc-remote/marketplaces/tools-market")
				switch tt.change {
				case "launcher":
					writePluginTestFile(t, bin, []byte(fakeLazyLauncher+"\n"), 0o755)
				case "descriptor":
					writePluginTestFile(t, bin+".binrun", []byte("{}\n"), 0o644)
				case "missing-descriptor":
					if err := os.Remove(bin + ".binrun"); err != nil {
						t.Fatal(err)
					}
				case "non-executable":
					if err := os.Chmod(bin, 0o644); err != nil {
						t.Fatal(err)
					}
				case "checkout-ref":
					writePluginTestFile(t, filepath.Join(source, ".fake-head"), []byte(strings.Repeat("b", 40)), 0o644)
				case "invalid-descriptor":
					for _, path := range []string{bin + ".binrun", filepath.Join(source, "plugin/bin/tool.binrun")} {
						writePluginTestFile(t, path, []byte("#!/usr/bin/env binrun\n{}\n"), 0o644)
					}
				case "matching-launcher":
					for _, path := range []string{bin, filepath.Join(source, "plugin/bin/tool")} {
						writePluginTestFile(t, path, []byte(fakeLazyLauncher+"exit 0\n"), 0o755)
					}
				case "matching-descriptor":
					for _, path := range []string{bin + ".binrun", filepath.Join(source, "plugin/bin/tool.binrun")} {
						writePluginTestFile(t, path, binrunDescriptor(t, strings.Repeat("3", 64)), 0o644)
					}
				}
				out, err := h.plugins("verify")
				if err == nil || !strings.Contains(out, tt.wantErr) {
					t.Fatalf("verify = %v\n%s\nwant failure containing %q", err, out, tt.wantErr)
				}
				if _, err := os.Stat(filepath.Join(h.fakes, "calls.log.binrun")); !os.IsNotExist(err) {
					t.Fatalf("verify invoked binrun: %v", err)
				}
			})
		}
	}
}

func TestPluginsRejectLazyLaunchersExecutableOnlyOutsideTheirPin(t *testing.T) {
	tests := []struct {
		name   string
		layout lazyLayout
	}{
		{"file", lazyLayout{unexecutable: []string{"plugin/bin/tool"}}},
		{"symlink", lazyLayout{link: "../scripts/launch.sh", unexecutable: []string{"plugin/scripts/launch.sh"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := lazyPluginHost(t, "tool", fakeLazyLauncher, tt.layout)
			out, err := h.plugins("install")
			if exitCode(err) != 1 || !strings.Contains(out, "differs from its pinned launcher or descriptor") {
				t.Fatalf("install accepted a launcher its pin does not make executable: %v\n%s", err, out)
			}
			h.unready()
			if _, err := os.Stat(filepath.Join(h.fakes, "calls.log.binrun")); !os.IsNotExist(err) {
				t.Fatalf("install invoked binrun: %v", err)
			}
		})
	}
}

func TestPluginsRejectLazyLaunchersTheirPinDoesNotCommit(t *testing.T) {
	tests := []struct {
		name    string
		layout  lazyLayout
		outside string
		wantErr string
	}{
		{name: "untracked-descriptor", layout: lazyLayout{untracked: []string{"plugin/bin/tool.binrun"}}, wantErr: "differs from its pinned launcher or descriptor"},
		{name: "untracked-target", layout: lazyLayout{link: "../scripts/launch.sh", untracked: []string{"plugin/scripts/launch.sh"}}, wantErr: "is not a committed file at"},
		{name: "escaping-target", layout: lazyLayout{link: "../../../launch.sh"}, outside: ".local/share/cc-remote/marketplaces/launch.sh", wantErr: "is not a committed file at"},
		{name: "absolute-target", layout: lazyLayout{link: filepath.Join(t.TempDir(), "launch.sh")}, wantErr: "is not a committed file at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := lazyPluginHost(t, "tool", fakeLazyLauncher, tt.layout)
			writePluginTestFile(t, filepath.Join(h.fakes, "binrun"), []byte(trapBinrun), 0o755)
			if tt.outside != "" {
				writePluginTestFile(t, filepath.Join(h.home, tt.outside), []byte(fakeLazyLauncher), 0o755)
			}
			out, err := h.plugins("install")
			if exitCode(err) != 1 || !strings.Contains(out, tt.wantErr) {
				t.Fatalf("install = %v\n%s\nwant exit 1 containing %q", err, out, tt.wantErr)
			}
			h.unready()
			if _, err := os.Stat(filepath.Join(h.fakes, "calls.log.binrun")); !os.IsNotExist(err) {
				t.Fatalf("install executed the installed launcher: %v", err)
			}
		})
	}
}

func TestPluginsKeepRuntimeProbesForMandatoryAndNonLazyBins(t *testing.T) {
	for _, plugin := range []string{"tool", "captain-hook"} {
		t.Run(plugin, func(t *testing.T) {
			launcher := fakeLazyLauncher
			if plugin == "tool" {
				launcher = "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$FAKE_LOG.probe\"\nexit 29\n"
			}
			h := lazyPluginHost(t, plugin, launcher, lazyLayout{})
			out, err := h.plugins("install")
			if exitCode(err) != 29 {
				t.Fatalf("install did not propagate the runtime probe failure: %v\n%s", err, out)
			}
			h.unready()
		})
	}
}

func lazyPluginHost(t *testing.T, plugin, launcher string, layout lazyLayout) pluginsHost {
	t.Helper()
	source := filepath.Join(t.TempDir(), "tools-market")
	manifest := map[string]any{"plugins": []map[string]string{{"name": plugin, "source": "./plugin"}}}
	writePluginTestFile(t, filepath.Join(source, ".claude-plugin/marketplace.json"), mustJSON(t, manifest), 0o644)
	bin := filepath.Join(source, "plugin/bin/tool")
	descriptor := layout.descriptor
	if descriptor == nil {
		descriptor = binrunDescriptor(t, digest)
	}
	writePluginTestFile(t, bin+".binrun", descriptor, 0o644)
	if layout.link == "" {
		writePluginTestFile(t, bin, []byte(launcher), 0o755)
	} else {
		target := layout.link
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(bin), target)
		}
		writePluginTestFile(t, target, []byte(launcher), 0o755)
		if err := os.Symlink(layout.link, bin); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range layout.files {
		writePluginTestFile(t, filepath.Join(source, rel), []byte(content), 0o755)
	}
	ref := commitPin(t, source, layout)
	inventory := Inventory{Version: SchemaVersion, Claude: Claude{Marketplaces: []Marketplace{{Name: "tools-market", GitHub: "owner/tools-market", Ref: ref}}, Plugins: []Plugin{{ID: plugin + "@tools-market", Version: "1.0.0", Bins: []string{"bin/tool"}}}}, Services: layout.services}
	catalog := map[string]any{"dir:tools-market": map[string]any{"name": "tools-market", "plugins": map[string]string{plugin: "1.0.0"}}}
	h := newPluginsHost(t, inventory, catalog, fakeState{}, nil)
	if err := os.Mkdir(filepath.Join(h.fakes, "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, filepath.Join(h.fakes, "sources/tools-market")); err != nil {
		t.Fatal(err)
	}
	writePluginTestFile(t, filepath.Join(h.fakes, "binrun"), []byte(fakeBinrun), 0o755)
	return h
}

func commitPin(t *testing.T, dir string, layout lazyLayout) string {
	t.Helper()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", "-A")
	for _, path := range layout.untracked {
		git("rm", "-q", "--cached", path)
	}
	for _, path := range layout.unexecutable {
		git("update-index", "--chmod=-x", path)
	}
	git("commit", "-q", "-m", "pin")
	return git("rev-parse", "HEAD")
}

func binrunDescriptor(t *testing.T, sha256 string) []byte {
	t.Helper()
	return append([]byte("#!/usr/bin/env binrun\n"), mustJSON(t, releaseBinary(sha256))...)
}

func releaseBinary(sha256 string) map[string]any {
	return map[string]any{
		"schema":  1,
		"kind":    "release-binary",
		"name":    "tool",
		"version": map[string]string{"static": "1.0.0"},
		"platforms": map[string]any{"linux-x86_64": map[string]any{
			"size":      123,
			"hash":      "sha256",
			"digest":    sha256,
			"path":      "tool",
			"format":    "tar.gz",
			"providers": []map[string]string{{"type": "github-release", "repo": "owner/tool", "tag": "v1.0.0", "name": "tool_1.0.0_linux_amd64.tar.gz"}},
		}},
	}
}

func writePluginTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
