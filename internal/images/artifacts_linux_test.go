package images

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	assets "github.com/yasyf/cc-remote/images"
)

func TestInstallArtifactPinsAndLinks(t *testing.T) {
	archive, sum := writeArchive(t, "tool-1.0/bin/tool", "#!/bin/sh\necho tool 1.0\n")
	root := t.TempDir()
	install := installCall(Artifact{Name: "tool", Version: "1.0", URL: "file://" + archive, SHA256: sum, Format: TarGz, Bins: map[string]string{"tool": "tool-1.0/bin/tool"}}, "tool_dir", "bin_dir")
	verify := strings.Join(verifyCalls(Artifact{Name: "tool", Version: "1.0", SHA256: sum, Format: TarGz, Bins: map[string]string{"tool": "tool-1.0/bin/tool"}, Verify: []string{"--version"}}, "tool_dir", "bin_dir"), "\n")

	runLibrary(t, root, install+"\n"+verify)
	if out, err := exec.Command(filepath.Join(root, "bin", "tool")).Output(); err != nil || string(out) != "tool 1.0\n" {
		t.Fatalf("linked tool printed %q, %v", out, err)
	}

	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	runLibrary(t, root, install+"\n"+verify)
}

func TestInstallArtifactRejectsDigestMismatch(t *testing.T) {
	archive, _ := writeArchive(t, "tool", "#!/bin/sh\n")
	root := t.TempDir()
	wrong := strings.Repeat("0", 64)
	install := installCall(Artifact{Name: "tool", Version: "1.0", URL: "file://" + archive, SHA256: wrong, Format: TarGz, Bins: map[string]string{"tool": "tool"}}, "tool_dir", "bin_dir")

	out, err := library(root, install).CombinedOutput()
	if err == nil {
		t.Fatalf("install succeeded against the wrong digest: %s", out)
	}
	if want := "does not match its pinned sha256 " + wrong; !strings.Contains(string(out), want) {
		t.Errorf("output %q lacks %q", out, want)
	}
	if _, err := os.Lstat(filepath.Join(root, "bin", "tool")); !os.IsNotExist(err) {
		t.Errorf("a failed install linked the tool: %v", err)
	}
}

func TestVerifyRejectsAnUnpinnedLink(t *testing.T) {
	archive, sum := writeArchive(t, "tool", "#!/bin/sh\n")
	root := t.TempDir()
	artifact := Artifact{Name: "tool", Version: "1.0", URL: "file://" + archive, SHA256: sum, Format: TarGz, Bins: map[string]string{"tool": "tool"}}
	runLibrary(t, root, installCall(artifact, "tool_dir", "bin_dir"))
	if err := os.Symlink("/bin/sh", filepath.Join(root, "bin", "other")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "bin", "other"), filepath.Join(root, "bin", "tool")); err != nil {
		t.Fatal(err)
	}

	out, err := library(root, strings.Join(verifyCalls(artifact, "tool_dir", "bin_dir"), "\n")).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "does not point at the pinned") {
		t.Fatalf("verify accepted a repointed link: %v %s", err, out)
	}
}

func TestVerifyLinkChecksOnlySpellingUntilThePublishRunsTheTarget(t *testing.T) {
	tests := []struct {
		name      string
		check     string
		points    string
		installed bool
		wantErr   bool
	}{
		{name: "spelling accepts a link whose target is still installing", check: "spelling", points: "target"},
		{name: "spelling rejects a repointed link", check: "spelling", points: "other", wantErr: true},
		{name: "full rejects a link whose target is missing", check: "full", points: "target", wantErr: true},
		{name: "full runs an installed target with its verify args", check: "full", points: "target", installed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			target, link, marker := filepath.Join(root, "target"), filepath.Join(root, "link"), filepath.Join(root, "ran")
			if tt.installed {
				if err := os.WriteFile(target, []byte("#!/bin/sh\nprintf '%s' \"$*\" > "+quote(marker)+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(root, tt.points), link); err != nil {
				t.Fatal(err)
			}
			out, err := library(root, "link_check="+tt.check+"\nverify_link "+quote(link)+" "+quote(target)+" --version").CombinedOutput()
			if (err != nil) != tt.wantErr || tt.wantErr && !strings.Contains(string(out), "cc-remote: "+link+" does not point at the pinned "+target) {
				t.Fatalf("verify_link = %v\n%s\nwant error %v", err, out, tt.wantErr)
			}
			ran, err := os.ReadFile(marker)
			switch {
			case tt.installed && (err != nil || string(ran) != "--version"):
				t.Errorf("the target ran with %q, %v; want --version", ran, err)
			case !tt.installed && !os.IsNotExist(err):
				t.Errorf("the target ran with %q, %v; want it untouched", ran, err)
			}
		})
	}
}

func library(root, script string) *exec.Cmd {
	raw, err := assets.FS.ReadFile("artifacts.sh")
	if err != nil {
		panic(err)
	}
	prelude := "set -euo pipefail\ntool_dir=" + quote(filepath.Join(root, "tools")) + "\nbin_dir=" + quote(filepath.Join(root, "bin")) + "\ntmp_dir=\"$(mktemp -d)\"\ntrap 'status=$?; drain_artifacts || status=$?; rm -rf \"$tmp_dir\"; exit \"$status\"' EXIT\n"
	return exec.Command("bash", "-c", prelude+string(raw)+"\n"+script)
}

func runLibrary(t *testing.T, root, script string) {
	t.Helper()
	if out, err := library(root, script).CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
}

func writeArchive(t *testing.T, member, content string) (string, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "tool.tar.gz")
	out, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	zipped := gzip.NewWriter(out)
	archive := tar.NewWriter(zipped)
	if err := archive.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	for _, closer := range []interface{ Close() error }{archive, zipped, out} {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return file, hex.EncodeToString(sum[:])
}

func TestArtifactQueueOverlapsAndCapsDownloads(t *testing.T) {
	var active, peak atomic.Int32
	arrivals := make(chan string, 6)
	release := make(chan struct{})
	var once sync.Once
	unlatch := func() { once.Do(func() { close(release) }) }
	defer unlatch()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := active.Add(1)
		defer active.Add(-1)
		for prior := peak.Load(); count > prior; prior = peak.Load() {
			if peak.CompareAndSwap(prior, count) {
				break
			}
		}
		arrivals <- r.URL.Path
		<-release
		if _, err := fmt.Fprint(w, "#!/bin/sh\n"); err != nil {
			t.Errorf("write download: %v", err)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	var script strings.Builder
	for i := range 6 {
		artifact := binaryArtifact(fmt.Sprintf("tool%d", i), server.URL+fmt.Sprintf("/%d", i), "#!/bin/sh\n")
		script.WriteString("queue_artifact " + installCall(artifact, "tool_dir", "bin_dir") + "\n")
	}
	script.WriteString("drain_artifacts\n")
	done := startArtifactScript(library(root, script.String()))
	for range 4 {
		awaitArtifactArrival(t, arrivals, unlatch, done)
	}
	if got := peak.Load(); got != 4 {
		t.Errorf("peak downloads = %d, want 4", got)
	}
	select {
	case extra := <-arrivals:
		t.Errorf("fifth download started before a worker drained: %s", extra)
	default:
	}
	unlatch()
	finishArtifactScript(t, done, false)
	if got := peak.Load(); got > 4 {
		t.Errorf("peak downloads = %d, exceeds 4", got)
	}
	for i := range 6 {
		if _, err := os.Stat(filepath.Join(root, "bin", fmt.Sprintf("tool%d", i))); err != nil {
			t.Errorf("installed tool%d: %v", i, err)
		}
	}
}

func TestArtifactQueueStagesRepeatedNamesSeparately(t *testing.T) {
	arrivals := make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	unlatch := func() { once.Do(func() { close(release) }) }
	defer unlatch()
	payloads := map[string]string{"/first": "#!/bin/sh\necho first\n", "/second": "#!/bin/sh\necho second\n"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivals <- r.URL.Path
		<-release
		if _, err := fmt.Fprint(w, payloads[r.URL.Path]); err != nil {
			t.Errorf("write download: %v", err)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	var script strings.Builder
	for _, name := range []string{"first", "second"} {
		artifact := binaryArtifact("tool", server.URL+"/"+name, payloads["/"+name])
		artifact.Dest = ".tools/" + name
		artifact.Bins = map[string]string{name: "tool"}
		script.WriteString("queue_artifact " + installCall(artifact, "tool_dir", "bin_dir") + "\n")
	}
	script.WriteString("drain_artifacts\n")
	cmd := library(root, script.String())
	cmd.Env = append(os.Environ(), "HOME="+root)
	done := startArtifactScript(cmd)
	for range 2 {
		awaitArtifactArrival(t, arrivals, unlatch, done)
	}
	unlatch()
	finishArtifactScript(t, done, false)
	for name, want := range payloads {
		raw, err := os.ReadFile(filepath.Join(root, ".tools", name[1:], "tool"))
		if err != nil || string(raw) != want {
			t.Errorf("installed %s = %q, %v, want %q", name, raw, err, want)
		}
	}
}

func TestArtifactQueueDrainsFailuresBeforeCleanup(t *testing.T) {
	for _, count := range []int{2, 5} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			arrivals := make(chan string, count)
			release := make(chan struct{})
			var once sync.Once
			unlatch := func() { once.Do(func() { close(release) }) }
			defer unlatch()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrivals <- r.URL.Path
				if r.URL.Path != "/0" {
					<-release
				}
				if _, err := fmt.Fprint(w, "#!/bin/sh\n"); err != nil {
					t.Errorf("write download: %v", err)
				}
			}))
			defer server.Close()
			root := t.TempDir()
			var script strings.Builder
			script.WriteString("printf '%s' \"$tmp_dir\" > " + quote(filepath.Join(root, "temporary")) + "\n")
			for i := range count {
				artifact := binaryArtifact(fmt.Sprintf("tool%d", i), server.URL+fmt.Sprintf("/%d", i), "#!/bin/sh\n")
				if i == 0 {
					artifact.SHA256 = strings.Repeat("0", 64)
				}
				script.WriteString("queue_artifact " + installCall(artifact, "tool_dir", "bin_dir") + "\n")
			}
			script.WriteString("drain_artifacts\nprintf ready > " + quote(filepath.Join(root, "ready")) + "\n")
			done := startArtifactScript(library(root, script.String()))
			for range min(count, 4) {
				awaitArtifactArrival(t, arrivals, unlatch, done)
			}
			select {
			case result := <-done:
				unlatch()
				t.Fatalf("installer exited before outstanding downloads finished: %v %s", result.err, result.out)
			default:
			}
			unlatch()
			finishArtifactScript(t, done, true)
			for i := 1; i < min(count, 4); i++ {
				if _, err := os.Stat(filepath.Join(root, "tools", fmt.Sprintf("tool%d-1.0", i), ".cc-remote-digest")); err != nil {
					t.Errorf("worker %d did not finish before failure returned: %v", i, err)
				}
			}
			for _, absent := range []string{"ready", "bin/tool0", "bin/tool4"} {
				if _, err := os.Stat(filepath.Join(root, absent)); !os.IsNotExist(err) {
					t.Errorf("failed installer left %s: %v", absent, err)
				}
			}
			temporary, err := os.ReadFile(filepath.Join(root, "temporary"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(string(temporary)); !os.IsNotExist(err) {
				t.Errorf("temporary directory survived cleanup: %v", err)
			}
		})
	}
}

func binaryArtifact(name, url, content string) Artifact {
	sum := sha256.Sum256([]byte(content))
	return Artifact{Name: name, Version: "1.0", URL: url, SHA256: hex.EncodeToString(sum[:]), Format: Binary}
}

type artifactScriptResult struct {
	out []byte
	err error
}

func startArtifactScript(cmd *exec.Cmd) <-chan artifactScriptResult {
	done := make(chan artifactScriptResult, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		done <- artifactScriptResult{out: out, err: err}
	}()
	return done
}

func awaitArtifactArrival(t *testing.T, arrivals <-chan string, release func(), done <-chan artifactScriptResult) {
	t.Helper()
	select {
	case <-arrivals:
	case result := <-done:
		release()
		t.Fatalf("installer exited before concurrent downloads: %v %s", result.err, result.out)
	case <-time.After(10 * time.Second):
		release()
		finishArtifactScript(t, done, true)
		t.Fatal("installer did not overlap latched downloads")
	}
}

func finishArtifactScript(t *testing.T, done <-chan artifactScriptResult, wantFailure bool) {
	t.Helper()
	result := <-done
	if (result.err != nil) != wantFailure {
		t.Fatalf("installer error = %v, want failure %t: %s", result.err, wantFailure, result.out)
	}
}

func TestProvisionDrainsArtifactsAtDebianBarriers(t *testing.T) {
	root := t.TempDir()
	payload := "#!/bin/sh\n"
	var lock sync.Mutex
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		defer lock.Unlock()
		events = append(events, r.URL.Path)
		if _, err := os.Stat(filepath.Join(root, "apt-active")); !os.IsNotExist(err) {
			t.Errorf("artifact fetch overlapped apt: %v", err)
		}
		switch r.URL.Path {
		case "/second.deb":
			if _, err := os.Stat(filepath.Join(root, "first-apt")); err != nil {
				t.Errorf("second Debian fetch overlapped the first apt install: %v", err)
			}
		case "/first", "/second", "/third":
			if _, err := os.Stat(filepath.Join(root, "second-apt")); err != nil {
				t.Errorf("tools fetched %s before the packages phase finished: %v", r.URL.Path, err)
			}
		}
		if _, err := fmt.Fprint(w, payload); err != nil {
			t.Errorf("write download: %v", err)
		}
	}))
	defer server.Close()
	inventory := Inventory{Version: SchemaVersion}
	for _, name := range []string{"first", "first.deb", "second", "second.deb", "third"} {
		artifact := binaryArtifact(name, server.URL+"/"+name, payload)
		if strings.HasSuffix(name, ".deb") {
			artifact.Format = Deb
			artifact.Bins = map[string]string{name: filepath.Join(root, "deb-target")}
		}
		inventory.System = append(inventory.System, artifact)
	}
	scripts, err := Render(inventory, "agents")
	if err != nil {
		t.Fatal(err)
	}
	fakes := t.TempDir()
	fakeApt := `#!/bin/bash
set -euo pipefail
if ! mkdir "$TEST_ROOT/apt-active"; then
  exit 91
fi
trap 'rmdir "$TEST_ROOT/apt-active"' EXIT
for arg in "$@"; do
  case "$arg" in
    */first.deb-1.0.deb) touch "$TEST_ROOT/first-apt" ;;
    */second.deb-1.0.deb) touch "$TEST_ROOT/second-apt" ;;
  esac
done
`
	for name, content := range map[string]string{"id": "#!/bin/sh\necho 0\n", "apt-get": fakeApt, "apt-cache": "#!/bin/sh\nexit 1\n"} {
		if err := os.WriteFile(filepath.Join(fakes, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "deb-target"), []byte(payload), 0o700); err != nil {
		t.Fatal(err)
	}
	provision := strings.NewReplacer("tool_dir=/opt/cc-remote/tools", "tool_dir="+quote(filepath.Join(root, "tools")), "bin_dir=/usr/local/bin", "bin_dir="+quote(filepath.Join(root, "bin")), "rm -rf /var/lib/apt/lists/*", "test -f "+quote(filepath.Join(root, "second-apt"))).Replace(string(scripts.ProvisionScript))
	for _, phase := range []string{PhasePackages, PhaseTools} {
		cmd := exec.Command("bash", "-c", provision, "provision.sh", phase)
		cmd.Env = append(os.Environ(), "PATH="+fakes+":"+os.Getenv("PATH"), "TEST_ROOT="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("provision %s failed: %v %s", phase, err, out)
		}
	}
	lock.Lock()
	defer lock.Unlock()
	if len(events) != 5 {
		t.Fatalf("downloads = %v, want five", events)
	}
	if got, want := strings.Join(slices.Concat(events[:2], slices.Sorted(slices.Values(events[2:]))), ","), "/first.deb,/second.deb,/first,/second,/third"; got != want {
		t.Errorf("download order = %s, want %s", got, want)
	}
}

func TestProvisionPrerequisitesRunAptOnlyWhenOneIsMissing(t *testing.T) {
	install := "update -qq\ninstall -y -qq --no-install-recommends ca-certificates curl git jq python3 unzip xz-utils\n"
	tests := []struct {
		name    string
		missing string
		apt     string
	}{
		{name: "everything present runs no apt"},
		{name: "a missing command installs the prerequisites", missing: "xz", apt: install},
		{name: "a missing CA bundle installs the prerequisites", missing: "ca-certificates.crt", apt: install},
	}
	scripts, err := Render(Inventory{Version: SchemaVersion}, "agents")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, fakes := t.TempDir(), t.TempDir()
			bundle := filepath.Join(root, "ca-certificates.crt")
			files := map[string]string{
				"id":                  "#!/bin/sh\necho 0\n",
				"apt-get":             "#!/bin/sh\necho \"$*\" >> \"$TEST_ROOT/apt\"\n",
				"curl":                "#!/bin/sh\n",
				"git":                 "#!/bin/sh\n",
				"jq":                  "#!/bin/sh\n",
				"python3":             "#!/bin/sh\n",
				"unzip":               "#!/bin/sh\n",
				"xz":                  "#!/bin/sh\n",
				"ca-certificates.crt": "",
			}
			delete(files, tt.missing)
			for name, content := range files {
				path := filepath.Join(fakes, name)
				if name == "ca-certificates.crt" {
					path = bundle
				}
				if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"mktemp", "rm"} {
				target, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(fakes, name)); err != nil {
					t.Fatal(err)
				}
			}
			provision := strings.ReplaceAll(string(scripts.ProvisionScript), "/etc/ssl/certs/ca-certificates.crt", bundle)
			cmd := exec.Command("bash", "-c", provision, "provision.sh", PhasePrerequisites)
			cmd.Env = append(os.Environ(), "PATH="+fakes, "TEST_ROOT="+root)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("provision prerequisites failed: %v\n%s", err, out)
			}
			apt, err := os.ReadFile(filepath.Join(root, "apt"))
			switch {
			case tt.apt == "" && !os.IsNotExist(err):
				t.Errorf("apt-get ran %q, %v; want no apt run", apt, err)
			case tt.apt != "" && (err != nil || string(apt) != tt.apt):
				t.Errorf("apt-get ran %q, %v; want %q", apt, err, tt.apt)
			}
		})
	}
}

func TestPluginsDrainArtifactsBeforeConsumers(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			archive, sum := writeArchive(t, "tool", "#!/bin/sh\n")
			tool := Artifact{Name: "tool", Version: "1.0", URL: "file://" + archive, SHA256: sum, Format: TarGz, Bins: map[string]string{"tool": "tool"}}
			if fail {
				tool.SHA256 = strings.Repeat("0", 64)
			}
			inventory := Inventory{Version: SchemaVersion, Tools: []Artifact{tool}, Prepare: []string{"test -f \"$HOME/.local/share/cc-remote/tools/tool-1.0/.cc-remote-digest\"", "touch \"$HOME/consumer\""}}
			host := newPluginsHost(t, inventory, nil, fakeState{}, nil)
			out, err := host.plugins("install")
			if (err != nil) != fail {
				t.Fatalf("install error = %v, want failure %t: %s", err, fail, out)
			}
			_, err = os.Stat(filepath.Join(host.home, "consumer"))
			if fail && !os.IsNotExist(err) {
				t.Errorf("failed install reached the consumer: %v", err)
			}
			if !fail && err != nil {
				t.Errorf("successful install omitted the consumer: %v", err)
			}
			host.unready()
		})
	}
}

func TestPluginsWorkersCannotConsumeTokenInput(t *testing.T) {
	archive, sum := writeArchive(t, "tool", "#!/bin/sh\n")
	tool := Artifact{Name: "tool", Version: "1.0", URL: "file://" + archive, SHA256: sum, Format: TarGz, Bins: map[string]string{"tool": "tool"}}
	inventory := Inventory{Version: SchemaVersion, Tools: []Artifact{tool}, Prepare: []string{"IFS= read -r remaining", "test \"$remaining\" = retained-input"}}
	host := newPluginsHost(t, inventory, nil, fakeState{}, nil)
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	fakeCurl := "#!/bin/bash\nset -euo pipefail\nif IFS= read -r line; then exit 92; fi\nif env | grep -q '^github_token='; then exit 93; fi\nexec " + quote(curl) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(host.fakes, "curl"), []byte(fakeCurl), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(host.fakes, "plugins.sh"), "install")
	cmd.Env = append(os.Environ(), "HOME="+host.home, "PATH="+host.fakes+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader("synthetic-token\nretained-input\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worker inherited token input or environment: %v %s", err, out)
	}
}
