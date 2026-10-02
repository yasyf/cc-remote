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
    if [ -n "${FC_STALE:-}" ]; then
      echo "$dir: caching, new cache contents: 1 fonts, 1 dirs"
    else
      find "$dir" -type d | sort | while read -r d; do
        echo "$d: skipping, existing cache is valid: 1 fonts, 0 dirs"
      done
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
	fakeLdconfig  = "#!/bin/sh\nprintf '%s\\n' \"ldconfig$*\" >> \"$TEST_ROOT/calls\"\n"
	fakeAptCache  = "#!/bin/sh\nprintf 'Package: libasound2t64\\n'\n"
	fakeInstaller = `#!/bin/bash
set -euo pipefail
printf '%s\n' "$*" >> "$TEST_ROOT/apt"
if [ "$1" = install ]; then
  for arg in "$@"; do
    case "$arg" in
      install | -*) ;;
      *) printf 'ii \t%s\n' "$arg" >> "$TEST_ROOT/installed" ;;
    esac
  done
fi
`
)

func scriptInventory() Inventory {
	return Inventory{
		Version: SchemaVersion,
		Apt: Apt{
			Install: []string{"openssh-server", "libnss3"},
			T64:     []string{"libasound2"},
			Payload: &AptPayload{
				Resident:  []string{"bubblewrap"},
				Closure:   []string{"libnss3", "libasound2t64", "fonts-x"},
				Bins:      []string{"certutil", "fc-match"},
				Fonts:     []string{"Noto Sans CJK JP"},
				Consumers: []string{".agent-browser/chrome", "/opt/cc-remote/tools/office/soffice.bin"},
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

func writeFakes(t *testing.T, dir string, fakes map[string]string) {
	t.Helper()
	for name, content := range fakes {
		writePluginTestFile(t, filepath.Join(dir, name), []byte(content), 0o700)
	}
}

func TestProvisionPackagesInstallsTheResidentSetForAPayload(t *testing.T) {
	full := "update -qq\ninstall -y -qq --no-install-recommends ca-certificates curl git jq python3 unzip xz-utils openssh-server libnss3 libasound2t64\n"
	seeds := "ca-certificates\ncurl\ngit\njq\npython3\nunzip\nxz-utils\nopenssh-server\nbubblewrap\n"
	tests := []struct {
		name    string
		mode    []string
		apt     string
		records bool
		wantErr string
	}{
		{name: "no mode installs everything and records the transaction", apt: full, records: true},
		{name: "full installs everything and records the transaction", mode: []string{PackagesFull}, apt: full, records: true},
		{name: "resident leaves the closure to the payload", mode: []string{PackagesResident}, apt: "update -qq\ninstall -y -qq --no-install-recommends ca-certificates curl git jq python3 unzip xz-utils openssh-server bubblewrap\n"},
		{name: "an unknown mode is a usage error", mode: []string{"bundle"}, wantErr: "provision: packages takes full or resident, not bundle"},
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
				"dpkg-query": "#!/bin/sh\ncat \"$TEST_ROOT/installed\"\n",
			})
			writePluginTestFile(t, filepath.Join(root, "installed"), []byte("ii \tbase-files\n"), 0o644)
			provision := strings.NewReplacer(
				"build_dir=/var/lib/cc-remote/build\n", "build_dir="+quote(build)+"\n",
				"rm -rf /var/lib/apt/lists/*", ":",
			).Replace(string(scripts.ProvisionScript))
			cmd := exec.Command("bash", slices.Concat([]string{"-c", provision, "provision.sh", PhasePackages}, tt.mode)...)
			cmd.Env = append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root)
			out, err := cmd.CombinedOutput()
			if tt.wantErr != "" {
				if exitCode(err) != 2 || !strings.Contains(string(out), tt.wantErr) {
					t.Fatalf("packages = %v\n%s\nwant exit 2 with %q", err, out, tt.wantErr)
				}
				if _, err := os.Stat(filepath.Join(root, "apt")); !os.IsNotExist(err) {
					t.Errorf("apt-get ran for a usage error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("packages failed: %v\n%s", err, out)
			}
			if apt, err := os.ReadFile(filepath.Join(root, "apt")); err != nil || string(apt) != tt.apt {
				t.Errorf("apt-get calls:\n%s\n%v\nwant:\n%s", apt, err, tt.apt)
			}
			if !tt.records {
				if _, err := os.Stat(build); !os.IsNotExist(err) {
					t.Errorf("resident mode recorded a transaction: %v", err)
				}
				return
			}
			installed, err := os.ReadFile(filepath.Join(root, "installed"))
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"seeds": seeds, "packages.before": "ii \tbase-files\n", "packages.after": string(installed)} {
				if got, err := os.ReadFile(filepath.Join(build, name)); err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want %q", name, got, err, want)
				}
			}
		})
	}
}

func TestProvisionLoaderRegistersTheClosureUnderTheLock(t *testing.T) {
	scripts, err := Render(scriptInventory(), "agents")
	if err != nil {
		t.Fatal(err)
	}
	root, fakes := t.TempDir(), t.TempDir()
	lock := filepath.Join(root, "lib", "cc-remote", "ldconfig.lock")
	writeFakes(t, fakes, map[string]string{"id": "#!/bin/sh\necho 0\n", "flock": fakeFlock, "ldconfig": fakeLdconfig})
	provision := strings.Replace(string(scripts.ProvisionScript), "closure_lock=/var/lib/cc-remote/ldconfig.lock\n", "closure_lock="+quote(lock)+"\n", 1)
	cmd := exec.Command("bash", "-c", provision, "provision.sh", PhaseLoader)
	cmd.Env = append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("loader failed: %v\n%s", err, out)
	}
	if got, want := logLines(t, filepath.Join(root, "calls")), []string{"flock " + lock + " ldconfig", "ldconfig"}; !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if info, err := os.Stat(filepath.Dir(lock)); err != nil || !info.IsDir() {
		t.Errorf("the lock directory is %v, %v; want a directory", info, err)
	}
}

func TestProvisionPayloadActivatesTheClosure(t *testing.T) {
	arch := machineArch(t)
	image := "hsqs closure payload"
	sum := sha256.Sum256([]byte(image))
	sha := hex.EncodeToString(sum[:])
	tests := []struct {
		name        string
		foreign     func(root, closure, payloads string) error
		loaderConf  func(closure string) string
		wantErr     func(root, closure, dir string) string
		fontsAbsent bool
	}{
		{name: "the payload's closure is exposed and configured"},
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
			fontsAbsent: true,
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
				"mount":      fakeMount,
				"sprite-env": "#!/bin/sh\nexit 0\n",
				"ldconfig":   fakeLdconfig,
			})
			writePluginTestFile(t, filepath.Join(store, sha+".sqfs.partial"), []byte(image), 0o644)
			writePluginTestFile(t, filepath.Join(dir, closure, "closure.json"), []byte("{}"), 0o644)
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
				if leftovers, _ := filepath.Glob(loaderConf + ".tmp"); len(leftovers) != 0 {
					t.Errorf("left %q", leftovers)
				}
				return
			}
			if err != nil {
				t.Fatalf("payload failed: %v\n%s", err, out)
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
			fonts := "<?xml version=\"1.0\"?>\n<!DOCTYPE fontconfig SYSTEM \"urn:fontconfig:fonts.dtd\">\n<fontconfig>\n  <dir>" + closure + "/usr/share/fonts</dir>\n  <cachedir>" + closure + "/var/cache/fontconfig</cachedir>\n</fontconfig>\n"
			for path, want := range map[string]string{loaderConf: closure + "/usr/lib/" + arch + "-linux-gnu\n", fontsConf: fonts} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want %q", path, got, err, want)
				}
				if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
					t.Errorf("%s mode = %v, %v; want 0644", path, info.Mode(), err)
				}
				if leftovers, _ := filepath.Glob(path + ".tmp"); len(leftovers) != 0 {
					t.Errorf("left %q", leftovers)
				}
			}
			helper, err := os.ReadFile(filepath.Join(root, "payload-mount.sh"))
			if err != nil || !strings.HasSuffix(string(helper), "done\nldconfig\n") || !strings.Contains(string(helper), "mount -t squashfs -o ro,nosuid,nodev,loop") {
				t.Errorf("the boot helper is %q, %v; want nosuid,nodev mounts followed by ldconfig", helper, err)
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
	t                                 *testing.T
	root, host, closure, build, fakes string
	lib                               string
	dpkg                              fakeDpkg
	payload                           AptPayload
	env                               []string
	contents                          map[string]string
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
		lib:      "/usr/lib/" + arch + "-linux-gnu",
		contents: map[string]string{},
		payload: AptPayload{
			Resident:  []string{"bubblewrap"},
			Closure:   []string{"libfoo1", "fonts-x", "hicolor-icon-theme", "shared-mime-info", "libgtk-3-0t64"},
			Bins:      []string{"footool"},
			Fonts:     []string{"X"},
			Consumers: []string{".agent-browser/chrome", "/opt/cc-remote/tools/office/soffice.bin"},
		},
	}
	lib := h.lib
	h.dpkg = fakeDpkg{
		Packages: map[string]*fakePackage{
			"libfoo1":             {Version: "1.0-1", Depends: "libc6 (>= 2.34)", Files: []string{lib, lib + "/libfoo.so.1.0", lib + "/libfoo.so.1", lib + "/libfoo.so", lib + "/libfoo-env.conf", "/lib/" + arch + "-linux-gnu/libalias.so.1", "/usr/bin/footool", "/usr/share/doc/libfoo1", "/usr/share/doc/libfoo1/copyright", "/usr/share/doc/libfoo1/NEWS.gz", "/usr/share/doc/libfoo1/missing.txt"}},
			"fonts-x":             {Version: "1.0", Arch: "all", Files: []string{"/usr/share/fonts/truetype/x", "/usr/share/fonts/truetype/x/X.ttf"}},
			"hicolor-icon-theme":  {Version: "0.18-2", Arch: "all", Files: []string{"/usr/share/icons/hicolor", "/usr/share/icons/hicolor/index.theme", "/usr/share/icons/hicolor/cursor.theme"}},
			"shared-mime-info":    {Version: "2.4-5", Files: []string{"/usr/share/mime/packages/freedesktop.org.xml"}},
			"libgtk-3-0t64":       {Version: "3.24.49-1", Files: []string{lib + "/gtk-3.0/3.0.0/immodules/im-x.so"}},
			"openssh-server":      {Version: "1:9.9p1-3", Depends: "libwrap0, ssh-sftp-server | openssh-sftp-server", Files: []string{"/usr/sbin/sshd"}},
			"libwrap0":            {Version: "7.6.q-35", Files: []string{lib + "/libwrap.so.0"}},
			"openssh-sftp-server": {Version: "1:9.9p1-3", Provides: "ssh-sftp-server", Files: []string{"/usr/lib/openssh/sftp-server"}},
			"base-files":          {Version: "13ubuntu10", Files: []string{"/etc/environment", "/etc"}},
			"libc6":               {Version: "2.42-1ubuntu1", Files: []string{lib + "/libc.so.6"}},
		},
		Owners: map[string][]string{},
	}
	h.file(lib+"/libfoo.so.1.0", "\x7fELF libfoo", 0o644)
	h.file(lib+"/libalias.so.1", "\x7fELF alias", 0o644)
	h.file("/usr/bin/footool", "\x7fELF footool", 0o755)
	h.file("/usr/share/doc/libfoo1/copyright", "copyright", 0o644)
	h.file("/usr/share/fonts/truetype/x/X.ttf", "ttf", 0o666)
	h.file("/usr/share/icons/hicolor/index.theme", "[Icon Theme]\n", 0o644)
	h.file("/usr/share/icons/hicolor/cursor.theme", "[Icon Theme]\nInherits=Adwaita\n", 0o644)
	h.file("/usr/share/icons/hicolor/icon-theme.cache", "icon cache", 0o644)
	h.file("/usr/share/mime/packages/freedesktop.org.xml", "<mime-info/>", 0o644)
	h.file("/usr/share/mime/mime.cache", "MIME", 0o644)
	h.file("/usr/share/mime/globs", "globs", 0o644)
	h.file("/usr/share/glib-2.0/schemas/gschemas.compiled", "GVariant", 0o644)
	h.file(lib+"/gio/modules/giomodule.cache", "gio", 0o644)
	h.file(lib+"/gtk-3.0/3.0.0/immodules/im-x.so", "\x7fELF im", 0o644)
	h.file(lib+"/gtk-3.0/3.0.0/immodules.cache", "\""+lib+"/gtk-3.0/3.0.0/immodules/im-x.so\" \n\"x\" \"X\" \"\" \"\" \"en\" \n", 0o644)
	h.file("/etc/os-release", "PRETTY_NAME=\"Ubuntu 26.04 LTS\"\nVERSION_ID=\"26.04\"\nVERSION=\"26.04 LTS (Resolute Raccoon)\"\n", 0o644)
	h.file("/home/u/.agent-browser/chrome", "\x7fELF chrome", 0o755)
	h.file("/opt/cc-remote/tools/office/soffice.bin", "\x7fELF soffice", 0o755)
	h.link(lib+"/libfoo.so.1", "libfoo.so.1.0")
	h.link(lib+"/libfoo.so", lib+"/libfoo.so.1.0")
	h.link(lib+"/libfoo-env.conf", "/etc/environment")
	h.link("/usr/share/doc/libfoo1/NEWS.gz", "../../common-licenses/GPL")
	h.link("/lib", "usr/lib")
	h.link("/etc/alternatives/x-cursor-theme", "/usr/share/icons/hicolor/cursor.theme")
	for _, dir := range []string{"/usr/local/bin", "/usr/local/share"} {
		if err := os.MkdirAll(filepath.Join(h.host, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	before := "ii \tbase-files\nii \tlibc6\n"
	after := before + "ii \tfonts-x\nii \thicolor-icon-theme\nii \tlibfoo1\nii \tlibgtk-3-0t64\nii \tlibwrap0\nii \topenssh-server\nii \topenssh-sftp-server\nii \tshared-mime-info\n"
	writePluginTestFile(t, filepath.Join(h.build, "packages.before"), []byte(before), 0o644)
	writePluginTestFile(t, filepath.Join(h.build, "packages.after"), []byte(after), 0o644)
	writePluginTestFile(t, filepath.Join(h.build, "seeds"), []byte("ca-certificates\nopenssh-server\nbubblewrap\n"), 0o644)
	writeFakes(t, h.fakes, map[string]string{"dpkg-query": fakeDpkgQuery, "ld.so": fakeLdso, "fc-cache": fakeFcCache, "runuser": fakeRunuser})
	return h
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
	writePluginTestFile(h.t, filepath.Join(h.root, "capture.py"), script, 0o644)
	writePluginTestFile(h.t, filepath.Join(h.root, "dpkg.json"), mustJSON(h.t, h.dpkg), 0o644)
	writePluginTestFile(h.t, filepath.Join(h.root, "closure.json"), mustJSON(h.t, closure(Inventory{Apt: Apt{Payload: &h.payload}})), 0o644)
	cmd := exec.Command("python3", filepath.Join(h.root, "capture.py"), filepath.Join(h.root, "closure.json"), h.host, h.closure, h.build, "/home/u")
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
		untouched bool
	}{
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
			if err := os.Chmod(filepath.Join(h.host, "usr/bin/footool"), 0o4755); err != nil {
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
			h.file("/usr/share/foodir/keep", "keep", 0o644)
			h.link("/usr/share/foolink", "foodir")
			h.file("/usr/share/foodir/evil", "evil", 0o644)
			h.dpkg.Packages["libfoo1"].Files = append(h.dpkg.Packages["libfoo1"].Files, "/usr/share/foodir", "/usr/share/foodir/keep", "/usr/share/foolink", "/usr/share/foolink/evil")
		}, wantErr: func(h *captureHost) string {
			return "cc-remote: /usr/share/foolink/evil would be written through " + h.closure + "/usr/share/foolink, which is not a directory inside the closure"
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
		}, wantErr: func(*captureHost) string { return "cc-remote: no module cache was generated under " + lib + "/gtk-3.0" }},
		{name: "a stale font cache is fatal", mutate: func(h *captureHost) { h.env = []string{"FC_STALE=1"} }, wantErr: func(h *captureHost) string {
			return "cc-remote: the font cache is not valid for every closure font directory: ['" + h.closure + "/usr/share/fonts: caching, new cache contents: 1 fonts, 1 dirs']"
		}},
		{name: "an unresolved consumer library is fatal", mutate: func(h *captureHost) { h.env = []string{"LDSO_MISSING=libmissing.so.9"} }, wantErr: func(*captureHost) string {
			return "cc-remote: /home/u/.agent-browser/chrome cannot load libmissing.so.9"
		}},
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
			h.checkCaptured(t, out, before)
		})
	}
}

func (h *captureHost) checkCaptured(t *testing.T, out string, before []string) {
	t.Helper()
	lib, closure := h.lib, h.closure
	captured := map[string]string{
		lib + "/libfoo.so.1.0":                         h.contents[lib+"/libfoo.so.1.0"],
		lib + "/libalias.so.1":                         h.contents[lib+"/libalias.so.1"],
		"/usr/bin/footool":                             h.contents["/usr/bin/footool"],
		"/usr/share/doc/libfoo1/copyright":             "copyright",
		"/usr/share/fonts/truetype/x/X.ttf":            "ttf",
		"/usr/share/icons/hicolor/index.theme":         h.contents["/usr/share/icons/hicolor/index.theme"],
		"/usr/share/icons/hicolor/cursor.theme":        h.contents["/usr/share/icons/hicolor/cursor.theme"],
		"/usr/share/mime/packages/freedesktop.org.xml": "<mime-info/>",
		lib + "/gtk-3.0/3.0.0/immodules/im-x.so":       h.contents[lib+"/gtk-3.0/3.0.0/immodules/im-x.so"],
	}
	generated := map[string]string{
		"/usr/share/mime/mime.cache":                    "MIME",
		"/usr/share/mime/globs":                         "globs",
		"/usr/share/glib-2.0/schemas/gschemas.compiled": "GVariant",
		lib + "/gio/modules/giomodule.cache":            "gio",
		"/usr/share/icons/hicolor/icon-theme.cache":     "icon cache",
		lib + "/gtk-3.0/3.0.0/immodules.cache":          "\"" + closure + lib + "/gtk-3.0/3.0.0/immodules/im-x.so\" \n\"x\" \"X\" \"\" \"\" \"en\" \n",
		"/usr/share/icons/default/index.theme":          h.contents["/usr/share/icons/hicolor/cursor.theme"],
		"/share/cc-remote/fonts.conf":                   "<?xml version=\"1.0\"?>\n<!DOCTYPE fontconfig SYSTEM \"urn:fontconfig:fonts.dtd\">\n<fontconfig>\n  <dir>" + closure + "/usr/share/fonts</dir>\n  <cachedir>" + closure + "/var/cache/fontconfig</cachedir>\n</fontconfig>\n",
		"/var/cache/fontconfig/abc-le64.cache-9":        "",
	}
	for path, want := range captured {
		if got, err := os.ReadFile(closure + path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	for path, want := range generated {
		got, err := os.ReadFile(closure + path)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
		if info, err := os.Lstat(closure + path); err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %v, %v; want 0644", path, info.Mode(), err)
		}
	}
	for path, want := range map[string]os.FileMode{"/usr/bin/footool": 0o755, "/usr/share/fonts/truetype/x/X.ttf": 0o644, lib + "/libfoo.so.1.0": 0o644} {
		if info, err := os.Lstat(closure + path); err != nil || info.Mode().Perm() != want || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Errorf("%s mode = %v, %v; want %v", path, info.Mode(), err, want)
		}
	}
	for path, want := range map[string]string{lib + "/libfoo.so.1": "libfoo.so.1.0", lib + "/libfoo.so": "libfoo.so.1.0", lib + "/libfoo-env.conf": "/etc/environment", "/usr/share/doc/libfoo1/NEWS.gz": "../../common-licenses/GPL"} {
		if got, err := os.Readlink(closure + path); err != nil || got != want {
			t.Errorf("%s -> %q, %v; want %q", path, got, err, want)
		}
	}
	for _, absent := range []string{"/usr/share/doc/libfoo1/missing.txt", "/lib", "/usr/sbin/sshd"} {
		if _, err := os.Lstat(closure + absent); !os.IsNotExist(err) {
			t.Errorf("the closure carries %s: %v", absent, err)
		}
	}
	if err := filepath.WalkDir(closure+"/usr/share/fonts", func(path string, entry os.DirEntry, err error) error {
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
	theme, err := os.Lstat(closure + "/usr/share/icons/hicolor")
	if err != nil {
		t.Fatal(err)
	}
	if cache, err := os.Lstat(closure + "/usr/share/icons/hicolor/icon-theme.cache"); err != nil || cache.ModTime().Nanosecond() != 0 || cache.ModTime().Before(theme.ModTime()) {
		t.Errorf("icon-theme.cache mtime = %v, %v; want whole seconds at or after the theme's %v", cache.ModTime(), err, theme.ModTime())
	}
	uid := strconv.Itoa(os.Getuid())
	conf := closure + "/share/cc-remote/fonts.conf"
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
		Packages  map[string]map[string]string `json:"packages"`
		Resident  []string                     `json:"resident"`
		Files     []map[string]string          `json:"files"`
		Generated map[string]map[string]string `json:"generated"`
		Links     []closureLink                `json:"links"`
		Consumers map[string][]string          `json:"consumers"`
		OS        map[string]string            `json:"os"`
		Libc6     string                       `json:"libc6"`
	}
	raw, err := os.ReadFile(closure + "/closure.json")
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
	case !reflect.DeepEqual(manifest.OS, map[string]string{"VERSION": "26.04 LTS (Resolute Raccoon)", "VERSION_ID": "26.04"}) || manifest.Libc6 != "2.42-1ubuntu1":
		t.Errorf("os = %v, libc6 = %q", manifest.OS, manifest.Libc6)
	case len(manifest.Files) != 13:
		t.Errorf("files = %d entries, want 13:\n%s", len(manifest.Files), raw)
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
	for path, want := range map[string]string{"/usr/local/bin/footool": closure + "/usr/bin/footool", "/usr/local/share/mime": closure + "/usr/share/mime", "/usr/local/share/glib-2.0/schemas": closure + "/usr/share/glib-2.0/schemas", "/usr/local/share/icons": closure + "/usr/share/icons"} {
		if got, err := os.Readlink(filepath.Join(h.host, path)); err != nil || got != want {
			t.Errorf("%s -> %q, %v; want %q", path, got, err, want)
		}
	}
	added := slices.DeleteFunc(h.entries(), func(path string) bool { return slices.Contains(before, path) })
	if want := []string{"/usr/local/bin/footool", "/usr/local/share/glib-2.0", "/usr/local/share/glib-2.0/schemas", "/usr/local/share/icons", "/usr/local/share/mime"}; !slices.Equal(added, want) {
		t.Errorf("the capture wrote %q on the host, want only %q", added, want)
	}
	var bytes int
	for _, path := range []string{lib + "/libfoo.so.1.0", lib + "/libalias.so.1", "/usr/bin/footool", "/usr/share/doc/libfoo1/copyright", "/usr/share/fonts/truetype/x/X.ttf", "/usr/share/icons/hicolor/index.theme", "/usr/share/icons/hicolor/cursor.theme", "/usr/share/mime/packages/freedesktop.org.xml", lib + "/gtk-3.0/3.0.0/immodules/im-x.so"} {
		bytes += len(h.contents[path])
	}
	if want := fmt.Sprintf("cc-remote: captured 5 packages, 13 files, %d bytes into %s\n", bytes, closure); out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestPluginsVerifyProvesTheClosureConsumers(t *testing.T) {
	tests := []struct {
		name     string
		linked   bool
		foreign  bool
		env      []string
		family   string
		wantErr  func(home, closure, bins string) string
		wantLdso func(home, closure, bins, fakes string) []string
	}{
		{
			name:   "without a closure link the bins resolve on PATH",
			family: "Noto Sans CJK JP,Noto Sans CJK JP Regular",
			wantLdso: func(home, _, _, fakes string) []string {
				return []string{"ld.so " + fakes + "/certutil", "ld.so " + fakes + "/fc-match", "ld.so " + home + "/.agent-browser/chrome", "ld.so /opt/cc-remote/tools/office/soffice.bin"}
			},
		},
		{
			name:   "with a closure link the bins must be its exposed links",
			linked: true,
			family: "Noto Sans CJK JP",
			wantLdso: func(home, _, bins, _ string) []string {
				return []string{"ld.so " + bins + "/certutil", "ld.so " + bins + "/fc-match", "ld.so " + home + "/.agent-browser/chrome", "ld.so /opt/cc-remote/tools/office/soffice.bin"}
			},
		},
		{
			name:    "an exposed bin pointing elsewhere is fatal",
			linked:  true,
			foreign: true,
			family:  "Noto Sans CJK JP",
			wantErr: func(_, closure, bins string) string {
				return "cc-remote: " + bins + "/fc-match does not point at the pinned " + closure + "/usr/bin/fc-match"
			},
		},
		{
			name:    "a missing library is fatal",
			env:     []string{"LDSO_MISSING=libmissing.so.9"},
			family:  "Noto Sans CJK JP",
			wantErr: func(_, _, _ string) string { return "cannot load libmissing.so.9" },
		},
		{
			name:    "an inexact font family is fatal",
			family:  "DejaVu Sans",
			wantErr: func(_, _, _ string) string { return "cc-remote: fc-match resolves Noto Sans CJK JP to DejaVu Sans" },
		},
	}
	scripts, err := Render(scriptInventory(), "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes, home := t.TempDir(), t.TempDir(), shortHome(t)
			closure, bins := filepath.Join(root, "closure"), filepath.Join(root, "bin")
			writeFakes(t, fakes, map[string]string{
				"ld.so":    fakeLdso,
				"fc-match": "#!/bin/sh\nprintf '%s\\n' \"fc-match $*\" >> \"$TEST_ROOT/fc-match.log\"\nprintf '%s' \"$FAMILY\"\n",
				"certutil": "#!/bin/sh\n",
			})
			writePluginTestFile(t, filepath.Join(home, ".agent-browser/chrome"), []byte("\x7fELF"), 0o755)
			if tt.linked {
				for _, bin := range []string{"certutil", "fc-match"} {
					writePluginTestFile(t, filepath.Join(root, "tree/usr/bin", bin), []byte("#!/bin/sh\n"), 0o755)
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
			).Replace(string(scripts.Plugins))
			writePluginTestFile(t, filepath.Join(fakes, "plugins.sh"), []byte(plugins), 0o700)
			cmd := exec.Command("bash", filepath.Join(fakes, "plugins.sh"), "verify")
			cmd.Env = append(append(os.Environ(), "HOME="+home, "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root, "FAMILY="+tt.family), tt.env...)
			out, err := cmd.CombinedOutput()
			if tt.wantErr != nil {
				if want := tt.wantErr(home, closure, bins); exitCode(err) != 1 || !strings.Contains(string(out), want) {
					t.Fatalf("verify = %v\n%s\nwant exit 1 with %q", err, out, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("verify failed: %v\n%s", err, out)
			}
			if got, want := logLines(t, filepath.Join(root, "ldso.log")), tt.wantLdso(home, closure, bins, fakes); !slices.Equal(got, want) {
				t.Errorf("ld.so calls = %q, want %q", got, want)
			}
			if got := logLines(t, filepath.Join(root, "fc-match.log")); !slices.Equal(got, []string{"fc-match -f %{family} Noto Sans CJK JP"}) {
				t.Errorf("fc-match calls = %q", got)
			}
		})
	}
}
