package images

import (
	"cmp"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	assets "github.com/yasyf/cc-remote/images"
)

const (
	fakeDpkgQuery = `#!/usr/bin/env python3
import json
import os
import sys

with open(os.environ["FAKE_DPKG"]) as fh:
    data = json.load(fh)
packages, owners, args = data["packages"], data["owners"], sys.argv[1:]
if args[0] == "-W" and len(args) == 3:
    for name in sorted(packages):
        print(packages[name].get("status", "ii ") + "\t" + name)
elif args[0] == "-W":
    code = 0
    for name in args[3:]:
        pkg = packages.get(name)
        if pkg is None:
            print("dpkg-query: no packages found matching " + name, file=sys.stderr)
            code = 1
            continue
        fields = {"Package": name, "Version": pkg["version"], "Architecture": pkg.get("arch") or "amd64", "Depends": pkg.get("depends") or "", "Pre-Depends": pkg.get("predepends") or "", "Provides": pkg.get("provides") or ""}
        out = args[2]
        for key, value in fields.items():
            out = out.replace("${" + key + "}", value)
        sys.stdout.write(out.replace("\\t", "\t").replace("\\n", "\n"))
    sys.exit(code)
elif args[0] == "-L":
    pkg = packages.get(args[1])
    if pkg is None:
        print("dpkg-query: package '" + args[1] + "' is not installed", file=sys.stderr)
        sys.exit(1)
    print("\n".join(pkg["files"]))
elif args[0] == "-S":
    if os.environ.get("FAKE_DPKG_SEARCH") == "crash":
        print("dpkg-query: error: cannot open the package database", file=sys.stderr)
        sys.exit(2)
    if os.environ.get("FAKE_DPKG_SEARCH") == "mute":
        sys.exit(1)
    code = 0
    for path in args[1:]:
        found = owners.get(path) or [name for name, pkg in packages.items() if path in pkg["files"]]
        if found:
            print(", ".join(found) + ": " + path)
        else:
            print("dpkg-query: no path found matching pattern " + path, file=sys.stderr)
            code = 1
    sys.exit(code)
else:
    sys.exit(91)
`
	fakeStatusQuery = `#!/usr/bin/env python3
import os
import sys

if os.environ.get("DPKG_FAIL"):
    sys.exit(2)
args = sys.argv[1:]
if len(args) > 3 and "${Status}" in args[2]:
    for name in sorted(args[3:]):
        print(name + " install ok installed 1.0-1")
elif "${db:Status-Status}" in args[2]:
    with open(os.path.join(os.environ["TEST_ROOT"], "base")) as fh:
        sys.stdout.write(fh.read())
else:
    with open(os.path.join(os.environ["TEST_ROOT"], "installed")) as fh:
        sys.stdout.write(fh.read())
`
	fakeCurl = `#!/bin/sh
while [ "$#" -gt 1 ]; do
  if [ "$1" = -o ]; then
    printf 'deb' > "$2"
    exit 0
  fi
  shift
done
exit 1
`
	fakeDpkgDeb = `#!/bin/sh
[ "$1" = -f ] && [ "$3" = Package ] || exit 9
if [ "$#" -gt 3 ]; then
  exec cat "$2.control"
fi
case "${DPKG_DEB:-}" in
  fail)
    echo "dpkg-deb: error: cannot read $2" >&2
    exit 2
    ;;
  empty) ;;
  *) echo orca-ide ;;
esac
`
	liveFontProof = `Font directories:
	/home/sprite/.local/share/fonts
	/usr/local/share/fonts
	/usr/share/fonts
	/home/sprite/.fonts
	@FONTS@
	/usr/share/fonts/truetype
	@FONTS@/opentype
	@FONTS@/truetype
	/usr/share/fonts/truetype/liberation
	@FONTS@/opentype/noto
	@FONTS@/truetype/freefont
	@FONTS@/truetype/noto
/home/sprite/.local/share/fonts: skipping, no such directory
/usr/local/share/fonts: skipping, existing cache is valid: 0 fonts, 0 dirs
/usr/share/fonts: skipping, existing cache is valid: 0 fonts, 1 dirs
/usr/share/fonts/truetype: skipping, existing cache is valid: 0 fonts, 1 dirs
/usr/share/fonts/truetype/liberation: skipping, existing cache is valid: 12 fonts, 0 dirs
/home/sprite/.fonts: skipping, no such directory
@FONTS@: skipping, existing cache is valid: 0 fonts, 2 dirs
@FONTS@/opentype: skipping, existing cache is valid: 0 fonts, 1 dirs
@FONTS@/opentype/noto: skipping, existing cache is valid: 30 fonts, 0 dirs
@FONTS@/truetype: skipping, existing cache is valid: 0 fonts, 2 dirs
@FONTS@/truetype/freefont: skipping, existing cache is valid: 12 fonts, 0 dirs
@FONTS@/truetype/noto: skipping, existing cache is valid: 1 fonts, 0 dirs
/usr/share/fonts/truetype: skipping, looped directory detected
@FONTS@/opentype: skipping, looped directory detected
@FONTS@/truetype: skipping, looped directory detected
/usr/share/fonts/truetype/liberation: skipping, looped directory detected
@FONTS@/opentype/noto: skipping, looped directory detected
@FONTS@/truetype/freefont: skipping, looped directory detected
@FONTS@/truetype/noto: skipping, looped directory detected
/opt/cc-remote/closure/var/cache/fontconfig: not cleaning unwritable cache directory
/var/cache/fontconfig: not cleaning non-existent cache directory
/home/sprite/.cache/fontconfig: not cleaning non-existent cache directory
/home/sprite/.fontconfig: not cleaning non-existent cache directory
fc-cache: succeeded
`
	fakeLdso = `#!/bin/sh
[ "$1" = --list ] || exit 9
printf '%s\n' "ld.so $2" >> "$TEST_ROOT/ldso.log"
arch="$(uname -m)"
printf '\tlibc.so.6 => /usr/lib/%s-linux-gnu/libc.so.6 (0x0)\n\tlibfoo.so.1 => /usr/lib/%s-linux-gnu/libfoo.so.1 (0x0)\n' "$arch" "$arch"
if [ -n "${LDSO_MISSING:-}" ]; then
  printf '\t%s => not found\n' "$LDSO_MISSING"
fi
`
	fakeFcCache = `#!/bin/sh
dir="$(sed -n 's|.*<dir>\(.*\)</dir>.*|\1|p' "$FONTCONFIG_FILE")"
cachedir="$(sed -n 's|.*<cachedir>\(.*\)</cachedir>.*|\1|p' "$FONTCONFIG_FILE")"
printf '%s\n' "fc-cache $* uid=$(id -u) conf=$FONTCONFIG_FILE" >> "$TEST_ROOT/fc-cache.log"
case "$1" in
  -f) : > "$cachedir/abc-le64.cache-9" ;;
  -v)
    if [ -n "${FC_PROOF:-}" ]; then
      sed "s|@FONTS@|$dir|g" "$FC_PROOF"
    elif [ -n "${FC_LOOP_ONLY:-}" ]; then
      echo "$dir: skipping, looped directory detected"
    elif [ -n "${FC_STALE:-}" ]; then
      echo "$dir: caching, new cache contents: 1 fonts, 1 dirs"
    else
      find "$dir" -type d | sort | while read -r d; do
        if [ -n "${FC_MISSING:-}" ] && [ "$d" != "$dir" ]; then
          continue
        fi
        echo "$d: skipping, existing cache is valid: 1 fonts, 0 dirs"
        if [ -n "${FC_LOOP:-}" ]; then
          echo "$d: skipping, looped directory detected"
        fi
      done
      if [ -n "${FC_UNKNOWN:-}" ]; then
        echo "$dir/unknown: skipping, existing cache is valid: 1 fonts, 0 dirs"
      fi
    fi
    echo "$cachedir: not cleaning unwritable cache directory"
    echo "fc-cache: succeeded"
    ;;
esac
`
	fakeRunuser = `#!/bin/sh
[ "$1" = -u ] && [ "$3" = -- ] || exit 9
printf '%s\n' "runuser $2" >> "$TEST_ROOT/runuser.log"
shift 3
exec "$@"
`
	fakeFlock = `#!/bin/sh
printf '%s\n' "flock $*" >> "$TEST_ROOT/calls"
shift
exec "$@"
`
	fakeLdconfig  = "#!/bin/sh\nprintf '%s\\n' \"ldconfig$*\" >> \"$TEST_ROOT/calls\"\n[ -z \"${LDCONFIG_FAIL:-}\" ] || exit 7\n"
	fakeAptCache  = "#!/bin/sh\nprintf 'Package: libasound2t64\\n'\n"
	fakeInstaller = `#!/bin/bash
set -euo pipefail
printf '%s\n' "$*" >> "$TEST_ROOT/apt"
if [ "$1" = install ] && [[ " $* " != *" --download-only "* ]]; then
  for arg in "$@"; do
    case "$arg" in
      install | -*) ;;
      *) printf 'ii \t%s\n' "$arg" >> "$TEST_ROOT/installed" ;;
    esac
  done
fi
exit "${APT_FAIL:-0}"
`
	fakeOnline     = "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"$TEST_ROOT/online\"\nexit 1\n"
	fakeMounted    = "#!/bin/sh\n[ \"$1\" = -q ] && [ -e \"$2/.mounted\" ]\n"
	fakeDpkgStatus = "#!/bin/sh\n[ \"$1\" = -s ] && grep -qx \"ii \t$2\" \"$TEST_ROOT/installed\"\n"
	payloadDigest  = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

var fakeClosureMount = strings.Replace(fakeMount, "{schemaVersion: 1,", "{schemaVersion: 2,", 1)

func scriptInventory() Inventory {
	return Inventory{
		Version: SchemaVersion,
		Apt: Apt{
			Install: []string{"openssh-server", "libnss3"},
			T64:     []string{"libasound2"},
			Payload: &AptPayload{
				Resident:    []string{"bubblewrap"},
				Closure:     []string{"libnss3", "libasound2t64", "fonts-x"},
				Bins:        []string{"certutil", "fc-match"},
				Fonts:       []string{"Noto Sans CJK JP"},
				Consumers:   []string{".agent-browser/chrome", "/opt/cc-remote/tools/office/soffice.bin"},
				Projections: []string{"/usr/share/X11/xkb"},
			},
		},
	}
}

func machineArch(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func elfWithInterp(interp string) []byte {
	interp += "\x00"
	elf := make([]byte, 120, 120+len(interp))
	copy(elf, "\x7fELF\x02\x01\x01")
	binary.LittleEndian.PutUint16(elf[16:], 3)
	binary.LittleEndian.PutUint16(elf[18:], 62)
	binary.LittleEndian.PutUint32(elf[20:], 1)
	binary.LittleEndian.PutUint64(elf[32:], 64)
	binary.LittleEndian.PutUint16(elf[52:], 64)
	binary.LittleEndian.PutUint16(elf[54:], 56)
	binary.LittleEndian.PutUint16(elf[56:], 1)
	binary.LittleEndian.PutUint32(elf[64:], 3)
	binary.LittleEndian.PutUint32(elf[68:], 4)
	binary.LittleEndian.PutUint64(elf[72:], 120)
	binary.LittleEndian.PutUint64(elf[96:], uint64(len(interp)))
	binary.LittleEndian.PutUint64(elf[104:], uint64(len(interp)))
	return append(elf, interp...)
}

func writeFakes(t *testing.T, dir string, fakes map[string]string) {
	t.Helper()
	for name, content := range fakes {
		writePluginTestFile(t, filepath.Join(dir, name), []byte(content), 0o700)
	}
}

func TestProvisionPackagesInstallsTheResidentSetForAPayload(t *testing.T) {
	download := "install -y -qq --no-install-recommends --download-only -o Dir::Cache::Archives=@BUILD@/debs/ bubblewrap ca-certificates curl git jq python3 unzip xz-utils openssh-server\n"
	full := "update -qq\n" + download + "install -y -qq --no-install-recommends ca-certificates curl git jq python3 unzip xz-utils openssh-server libnss3 libasound2t64 bubblewrap\n"
	seeds := "bubblewrap\nca-certificates\ncurl\ngit\njq\npython3\nunzip\nxz-utils\nopenssh-server\n"
	tests := []struct {
		name    string
		mode    []string
		apt     string
		exit    int
		wantErr string
	}{
		{name: "no mode downloads the resident set, installs everything, and records the transaction", apt: full},
		{name: "full downloads the resident set, installs everything, and records the transaction", mode: []string{PackagesFull}, apt: full},
		{name: "an unknown mode is a usage error", mode: []string{"bundle"}, exit: 2, wantErr: "provision: packages takes full or resident, not bundle"},
	}
	scripts, err := Render(scriptInventory(), "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes := t.TempDir(), t.TempDir()
			build := filepath.Join(root, "build")
			writeFakes(t, fakes, map[string]string{
				"id":         "#!/bin/sh\necho 0\n",
				"apt-get":    fakeInstaller,
				"apt-cache":  fakeAptCache,
				"dpkg-query": fakeStatusQuery,
			})
			writePluginTestFile(t, filepath.Join(root, "installed"), []byte("ii \tbase-files\n"), 0o644)
			writePluginTestFile(t, filepath.Join(root, "base"), []byte("installed\tbase-files=13\nconfig-files\told=1\n"), 0o644)
			provision := strings.NewReplacer(
				"build_dir=/var/lib/cc-remote/build\n", "build_dir="+quote(build)+"\n",
				"rm -rf /var/lib/apt/lists/*", ":",
			).Replace(string(scripts.ProvisionScript))
			cmd := exec.Command("bash", slices.Concat([]string{"-c", provision, "provision.sh", PhasePackages}, tt.mode)...)
			cmd.Env = append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root)
			out, err := cmd.CombinedOutput()
			apt, aptErr := os.ReadFile(filepath.Join(root, "apt"))
			if tt.wantErr != "" {
				if exitCode(err) != tt.exit || !strings.Contains(string(out), tt.wantErr) {
					t.Fatalf("packages = %v\n%s\nwant exit %d with %q", err, out, tt.exit, tt.wantErr)
				}
				if !os.IsNotExist(aptErr) {
					t.Errorf("apt-get ran for a usage error: %q %v", apt, aptErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("packages failed: %v\n%s", err, out)
			}
			if want := strings.ReplaceAll(tt.apt, "@BUILD@", build); aptErr != nil || string(apt) != want {
				t.Errorf("apt-get calls:\n%s\n%v\nwant:\n%s", apt, aptErr, want)
			}
			installed, err := os.ReadFile(filepath.Join(root, "installed"))
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"seeds": seeds, "packages.before": "ii \tbase-files\n", "packages.after": string(installed), "base": "base-files=13\n"} {
				if got, err := os.ReadFile(filepath.Join(build, name)); err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want %q", name, got, err, want)
				}
			}
			if info, err := os.Stat(filepath.Join(build, "debs", "partial")); err != nil || !info.IsDir() {
				t.Errorf("the download cache lacks its partial directory: %v", err)
			}
		})
	}
}

type capturedDeb struct {
	Architecture string `json:"architecture"`
	File         string `json:"file"`
	Package      string `json:"package"`
	SHA256       string `json:"sha256,omitempty"`
	Version      string `json:"version"`
}

type capturedDebs struct {
	Artifacts []capturedDeb `json:"artifacts"`
	Base      []string      `json:"base"`
	Debs      []capturedDeb `json:"debs"`
}

func writeCapturedPayload(t *testing.T, root string, manifest capturedDebs, contents map[string]string, mounted bool) string {
	t.Helper()
	payload := filepath.Join(root, "payload", payloadDigest)
	debs := filepath.Join(payload, "opt/cc-remote/debs")
	for i, deb := range manifest.Debs {
		sum := sha256.Sum256([]byte(contents[deb.File]))
		manifest.Debs[i].SHA256 = hex.EncodeToString(sum[:])
	}
	for file, content := range contents {
		writePluginTestFile(t, filepath.Join(debs, file), []byte(content), 0o644)
	}
	writePluginTestFile(t, filepath.Join(debs, "debs.json"), mustJSON(t, manifest), 0o644)
	if mounted {
		writePluginTestFile(t, filepath.Join(payload, ".mounted"), nil, 0o644)
	}
	return debs
}

func TestProvisionPackagesInstallsTheCapturedDebsOffline(t *testing.T) {
	base := "installed\tbase-files=13\ninstalled\tlibc6=2.42-1\n"
	install := "install -y -qq --no-download --no-install-recommends @DEBS@/bubblewrap_0.11.0-2_amd64.deb @DEBS@/openssh-server_1%3a9.9p1-3_amd64.deb\n"
	drifted := "cc-remote: the packages on this machine differ from the base its payload captured the resident packages against (-payload +machine), so they cannot install offline; rebuild the payload on this base:\n"
	shadowing := "cc-remote: the resident install left closure packages installed, whose system copies would shadow the payload; move them to apt.payload.resident or recreate the machine:\n"
	tests := []struct {
		name      string
		args      []string
		base      string
		installed string
		env       []string
		unmounted bool
		corrupt   bool
		remove    bool
		apt       string
		exit      int
		wantErr   string
	}{
		{name: "the captured debs install offline", apt: install},
		{name: "a repeat run over the installed resident packages passes", base: base + "installed\tbubblewrap=0.11.0-2\ninstalled\topenssh-server=1:9.9p1-3\n", apt: install},
		{name: "packages dpkg only knows or keeps the configuration of are not part of the base", base: base + "config-files\told=1\nnot-installed\tgone=2\n", apt: install},
		{name: "a drifted base version fails before apt", base: "installed\tbase-files=14\ninstalled\tlibc6=2.42-1\n", exit: 1, wantErr: drifted + "-base-files=13\n+base-files=14\n"},
		{name: "an extra base package fails before apt", base: base + "installed\tvim=2\n", exit: 1, wantErr: drifted + "+vim=2\n"},
		{name: "a missing base package fails before apt", base: "installed\tbase-files=13\n", exit: 1, wantErr: drifted + "-libc6=2.42-1\n"},
		{name: "a corrupted deb fails before apt", corrupt: true, exit: 1, wantErr: "/openssh-server_1%3a9.9p1-3_amd64.deb does not match its sha256 "},
		{name: "an unmounted payload fails before apt", unmounted: true, exit: 1, wantErr: "cc-remote: payload " + payloadDigest + " is not mounted at "},
		{name: "resident without the payload sha256 is a usage error", args: []string{PackagesResident}, exit: 2, wantErr: "provision: packages resident takes the payload sha256"},
		{name: "a failing apt install propagates its status", env: []string{"APT_FAIL=100"}, apt: install, exit: 100},
		{name: "a closure package left installed is fatal", installed: "ii \tlibnss3\n", apt: install, exit: 1, wantErr: shadowing + "libnss3 install ok installed 1.0-1\n"},
		{name: "a failing package listing fails before apt", env: []string{"DPKG_FAIL=1"}, exit: 2},
		{name: "apt.remove still runs offline and its packages leave the base", remove: true, base: base + "installed\twatchman=2025.1\n", installed: "ii \twatchman\n", apt: install + "remove -y -qq watchman\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inventory := scriptInventory()
			if tt.remove {
				inventory.Apt.Remove = []string{"watchman"}
			}
			scripts, err := Render(inventory, "agents")
			if err != nil {
				t.Fatal(err)
			}
			root, fakes := t.TempDir(), t.TempDir()
			writeFakes(t, fakes, map[string]string{
				"id":         "#!/bin/sh\necho 0\n",
				"apt-get":    fakeInstaller,
				"apt-cache":  fakeOnline,
				"curl":       fakeOnline,
				"dpkg-query": fakeStatusQuery,
				"dpkg":       fakeDpkgStatus,
				"mountpoint": fakeMounted,
			})
			contents := map[string]string{"bubblewrap_0.11.0-2_amd64.deb": "bubblewrap deb", "openssh-server_1%3a9.9p1-3_amd64.deb": "openssh-server deb"}
			captured := []string{"base-files=13", "libc6=2.42-1"}
			if tt.remove {
				captured = append(captured, "watchman=2024.1")
			}
			debs := writeCapturedPayload(t, root, capturedDebs{
				Artifacts: []capturedDeb{},
				Base:      captured,
				Debs: []capturedDeb{
					{Architecture: "amd64", File: "bubblewrap_0.11.0-2_amd64.deb", Package: "bubblewrap", Version: "0.11.0-2"},
					{Architecture: "amd64", File: "openssh-server_1%3a9.9p1-3_amd64.deb", Package: "openssh-server", Version: "1:9.9p1-3"},
				},
			}, contents, !tt.unmounted)
			if tt.corrupt {
				writePluginTestFile(t, filepath.Join(debs, "openssh-server_1%3a9.9p1-3_amd64.deb"), []byte("tampered"), 0o644)
			}
			writePluginTestFile(t, filepath.Join(root, "installed"), []byte("ii \tbase-files\n"+tt.installed), 0o644)
			writePluginTestFile(t, filepath.Join(root, "base"), []byte(cmp.Or(tt.base, base)), 0o644)
			provision := strings.NewReplacer(
				"build_dir=/var/lib/cc-remote/build\n", "build_dir="+quote(filepath.Join(root, "build"))+"\n",
				"payload_root=/opt/cc-remote/payload\n", "payload_root="+quote(filepath.Join(root, "payload"))+"\n",
				"rm -rf /var/lib/apt/lists/*", ":",
			).Replace(string(scripts.ProvisionScript))
			args := tt.args
			if args == nil {
				args = []string{PackagesResident, payloadDigest}
			}
			cmd := exec.Command("bash", slices.Concat([]string{"-c", provision, "provision.sh", PhasePackages}, args)...)
			cmd.Env = append(append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root), tt.env...)
			out, err := cmd.CombinedOutput()
			if exitCode(err) != tt.exit || !strings.Contains(string(out), tt.wantErr) {
				t.Fatalf("packages = %v\n%s\nwant exit %d with %q", err, out, tt.exit, tt.wantErr)
			}
			apt, aptErr := os.ReadFile(filepath.Join(root, "apt"))
			switch want := strings.ReplaceAll(tt.apt, "@DEBS@", debs); {
			case want == "" && !os.IsNotExist(aptErr):
				t.Errorf("apt-get ran before the payload's packages were proven: %q %v", apt, aptErr)
			case want != "" && (aptErr != nil || string(apt) != want):
				t.Errorf("apt-get calls:\n%s\n%v\nwant:\n%s", apt, aptErr, want)
			}
			if online, err := os.ReadFile(filepath.Join(root, "online")); !os.IsNotExist(err) {
				t.Errorf("the offline install reached for the network: %q %v", online, err)
			}
			if _, err := os.Stat(filepath.Join(root, "build")); !os.IsNotExist(err) {
				t.Errorf("the offline install recorded a build: %v", err)
			}
		})
	}
}

func TestProvisionPackagesSeedsTheDebPackage(t *testing.T) {
	sum := sha512.Sum512([]byte("deb"))
	inventory := scriptInventory()
	inventory.System = []Artifact{{Name: "orca", Version: "1.4.215", URL: "https://example.invalid/orca.deb", SHA512: hex.EncodeToString(sum[:]), Format: Deb, Bins: map[string]string{"orca": "/opt/Orca/orca-ide"}}}
	full := "update -qq\ninstall -y -qq --no-install-recommends --download-only -o Dir::Cache::Archives=@BUILD@/debs/ bubblewrap ca-certificates curl git jq python3 unzip xz-utils openssh-server @BUILD@/artifacts/orca-1.4.215.deb\ninstall -y -qq --no-install-recommends ca-certificates curl git jq python3 unzip xz-utils openssh-server libnss3 libasound2t64 bubblewrap\ninstall -y -qq @BUILD@/artifacts/orca-1.4.215.deb\n"
	resident := "install -y -qq --no-download --no-install-recommends\n"
	tests := []struct {
		name    string
		args    []string
		env     []string
		copy    string
		exit    int
		wantErr string
		seeds   string
		apt     string
		fetches int
	}{
		{name: "full mode fetches the deb once, downloads with it, and seeds the package it declares", args: []string{PackagesFull}, seeds: "bubblewrap\nca-certificates\ncurl\ngit\njq\npython3\nunzip\nxz-utils\nopenssh-server\norca-ide\n", apt: full, fetches: 1},
		{name: "resident mode installs the captured deb offline", args: []string{PackagesResident, payloadDigest}, copy: "deb", apt: resident + "install -y -qq --no-download @DEBS@/orca-1.4.215.deb\n"},
		{name: "a captured deb off its pin fails before apt installs it", args: []string{PackagesResident, payloadDigest}, copy: "tampered", apt: resident, exit: 1, wantErr: "@DEBS@/orca-1.4.215.deb does not match its pinned sha512 "},
		{name: "a deb naming no package is fatal", args: []string{PackagesFull}, env: []string{"DPKG_DEB=empty"}, exit: 1, wantErr: "orca-1.4.215.deb names no Package", fetches: 1},
		{name: "a failing dpkg-deb is fatal", args: []string{PackagesFull}, env: []string{"DPKG_DEB=fail"}, exit: 2, wantErr: "dpkg-deb: error: cannot read", fetches: 1},
	}
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes := t.TempDir(), t.TempDir()
			build, tools, bins, ide := filepath.Join(root, "build"), filepath.Join(root, "tools"), filepath.Join(root, "bin"), filepath.Join(root, "orca-ide")
			writeFakes(t, fakes, map[string]string{
				"id":         "#!/bin/sh\necho 0\n",
				"apt-get":    fakeInstaller,
				"apt-cache":  fakeAptCache,
				"dpkg-query": fakeStatusQuery,
				"curl":       "#!/bin/sh\nprintf 'curl\\n' >> \"$TEST_ROOT/fetches\"\n" + strings.TrimPrefix(fakeCurl, "#!/bin/sh\n"),
				"dpkg-deb":   fakeDpkgDeb,
				"mountpoint": fakeMounted,
			})
			debs := writeCapturedPayload(t, root, capturedDebs{
				Artifacts: []capturedDeb{{Architecture: "amd64", File: "orca-1.4.215.deb", Package: "orca-ide", Version: "1.4.215"}},
				Base:      []string{"base-files=13"},
				Debs:      []capturedDeb{},
			}, map[string]string{"orca-1.4.215.deb": tt.copy}, true)
			writePluginTestFile(t, ide, []byte("#!/bin/sh\n"), 0o755)
			writePluginTestFile(t, filepath.Join(root, "installed"), []byte("ii \tbase-files\n"), 0o644)
			writePluginTestFile(t, filepath.Join(root, "base"), []byte("installed\tbase-files=13\n"), 0o644)
			provision := strings.NewReplacer(
				"build_dir=/var/lib/cc-remote/build\n", "build_dir="+quote(build)+"\n",
				"payload_root=/opt/cc-remote/payload\n", "payload_root="+quote(filepath.Join(root, "payload"))+"\n",
				"tool_dir=/opt/cc-remote/tools\n", "tool_dir="+quote(tools)+"\n",
				"bin_dir=/usr/local/bin\n", "bin_dir="+quote(bins)+"\n",
				"'/opt/Orca/orca-ide'", quote(ide),
				"rm -rf /var/lib/apt/lists/*", ":",
			).Replace(string(scripts.ProvisionScript))
			cmd := exec.Command("bash", slices.Concat([]string{"-c", provision, "provision.sh", PhasePackages}, tt.args)...)
			cmd.Env = append(append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root), tt.env...)
			out, err := cmd.CombinedOutput()
			paths := strings.NewReplacer("@BUILD@", build, "@DEBS@", debs)
			if want := paths.Replace(tt.wantErr); exitCode(err) != tt.exit || !strings.Contains(string(out), want) {
				t.Fatalf("packages = %v\n%s\nwant exit %d with %q", err, out, tt.exit, want)
			}
			if apt, err := os.ReadFile(filepath.Join(root, "apt")); tt.apt != "" && (err != nil || string(apt) != paths.Replace(tt.apt)) {
				t.Errorf("apt-get calls:\n%s\n%v\nwant:\n%s", apt, err, paths.Replace(tt.apt))
			}
			if fetches, _ := os.ReadFile(filepath.Join(root, "fetches")); strings.Count(string(fetches), "curl\n") != tt.fetches {
				t.Errorf("curl ran %q, want %d fetches", fetches, tt.fetches)
			}
			seeds, err := os.ReadFile(filepath.Join(build, "seeds"))
			switch {
			case tt.args[0] == PackagesResident && !os.IsNotExist(err):
				t.Errorf("resident mode recorded seeds %q, %v", seeds, err)
			case tt.exit != 0 && tt.args[0] == PackagesFull && (err != nil || strings.Contains(string(seeds), "orca")):
				t.Errorf("seeds = %q, %v; want the apt seeds without any deb package", seeds, err)
			case tt.exit == 0 && tt.args[0] == PackagesFull && (err != nil || string(seeds) != tt.seeds):
				t.Errorf("seeds = %q, %v; want %q", seeds, err, tt.seeds)
			}
		})
	}
}

func TestAClosurePayloadDeclaresSchemaTwoAndPacksItsDebs(t *testing.T) {
	bare := scriptInventory()
	bare.Apt.Payload = nil
	tests := []struct {
		name      string
		inventory Inventory
		schema    string
		debs      bool
	}{
		{name: "a closure payload", inventory: scriptInventory(), schema: "2", debs: true},
		{name: "a payload without a closure", inventory: bare, schema: "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scripts, err := Render(tt.inventory, "agents")
			if err != nil {
				t.Fatal(err)
			}
			script := string(scripts.ProvisionScript)
			if got := strings.Count(script, "{schemaVersion: "+tt.schema+", tools: $tools, home: $home, arch: $arch, os: $os}"); got != 2 {
				t.Errorf("the pack writer and the payload check name schemaVersion %s %d times, want 2", tt.schema, got)
			}
			if got := strings.Contains(script, `pack_path required "$debs_dir"`); got != tt.debs {
				t.Errorf("the pack carries the captured debs: %v, want %v", got, tt.debs)
			}
		})
	}
}

func TestProvisionLoaderRegistersTheClosureUnderTheLock(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		exit int
	}{
		{name: "a registered closure is marked for the boot remount"},
		{name: "a failed ldconfig leaves the closure unmarked", env: []string{"LDCONFIG_FAIL=1"}, exit: 7},
	}
	scripts, err := Render(scriptInventory(), "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes := t.TempDir(), t.TempDir()
			lock, marker := filepath.Join(root, "lib", "cc-remote", "ldconfig.lock"), filepath.Join(root, "lib", "cc-remote", "closure.registered")
			writeFakes(t, fakes, map[string]string{"id": "#!/bin/sh\necho 0\n", "flock": fakeFlock, "ldconfig": fakeLdconfig})
			provision := strings.NewReplacer(
				"closure_lock=/var/lib/cc-remote/ldconfig.lock\n", "closure_lock="+quote(lock)+"\n",
				"closure_registered=/var/lib/cc-remote/closure.registered\n", "closure_registered="+quote(marker)+"\n",
			).Replace(string(scripts.ProvisionScript))
			cmd := exec.Command("bash", "-c", provision, "provision.sh", PhaseLoader)
			cmd.Env = append(append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root), tt.env...)
			out, err := cmd.CombinedOutput()
			if got, want := logLines(t, filepath.Join(root, "calls")), []string{"flock " + lock + " ldconfig", "ldconfig"}; !slices.Equal(got, want) {
				t.Errorf("calls = %q, want %q", got, want)
			}
			if tt.exit != 0 {
				if exitCode(err) != tt.exit {
					t.Fatalf("loader = %v\n%s\nwant exit %d", err, out, tt.exit)
				}
				if _, err := os.Lstat(marker); !os.IsNotExist(err) {
					t.Errorf("a failed ldconfig marked the closure registered: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loader failed: %v\n%s", err, out)
			}
			if info, err := os.Stat(filepath.Dir(lock)); err != nil || !info.IsDir() {
				t.Errorf("the lock directory is %v, %v; want a directory", info, err)
			}
			if info, err := os.Stat(marker); err != nil || info.Size() != 0 {
				t.Errorf("the registration marker is %v, %v; want an empty file", info, err)
			}
		})
	}
}

func TestProvisionPayloadActivatesTheClosure(t *testing.T) {
	arch := machineArch(t)
	image := "hsqs closure payload"
	sum := sha256.Sum256([]byte(image))
	sha := hex.EncodeToString(sum[:])
	tests := []struct {
		name    string
		foreign func(root, closure, payloads string) error
		wantErr func(root, closure, dir string) string
	}{
		{name: "the payload's closure is exposed and configured"},
		{
			name: "an existing projection link is accepted",
			foreign: func(root, closure, _ string) error {
				if err := os.MkdirAll(filepath.Join(root, "usr/share/X11"), 0o755); err != nil {
					return err
				}
				return os.Symlink(closure+root+"/usr/share/X11/xkb", filepath.Join(root, "usr/share/X11/xkb"))
			},
		},
		{
			name:    "a foreign projection path is fatal",
			foreign: func(root, _, _ string) error { return os.MkdirAll(filepath.Join(root, "usr/share/X11/xkb"), 0o755) },
			wantErr: func(root, _, _ string) string {
				return "cc-remote: " + root + "/usr/share/X11/xkb exists and is not the closure's link, so the payload cannot project it"
			},
		},
		{
			name: "a projection the closure lacks is fatal",
			foreign: func(root, closure, payloads string) error {
				return os.RemoveAll(filepath.Join(payloads, sha, closure, root, "usr/share/X11"))
			},
			wantErr: func(root, _, _ string) string {
				return "cc-remote: the closure has no directory " + root + "/usr/share/X11/xkb to project"
			},
		},
		{
			name: "a projection resolving outside the closure is fatal",
			foreign: func(root, closure, payloads string) error {
				link := filepath.Join(payloads, sha, closure, root, "usr/share/X11/xkb")
				if err := os.Remove(link); err != nil {
					return err
				}
				return os.Symlink("/etc", link)
			},
			wantErr: func(root, _, _ string) string {
				return "cc-remote: " + root + "/usr/share/X11/xkb resolves to /etc, outside the closure, so the payload cannot project it"
			},
		},
		{
			name: "a stale closure link from an older payload is replaced",
			foreign: func(_, closure, payloads string) error {
				return os.Symlink(filepath.Join(payloads, "older")+closure, closure)
			},
		},
		{
			name: "an identical conf written earlier is accepted",
			foreign: func(root, closure, _ string) error {
				return os.WriteFile(filepath.Join(root, "etc/ld.so.conf.d/zz-cc-remote-closure.conf"), []byte(closure+"/usr/lib/"+arch+"-linux-gnu\n"), 0o600)
			},
		},
		{
			name:    "a foreign closure path is fatal",
			foreign: func(_, closure, _ string) error { return os.MkdirAll(closure, 0o755) },
			wantErr: func(_, closure, dir string) string {
				return "cc-remote: " + closure + " is not a link to " + dir + closure + ", so the payload cannot own it"
			},
		},
		{
			name: "a foreign exposure path is fatal",
			foreign: func(root, _, _ string) error {
				return os.WriteFile(filepath.Join(root, "usr/local/bin/certutil"), []byte("#!/bin/sh\n"), 0o755)
			},
			wantErr: func(root, closure, _ string) string {
				return "cc-remote: " + root + "/usr/local/bin/certutil is not a link to " + closure + "/usr/bin/certutil, so the payload cannot own it"
			},
		},
		{
			name: "a loader conf written by something else is fatal",
			foreign: func(root, _, _ string) error {
				return os.WriteFile(filepath.Join(root, "etc/ld.so.conf.d/zz-cc-remote-closure.conf"), []byte("/usr/lib/foreign\n"), 0o644)
			},
			wantErr: func(root, _, _ string) string {
				return "cc-remote: " + root + "/etc/ld.so.conf.d/zz-cc-remote-closure.conf exists and is not the closure configuration this payload writes"
			},
		},
	}
	scripts, err := Render(scriptInventory(), "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes := t.TempDir(), t.TempDir()
			store, payloads, closure := filepath.Join(root, "store"), filepath.Join(root, "payload"), filepath.Join(root, "closure")
			loaderConf, fontsConf := filepath.Join(root, "etc/ld.so.conf.d/zz-cc-remote-closure.conf"), filepath.Join(root, "etc/fonts/conf.d/99-cc-remote-closure.conf")
			dir := filepath.Join(payloads, sha)
			writeFakes(t, fakes, map[string]string{
				"id":         "#!/bin/sh\necho 0\n",
				"mountpoint": "#!/bin/sh\n[ -e \"$2/.mounted\" ]\n",
				"findmnt":    "#!/bin/sh\n[ \"$*\" = \"-no SOURCE $3\" ] || exit 2\nexec cat \"$3/.source\"\n",
				"mount":      fakeClosureMount,
				"sprite-env": "#!/bin/sh\nexit 0\n",
				"ldconfig":   fakeLdconfig,
			})
			writePluginTestFile(t, filepath.Join(store, sha+".sqfs.admitted"), []byte(image), 0o600)
			writePluginTestFile(t, filepath.Join(dir, closure, "closure.json"), []byte("{}"), 0o644)
			writePluginTestFile(t, filepath.Join(dir, closure, root, "usr/share/xkeyboard-config-2/rules/evdev"), []byte("xkb"), 0o644)
			if err := os.MkdirAll(filepath.Join(dir, closure, root, "usr/share/X11"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../xkeyboard-config-2", filepath.Join(dir, closure, root, "usr/share/X11/xkb")); err != nil {
				t.Fatal(err)
			}
			for _, bin := range []string{"certutil", "fc-match"} {
				writePluginTestFile(t, filepath.Join(dir, closure, "usr/bin", bin), []byte("\x7fELF"), 0o755)
				if err := os.MkdirAll(filepath.Join(dir, root, "usr/local/bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(closure, "usr/bin", bin), filepath.Join(dir, root, "usr/local/bin", bin)); err != nil {
					t.Fatal(err)
				}
			}
			for _, share := range closureShares {
				if err := os.MkdirAll(filepath.Join(dir, closure, "usr/share", share), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, root, "usr/local/share", share)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(closure, "usr/share", share), filepath.Join(dir, root, "usr/local/share", share)); err != nil {
					t.Fatal(err)
				}
			}
			for _, sub := range []string{"usr/local/bin", "usr/local/share", "etc/ld.so.conf.d"} {
				if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.foreign != nil {
				if err := tt.foreign(root, closure, payloads); err != nil {
					t.Fatal(err)
				}
			}
			provision := strings.NewReplacer(
				"payload_root=/opt/cc-remote/payload\n", "payload_root="+quote(payloads)+"\n",
				"payload_store=/var/lib/cc-remote/payload\n", "payload_store="+quote(store)+"\n",
				"closure_root=/opt/cc-remote/closure\n", "closure_root="+quote(closure)+"\n",
				"closure_loader_conf=/etc/ld.so.conf.d/zz-cc-remote-closure.conf\n", "closure_loader_conf="+quote(loaderConf)+"\n",
				"closure_fonts_conf=/etc/fonts/conf.d/99-cc-remote-closure.conf\n", "closure_fonts_conf="+quote(fontsConf)+"\n",
				"expose link required '/opt/cc-remote/closure'\n", "expose link required "+quote(closure)+"\n",
				"'/usr/local/bin/", "'"+root+"/usr/local/bin/",
				"'/usr/local/share/", "'"+root+"/usr/local/share/",
				"closure_project '/usr/share/X11/xkb'\n", "closure_project "+quote(root+"/usr/share/X11/xkb")+"\n",
				` /opt/cc-remote/payload-mount.sh`+"\n", " "+quote(filepath.Join(root, "payload-mount.sh"))+"\n",
			).Replace(string(scripts.ProvisionScript))
			cmd := exec.Command("bash", "-c", provision, "provision.sh", PhasePayload, sha, "tools-fingerprint")
			cmd.Env = append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root, "SUDO_USER=root", "FINGERPRINT=tools-fingerprint")
			out, err := cmd.CombinedOutput()
			if _, err := os.Stat(filepath.Join(root, "calls")); !os.IsNotExist(err) {
				t.Errorf("the payload phase ran ldconfig: %v", err)
			}
			if tt.wantErr != nil {
				if want := tt.wantErr(root, closure, dir); exitCode(err) != 1 || !strings.Contains(string(out), want) {
					t.Fatalf("payload = %v\n%s\nwant exit 1 with %q", err, out, want)
				}
				if _, err := os.Stat(fontsConf); !os.IsNotExist(err) {
					t.Errorf("a refused activation wrote the fontconfig snippet: %v", err)
				}
				if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(loaderConf), ".cc-remote.*")); len(leftovers) != 0 {
					t.Errorf("left %q", leftovers)
				}
				return
			}
			if err != nil {
				t.Fatalf("payload failed: %v\n%s", err, out)
			}
			boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
			if err != nil {
				t.Fatal(err)
			}
			if marked, err := os.ReadFile(filepath.Join(store, sha+".sqfs.boot")); err != nil || string(marked) != string(boot) {
				t.Errorf("the boot marker is %q, %v; want this boot's id %q so the remount service skips the mount's rescan", marked, err, boot)
			}
			if target, err := os.Readlink(closure); err != nil || target != dir+closure {
				t.Errorf("closure -> %q, %v; want %q", target, err, dir+closure)
			}
			for _, bin := range []string{"certutil", "fc-match"} {
				if target, err := os.Readlink(filepath.Join(root, "usr/local/bin", bin)); err != nil || target != filepath.Join(closure, "usr/bin", bin) {
					t.Errorf("%s -> %q, %v; want the closure's bin", bin, target, err)
				}
			}
			for _, share := range closureShares {
				if target, err := os.Readlink(filepath.Join(root, "usr/local/share", share)); err != nil || target != filepath.Join(closure, "usr/share", share) {
					t.Errorf("%s -> %q, %v; want the closure's share tree", share, target, err)
				}
			}
			if target, err := os.Readlink(filepath.Join(root, "usr/share/X11/xkb")); err != nil || target != closure+root+"/usr/share/X11/xkb" {
				t.Errorf("xkb -> %q, %v; want the closure's projection", target, err)
			}
			fonts := "<?xml version=\"1.0\"?>\n<!DOCTYPE fontconfig SYSTEM \"urn:fontconfig:fonts.dtd\">\n<fontconfig>\n  <dir>" + closure + "/usr/share/fonts</dir>\n  <cachedir>" + closure + "/var/cache/fontconfig</cachedir>\n  <include ignore_missing=\"yes\">" + closure + "/etc/fonts/conf.d</include>\n</fontconfig>\n"
			for path, want := range map[string]string{loaderConf: closure + "/usr/lib/" + arch + "-linux-gnu\n", fontsConf: fonts} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want %q", path, got, err, want)
				}
				if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
					t.Errorf("%s mode = %v, %v; want 0644", path, info.Mode(), err)
				}
				if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".cc-remote.*")); len(leftovers) != 0 {
					t.Errorf("left %q", leftovers)
				}
			}
			helper, err := os.ReadFile(filepath.Join(root, "payload-mount.sh"))
			if err != nil || !strings.HasSuffix(string(helper), "done\nif [ -e /var/lib/cc-remote/closure.registered ]; then\n  flock /var/lib/cc-remote/ldconfig.lock ldconfig\nfi\n") || !strings.Contains(string(helper), "mount -t squashfs -o ro,nosuid,nodev,loop") {
				t.Errorf("the boot helper is %q, %v; want nosuid,nodev mounts followed by a locked ldconfig once the loader has registered the closure", helper, err)
			}
			marker, lock, bootFakes := filepath.Join(root, "closure.registered"), filepath.Join(root, "ldconfig.lock"), filepath.Join(root, "boot-fakes")
			boot := strings.NewReplacer(
				"/var/lib/cc-remote/payload", store,
				"/opt/cc-remote/payload", payloads,
				"/var/lib/cc-remote/closure.registered", marker,
				"/var/lib/cc-remote/ldconfig.lock", lock,
			).Replace(string(helper))
			writePluginTestFile(t, filepath.Join(root, "boot.sh"), []byte(boot), 0o700)
			writeFakes(t, bootFakes, map[string]string{"flock": fakeFlock})
			wantCalls := []string{"flock 9"}
			for _, registered := range []bool{false, true} {
				if registered {
					writePluginTestFile(t, marker, nil, 0o644)
					wantCalls = append(wantCalls, "flock 9", "flock "+lock+" ldconfig", "ldconfig")
				}
				cmd := exec.Command("sh", filepath.Join(root, "boot.sh"))
				cmd.Env = append(os.Environ(), "PATH="+bootFakes+":"+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("boot helper (registered=%t) failed: %v\n%s", registered, err, out)
				}
				if got := logLines(t, filepath.Join(root, "calls")); !slices.Equal(got, wantCalls) {
					t.Errorf("boot helper (registered=%t) calls = %q, want %q", registered, got, wantCalls)
				}
			}
		})
	}
}

type fakePackage struct {
	Version  string   `json:"version"`
	Arch     string   `json:"arch,omitempty"`
	Depends  string   `json:"depends,omitempty"`
	Provides string   `json:"provides,omitempty"`
	Files    []string `json:"files"`
}

type fakeDpkg struct {
	Packages map[string]*fakePackage `json:"packages"`
	Owners   map[string][]string     `json:"owners"`
}

type captureHost struct {
	t                                       *testing.T
	root, host, closure, build, fakes, debs string
	downloaded                              bool
	lib                                     string
	dpkg                                    fakeDpkg
	payload                                 AptPayload
	env                                     []string
	contents                                map[string]string
}

func newCaptureHost(t *testing.T) *captureHost {
	t.Helper()
	root := t.TempDir()
	arch := machineArch(t)
	h := &captureHost{
		t:        t,
		root:     root,
		host:     filepath.Join(root, "host"),
		closure:  filepath.Join(root, "closure"),
		build:    filepath.Join(root, "build"),
		fakes:    filepath.Join(root, "fakes"),
		debs:     filepath.Join(root, "debs"),
		lib:      "/usr/lib/" + arch + "-linux-gnu",
		contents: map[string]string{},
		payload: AptPayload{
			Resident:    []string{"bubblewrap"},
			Closure:     []string{"libfoo1", "fonts-x", "hicolor-icon-theme", "shared-mime-info", "libgtk-3-0t64", "xkb-data"},
			Bins:        []string{"footool"},
			Fonts:       []string{"X"},
			Consumers:   []string{".agent-browser/chrome", "/opt/cc-remote/tools/office/soffice.bin"},
			Projections: []string{"/usr/share/themes", "/usr/share/X11/xkb"},
		},
	}
	lib := h.lib
	h.dpkg = fakeDpkg{
		Packages: map[string]*fakePackage{
			"libfoo1":             {Version: "1.0-1", Depends: "libc6 (>= 2.34)", Files: []string{lib, lib + "/libfoo.so.1.0", lib + "/libfoo.so.1", lib + "/libfoo.so", lib + "/libfoo-env.conf", "/lib/" + arch + "-linux-gnu/libalias.so.1", lib + "/gio/modules/libgiofoo.so", "/usr/bin/footool", "/usr/share/doc/libfoo1", "/usr/share/doc/libfoo1/copyright", "/usr/share/doc/libfoo1/NEWS.gz", "/usr/share/doc/libfoo1/missing.txt"}},
			"fonts-x":             {Version: "1.0", Arch: "all", Files: []string{"/usr/share/fonts/truetype/x", "/usr/share/fonts/truetype/x/X.ttf"}},
			"hicolor-icon-theme":  {Version: "0.18-2", Arch: "all", Files: []string{"/usr/share/icons/hicolor", "/usr/share/icons/hicolor/index.theme", "/usr/share/icons/hicolor/cursor.theme"}},
			"shared-mime-info":    {Version: "2.4-5", Files: []string{"/usr/share/mime/packages/freedesktop.org.xml"}},
			"libgtk-3-0t64":       {Version: "3.24.49-1", Files: []string{lib + "/gtk-3.0/3.0.0/immodules/im-x.so", "/usr/share/glib-2.0/schemas/org.x.gschema.xml", "/usr/share/themes", "/usr/share/themes/Default/gtk-3.0/gtk.css"}},
			"xkb-data":            {Version: "2.44-1", Arch: "all", Files: []string{"/usr/share/X11", "/usr/share/X11/xkb", "/usr/share/xkeyboard-config-2", "/usr/share/xkeyboard-config-2/rules", "/usr/share/xkeyboard-config-2/rules/evdev"}},
			"openssh-server":      {Version: "1:9.9p1-3", Depends: "libwrap0, ssh-sftp-server | openssh-sftp-server", Files: []string{"/usr/sbin/sshd"}},
			"libwrap0":            {Version: "7.6.q-35", Files: []string{lib + "/libwrap.so.0"}},
			"openssh-sftp-server": {Version: "1:9.9p1-3", Provides: "ssh-sftp-server", Files: []string{"/usr/lib/openssh/sftp-server"}},
			"base-files":          {Version: "13ubuntu10", Files: []string{"/etc/environment", "/etc"}},
			"libc6":               {Version: "2.42-1ubuntu1", Files: []string{lib + "/libc.so.6"}},
		},
		Owners: map[string][]string{},
	}
	elf := string(elfWithInterp("/usr/lib/fake-ld.so"))
	writePluginTestFile(t, filepath.Join(h.host, "usr/lib/fake-ld.so"), []byte(fakeLdso), 0o700)
	h.file(lib+"/libfoo.so.1.0", "\x7fELF libfoo", 0o644)
	h.file(lib+"/libalias.so.1", "\x7fELF alias", 0o644)
	h.file(lib+"/gio/modules/libgiofoo.so", "\x7fELF gio", 0o644)
	h.file("/usr/bin/footool", elf, 0o755)
	h.file("/usr/share/doc/libfoo1/copyright", "copyright", 0o644)
	h.file("/usr/share/glib-2.0/schemas/org.x.gschema.xml", "<schemalist/>", 0o644)
	h.file("/usr/share/themes/Default/gtk-3.0/gtk.css", "css", 0o644)
	h.file(lib+"/gdk-pixbuf-2.0/2.10.0/loaders.cache", "\""+lib+"/gdk-pixbuf-2.0/2.10.0/loaders/libpixbufloader-x.so\"\n", 0o644)
	h.file("/usr/share/fonts/truetype/x/X.ttf", "ttf", 0o666)
	h.file("/usr/share/icons/hicolor/index.theme", "[Icon Theme]\n", 0o644)
	h.file("/usr/share/icons/hicolor/cursor.theme", "[Icon Theme]\nInherits=Adwaita\n", 0o644)
	h.file("/usr/share/icons/hicolor/icon-theme.cache", "icon cache", 0o644)
	h.file("/usr/share/mime/packages/freedesktop.org.xml", "<mime-info/>", 0o644)
	h.file("/usr/share/mime/packages/io.systemd.xml", "<mime-info/>", 0o644)
	h.file("/usr/share/xkeyboard-config-2/rules/evdev", "evdev", 0o644)
	h.file("/usr/share/mime/mime.cache", "MIME", 0o644)
	h.file("/usr/share/mime/globs", "globs", 0o644)
	h.file("/usr/share/glib-2.0/schemas/gschemas.compiled", "GVariant", 0o644)
	h.file(lib+"/gio/modules/giomodule.cache", "gio", 0o644)
	h.file(lib+"/gtk-3.0/3.0.0/immodules/im-x.so", "\x7fELF im", 0o644)
	h.file(lib+"/gtk-3.0/3.0.0/immodules.cache", "\""+lib+"/gtk-3.0/3.0.0/immodules/im-x.so\" \n\"x\" \"X\" \"\" \"\" \"en\" \n", 0o644)
	h.file("/etc/os-release", "PRETTY_NAME=\"Ubuntu 26.04 LTS\"\nVERSION_ID=\"26.04\"\nVERSION=\"26.04 LTS (Resolute Raccoon)\"\n", 0o644)
	h.file("/home/u/.agent-browser/chrome", elf, 0o755)
	h.file("/opt/cc-remote/tools/office/soffice.bin", elf, 0o755)
	h.link(lib+"/libfoo.so.1", "libfoo.so.1.0")
	h.link(lib+"/libfoo.so", lib+"/libfoo.so.1.0")
	h.link(lib+"/libfoo-env.conf", "/etc/environment")
	h.link("/usr/share/doc/libfoo1/NEWS.gz", "../../common-licenses/GPL")
	h.link("/lib", "usr/lib")
	h.link("/etc/alternatives/x-cursor-theme", "/usr/share/icons/hicolor/cursor.theme")
	h.link("/usr/share/X11/xkb", "../xkeyboard-config-2")
	for _, dir := range []string{"/usr/local/bin", "/usr/local/share"} {
		if err := os.MkdirAll(filepath.Join(h.host, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	before := "ii \tbase-files\nii \tlibc6\n"
	after := before + "ii \tfonts-x\nii \thicolor-icon-theme\nii \tlibfoo1\nii \tlibgtk-3-0t64\nii \tlibwrap0\nii \topenssh-server\nii \topenssh-sftp-server\nii \tshared-mime-info\nii \txkb-data\n"
	writePluginTestFile(t, filepath.Join(h.build, "packages.before"), []byte(before), 0o644)
	writePluginTestFile(t, filepath.Join(h.build, "packages.after"), []byte(after), 0o644)
	writePluginTestFile(t, filepath.Join(h.build, "seeds"), []byte("ca-certificates\nopenssh-server\nbubblewrap\n"), 0o644)
	writeFakes(t, h.fakes, map[string]string{"dpkg-query": fakeDpkgQuery, "fc-cache": fakeFcCache, "runuser": fakeRunuser, "dpkg-deb": fakeDpkgDeb})
	return h
}

func (h *captureHost) listed(record string) []string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.build, record))
	if err != nil {
		h.t.Fatal(err)
	}
	var packages []string
	for line := range strings.Lines(string(raw)) {
		if status, pkg, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "\t"); strings.HasPrefix(status, "ii") {
			packages = append(packages, pkg)
		}
	}
	return packages
}

func (h *captureHost) version(pkg string) (string, string) {
	if p, ok := h.dpkg.Packages[pkg]; ok {
		return p.Version, cmp.Or(p.Arch, "amd64")
	}
	return "0", "amd64"
}

func (h *captureHost) deb(dir, pkg, version string) string {
	h.t.Helper()
	_, arch := h.version(pkg)
	file := pkg + "_" + strings.ReplaceAll(version, ":", "%3a") + "_" + arch + ".deb"
	path := filepath.Join(h.build, dir, file)
	writePluginTestFile(h.t, path, []byte("deb "+pkg+" "+version), 0o644)
	writePluginTestFile(h.t, path+".control", []byte("Package: "+pkg+"\nVersion: "+version+"\nArchitecture: "+arch+"\n"), 0o644)
	return file
}

func (h *captureHost) download() {
	h.t.Helper()
	h.downloaded = true
	before, after := h.listed("packages.before"), h.listed("packages.after")
	var base strings.Builder
	for _, pkg := range before {
		version, _ := h.version(pkg)
		base.WriteString(pkg + "=" + version + "\n")
	}
	writePluginTestFile(h.t, filepath.Join(h.build, "base"), []byte(base.String()), 0o644)
	for _, dir := range []string{"debs/partial", "artifacts"} {
		if err := os.MkdirAll(filepath.Join(h.build, dir), 0o755); err != nil {
			h.t.Fatal(err)
		}
	}
	for _, pkg := range after {
		if !slices.Contains(before, pkg) && !slices.Contains(h.payload.Closure, pkg) {
			version, _ := h.version(pkg)
			h.deb("debs", pkg, version)
		}
	}
}

func (h *captureHost) file(path, content string, mode os.FileMode) {
	h.t.Helper()
	writePluginTestFile(h.t, filepath.Join(h.host, path), []byte(content), 0o644)
	if err := os.Chmod(filepath.Join(h.host, path), mode); err != nil {
		h.t.Fatal(err)
	}
	h.contents[path] = content
}

func (h *captureHost) link(path, target string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(h.host, path)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(h.host, path)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *captureHost) proof(output string) string {
	h.t.Helper()
	path := filepath.Join(h.root, "fc-cache.out")
	writePluginTestFile(h.t, path, []byte(output), 0o644)
	return path
}

func (h *captureHost) liveFonts() {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.host, "usr/share/fonts")); err != nil {
		h.t.Fatal(err)
	}
	files := []string{"/usr/share/fonts/opentype", "/usr/share/fonts/opentype/noto", "/usr/share/fonts/opentype/noto/N.otf", "/usr/share/fonts/truetype", "/usr/share/fonts/truetype/freefont", "/usr/share/fonts/truetype/freefont/F.ttf", "/usr/share/fonts/truetype/noto", "/usr/share/fonts/truetype/noto/N.ttf"}
	h.dpkg.Packages["fonts-x"].Files = files
	for _, path := range files {
		if filepath.Ext(path) != "" {
			h.file(path, "font", 0o644)
		}
	}
}

func (h *captureHost) resident(t *testing.T) []string {
	t.Helper()
	var manifest struct {
		Resident []string `json:"resident"`
	}
	raw, err := os.ReadFile(h.closure + "/closure.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("closure.json: %v\n%s", err, raw)
	}
	return manifest.Resident
}

func (h *captureHost) record(extra map[string]string) {
	h.t.Helper()
	for name, lines := range extra {
		recorded, err := os.ReadFile(filepath.Join(h.build, name))
		if err != nil {
			h.t.Fatal(err)
		}
		writePluginTestFile(h.t, filepath.Join(h.build, name), append(recorded, lines...), 0o644)
	}
}

func (h *captureHost) entries() []string {
	h.t.Helper()
	var paths []string
	if err := filepath.WalkDir(h.host, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != h.host {
			paths = append(paths, strings.TrimPrefix(path, h.host))
		}
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
	return paths
}

func (h *captureHost) run() (string, error) {
	h.t.Helper()
	script, err := assets.FS.ReadFile("capture.py")
	if err != nil {
		h.t.Fatal(err)
	}
	loader, err := assets.FS.ReadFile("loader.py")
	if err != nil {
		h.t.Fatal(err)
	}
	writePluginTestFile(h.t, filepath.Join(h.root, "capture.py"), script, 0o644)
	writePluginTestFile(h.t, filepath.Join(h.root, "loader.py"), loader, 0o644)
	writePluginTestFile(h.t, filepath.Join(h.root, "dpkg.json"), mustJSON(h.t, h.dpkg), 0o644)
	writePluginTestFile(h.t, filepath.Join(h.root, "closure.json"), mustJSON(h.t, closure(Inventory{Apt: Apt{Payload: &h.payload}})), 0o644)
	if !h.downloaded {
		h.download()
	}
	cmd := exec.Command("python3", filepath.Join(h.root, "capture.py"), filepath.Join(h.root, "closure.json"), h.host, h.closure, h.build, "/home/u", filepath.Join(h.root, "loader.py"), h.debs)
	cmd.Env = append(append(os.Environ(), "PATH="+h.fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+h.root, "FAKE_DPKG="+filepath.Join(h.root, "dpkg.json"), "SUDO_USER=u"), h.env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCaptureBuildsTheClosure(t *testing.T) {
	arch := machineArch(t)
	lib := "/usr/lib/" + arch + "-linux-gnu"
	tests := []struct {
		name      string
		mutate    func(h *captureHost)
		wantErr   func(h *captureHost) string
		check     func(t *testing.T, h *captureHost)
		untouched bool
	}{
		{name: "a projection owned by a package outside the closure is fatal", mutate: func(h *captureHost) {
			h.dpkg.Owners["/usr/share/themes"] = []string{"libgtk-3-0t64", "base-files"}
		}, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.projections: /usr/share/themes is also owned by base-files, so a link there would hide their files"
		}, untouched: true},
		{name: "a projection no closure package ships as a directory is fatal", mutate: func(h *captureHost) { h.payload.Projections = []string{"/usr/share/nothere"} }, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.projections: /usr/share/nothere is not a directory a closure package ships"
		}, untouched: true},
		{name: "a projection resolving outside the closure is fatal", mutate: func(h *captureHost) {
			if err := os.Remove(filepath.Join(h.host, "usr/share/X11/xkb")); err != nil {
				t.Fatal(err)
			}
			h.link("/usr/share/X11/xkb", "/etc")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.projections: /usr/share/X11/xkb resolves to /etc, outside the closure"
		}},
		{name: "share trees the closure lacks are not exposed", mutate: func(h *captureHost) {
			h.dpkg.Packages["libgtk-3-0t64"].Files = slices.DeleteFunc(h.dpkg.Packages["libgtk-3-0t64"].Files, func(path string) bool { return path == "/usr/share/glib-2.0/schemas/org.x.gschema.xml" })
			if err := os.Remove(filepath.Join(h.host, "usr/share/glib-2.0/schemas/org.x.gschema.xml")); err != nil {
				t.Fatal(err)
			}
		}, check: func(t *testing.T, h *captureHost) {
			if _, err := os.Lstat(filepath.Join(h.host, "usr/local/share/glib-2.0/schemas")); !os.IsNotExist(err) {
				t.Errorf("the capture exposed a schemas tree the closure lacks: %v", err)
			}
			if _, err := os.Lstat(h.closure + "/usr/share/glib-2.0/schemas/gschemas.compiled"); !os.IsNotExist(err) {
				t.Errorf("the closure carries a schema cache without any schema: %v", err)
			}
			for _, link := range []string{"usr/local/share/mime", "usr/local/share/icons", "usr/local/bin/footool"} {
				if _, err := os.Readlink(filepath.Join(h.host, link)); err != nil {
					t.Errorf("%s is not exposed: %v", link, err)
				}
			}
		}},
		{name: "a captured module without its generated cache is fatal", mutate: func(h *captureHost) {
			if err := os.Remove(filepath.Join(h.host, lib, "gio/modules/giomodule.cache")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(*captureHost) string {
			return "cc-remote: " + lib + "/gio/modules/giomodule.cache was not generated on the build machine"
		}},
		{name: "modules the closure lacks need no cache", mutate: func(h *captureHost) {
			h.dpkg.Packages["libfoo1"].Files = slices.DeleteFunc(h.dpkg.Packages["libfoo1"].Files, func(path string) bool { return path == lib+"/gio/modules/libgiofoo.so" })
			if err := os.Remove(filepath.Join(h.host, lib, "gio/modules/libgiofoo.so")); err != nil {
				t.Fatal(err)
			}
		}, check: func(t *testing.T, h *captureHost) {
			if _, err := os.Lstat(h.closure + lib + "/gio/modules/giomodule.cache"); !os.IsNotExist(err) {
				t.Errorf("the closure carries a gio module cache without any gio module: %v", err)
			}
			if _, err := os.Lstat(h.closure + lib + "/gtk-3.0/3.0.0/immodules.cache"); err != nil {
				t.Errorf("the closure lacks the cache of the gtk modules it carries: %v", err)
			}
		}},
		{name: "a missing consumer is fatal before any write", mutate: func(h *captureHost) {
			if err := os.Remove(filepath.Join(h.host, "home/u/.agent-browser/chrome")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.consumers: .agent-browser/chrome is not a file at /home/u/.agent-browser/chrome"
		}, untouched: true},
		{name: "an undeclared package in the measured closure is fatal", mutate: func(h *captureHost) {
			h.payload.Closure = slices.DeleteFunc(h.payload.Closure, func(name string) bool { return name == "shared-mime-info" })
		}, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.closure differs from the measured partition: resident or absent [], undeclared ['shared-mime-info']"
		}, untouched: true},
		{name: "a declared package the resolver keeps resident is fatal", mutate: func(h *captureHost) {
			h.payload.Closure = append(h.payload.Closure, "libwrap0")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.closure differs from the measured partition: resident or absent ['libwrap0'], undeclared []"
		}, untouched: true},
		{name: "missing transaction records are fatal", mutate: func(h *captureHost) {
			if err := os.Remove(filepath.Join(h.build, "seeds")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(h *captureHost) string {
			return "cc-remote: " + h.build + "/seeds is missing, so this machine did not run packages full"
		}, untouched: true},
		{name: "a bin no closure package ships is fatal", mutate: func(h *captureHost) { h.payload.Bins = []string{"nosuch"} }, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.bins: no closure package ships /usr/bin/nosuch"
		}, untouched: true},
		{name: "an owned exposure path is fatal", mutate: func(h *captureHost) { h.dpkg.Owners["/usr/local/share/mime"] = []string{"evil"} }, wantErr: func(*captureHost) string {
			return "cc-remote: /usr/local/share/mime is owned by a package, so the payload cannot own it"
		}, untouched: true},
		{name: "an exposure path already on the build machine is fatal", mutate: func(h *captureHost) { h.file("/usr/local/bin/footool", "#!/bin/sh\n", 0o755) }, wantErr: func(*captureHost) string {
			return "cc-remote: /usr/local/bin/footool already exists on the build machine"
		}, untouched: true},
		{name: "an existing closure root is fatal", mutate: func(h *captureHost) {
			if err := os.MkdirAll(h.closure, 0o755); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(h *captureHost) string { return "cc-remote: " + h.closure + " already exists on the build machine" }},
		{name: "a setuid file is fatal", mutate: func(h *captureHost) {
			if err := os.Chmod(filepath.Join(h.host, "usr/bin/footool"), 0o755|os.ModeSetuid); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(*captureHost) string { return "cc-remote: libfoo1 ships setuid or setgid /usr/bin/footool" }},
		{name: "a special file is fatal", mutate: func(h *captureHost) {
			if err := syscall.Mkfifo(filepath.Join(h.host, lib, "libfoo.fifo"), 0o644); err != nil {
				t.Fatal(err)
			}
			h.dpkg.Packages["libfoo1"].Files = append(h.dpkg.Packages["libfoo1"].Files, lib+"/libfoo.fifo")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: libfoo1 ships " + lib + "/libfoo.fifo, which is neither a regular file nor a symlink"
		}},
		{name: "an unowned absolute symlink leaving the closure is fatal", mutate: func(h *captureHost) {
			h.link(lib+"/libstray.so", "/opt/stray/lib.so")
			h.dpkg.Packages["libfoo1"].Files = append(h.dpkg.Packages["libfoo1"].Files, lib+"/libstray.so")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: symlink " + lib + "/libstray.so -> /opt/stray/lib.so leaves the closure and no pre-existing package owns its target (None)"
		}},
		{name: "an absolute symlink into a resident package is fatal", mutate: func(h *captureHost) {
			h.link(lib+"/libsshd.so", "/usr/sbin/sshd")
			h.dpkg.Packages["libfoo1"].Files = append(h.dpkg.Packages["libfoo1"].Files, lib+"/libsshd.so")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: symlink " + lib + "/libsshd.so -> /usr/sbin/sshd leaves the closure and no pre-existing package owns its target (['openssh-server'])"
		}},
		{name: "a relative symlink escaping the closure is fatal", mutate: func(h *captureHost) {
			if err := os.Remove(filepath.Join(h.host, "usr/share/doc/libfoo1/NEWS.gz")); err != nil {
				t.Fatal(err)
			}
			h.link("/usr/share/doc/libfoo1/NEWS.gz", "../../../../../../../etc/passwd")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: libfoo1 symlink /usr/share/doc/libfoo1/NEWS.gz -> ../../../../../../../etc/passwd escapes the closure"
		}},
		{name: "a captured directory symlink never redirects a write", mutate: func(h *captureHost) {
			h.link("/usr/share/libfoo", "/etc")
			h.file("/etc/libfoo-redirected", "x", 0o644)
			h.dpkg.Packages["libfoo1"].Files = append(h.dpkg.Packages["libfoo1"].Files, "/usr/share/libfoo", "/usr/share/libfoo/libfoo-redirected")
		}, wantErr: func(h *captureHost) string {
			return "cc-remote: /usr/share/libfoo would be written through " + h.closure + "/usr/share/libfoo, which is not a directory inside the closure"
		}},
		{name: "a missing generated mime database is fatal", mutate: func(h *captureHost) {
			if err := os.Remove(filepath.Join(h.host, "usr/share/mime/mime.cache")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(*captureHost) string {
			return "cc-remote: /usr/share/mime/mime.cache was not generated on the build machine"
		}},
		{name: "a module cache naming a module outside the closure is fatal", mutate: func(h *captureHost) {
			h.file(lib+"/gtk-3.0/3.0.0/immodules.cache", "\""+lib+"/gtk-3.0/3.0.0/immodules/im-missing.so\" \n", 0o644)
		}, wantErr: func(*captureHost) string {
			return "cc-remote: " + lib + "/gtk-3.0/3.0.0/immodules.cache names " + lib + "/gtk-3.0/3.0.0/immodules/im-missing.so, which the closure lacks"
		}},
		{name: "no gtk module cache is fatal", mutate: func(h *captureHost) {
			if err := os.Remove(filepath.Join(h.host, lib, "gtk-3.0/3.0.0/immodules.cache")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(*captureHost) string {
			return "cc-remote: " + lib + "/gtk-3.0/3.0.0/immodules.cache was not generated on the build machine"
		}},
		{name: "a stale font cache is fatal", mutate: func(h *captureHost) { h.env = []string{"FC_STALE=1"} }, wantErr: func(h *captureHost) string {
			return "cc-remote: the font cache is not valid for every closure font directory: ['" + h.closure + "/usr/share/fonts: caching, new cache contents: 1 fonts, 1 dirs']"
		}},
		{name: "a duplicate loop after a valid font directory scan is accepted", mutate: func(h *captureHost) { h.env = []string{"FC_LOOP=1"} }},
		{name: "a font directory loop without a valid scan is fatal", mutate: func(h *captureHost) { h.env = []string{"FC_LOOP_ONLY=1"} }, wantErr: func(h *captureHost) string {
			return "cc-remote: the font cache is not valid for every closure font directory: ['" + h.closure + "/usr/share/fonts: skipping, looped directory detected']"
		}},
		{name: "a font directory omitted from the proof is fatal", mutate: func(h *captureHost) { h.env = []string{"FC_MISSING=1"} }, wantErr: func(h *captureHost) string {
			return "cc-remote: the font cache has no valid scan for closure font directories: ['" + h.closure + "/usr/share/fonts/truetype', '" + h.closure + "/usr/share/fonts/truetype/x']"
		}},
		{name: "a font directory absent from the closure is fatal", mutate: func(h *captureHost) { h.env = []string{"FC_UNKNOWN=1"} }, wantErr: func(h *captureHost) string {
			return "cc-remote: the font cache is not valid for every closure font directory: ['" + h.closure + "/usr/share/fonts/unknown: skipping, existing cache is valid: 1 fonts, 0 dirs']"
		}},
		{name: "a looped directory before its valid scan is fatal", mutate: func(h *captureHost) {
			h.env = []string{"FC_PROOF=" + h.proof("@FONTS@/truetype/x: skipping, looped directory detected\n@FONTS@: skipping, existing cache is valid: 0 fonts, 1 dirs\n@FONTS@/truetype: skipping, existing cache is valid: 0 fonts, 1 dirs\n@FONTS@/truetype/x: skipping, existing cache is valid: 1 fonts, 0 dirs\n")}
		}, wantErr: func(h *captureHost) string {
			return "cc-remote: the font cache is not valid for every closure font directory: ['" + h.closure + "/usr/share/fonts/truetype/x: skipping, looped directory detected']"
		}},
		{name: "a closure font directory fc-cache never scanned is fatal", mutate: func(h *captureHost) {
			h.env = []string{"FC_PROOF=" + h.proof("@FONTS@: skipping, existing cache is valid: 0 fonts, 1 dirs\n@FONTS@/truetype: skipping, existing cache is valid: 0 fonts, 1 dirs\n")}
		}, wantErr: func(h *captureHost) string {
			return "cc-remote: the font cache has no valid scan for closure font directories: ['" + h.closure + "/usr/share/fonts/truetype/x']"
		}},
		{name: "the live fc-cache output with covered loops passes", mutate: func(h *captureHost) {
			h.liveFonts()
			h.env = []string{"FC_PROOF=" + h.proof(liveFontProof)}
		}, check: func(t *testing.T, h *captureHost) {
			for _, path := range []string{"/usr/share/fonts/opentype/noto/N.otf", "/usr/share/fonts/truetype/freefont/F.ttf", "/usr/share/fonts/truetype/noto/N.ttf", "/var/cache/fontconfig/abc-le64.cache-9", "/closure.json"} {
				if _, err := os.Lstat(h.closure + path); err != nil {
					t.Errorf("the closure lacks %s: %v", path, err)
				}
			}
		}},
		{name: "an unresolved consumer library is fatal", mutate: func(h *captureHost) { h.env = []string{"LDSO_MISSING=libmissing.so.9"} }, wantErr: func(*captureHost) string {
			return "cc-remote: /home/u/.agent-browser/chrome cannot load libmissing.so.9"
		}},
		{name: "a dpkg-query search failing for another reason is fatal", mutate: func(h *captureHost) { h.env = []string{"FAKE_DPKG_SEARCH=crash"} }, wantErr: func(*captureHost) string {
			return "cc-remote: dpkg-query -S exited 2: dpkg-query: error: cannot open the package database"
		}, untouched: true},
		{name: "a dpkg-query search refusing without a report is fatal", mutate: func(h *captureHost) { h.env = []string{"FAKE_DPKG_SEARCH=mute"} }, wantErr: func(*captureHost) string {
			return "cc-remote: dpkg-query -S exited 1 without reporting why"
		}, untouched: true},
		{name: "a symlinked font directory is fatal", mutate: func(h *captureHost) {
			if err := os.RemoveAll(filepath.Join(h.host, "usr/share/fonts")); err != nil {
				h.t.Fatal(err)
			}
			h.link("/usr/share/fonts", "/etc")
			h.dpkg.Packages["fonts-x"].Files = []string{"/usr/share/fonts"}
		}, wantErr: func(h *captureHost) string {
			return "cc-remote: /usr/share/fonts would be written through " + h.closure + "/usr/share/fonts, which is not a directory inside the closure"
		}},
		{name: "a dependency the base already satisfies stays in the closure", mutate: func(h *captureHost) {
			h.dpkg.Packages["base-files"].Provides = "base-virtual"
			h.dpkg.Packages["openssh-server"].Depends += ", libc6 | libfoo1, base-virtual | libfoo1"
		}},
		{name: "a base package removed during provisioning is absent from the partition query", mutate: func(h *captureHost) {
			h.record(map[string]string{"packages.before": "ii \twatchman\n"})
		}},
		{name: "a seeded deb artifact keeps its dependencies resident", mutate: func(h *captureHost) {
			h.dpkg.Packages["orca-ide"] = &fakePackage{Version: "1.4.215", Depends: "libnew1", Files: []string{"/opt/Orca/orca-ide"}}
			h.dpkg.Packages["libnew1"] = &fakePackage{Version: "1.0-1", Files: []string{lib + "/libnew.so.1"}}
			h.record(map[string]string{"packages.after": "ii \tlibnew1\nii \torca-ide\n", "seeds": "orca-ide\n"})
		}, check: func(t *testing.T, h *captureHost) {
			if got, want := h.resident(t), []string{"libnew1", "libwrap0", "openssh-server", "openssh-sftp-server", "orca-ide"}; !slices.Equal(got, want) {
				t.Errorf("resident = %v, want %v", got, want)
			}
		}},
		{name: "a resident seed already on the base keeps its new dependency resident", mutate: func(h *captureHost) {
			h.dpkg.Packages["bubblewrap"] = &fakePackage{Version: "0.11.0-1", Depends: "libnew2 (>= 1.0)", Files: []string{"/usr/bin/bwrap"}}
			h.dpkg.Packages["libnew2"] = &fakePackage{Version: "1.0-1", Files: []string{lib + "/libnew2.so.2"}}
			h.record(map[string]string{"packages.before": "ii \tbubblewrap\n", "packages.after": "ii \tbubblewrap\nii \tlibnew2\n"})
		}, check: func(t *testing.T, h *captureHost) {
			if got, want := h.resident(t), []string{"libnew2", "libwrap0", "openssh-server", "openssh-sftp-server"}; !slices.Equal(got, want) {
				t.Errorf("resident = %v, want %v", got, want)
			}
		}},
		{name: "a resident root reaching a new package through an upgraded base package is fatal", mutate: func(h *captureHost) {
			h.dpkg.Packages["openssh-server"].Depends += ", libmiddle1 (>= 2)"
			h.dpkg.Packages["libmiddle1"] = &fakePackage{Version: "2.0-1", Depends: "libnew3", Files: []string{lib + "/libmiddle.so.1"}}
			h.dpkg.Packages["libnew3"] = &fakePackage{Version: "1.0-1", Files: []string{lib + "/libnew3.so.3"}}
			h.payload.Closure = append(h.payload.Closure, "libnew3")
			h.record(map[string]string{"packages.before": "ii \tlibmiddle1\n", "packages.after": "ii \tlibmiddle1\nii \tlibnew3\n"})
		}, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.resident: openssh-server reaches the new package libnew3 through the base package libmiddle1 (openssh-server -> libmiddle1 -> libnew3), and packages.before records no versions to show whether a resident install upgrades libmiddle1; move libnew3 to apt.payload.resident"
		}, untouched: true},
		{name: "a base package a closure package upgraded is fatal when a resident root reaches it", mutate: func(h *captureHost) {
			h.dpkg.Packages["openssh-server"].Depends += ", libmiddle1"
			h.dpkg.Packages["libfoo1"].Depends += ", libmiddle1 (>= 2)"
			h.dpkg.Packages["libmiddle1"] = &fakePackage{Version: "2.0-1", Depends: "libnew3", Files: []string{lib + "/libmiddle.so.1"}}
			h.dpkg.Packages["libnew3"] = &fakePackage{Version: "1.0-1", Files: []string{lib + "/libnew3.so.3"}}
			h.payload.Closure = append(h.payload.Closure, "libnew3")
			h.record(map[string]string{"packages.before": "ii \tlibmiddle1\n", "packages.after": "ii \tlibmiddle1\nii \tlibnew3\n"})
		}, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.resident: openssh-server reaches the new package libnew3 through the base package libmiddle1 (openssh-server -> libmiddle1 -> libnew3), and packages.before records no versions to show whether a resident install upgrades libmiddle1; move libnew3 to apt.payload.resident"
		}, untouched: true},
		{name: "a resident seed on the base reaching a new package through another base package is fatal", mutate: func(h *captureHost) {
			h.dpkg.Packages["bubblewrap"] = &fakePackage{Version: "0.11.0-1", Depends: "libmiddle1 (>= 2)", Files: []string{"/usr/bin/bwrap"}}
			h.dpkg.Packages["libmiddle1"] = &fakePackage{Version: "2.0-1", Depends: "libnew2", Files: []string{lib + "/libmiddle.so.1"}}
			h.dpkg.Packages["libnew2"] = &fakePackage{Version: "1.0-1", Files: []string{lib + "/libnew2.so.2"}}
			h.payload.Closure = append(h.payload.Closure, "libnew2")
			h.record(map[string]string{"packages.before": "ii \tbubblewrap\nii \tlibmiddle1\n", "packages.after": "ii \tbubblewrap\nii \tlibmiddle1\nii \tlibnew2\n"})
		}, wantErr: func(*captureHost) string {
			return "cc-remote: apt.payload.resident: bubblewrap reaches the new package libnew2 through the base package libmiddle1 (bubblewrap -> libmiddle1 -> libnew2), and packages.before records no versions to show whether a resident install upgrades libmiddle1; move libnew2 to apt.payload.resident"
		}, untouched: true},
		{name: "a new package moved to apt.payload.resident behind a base package stays resident", mutate: func(h *captureHost) {
			h.dpkg.Packages["bubblewrap"] = &fakePackage{Version: "0.11.0-1", Depends: "libmiddle1 (>= 2)", Files: []string{"/usr/bin/bwrap"}}
			h.dpkg.Packages["libmiddle1"] = &fakePackage{Version: "2.0-1", Depends: "libnew2", Files: []string{lib + "/libmiddle.so.1"}}
			h.dpkg.Packages["libnew2"] = &fakePackage{Version: "1.0-1", Files: []string{lib + "/libnew2.so.2"}}
			h.payload.Resident = append(h.payload.Resident, "libnew2")
			h.record(map[string]string{"packages.before": "ii \tbubblewrap\nii \tlibmiddle1\n", "packages.after": "ii \tbubblewrap\nii \tlibmiddle1\nii \tlibnew2\n", "seeds": "libnew2\n"})
		}, check: func(t *testing.T, h *captureHost) {
			if got, want := h.resident(t), []string{"libnew2", "libwrap0", "openssh-server", "openssh-sftp-server"}; !slices.Equal(got, want) {
				t.Errorf("resident = %v, want %v", got, want)
			}
		}},
		{name: "the resident download is captured beside the closure", check: func(t *testing.T, h *captureHost) {
			want := capturedDebs{Artifacts: []capturedDeb{}, Base: []string{"base-files=13ubuntu10", "libc6=2.42-1ubuntu1"}}
			for _, deb := range []capturedDeb{{Package: "libwrap0", Version: "7.6.q-35"}, {Package: "openssh-server", Version: "1:9.9p1-3"}, {Package: "openssh-sftp-server", Version: "1:9.9p1-3"}} {
				deb.Architecture, deb.File = "amd64", deb.Package+"_"+strings.ReplaceAll(deb.Version, ":", "%3a")+"_amd64.deb"
				sum := sha256.Sum256([]byte("deb " + deb.Package + " " + deb.Version))
				deb.SHA256 = hex.EncodeToString(sum[:])
				want.Debs = append(want.Debs, deb)
			}
			raw, err := json.MarshalIndent(want, "", " ")
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(h.debs, "debs.json")); err != nil || string(got) != string(raw)+"\n" {
				t.Errorf("debs.json = %s, %v; want %s", got, err, raw)
			}
			entries, err := os.ReadDir(h.debs)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if wantNames := []string{"debs.json", want.Debs[0].File, want.Debs[1].File, want.Debs[2].File}; !slices.Equal(names, wantNames) {
				t.Errorf("the captured debs are %q, want %q", names, wantNames)
			}
			for _, deb := range want.Debs {
				copied, err := os.ReadFile(filepath.Join(h.debs, deb.File))
				downloaded, downloadErr := os.ReadFile(filepath.Join(h.build, "debs", deb.File))
				if err != nil || downloadErr != nil || string(copied) != string(downloaded) {
					t.Errorf("%s was copied as %q (%v), want the download %q (%v)", deb.File, copied, err, downloaded, downloadErr)
				}
			}
		}},
		{name: "an artifact deb is captured beside the resident download", mutate: func(h *captureHost) {
			h.dpkg.Packages["orca-ide"] = &fakePackage{Version: "1.4.215", Files: []string{"/opt/Orca/orca-ide"}}
			h.record(map[string]string{"packages.after": "ii \torca-ide\n", "seeds": "orca-ide\n"})
			h.download()
			if err := os.Remove(filepath.Join(h.build, "debs", "orca-ide_1.4.215_amd64.deb")); err != nil {
				t.Fatal(err)
			}
			h.deb("artifacts", "orca-ide", "1.4.215")
		}, check: func(t *testing.T, h *captureHost) {
			var got capturedDebs
			raw, err := os.ReadFile(filepath.Join(h.debs, "debs.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if want := []capturedDeb{{Architecture: "amd64", File: "orca-ide_1.4.215_amd64.deb", Package: "orca-ide", Version: "1.4.215"}}; !reflect.DeepEqual(got.Artifacts, want) || len(got.Debs) != 3 {
				t.Errorf("debs.json artifacts = %+v with %d debs, want %+v with 3", got.Artifacts, len(got.Debs), want)
			}
			if copied, err := os.ReadFile(filepath.Join(h.debs, "orca-ide_1.4.215_amd64.deb")); err != nil || string(copied) != "deb orca-ide 1.4.215" {
				t.Errorf("the artifact deb was copied as %q, %v", copied, err)
			}
		}},
		{name: "a build without the resident download is fatal", mutate: func(h *captureHost) {
			h.download()
			if err := os.RemoveAll(filepath.Join(h.build, "debs")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(h *captureHost) string {
			return "cc-remote: " + h.build + "/debs is missing, so this machine did not run packages full"
		}, untouched: true},
		{name: "a closure package in the resident download is fatal", mutate: func(h *captureHost) {
			h.download()
			h.deb("debs", "libfoo1", "1.0-1")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: the resident download installs the closure packages ['libfoo1'], whose system copies would shadow the payload"
		}, untouched: true},
		{name: "a resident download short of the partition is fatal", mutate: func(h *captureHost) {
			h.download()
			if err := os.Remove(filepath.Join(h.build, "debs", "libwrap0_7.6.q-35_amd64.deb")); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(*captureHost) string {
			return "cc-remote: the resident download differs from the measured partition: downloaded beyond it [], partition beyond it ['libwrap0']"
		}, untouched: true},
		{name: "a resident download at another version than the install is fatal", mutate: func(h *captureHost) {
			h.download()
			if err := os.Remove(filepath.Join(h.build, "debs", "openssh-server_1%3a9.9p1-3_amd64.deb")); err != nil {
				t.Fatal(err)
			}
			h.deb("debs", "openssh-server", "1:9.9p1-4")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: the resident download is not what packages full installed: ['openssh-server 1:9.9p1-4 (installed 1:9.9p1-3)']"
		}, untouched: true},
		{name: "a package downloaded twice is fatal", mutate: func(h *captureHost) {
			h.download()
			h.deb("artifacts", "libwrap0", "7.6.q-35")
		}, wantErr: func(*captureHost) string {
			return "cc-remote: the resident download names ['libwrap0'] more than once"
		}, untouched: true},
		{name: "an existing debs directory is fatal", mutate: func(h *captureHost) {
			if err := os.MkdirAll(h.debs, 0o755); err != nil {
				t.Fatal(err)
			}
		}, wantErr: func(h *captureHost) string {
			return "cc-remote: " + h.debs + " already exists on the build machine"
		}, untouched: true},
		{name: "the closure is captured, sealed and exposed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCaptureHost(t)
			if tt.mutate != nil {
				tt.mutate(h)
			}
			before := h.entries()
			out, err := h.run()
			if tt.wantErr != nil {
				if want := tt.wantErr(h); exitCode(err) != 1 || !strings.Contains(out, want) {
					t.Fatalf("capture = %v\n%s\nwant exit 1 with %q", err, out, want)
				}
				if _, err := os.Lstat(h.closure); tt.untouched && !os.IsNotExist(err) {
					t.Errorf("a refused capture created %s: %v", h.closure, err)
				}
				if got := h.entries(); !slices.Equal(got, before) {
					t.Errorf("a refused capture changed the host: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("capture failed: %v\n%s", err, out)
			}
			if tt.check != nil {
				tt.check(t, h)
				return
			}
			h.checkCaptured(t, out, before)
		})
	}
}

func (h *captureHost) checkCaptured(t *testing.T, out string, before []string) {
	t.Helper()
	lib, tree := h.lib, h.closure
	captured := map[string]string{
		lib + "/libfoo.so.1.0":                          h.contents[lib+"/libfoo.so.1.0"],
		lib + "/libalias.so.1":                          h.contents[lib+"/libalias.so.1"],
		"/usr/bin/footool":                              h.contents["/usr/bin/footool"],
		"/usr/share/doc/libfoo1/copyright":              "copyright",
		"/usr/share/fonts/truetype/x/X.ttf":             "ttf",
		"/usr/share/icons/hicolor/index.theme":          h.contents["/usr/share/icons/hicolor/index.theme"],
		"/usr/share/icons/hicolor/cursor.theme":         h.contents["/usr/share/icons/hicolor/cursor.theme"],
		"/usr/share/mime/packages/freedesktop.org.xml":  "<mime-info/>",
		lib + "/gtk-3.0/3.0.0/immodules/im-x.so":        h.contents[lib+"/gtk-3.0/3.0.0/immodules/im-x.so"],
		lib + "/gio/modules/libgiofoo.so":               h.contents[lib+"/gio/modules/libgiofoo.so"],
		"/usr/share/xkeyboard-config-2/rules/evdev":     "evdev",
		"/usr/share/glib-2.0/schemas/org.x.gschema.xml": "<schemalist/>",
		"/usr/share/themes/Default/gtk-3.0/gtk.css":     "css",
	}
	generated := map[string]string{
		"/usr/share/mime/mime.cache":                    "MIME",
		"/usr/share/mime/globs":                         "globs",
		"/usr/share/glib-2.0/schemas/gschemas.compiled": "GVariant",
		lib + "/gio/modules/giomodule.cache":            "gio",
		"/usr/share/icons/hicolor/icon-theme.cache":     "icon cache",
		lib + "/gtk-3.0/3.0.0/immodules.cache":          "\"" + tree + lib + "/gtk-3.0/3.0.0/immodules/im-x.so\" \n\"x\" \"X\" \"\" \"\" \"en\" \n",
		"/usr/share/icons/default/index.theme":          h.contents["/usr/share/icons/hicolor/cursor.theme"],
		"/share/cc-remote/fonts.conf":                   "<?xml version=\"1.0\"?>\n<!DOCTYPE fontconfig SYSTEM \"urn:fontconfig:fonts.dtd\">\n<fontconfig>\n  <dir>" + tree + "/usr/share/fonts</dir>\n  <cachedir>" + tree + "/var/cache/fontconfig</cachedir>\n</fontconfig>\n",
		"/var/cache/fontconfig/abc-le64.cache-9":        "",
	}
	for path, want := range captured {
		if got, err := os.ReadFile(tree + path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	for path, want := range generated {
		got, err := os.ReadFile(tree + path)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
		if info, err := os.Lstat(tree + path); err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %v, %v; want 0644", path, info.Mode(), err)
		}
	}
	for path, want := range map[string]os.FileMode{"/usr/bin/footool": 0o755, "/usr/share/fonts/truetype/x/X.ttf": 0o644, lib + "/libfoo.so.1.0": 0o644} {
		if info, err := os.Lstat(tree + path); err != nil || info.Mode().Perm() != want || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Errorf("%s mode = %v, %v; want %v", path, info.Mode(), err, want)
		}
	}
	for path, want := range map[string]string{lib + "/libfoo.so.1": "libfoo.so.1.0", lib + "/libfoo.so": "libfoo.so.1.0", lib + "/libfoo-env.conf": "/etc/environment", "/usr/share/doc/libfoo1/NEWS.gz": "../../common-licenses/GPL", "/usr/share/X11/xkb": "../xkeyboard-config-2"} {
		if got, err := os.Readlink(tree + path); err != nil || got != want {
			t.Errorf("%s -> %q, %v; want %q", path, got, err, want)
		}
	}
	for _, absent := range []string{"/usr/share/doc/libfoo1/missing.txt", "/lib", "/usr/sbin/sshd", "/usr/lib/fake-ld.so", lib + "/gdk-pixbuf-2.0/2.10.0/loaders.cache", "/usr/share/mime/packages/io.systemd.xml"} {
		if _, err := os.Lstat(tree + absent); !os.IsNotExist(err) {
			t.Errorf("the closure carries %s: %v", absent, err)
		}
	}
	if err := filepath.WalkDir(tree+"/usr/share/fonts", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if info, err := os.Lstat(path); err != nil || (entry.IsDir() && info.ModTime().Nanosecond() != 0) {
			t.Errorf("%s mtime = %v, %v; want whole seconds", path, info.ModTime(), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	theme, err := os.Lstat(tree + "/usr/share/icons/hicolor")
	if err != nil {
		t.Fatal(err)
	}
	if cache, err := os.Lstat(tree + "/usr/share/icons/hicolor/icon-theme.cache"); err != nil || cache.ModTime().Nanosecond() != 0 || cache.ModTime().Before(theme.ModTime()) {
		t.Errorf("icon-theme.cache mtime = %v, %v; want whole seconds at or after the theme's %v", cache.ModTime(), err, theme.ModTime())
	}
	uid := strconv.Itoa(os.Getuid())
	conf := tree + "/share/cc-remote/fonts.conf"
	if got, want := logLines(t, filepath.Join(h.root, "fc-cache.log")), []string{"fc-cache -f uid=" + uid + " conf=" + conf, "fc-cache -v uid=" + uid + " conf=" + conf}; !slices.Equal(got, want) {
		t.Errorf("fc-cache calls = %q, want %q", got, want)
	}
	if got := logLines(t, filepath.Join(h.root, "runuser.log")); !slices.Equal(got, []string{"runuser u"}) {
		t.Errorf("runuser calls = %q, want the unprivileged proof", got)
	}
	if got, want := logLines(t, filepath.Join(h.root, "ldso.log")), []string{"ld.so " + h.host + "/home/u/.agent-browser/chrome", "ld.so " + h.host + "/opt/cc-remote/tools/office/soffice.bin", "ld.so " + h.host + "/usr/bin/footool"}; !slices.Equal(got, want) {
		t.Errorf("ld.so calls = %q, want %q", got, want)
	}
	var manifest struct {
		Packages    map[string]map[string]string `json:"packages"`
		Resident    []string                     `json:"resident"`
		Files       []map[string]string          `json:"files"`
		Generated   map[string]map[string]string `json:"generated"`
		Links       []closureLink                `json:"links"`
		Projections []string                     `json:"projections"`
		Consumers   map[string][]string          `json:"consumers"`
		OS          map[string]string            `json:"os"`
		Libc6       string                       `json:"libc6"`
	}
	raw, err := os.ReadFile(tree + "/closure.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("closure.json: %v\n%s", err, raw)
	}
	wantPackages := map[string]map[string]string{
		"libfoo1":            {"version": "1.0-1", "arch": "amd64"},
		"fonts-x":            {"version": "1.0", "arch": "all"},
		"hicolor-icon-theme": {"version": "0.18-2", "arch": "all"},
		"shared-mime-info":   {"version": "2.4-5", "arch": "amd64"},
		"libgtk-3-0t64":      {"version": "3.24.49-1", "arch": "amd64"},
		"xkb-data":           {"version": "2.44-1", "arch": "all"},
	}
	wantConsumers := map[string][]string{"/home/u/.agent-browser/chrome": {"libfoo1"}, "/opt/cc-remote/tools/office/soffice.bin": {"libfoo1"}, "/usr/bin/footool": {"libfoo1"}}
	wantLinks := closure(Inventory{Apt: Apt{Payload: &h.payload}}).Links
	switch {
	case !reflect.DeepEqual(manifest.Packages, wantPackages):
		t.Errorf("packages = %v, want %v", manifest.Packages, wantPackages)
	case !slices.Equal(manifest.Resident, []string{"libwrap0", "openssh-server", "openssh-sftp-server"}):
		t.Errorf("resident = %v", manifest.Resident)
	case !reflect.DeepEqual(manifest.Consumers, wantConsumers):
		t.Errorf("consumers = %v, want %v", manifest.Consumers, wantConsumers)
	case !reflect.DeepEqual(manifest.Links, wantLinks):
		t.Errorf("links = %v, want %v", manifest.Links, wantLinks)
	case !slices.Equal(manifest.Projections, []string{"/usr/share/themes", "/usr/share/X11/xkb"}):
		t.Errorf("projections = %v", manifest.Projections)
	case !reflect.DeepEqual(manifest.OS, map[string]string{"VERSION": "26.04 LTS (Resolute Raccoon)", "VERSION_ID": "26.04"}) || manifest.Libc6 != "2.42-1ubuntu1":
		t.Errorf("os = %v, libc6 = %q", manifest.OS, manifest.Libc6)
	case len(manifest.Files) != 18:
		t.Errorf("files = %d entries, want 18:\n%s", len(manifest.Files), raw)
	}
	sum := sha256.Sum256([]byte(h.contents["/usr/bin/footool"]))
	wantFile := map[string]string{"path": "/usr/bin/footool", "package": "libfoo1", "sha256": hex.EncodeToString(sum[:]), "mode": "0755"}
	if i := slices.IndexFunc(manifest.Files, func(f map[string]string) bool { return f["path"] == "/usr/bin/footool" }); i < 0 || !reflect.DeepEqual(manifest.Files[i], wantFile) {
		t.Errorf("footool entry = %v, want %v", manifest.Files, wantFile)
	}
	if i := slices.IndexFunc(manifest.Files, func(f map[string]string) bool { return f["path"] == lib+"/libalias.so.1" }); i < 0 {
		t.Errorf("the /lib alias was not canonicalised into %s:\n%s", lib, raw)
	}
	if i := slices.IndexFunc(manifest.Files, func(f map[string]string) bool { return f["path"] == "/usr/share/fonts/truetype/x/X.ttf" }); i < 0 || manifest.Files[i]["mode"] != "0644" {
		t.Errorf("X.ttf entry = %v, want mode 0644", manifest.Files)
	}
	sum = sha256.Sum256([]byte("MIME"))
	for path, want := range map[string]map[string]string{
		"/usr/share/mime/mime.cache":                {"how": "copy", "sha256": hex.EncodeToString(sum[:]), "mode": "0644"},
		"/share/cc-remote/fonts.conf":               {"how": "snippet"},
		"/var/cache/fontconfig/abc-le64.cache-9":    {"how": "fontconfig"},
		"/usr/share/icons/default/index.theme":      {"how": "x-cursor-theme"},
		lib + "/gtk-3.0/3.0.0/immodules.cache":      {"how": "rewrite"},
		"/usr/share/icons/hicolor/icon-theme.cache": {"how": "copy"},
	} {
		got := manifest.Generated[path]
		for key, value := range want {
			if got[key] != value {
				t.Errorf("generated[%s][%s] = %q, want %q", path, key, got[key], value)
			}
		}
		if !sha256Pattern.MatchString(got["sha256"]) || got["mode"] != "0644" {
			t.Errorf("generated[%s] = %v, want a sha256 and mode 0644", path, got)
		}
	}
	for path, want := range map[string]string{"/usr/local/bin/footool": tree + "/usr/bin/footool", "/usr/local/share/mime": tree + "/usr/share/mime", "/usr/local/share/glib-2.0/schemas": tree + "/usr/share/glib-2.0/schemas", "/usr/local/share/icons": tree + "/usr/share/icons"} {
		if got, err := os.Readlink(filepath.Join(h.host, path)); err != nil || got != want {
			t.Errorf("%s -> %q, %v; want %q", path, got, err, want)
		}
	}
	added := slices.DeleteFunc(h.entries(), func(path string) bool { return slices.Contains(before, path) })
	if want := []string{"/usr/local/bin/footool", "/usr/local/share/glib-2.0", "/usr/local/share/glib-2.0/schemas", "/usr/local/share/icons", "/usr/local/share/mime"}; !slices.Equal(added, want) {
		t.Errorf("the capture wrote %q on the host, want only %q", added, want)
	}
	var bytes int
	for _, path := range []string{lib + "/libfoo.so.1.0", lib + "/libalias.so.1", lib + "/gio/modules/libgiofoo.so", "/usr/bin/footool", "/usr/share/doc/libfoo1/copyright", "/usr/share/fonts/truetype/x/X.ttf", "/usr/share/icons/hicolor/index.theme", "/usr/share/icons/hicolor/cursor.theme", "/usr/share/mime/packages/freedesktop.org.xml", lib + "/gtk-3.0/3.0.0/immodules/im-x.so", "/usr/share/glib-2.0/schemas/org.x.gschema.xml", "/usr/share/themes/Default/gtk-3.0/gtk.css", "/usr/share/xkeyboard-config-2/rules/evdev"} {
		bytes += len(h.contents[path])
	}
	if want := fmt.Sprintf("cc-remote: captured 6 packages, 18 files, %d bytes into %s\n", bytes, tree); out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestPluginsVerifyProvesTheClosureConsumers(t *testing.T) {
	tests := []struct {
		name     string
		linked   bool
		foreign  bool
		script   bool
		env      []string
		family   string
		wantErr  func(home, closure, bins string) string
		wantLdso func(home, bins, office string) []string
		progress string
	}{
		{
			name:   "without a closure link the bins only need to be on PATH",
			family: "Noto Sans CJK JP,Noto Sans CJK JP Regular",
			wantLdso: func(home, _, office string) []string {
				return []string{"ld.so " + home + "/.agent-browser/chrome", "ld.so " + office}
			},
			progress: "font:0",
		},
		{
			name:   "with a closure link the bins must be its exposed links",
			linked: true,
			family: "Noto Sans CJK JP",
			wantLdso: func(home, bins, office string) []string {
				return []string{"ld.so " + bins + "/certutil", "ld.so " + bins + "/fc-match", "ld.so " + home + "/.agent-browser/chrome", "ld.so " + office}
			},
			progress: "font:0",
		},
		{
			name:   "a consumer that is not an ELF is fatal",
			script: true,
			family: "Noto Sans CJK JP",
			wantErr: func(home, _, _ string) string {
				return "cc-remote: " + home + "/.agent-browser/chrome is not a little-endian ELF64 file"
			},
			progress: "consumer:0",
		},
		{
			name:    "an exposed bin pointing elsewhere is fatal",
			linked:  true,
			foreign: true,
			family:  "Noto Sans CJK JP",
			wantErr: func(_, closure, bins string) string {
				return "cc-remote: " + bins + "/fc-match does not point at the pinned " + closure + "/usr/bin/fc-match"
			},
			progress: "closure_bin:fc-match",
		},
		{
			name:     "a missing library is fatal",
			env:      []string{"LDSO_MISSING=libmissing.so.9"},
			family:   "Noto Sans CJK JP",
			wantErr:  func(_, _, _ string) string { return "cannot load libmissing.so.9" },
			progress: "consumer:0",
		},
		{
			name:     "an inexact font family is fatal",
			family:   "DejaVu Sans",
			wantErr:  func(_, _, _ string) string { return "cc-remote: fc-match resolves Noto Sans CJK JP to DejaVu Sans" },
			progress: "font:0",
		},
	}
	scripts, err := Render(scriptInventory(), "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes, home := t.TempDir(), t.TempDir(), shortHome(t)
			closure, bins, office := filepath.Join(root, "closure"), filepath.Join(root, "bin"), filepath.Join(root, "soffice.bin")
			writeFakes(t, fakes, map[string]string{
				"ld.so":    fakeLdso,
				"fc-match": "#!/bin/sh\nprintf '%s\\n' \"fc-match $*\" >> \"$TEST_ROOT/fc-match.log\"\nprintf '%s' \"$FAMILY\"\n",
				"certutil": "#!/bin/sh\n",
			})
			elf := elfWithInterp(filepath.Join(fakes, "ld.so"))
			chrome := elf
			if tt.script {
				chrome = []byte("#!/bin/sh\n")
			}
			writePluginTestFile(t, filepath.Join(home, ".agent-browser/chrome"), chrome, 0o755)
			writePluginTestFile(t, office, elf, 0o755)
			if tt.linked {
				for _, bin := range []string{"certutil", "fc-match"} {
					writePluginTestFile(t, filepath.Join(root, "tree/usr/bin", bin), elf, 0o755)
					target := filepath.Join(closure, "usr/bin", bin)
					if tt.foreign && bin == "fc-match" {
						target = filepath.Join(fakes, bin)
					}
					if err := os.MkdirAll(bins, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, filepath.Join(bins, bin)); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(filepath.Join(root, "tree"), closure); err != nil {
					t.Fatal(err)
				}
			}
			plugins := strings.NewReplacer(
				"closure_root=/opt/cc-remote/closure\n", "closure_root="+quote(closure)+"\n",
				"system_bin_dir=/usr/local/bin\n", "system_bin_dir="+quote(bins)+"\n",
				"'/opt/cc-remote/tools/office/soffice.bin'", quote(office),
			).Replace(string(scripts.Plugins))
			writePluginTestFile(t, filepath.Join(fakes, "plugins.sh"), []byte(plugins), 0o700)
			cmd := exec.Command("bash", filepath.Join(fakes, "plugins.sh"), "verify")
			cmd.Env = append(append(os.Environ(), "HOME="+home, "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root, "FAMILY="+tt.family), tt.env...)
			out, err := cmd.CombinedOutput()
			if recorded, readErr := os.ReadFile(filepath.Join(home, ".cc-remote", "verify-progress")); readErr != nil || string(recorded) != tt.progress+"\n" {
				t.Errorf("verify progress = %q, %v, want %q", recorded, readErr, tt.progress)
			}
			if tt.wantErr != nil {
				if want := tt.wantErr(home, closure, bins); exitCode(err) != 1 || !strings.Contains(string(out), want) {
					t.Fatalf("verify = %v\n%s\nwant exit 1 with %q", err, out, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("verify failed: %v\n%s", err, out)
			}
			if got, want := logLines(t, filepath.Join(root, "ldso.log")), tt.wantLdso(home, bins, office); !slices.Equal(got, want) {
				t.Errorf("ld.so calls = %q, want %q", got, want)
			}
			if got := logLines(t, filepath.Join(root, "fc-match.log")); !slices.Equal(got, []string{"fc-match -f %{family} Noto Sans CJK JP"}) {
				t.Errorf("fc-match calls = %q", got)
			}
		})
	}
}
