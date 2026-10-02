package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	nativeRunnerTag = "v0.8.0"
	nativeCache     = ".claude/plugins/cache/tools-market/hook/1.0.0"
	nativeAssetURL  = "https://github.com/owner/tool/releases/download/v1.0.0/tool_1.0.0_linux_amd64.tar.gz"
	runnerAssetURL  = "https://github.com/owner/binrun/releases/download/v0.8.0/binrun_0.8.0_linux_amd64.tar.gz"
)

var nativeMembers = map[string]string{"tool": "#!/bin/sh\necho tool\n", "LICENSE": "MIT\n"}

const fakeGetent = `#!/bin/sh
[ "$1" = passwd ] || exit 2
printf '%s:x:1000:1000::%s:/bin/sh\n' "$2" "$HOME"
`

const servingCurl = `#!/bin/sh
echo "curl $*" >> "$FAKE_LOG"
out= url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    --retry | --retry-delay) shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
exec cp "$(dirname "$FAKE_LOG")/assets/$(basename "$url")" "$out"
`

const fakeRunner = `#!/usr/bin/env python3
import json
import os
import shutil
import sys
import tarfile

fakes = os.path.dirname(os.environ["FAKE_LOG"])
with open(os.environ["FAKE_LOG"] + ".binrun", "a") as log:
    log.write(json.dumps(sys.argv[1:]) + "\n")
if sys.argv[1:3] != ["--", "fetch"] or len(sys.argv) != 4:
    sys.exit(29)
with open(sys.argv[3]) as f:
    desc = json.loads(f.read().split("\n", 1)[1])
entry = desc["platforms"]["linux-x86_64"]
digest = entry["digest"]
root = os.path.join(os.environ["HOME"], ".daemonkit/cache", digest[:2], digest)
if os.path.isfile(os.path.join(root, entry["path"])) and os.path.isfile(os.path.join(root, "meta.json")):
    sys.exit(0)
if os.path.islink(root):
    os.unlink(root)
shutil.rmtree(root, ignore_errors=True)
os.makedirs(root, mode=0o700)
with tarfile.open(os.path.join(fakes, "assets", entry["providers"][0]["name"])) as archive:
    archive.extractall(root)
os.chmod(os.path.join(root, entry["path"]), 0o755)
meta = {"name": desc["name"], "tag": entry["providers"][0]["tag"], "digest": digest, "fetched_at": "2026-01-01T00:00:00Z"}
tamper = ""
if os.path.exists(os.path.join(fakes, "tamper")):
    with open(os.path.join(fakes, "tamper")) as f:
        tamper = f.read().strip()
if tamper == "meta-digest":
    meta["digest"] = "0" * 64
elif tamper == "meta-name":
    meta["name"] = "other"
elif tamper == "meta-tag":
    meta["tag"] = "v9.9.9"
elif tamper == "extra-file":
    open(os.path.join(root, "extra"), "w").close()
elif tamper == "symlink-member":
    os.symlink(entry["path"], os.path.join(root, "alias"))
elif tamper == "entrypoint-bytes":
    with open(os.path.join(root, entry["path"]), "a") as f:
        f.write("\n")
elif tamper == "symlink-entrypoint":
    os.remove(os.path.join(root, entry["path"]))
    os.symlink("LICENSE", os.path.join(root, entry["path"]))
with open(os.path.join(root, "meta.json"), "w") as f:
    json.dump(meta, f)
`

const nativeLauncher = `#!/bin/sh
echo "launcher $*" >> "$FAKE_LOG"
RUNNER_REPO="owner/binrun"
RUNNER_TAG="v0.8.0"
RUNNER_SHA_linux_amd64="%s"
ROOT="$(dirname "$(dirname "$0")")"
DESCRIPTOR="$ROOT/bin/tool.binrun"
RUNNER_BIN="$HOME/.daemonkit/binrun/$RUNNER_TAG/binrun"
exec "$RUNNER_BIN" "$DESCRIPTOR" "$@"
`

type nativeFixture struct {
	h        pluginsHost
	digest   string
	launcher string
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzMembers(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, header := range headers {
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func nativeAssets(t *testing.T) (asset, runner []byte) {
	t.Helper()
	return tarGz(t, nativeMembers), tarGz(t, map[string]string{"binrun": fakeRunner})
}

type nativeOptions struct {
	edit   func(map[string]any)
	layout lazyLayout
	runner []byte
}

func nativeHost(t *testing.T, opts nativeOptions) nativeFixture {
	t.Helper()
	asset, runner := nativeAssets(t)
	if opts.runner != nil {
		runner = opts.runner
	}
	digest := sha256Hex(asset)
	descriptor := releaseBinary(digest)
	if opts.edit != nil {
		opts.edit(descriptor)
	}
	launcher := fmt.Sprintf(nativeLauncher, sha256Hex(runner))
	layout := opts.layout
	layout.descriptor = append([]byte("#!/usr/bin/env binrun\n"), mustJSON(t, descriptor)...)
	h := lazyPluginHost(t, "hook", launcher, layout)
	for name, data := range map[string][]byte{
		"curl": []byte(servingCurl),
		"assets/" + filepath.Base(nativeAssetURL): asset,
		"assets/" + filepath.Base(runnerAssetURL): runner,
	} {
		writePluginTestFile(t, filepath.Join(h.fakes, name), data, 0o755)
	}
	return nativeFixture{h: h, digest: digest, launcher: launcher}
}

func nativeRoot(home, digest string) string {
	return filepath.Join(home, ".daemonkit/cache", digest[:2], digest)
}

func writeNativeRoot(t *testing.T, root, digest string) {
	t.Helper()
	for name, data := range nativeMembers {
		writePluginTestFile(t, filepath.Join(root, name), []byte(data), 0o600)
	}
	if err := os.Chmod(filepath.Join(root, "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{"name": "tool", "tag": "v1.0.0", "digest": digest, "fetched_at": "2026-01-01T00:00:00Z"}
	writePluginTestFile(t, filepath.Join(root, "meta.json"), mustJSON(t, meta), 0o600)
}

func runnerFetches(t *testing.T, h pluginsHost) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.fakes, "calls.log.binrun"))
	if err != nil {
		t.Fatalf("the pinned runner never ran: %v", err)
	}
	return strings.Count(string(raw), `["--", "fetch", `)
}

func curlCalls(h pluginsHost) []string {
	var calls []string
	for _, call := range h.calls() {
		if strings.HasPrefix(call, "curl ") {
			calls = append(calls, call)
		}
	}
	return calls
}

func TestPluginsNativesMaterializeAndVerifyEveryPinnedLauncher(t *testing.T) {
	f := nativeHost(t, nativeOptions{})
	for _, phase := range []string{"install", "natives", "natives", "verify"} {
		if out, err := f.h.plugins(phase); err != nil {
			t.Fatalf("%s: %v\n%s", phase, err, out)
		}
	}
	root := nativeRoot(f.h.home, f.digest)
	for _, rel := range []string{"tool", "LICENSE", "meta.json"} {
		if info, err := os.Lstat(filepath.Join(root, rel)); err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s/%s is %v, %v; want a regular file", root, rel, info, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(root, "tool")); err != nil || string(got) != nativeMembers["tool"] {
		t.Errorf("entrypoint = %q, %v; want the release member", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(f.h.home, ".daemonkit/binrun", nativeRunnerTag, "binrun")); err != nil || string(got) != fakeRunner {
		t.Errorf("runner = %v; want the pinned binrun release", err)
	}
	if got := runnerFetches(t, f.h); got != 1 {
		t.Errorf("the runner materialized the native %d times, want once", got)
	}
	calls := curlCalls(f.h)
	want := []string{
		"curl -sSfL --retry 3 --retry-all-errors --retry-delay 2 " + runnerAssetURL,
		"curl -sSfL --retry 3 --retry-all-errors --retry-delay 2 " + nativeAssetURL,
	}
	for _, call := range want {
		if n := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, call+" -o ") }); n < 0 {
			t.Errorf("natives never fetched %q:\n%s", call, strings.Join(calls, "\n"))
		}
	}
	if len(calls) != 4 {
		t.Errorf("natives made %d downloads, want the runner and the asset per run:\n%s", len(calls), strings.Join(calls, "\n"))
	}
}

func TestPluginsNativesRejectAMismatchedRoot(t *testing.T) {
	linuxEntry := func(d map[string]any) map[string]any {
		return d["platforms"].(map[string]any)["linux-x86_64"].(map[string]any)
	}
	tests := []struct {
		name    string
		tamper  string
		edit    func(map[string]any)
		runner  []byte
		prepare func(t *testing.T, f nativeFixture)
		wantErr string
	}{
		{name: "runner archive symlink member", runner: tarGzMembers(t, tar.Header{Typeflag: tar.TypeSymlink, Name: "binrun", Linkname: "nowhere", Mode: 0o777}), wantErr: "binrun in " + filepath.Base(runnerAssetURL) + " is not one regular archive member"},
		{name: "runner archive hardlink member", runner: tarGzMembers(t, tar.Header{Typeflag: tar.TypeLink, Name: "binrun", Linkname: filepath.Base(runnerAssetURL), Mode: 0o755}), wantErr: "binrun in " + filepath.Base(runnerAssetURL) + " is not one regular archive member"},
		{name: "runner archive directory member", runner: tarGzMembers(t, tar.Header{Typeflag: tar.TypeDir, Name: "binrun", Mode: 0o755}), wantErr: "binrun in " + filepath.Base(runnerAssetURL) + " is not one regular archive member"},
		{name: "runner archive duplicate member", runner: tarGzMembers(t, tar.Header{Typeflag: tar.TypeReg, Name: "binrun", Mode: 0o755}, tar.Header{Typeflag: tar.TypeReg, Name: "binrun", Mode: 0o755}), wantErr: "binrun in " + filepath.Base(runnerAssetURL) + " is not one regular archive member"},
		{name: "runner archive without the member", runner: tarGzMembers(t, tar.Header{Typeflag: tar.TypeReg, Name: "README", Mode: 0o644}), wantErr: "binrun in " + filepath.Base(runnerAssetURL) + " is not one regular archive member"},
		{name: "symlink entrypoint", tamper: "symlink-entrypoint", wantErr: "is not a daemonkit cache entry"},
		{name: "meta.json digest", tamper: "meta-digest", wantErr: "is not a daemonkit cache entry"},
		{name: "meta.json name", tamper: "meta-name", wantErr: "is not a daemonkit cache entry"},
		{name: "meta.json tag", tamper: "meta-tag", wantErr: "is not a daemonkit cache entry"},
		{name: "extra member", tamper: "extra-file", wantErr: "differs from " + nativeAssetURL},
		{name: "symlink member", tamper: "symlink-member", wantErr: "is not a daemonkit cache entry"},
		{name: "entrypoint bytes", tamper: "entrypoint-bytes", wantErr: "differs from " + nativeAssetURL},
		{name: "symlinked root", wantErr: "is not a daemonkit cache entry", prepare: func(t *testing.T, f nativeFixture) {
			elsewhere := filepath.Join(f.h.home, "elsewhere")
			writeNativeRoot(t, elsewhere, f.digest)
			root := nativeRoot(f.h.home, f.digest)
			if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, root); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "asset sha256", wantErr: "does not match its pinned sha256", prepare: func(t *testing.T, f nativeFixture) {
			writePluginTestFile(t, filepath.Join(f.h.fakes, "assets", filepath.Base(nativeAssetURL)), tarGz(t, map[string]string{"tool": "#!/bin/sh\necho other\n"}), 0o644)
		}},
		{name: "runner sha256", wantErr: "does not match its pinned sha256", prepare: func(t *testing.T, f nativeFixture) {
			writePluginTestFile(t, filepath.Join(f.h.fakes, "assets", filepath.Base(runnerAssetURL)), tarGz(t, map[string]string{"binrun": "#!/bin/sh\nexit 0\n"}), 0o644)
		}},
		{name: "missing platform", wantErr: "no linux-x86_64 entry", edit: func(d map[string]any) {
			d["platforms"] = map[string]any{"macos-aarch64": linuxEntry(d)}
		}},
		{name: "path escapes the root", wantErr: "malformed linux-x86_64 entry", edit: func(d map[string]any) {
			linuxEntry(d)["path"] = "../tool"
		}},
		{name: "unknown format", wantErr: "malformed linux-x86_64 entry", edit: func(d map[string]any) {
			linuxEntry(d)["format"] = "tar.xz"
		}},
		{name: "DAEMONKIT_HOME set", wantErr: "DAEMONKIT_HOME unset", prepare: func(t *testing.T, _ nativeFixture) {
			t.Setenv("DAEMONKIT_HOME", "/elsewhere")
		}},
		{name: "passwd home differs", wantErr: "passwd home of", prepare: func(t *testing.T, f nativeFixture) {
			writePluginTestFile(t, filepath.Join(f.h.fakes, "getent"), []byte("#!/bin/sh\nprintf '%s:x:1000:1000::/elsewhere:/bin/sh\\n' \"$2\"\n"), 0o700)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := nativeHost(t, nativeOptions{edit: tt.edit, runner: tt.runner})
			if tt.tamper != "" {
				writePluginTestFile(t, filepath.Join(f.h.fakes, "tamper"), []byte(tt.tamper), 0o644)
			}
			if tt.prepare != nil {
				tt.prepare(t, f)
			}
			out, err := f.h.plugins("install")
			if err == nil {
				out, err = f.h.plugins("natives")
			}
			if exitCode(err) != 1 || !strings.Contains(out, tt.wantErr) {
				t.Fatalf("natives = %v\n%s\nwant exit 1 mentioning %q", err, out, tt.wantErr)
			}
			if tt.runner == nil {
				return
			}
			if _, err := os.Lstat(filepath.Join(f.h.fakes, "calls.log.binrun")); !os.IsNotExist(err) {
				t.Errorf("a rejected runner archive still ran: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(f.h.home, ".daemonkit/binrun")); !os.IsNotExist(err) {
				t.Errorf("a rejected runner archive was still installed: %v", err)
			}
		})
	}
}

func TestPluginsNativesProvePinsBeforeTheRunner(t *testing.T) {
	unpinned := strings.Repeat("0", 64)
	serviceLauncher := fmt.Sprintf(strings.ReplaceAll(nativeLauncher, "bin/tool.binrun", "bin/svc.binrun"), unpinned)
	tests := []struct {
		name    string
		layout  lazyLayout
		drift   func(t *testing.T, f nativeFixture)
		wantErr string
	}{
		{name: "installed launcher drift", wantErr: "cc-remote: plugin hook@tools-market bin/tool differs from its pinned launcher or descriptor", drift: func(t *testing.T, f nativeFixture) {
			writePluginTestFile(t, filepath.Join(f.h.home, nativeCache, "bin/tool"), []byte(f.launcher+"exit 7\n"), 0o755)
		}},
		{name: "installed descriptor drift", wantErr: "cc-remote: plugin hook@tools-market bin/tool differs from its pinned launcher or descriptor", drift: func(t *testing.T, f nativeFixture) {
			writePluginTestFile(t, filepath.Join(f.h.home, nativeCache, "bin/tool.binrun"), binrunDescriptor(t, unpinned), 0o644)
		}},
		{name: "untracked service command", wantErr: "cc-remote: plugin hook@tools-market bin/svc is not a committed file at ", layout: lazyLayout{
			files:     map[string]string{"plugin/bin/svc": serviceLauncher, "plugin/bin/svc.binrun": string(binrunDescriptor(t, unpinned))},
			untracked: []string{"plugin/bin/svc", "plugin/bin/svc.binrun"},
			services:  []Service{{Name: "svc", Plugin: "hook@tools-market", Command: []string{"bin/svc"}}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := nativeHost(t, nativeOptions{layout: tt.layout})
			if out, err := f.h.plugins("install"); err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			if tt.drift != nil {
				tt.drift(t, f)
			}
			out, err := f.h.plugins("natives")
			if exitCode(err) != 1 || !strings.Contains(out, tt.wantErr) {
				t.Fatalf("natives = %v\n%s\nwant exit 1 with %q", err, out, tt.wantErr)
			}
			if _, err := os.Lstat(filepath.Join(f.h.fakes, "calls.log.binrun")); !os.IsNotExist(err) {
				t.Errorf("the runner ran before the pin proof: %v", err)
			}
			if calls := curlCalls(f.h); len(calls) != 0 {
				t.Errorf("natives downloaded before the pin proof:\n%s", strings.Join(calls, "\n"))
			}
		})
	}
}

func blobOID(content string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))
	return hex.EncodeToString(sum[:])
}

func TestPluginsPinProofFailsClosedOnAnUnreadableBlob(t *testing.T) {
	f := nativeHost(t, nativeOptions{})
	if out, err := f.h.plugins("install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	oid := blobOID(f.launcher)
	object := filepath.Join(f.h.home, ".local/share/cc-remote/marketplaces/tools-market/.git/objects", oid[:2], oid[2:])
	if err := os.Remove(object); err != nil {
		t.Fatalf("the committed launcher is not a loose object: %v", err)
	}
	want := "cc-remote: cannot read the committed bin/tool of plugin hook@tools-market at "
	for _, phase := range []string{"natives", "verify"} {
		out, err := f.h.plugins(phase)
		if exitCode(err) != 1 || !strings.Contains(out, want) {
			t.Fatalf("%s = %v\n%s\nwant exit 1 with %q", phase, err, out, want)
		}
	}
	for _, call := range f.h.calls() {
		if strings.HasPrefix(call, "launcher ") || strings.HasPrefix(call, "curl ") {
			t.Errorf("an unreadable committed blob still led to %q", call)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.h.fakes, "calls.log.binrun")); !os.IsNotExist(err) {
		t.Errorf("an unreadable committed blob still ran the runner: %v", err)
	}
}

func TestPluginsNativesSkipNonBinrunBins(t *testing.T) {
	h := lazyPluginHost(t, "hook", "#!/bin/sh\nexit 0\n", lazyLayout{})
	writePluginTestFile(t, filepath.Join(h.fakes, "curl"), []byte(failingCurl), 0o700)
	for _, phase := range []string{"install", "natives"} {
		if out, err := h.plugins(phase); err != nil {
			t.Fatalf("%s: %v\n%s", phase, err, out)
		}
	}
	if _, err := os.Lstat(filepath.Join(h.home, ".daemonkit")); !os.IsNotExist(err) {
		t.Errorf("natives touched ~/.daemonkit for a plain launcher: %v", err)
	}
	if calls := curlCalls(h); len(calls) != 0 {
		t.Errorf("natives downloaded for a plain launcher:\n%s", strings.Join(calls, "\n"))
	}
}

func TestPluginsInstallExposesNativeRoots(t *testing.T) {
	tests := []struct {
		name   string
		omit   string
		poison string
		linked bool
	}{
		{name: "every native root"},
		{name: "existing links into the payload", linked: true},
		{name: "missing root", omit: "root"},
		{name: "root without meta.json", omit: "meta.json"},
		{name: "missing runner", omit: "runner"},
		{name: "a directory at the root", poison: "root-dir"},
		{name: "an unrelated link at the root", poison: "root-link"},
		{name: "a directory at the runner", poison: "runner-dir"},
		{name: "an unrelated link at the runner", poison: "runner-link"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := nativeHost(t, nativeOptions{})
			payload := filepath.Join(t.TempDir(), f.digest)
			root, runner := nativeRoot(f.h.home, f.digest), filepath.Join(f.h.home, ".daemonkit/binrun", nativeRunnerTag)
			elsewhere := filepath.Join(f.h.home, "elsewhere")
			poisoned := map[bool]string{true: root, false: runner}[strings.HasPrefix(tt.poison, "root")]
			switch {
			case strings.HasSuffix(tt.poison, "-dir"):
				writePluginTestFile(t, filepath.Join(poisoned, "owner-data"), []byte("keep\n"), 0o600)
			case strings.HasSuffix(tt.poison, "-link"):
				if err := os.MkdirAll(filepath.Dir(poisoned), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, poisoned); err != nil {
					t.Fatal(err)
				}
			}
			if tt.linked {
				for _, path := range []string{root, runner} {
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(payload, path), path); err != nil {
						t.Fatal(err)
					}
				}
			}
			writePluginTestFile(t, filepath.Join(payload, f.h.home, nativeCache, "bin/tool"), []byte(f.launcher), 0o755)
			writePluginTestFile(t, filepath.Join(payload, f.h.home, nativeCache, "bin/tool.binrun"), binrunDescriptor(t, f.digest), 0o644)
			if tt.omit != "root" {
				writeNativeRoot(t, filepath.Join(payload, root), f.digest)
			}
			if tt.omit == "meta.json" {
				if err := os.Remove(filepath.Join(payload, root, "meta.json")); err != nil {
					t.Fatal(err)
				}
			}
			if tt.omit != "runner" {
				writePluginTestFile(t, filepath.Join(payload, runner, "binrun"), []byte(fakeRunner), 0o755)
			}
			rendered, err := os.ReadFile(filepath.Join(f.h.fakes, "plugins.sh"))
			if err != nil {
				t.Fatal(err)
			}
			functions, _, found := strings.Cut(string(rendered), "\ncase \"$phase\" in\n")
			if !found {
				t.Fatalf("plugins.sh has no phase dispatch:\n%s", rendered)
			}
			expose := "expose_native \"$HOME/\"" + quote(nativeCache) + " 'bin/tool'\n"
			writePluginTestFile(t, filepath.Join(f.h.fakes, "expose.sh"), []byte(functions+"\npayload=\"$2\"\nnative_home\n"+expose+expose), 0o700)
			out, err := f.h.run("bash", filepath.Join(f.h.fakes, "expose.sh"), "install", payload)
			if _, err := os.Lstat(filepath.Join(f.h.fakes, "calls.log.binrun")); !os.IsNotExist(err) {
				t.Errorf("exposing the payload ran the runner: %v", err)
			}
			var wantErr string
			switch {
			case tt.omit == "root":
				wantErr = "cc-remote: payload " + payload + " lacks " + root
			case tt.omit == "runner":
				wantErr = "cc-remote: payload " + payload + " lacks " + runner
			case tt.omit == "meta.json":
				wantErr = "cc-remote: payload " + payload + " lacks a verified tool v1.0.0 native at " + root
			case tt.poison != "":
				wantErr = "cc-remote: " + poisoned + " is not a link to the payload copy " + filepath.Join(payload, poisoned)
			}
			if wantErr != "" {
				if exitCode(err) != 1 || !strings.Contains(out, wantErr) {
					t.Fatalf("expose = %v\n%s\nwant exit 1 with %q", err, out, wantErr)
				}
				switch {
				case strings.HasSuffix(tt.poison, "-dir"):
					if info, err := os.Lstat(poisoned); err != nil || !info.IsDir() {
						t.Errorf("the poisoned directory %s became %v, %v", poisoned, info, err)
					}
					if got, err := os.ReadFile(filepath.Join(poisoned, "owner-data")); err != nil || string(got) != "keep\n" {
						t.Errorf("owner data under %s = %q, %v; want it untouched", poisoned, got, err)
					}
				case strings.HasSuffix(tt.poison, "-link"):
					if got, err := os.Readlink(poisoned); err != nil || got != elsewhere {
						t.Errorf("the unrelated link %s became %q, %v", poisoned, got, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("expose failed: %v\n%s", err, out)
			}
			for _, path := range []string{root, runner} {
				if got, err := os.Readlink(path); err != nil || got != filepath.Join(payload, path) {
					t.Errorf("%s links to %q, %v; want the payload copy", path, got, err)
				}
			}
			if entries, err := os.ReadDir(filepath.Dir(root)); err != nil || len(entries) != 1 {
				t.Errorf("the shard holds %v, %v; want the one shared link", entries, err)
			}
		})
	}
}

func TestProvisionPackAddsOnlyVerifiedNativeRoots(t *testing.T) {
	tests := []struct {
		name    string
		corrupt bool
		wantErr string
	}{
		{name: "a shared digest is archived once beside the runner"},
		{name: "a meta.json mismatch fails the pack", corrupt: true, wantErr: "lacks a verified tool v1.0.0 native"},
	}
	inventory := Inventory{Version: SchemaVersion, Claude: Claude{
		Marketplaces: []Marketplace{toolsRef},
		Plugins: []Plugin{
			{ID: "a@tools-market", Version: "1.0.0", Bins: []string{"bin/tool"}},
			{ID: "b@tools-market", Version: "1.0.0", Bins: []string{"bin/tool"}},
		},
	}}
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatal(err)
	}
	asset, runner := nativeAssets(t)
	digest := sha256Hex(asset)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, home, fakes := t.TempDir(), t.TempDir(), t.TempDir()
			for name, content := range map[string]string{
				"id":         "#!/bin/sh\necho 0\n",
				"apt-get":    "#!/bin/sh\nexit 0\n",
				"getent":     fakeGetent,
				"mksquashfs": "#!/bin/sh\nexec cat > \"$2\"\n",
			} {
				writePluginTestFile(t, filepath.Join(fakes, name), []byte(content), 0o700)
			}
			for _, rel := range []string{
				".local/share/cc-remote/marketplaces/tools-market/.fake-head",
				".claude/plugins/installed_plugins.json",
				".claude/plugins/known_marketplaces.json",
				".claude/settings.json",
				".daemonkit/locks/0123456789abcdef.lock",
				".daemonkit/a/tool/service.json",
				".daemonkit/cache/ff/" + strings.Repeat("f", 64) + "/meta.json",
			} {
				writePluginTestFile(t, filepath.Join(home, rel), []byte("{}\n"), 0o600)
			}
			for _, plugin := range []string{"a", "b"} {
				bin := filepath.Join(home, ".claude/plugins/cache/tools-market", plugin, "1.0.0/bin/tool")
				writePluginTestFile(t, bin, []byte(fmt.Sprintf(nativeLauncher, sha256Hex(runner))), 0o755)
				writePluginTestFile(t, bin+".binrun", binrunDescriptor(t, digest), 0o644)
			}
			writeNativeRoot(t, nativeRoot(home, digest), digest)
			writePluginTestFile(t, filepath.Join(home, ".daemonkit/binrun", nativeRunnerTag, "binrun"), []byte(fakeRunner), 0o755)
			if tt.corrupt {
				meta := map[string]string{"name": "tool", "tag": "v1.0.0", "digest": strings.Repeat("0", 64)}
				writePluginTestFile(t, filepath.Join(nativeRoot(home, digest), "meta.json"), mustJSON(t, meta), 0o600)
			}
			script := strings.ReplaceAll(string(scripts.ProvisionScript), "/var/lib/cc-remote/build", filepath.Join(root, "build"))
			writePluginTestFile(t, filepath.Join(fakes, "provision.sh"), []byte(script), 0o700)
			cmd := exec.Command("bash", filepath.Join(fakes, "provision.sh"), "pack", "tools-fingerprint")
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+fakes+":"+os.Getenv("PATH"), "SUDO_USER=builder")
			out, err := cmd.CombinedOutput()
			if tt.wantErr != "" {
				if exitCode(err) != 1 || !strings.Contains(string(out), tt.wantErr) {
					t.Fatalf("pack = %v\n%s\nwant exit 1 with %q", err, out, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("pack: %v\n%s", err, out)
			}
			listing, err := exec.Command("tar", "-tf", filepath.Join(root, "build/payload.sqfs")).Output()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, line := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
				if rel, ok := strings.CutPrefix("/"+line, home+"/.daemonkit/"); ok {
					got = append(got, strings.TrimSuffix(rel, "/"))
				}
			}
			slices.Sort(got)
			cache := "cache/" + digest[:2] + "/" + digest
			want := []string{
				"binrun/" + nativeRunnerTag,
				"binrun/" + nativeRunnerTag + "/binrun",
				cache,
				cache + "/LICENSE",
				cache + "/meta.json",
				cache + "/tool",
			}
			if !slices.Equal(got, want) {
				t.Errorf("packed ~/.daemonkit entries = %v, want %v", got, want)
			}
		})
	}
}
