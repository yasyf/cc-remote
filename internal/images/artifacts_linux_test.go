package images

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

func library(root, script string) *exec.Cmd {
	raw, err := assets.FS.ReadFile("artifacts.sh")
	if err != nil {
		panic(err)
	}
	prelude := "set -euo pipefail\ntool_dir=" + quote(filepath.Join(root, "tools")) + "\nbin_dir=" + quote(filepath.Join(root, "bin")) + "\ntmp_dir=\"$(mktemp -d)\"\n"
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
