package images

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProbeChecksExecutabilityByLinkMode(t *testing.T) {
	scripts, err := Render(scriptInventory(), "agents")
	if err != nil {
		t.Fatal(err)
	}
	plugins := string(scripts.Plugins)
	start := strings.Index(plugins, "\nprobe() {\n")
	if start < 0 {
		t.Fatal("plugins.sh with apt.payload defines no probe")
	}
	probe := plugins[start+1 : start+strings.Index(plugins[start:], "\n}\n")+3]
	tests := []struct {
		name     string
		check    string
		mode     os.FileMode
		body     string
		args     []string
		wantExit int
		wantOut  string
		wantRan  string
	}{
		{name: "full runs the target with its verify args", check: "full", mode: 0o755, args: []string{"--version"}, wantRan: "--version"},
		{name: "full propagates a failing target", check: "full", mode: 0o755, body: "exit 73\n", args: []string{"--version"}, wantExit: 73, wantRan: "--version"},
		{name: "full without verify args leaves the target unrun", check: "full", mode: 0o755},
		{name: "resolved accepts an executable target without running it", check: "resolved", mode: 0o755, args: []string{"--version"}},
		{name: "resolved rejects a non-executable target", check: "resolved", mode: 0o644, args: []string{"--version"}, wantExit: 1, wantOut: " is not executable"},
		{name: "resolved rejects a dangling link", check: "resolved", args: []string{"--version"}, wantExit: 1, wantOut: " is not executable"},
		{name: "spelling neither checks nor runs the target", check: "spelling", mode: 0o644, args: []string{"--version"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			target, link, marker := filepath.Join(root, "target"), filepath.Join(root, "link"), filepath.Join(root, "ran")
			if tt.mode != 0 {
				if err := os.WriteFile(target, []byte("#!/bin/sh\nprintf '%s' \"$*\" > "+quote(marker)+"\n"+tt.body), tt.mode); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			words := []string{"probe", quote(link)}
			for _, arg := range tt.args {
				words = append(words, quote(arg))
			}
			out, err := exec.Command("bash", "-c", "set -euo pipefail\nlink_check="+tt.check+"\n"+probe+strings.Join(words, " ")).CombinedOutput()
			if got := exitCode(err); got != tt.wantExit || got == 0 && err != nil {
				t.Fatalf("probe = %v\n%s\nwant exit %d", err, out, tt.wantExit)
			}
			if tt.wantOut != "" && !strings.Contains(string(out), "cc-remote: "+link+tt.wantOut) {
				t.Errorf("probe output = %q, want cc-remote: %s%s", out, link, tt.wantOut)
			}
			ran, err := os.ReadFile(marker)
			switch {
			case tt.wantRan != "" && (err != nil || string(ran) != tt.wantRan):
				t.Errorf("the target ran with %q, %v; want %q", ran, err, tt.wantRan)
			case tt.wantRan == "" && !os.IsNotExist(err):
				t.Errorf("the target ran with %q, %v; want it untouched", ran, err)
			}
		})
	}
}

func TestPluginsPublishProvesLinksWithoutRunningVersions(t *testing.T) {
	const stamp = "stamp-1"
	inventory := scriptInventory()
	inventory.System = []Artifact{{Name: "tool", Version: "1.0.0", URL: "https://example.com/tool", SHA256: digest, Format: Binary, Verify: []string{"--version"}}}
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		phase    string
		mutation string
		family   string
		env      []string
		wantErr  func(tools, bins string) string
		wantRan  []string
	}{
		{name: "publish proves every link without running a version", phase: "publish"},
		{name: "verify still runs every version", phase: "verify", wantRan: []string{"--version"}},
		{
			name: "publish rejects a repointed tool link", phase: "publish", mutation: "repointed",
			wantErr: func(tools, bins string) string {
				return "cc-remote: " + bins + "/tool does not point at the pinned " + tools + "/tool-1.0.0/tool"
			},
		},
		{
			name: "publish rejects a non-executable tool", phase: "publish", mutation: "tool-mode",
			wantErr: func(_, bins string) string { return "cc-remote: " + bins + "/tool is not executable" },
		},
		{
			name: "verify rejects a non-executable tool", phase: "verify", mutation: "tool-mode",
			wantErr: func(tools, bins string) string {
				return "cc-remote: " + bins + "/tool does not point at the pinned " + tools + "/tool-1.0.0/tool"
			},
		},
		{
			name: "publish rejects an unpinned tool", phase: "publish", mutation: "digest",
			wantErr: func(tools, _ string) string {
				return "cc-remote: " + tools + "/tool-1.0.0 is not installed at its pinned digest " + digest
			},
		},
		{
			name: "publish rejects a non-executable closure bin", phase: "publish", mutation: "closure-mode",
			wantErr: func(_, bins string) string { return "cc-remote: " + bins + "/fc-match is not executable" },
		},
		{
			name: "publish rejects a missing library", phase: "publish", env: []string{"LDSO_MISSING=libmissing.so.9"},
			wantErr: func(_, _ string) string { return "cannot load libmissing.so.9" },
		},
		{
			name: "publish rejects an inexact font", phase: "publish", family: "DejaVu Sans",
			wantErr: func(_, _ string) string { return "cc-remote: fc-match resolves Noto Sans CJK JP to DejaVu Sans" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes, home := t.TempDir(), t.TempDir(), shortHome(t)
			tools, bins, closure, office := filepath.Join(root, "tools"), filepath.Join(root, "bin"), filepath.Join(root, "closure"), filepath.Join(root, "soffice.bin")
			writeFakes(t, fakes, map[string]string{
				"ld.so":    fakeLdso,
				"fc-match": "#!/bin/sh\nprintf '%s' \"$FAMILY\"\n",
				"certutil": "#!/bin/sh\n",
			})
			elf := elfWithInterp(filepath.Join(fakes, "ld.so"))
			writePluginTestFile(t, filepath.Join(home, ".agent-browser/chrome"), elf, 0o755)
			writePluginTestFile(t, office, elf, 0o755)
			pin := digest
			if tt.mutation == "digest" {
				pin = strings.Repeat("0", 64)
			}
			writePluginTestFile(t, filepath.Join(tools, "tool-1.0.0", ".cc-remote-digest"), []byte(pin+"\n"), 0o644)
			mode := os.FileMode(0o755)
			if tt.mutation == "tool-mode" {
				mode = 0o644
			}
			writePluginTestFile(t, filepath.Join(tools, "tool-1.0.0", "tool"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_ROOT/tool.log\"\n"), mode)
			links := map[string]string{
				"tool":     filepath.Join(tools, "tool-1.0.0", "tool"),
				"certutil": filepath.Join(closure, "usr/bin/certutil"),
				"fc-match": filepath.Join(closure, "usr/bin/fc-match"),
			}
			if tt.mutation == "repointed" {
				links["tool"] = filepath.Join(fakes, "certutil")
			}
			for _, bin := range []string{"certutil", "fc-match"} {
				binMode := os.FileMode(0o755)
				if tt.mutation == "closure-mode" && bin == "fc-match" {
					binMode = 0o644
				}
				writePluginTestFile(t, filepath.Join(root, "tree/usr/bin", bin), elf, binMode)
			}
			if err := os.MkdirAll(bins, 0o755); err != nil {
				t.Fatal(err)
			}
			for bin, target := range links {
				if err := os.Symlink(target, filepath.Join(bins, bin)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(root, "tree"), closure); err != nil {
				t.Fatal(err)
			}
			plugins := strings.NewReplacer(
				"closure_root=/opt/cc-remote/closure\n", "closure_root="+quote(closure)+"\n",
				"system_tool_dir=/opt/cc-remote/tools\n", "system_tool_dir="+quote(tools)+"\n",
				"system_bin_dir=/usr/local/bin\n", "system_bin_dir="+quote(bins)+"\n",
				"'/opt/cc-remote/tools/office/soffice.bin'", quote(office),
			).Replace(string(scripts.Plugins))
			writePluginTestFile(t, filepath.Join(fakes, "plugins.sh"), []byte(plugins), 0o700)
			family := "Noto Sans CJK JP"
			if tt.family != "" {
				family = tt.family
			}
			cmd := exec.Command("bash", filepath.Join(fakes, "plugins.sh"), tt.phase, stamp)
			cmd.Env = append(append(os.Environ(), "HOME="+home, "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root, "FAMILY="+family), tt.env...)
			out, err := cmd.CombinedOutput()
			ready, readyErr := os.ReadFile(filepath.Join(home, ".cc-remote", "ready"))
			if got := logLines(t, filepath.Join(root, "tool.log")); !slices.Equal(got, tt.wantRan) {
				t.Errorf("%s ran the tool with %q, want %q", tt.phase, got, tt.wantRan)
			}
			if tt.wantErr != nil {
				if want := tt.wantErr(tools, bins); exitCode(err) != 1 || !strings.Contains(string(out), want) {
					t.Fatalf("%s = %v\n%s\nwant exit 1 with %q", tt.phase, err, out, want)
				}
				if !os.IsNotExist(readyErr) {
					t.Errorf("a failed %s wrote the ready stamp %q, %v", tt.phase, ready, readyErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", tt.phase, err, out)
			}
			if got, want := logLines(t, filepath.Join(root, "ldso.log")), []string{"ld.so " + bins + "/certutil", "ld.so " + bins + "/fc-match", "ld.so " + home + "/.agent-browser/chrome", "ld.so " + office}; !slices.Equal(got, want) {
				t.Errorf("ld.so calls = %q, want %q", got, want)
			}
			switch tt.phase {
			case "publish":
				if readyErr != nil || string(ready) != stamp+"\n" {
					t.Errorf("ready stamp = %q, %v, want %q", ready, readyErr, stamp+"\n")
				}
			case "verify":
				if !os.IsNotExist(readyErr) {
					t.Errorf("verify wrote the ready stamp %q, %v", ready, readyErr)
				}
			}
		})
	}
}
